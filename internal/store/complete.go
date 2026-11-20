package store

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
)

// Run states. Gordon 04's Kelos observer moves a run to active and
// finished and keeps task_exists current; until then a submitted run
// stays submitting, which the router treats as active.
const (
	runSubmitting = "submitting"
	runFinished   = "finished"
)

// requestKinds are the event kinds whose request key names an owner
// request the router deduplicates (a `/gordon` comment or review). A
// task.updated's request key names the task, which changes many times.
var requestKinds = map[event.Kind]bool{
	event.VikunjaTaskCommentCreated: true,
	event.GitHubIssueComment:        true,
	event.GitHubReview:              true,
	event.GitHubReviewComment:       true,
}

// Complete finishes a claimed row with its decision, in one transaction
// fenced by the lease: the decision; for each family Gordon executes
// (owner gordon, valid table, shadow false), its runs and its actions in
// the outbox; and the handled request. With shadow, nothing is queued for
// execution and no run is recorded.
//
// A run another worker recorded since the router read the ledger is
// dropped with its submit action, so two deliveries of one request never
// queue two submissions; Complete returns the decision as recorded.
func (s *Store) Complete(ctx context.Context, l Lease, dec route.Decision, shadow bool) (route.Decision, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var kind, requestKey string
		err := tx.QueryRow(ctx, `UPDATE inbox SET state = 'done', finished_at = now(), last_error = '',
				lease_token = NULL, lease_owner = NULL, lease_until = NULL
			WHERE id = $1 AND lease_token = $2::uuid AND state = 'pending'
			RETURNING kind, request_key`, l.ID, l.Token).Scan(&kind, &requestKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		executed := false
		for i := range dec.Families {
			fd := &dec.Families[i]
			if shadow || !fd.Execute || (fd.Outcome != route.Dispatch && fd.Outcome != route.Act) {
				continue
			}
			if err := recordFamily(ctx, tx, l.ID, requestKey, fd); err != nil {
				return err
			}
			executed = executed || len(fd.Runs) > 0 || len(fd.Actions) > 0
		}
		if len(dec.Families) > 0 {
			dec.Outcome = route.Overall(dec.Families)
		}
		if requestKinds[event.Kind(kind)] && requestKey != "" &&
			(dec.Outcome == route.Dispatch || dec.Outcome == route.Act) {
			if _, err := tx.Exec(ctx, `INSERT INTO requests (request_key, inbox_id, outcome, executed)
				VALUES ($1, $2, $3, $4) ON CONFLICT (request_key) DO NOTHING`,
				requestKey, l.ID, string(dec.Outcome), executed); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO decisions (inbox_id, outcome, reason, executed, decision)
			VALUES ($1, $2, $3, $4, $5)`, l.ID, string(dec.Outcome), dec.Reason, executed, dec)
		return err
	})
	return dec, err
}

// recordFamily records an executed family's runs and queues its actions.
func recordFamily(ctx context.Context, tx pgx.Tx, inboxID int64, requestKey string, fd *route.FamilyDecision) error {
	runs := fd.Runs[:0:0]
	dupReason := ""
	for _, rn := range fd.Runs {
		taken, reason, err := recordRun(ctx, tx, inboxID, requestKey, rn)
		if err != nil {
			return err
		}
		if !taken {
			runs = append(runs, rn)
			continue
		}
		dupReason = reason
		submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, rn.Name), "").Key
		fd.Actions = slices.DeleteFunc(fd.Actions, func(a action.Action) bool { return a.Key == submit })
	}
	fd.Runs = runs
	switch {
	case len(fd.Runs) == 0 && len(fd.Actions) == 0:
		fd.Outcome, fd.Reason = route.Duplicate, dupReason
		return nil
	case len(fd.Runs) == 0:
		fd.Outcome = route.Act
	}
	for _, a := range fd.Actions {
		if _, err := tx.Exec(ctx, `INSERT INTO actions
			(inbox_id, key, kind, target, value, detail, family, project)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (inbox_id, key) DO NOTHING`,
			inboxID, a.Key, string(a.Kind), string(a.Target), a.Value, a.Detail,
			string(fd.Family), fd.Project); err != nil {
			return err
		}
	}
	return nil
}

// recordRun records a new run, or a new generation of a finished run
// whose Task is gone (the owner's restart after cleanup); taken reports a
// run that is still active or present, with the router's reason.
func recordRun(ctx context.Context, tx pgx.Tx, inboxID int64, requestKey string, rn route.Run) (taken bool, reason string, err error) {
	rec, ok, err := runRecord(ctx, tx, rn.Name, true)
	if err != nil {
		return false, "", err
	}
	if ok && (rec.Active || rec.Exists) {
		return true, takenReason(rec), nil
	}
	var name string
	err = tx.QueryRow(ctx, `INSERT INTO runs
			(name, family, project, request_key, inbox_id, identity, profile, config_version, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (name) DO UPDATE SET family = EXCLUDED.family, project = EXCLUDED.project,
			request_key = EXCLUDED.request_key, inbox_id = EXCLUDED.inbox_id,
			identity = EXCLUDED.identity, profile = EXCLUDED.profile,
			config_version = EXCLUDED.config_version, state = EXCLUDED.state,
			task_exists = false, generation = runs.generation + 1, updated_at = now()
		WHERE runs.state = 'finished' AND NOT runs.task_exists
		RETURNING name`,
		rn.Name, string(rn.Identity.Family), rn.Identity.Project, requestKey, inboxID,
		rn.Identity, rn.Profile, rn.Profile.ConfigVersion, runSubmitting).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another transaction inserted the run after our read.
		return true, route.ReasonRunActive, nil
	}
	return false, "", err
}

func takenReason(rec route.RunRecord) string {
	if rec.Active {
		return route.ReasonRunActive
	}
	return route.ReasonRunExists
}
