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

// Run states: submitting until its Task is created, active while the
// Task has not finished, finished once it has (or is gone). The Kelos
// observer (internal/submit) keeps state and task_exists current.
const (
	runSubmitting = "submitting"
	runActive     = "active"
	runFinished   = "finished"
)

// RunFinished is a finished run's state.
const RunFinished = runFinished

// RunSubmitting is the state of a run whose Task is not created yet.
const RunSubmitting = runSubmitting

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
// fenced by the lease: the decision; for each family that has runs or
// actions, its runs and its actions in the outbox (an unanswered owner
// request's reaction and notice included); the decision's requeues
// (synthetic inbox rows); and the handled request.
//
// The owner request is claimed first: when another delivery of it was
// recorded since the router read the ledger, this one queues nothing and
// is a duplicate. A run another worker recorded meanwhile is dropped with
// its submit action, so two deliveries never queue two submissions.
// Complete returns the decision as recorded.
func (s *Store) Complete(ctx context.Context, l Lease, dec route.Decision) (route.Decision, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		kind, requestKey, err := finishItem(ctx, tx, l)
		if err != nil {
			return err
		}
		claims := requestKinds[event.Kind(kind)] && requestKey != "" && recordsRequest(dec)
		if claims {
			if claims, err = claimRequest(ctx, tx, requestKey, l.ID, dec.Outcome); err != nil {
				return err
			}
			if !claims {
				dec = handledElsewhere(dec)
			}
		}
		executed := false
		for i := range dec.Families {
			fd := &dec.Families[i]
			if len(fd.Runs) == 0 && len(fd.Actions) == 0 {
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
		if _, err := enqueue(ctx, tx, requeues(dec)); err != nil {
			return err
		}
		if claims { // this row holds the request: record its final outcome
			if _, err := tx.Exec(ctx, `UPDATE requests SET outcome = $2, executed = $3 WHERE request_key = $1`,
				requestKey, string(dec.Outcome), executed); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO decisions (inbox_id, outcome, reason, executed, decision)
			VALUES ($1, $2, $3, $4, $5)`, l.ID, string(dec.Outcome), dec.Reason, executed, dec)
		return err
	})
	return dec, err
}

// finishItem marks the leased row done; ErrLeaseLost when the lease is
// no longer this worker's.
func finishItem(ctx context.Context, tx pgx.Tx, l Lease) (kind, requestKey string, err error) {
	err = tx.QueryRow(ctx, `UPDATE inbox SET state = 'done', finished_at = now(), last_error = '',
			lease_token = NULL, lease_owner = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2::uuid AND state = 'pending'
		RETURNING kind, request_key`, l.ID, l.Token).Scan(&kind, &requestKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrLeaseLost
	}
	return kind, requestKey, err
}

// recordsRequest reports a decision that handles its owner request: one
// that dispatches or acts, or queues writes (an unanswered request's
// reaction and notice).
func recordsRequest(dec route.Decision) bool {
	o := dec.Outcome
	if len(dec.Families) > 0 {
		o = route.Overall(dec.Families)
	}
	if o == route.Dispatch || o == route.Act {
		return true
	}
	for _, fd := range dec.Families {
		if len(fd.Runs) > 0 || len(fd.Actions) > 0 {
			return true
		}
	}
	return false
}

// claimRequest records the owner request as handled by this row; fresh is
// false when another delivery's decision already did. A concurrent claim
// waits on the key until that transaction ends.
func claimRequest(ctx context.Context, tx pgx.Tx, key string, inboxID int64, o route.Outcome) (fresh bool, err error) {
	var got string
	err = tx.QueryRow(ctx, `INSERT INTO requests (request_key, inbox_id, outcome, executed)
		VALUES ($1, $2, $3, false) ON CONFLICT (request_key) DO NOTHING RETURNING request_key`,
		key, inboxID, string(o)).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// handledElsewhere is dec once another delivery handled its request:
// every family a duplicate, nothing to run or write.
func handledElsewhere(dec route.Decision) route.Decision {
	families := make([]route.FamilyDecision, len(dec.Families))
	for i, fd := range dec.Families {
		fd.Outcome, fd.Reason, fd.Runs, fd.Actions = route.Duplicate, route.ReasonRequestHandled, nil, nil
		families[i] = fd
	}
	dec.Families, dec.Outcome, dec.Reason = families, route.Duplicate, route.ReasonRequestHandled
	return dec
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
		fd.Actions = dropWithDependents(fd.Actions, submit)
	}
	fd.Runs = runs
	switch {
	case dupReason == "":
		// No run dropped: the router's outcome stands (an unanswered
		// request keeps its ignore, stale or conflict).
	case len(fd.Runs) == 0 && len(fd.Actions) == 0:
		fd.Outcome, fd.Reason = route.Duplicate, dupReason
		return nil
	case len(fd.Runs) == 0:
		fd.Outcome = route.Act
	}
	return insertActions(ctx, tx, inboxID, fd)
}

// insertActions queues a family's actions on an inbox row, each key once.
func insertActions(ctx context.Context, tx pgx.Tx, inboxID int64, fd *route.FamilyDecision) error {
	for _, a := range fd.Actions {
		if _, err := tx.Exec(ctx, `INSERT INTO actions
			(inbox_id, key, kind, target, value, detail, family, project, after_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (inbox_id, key) DO NOTHING`,
			inboxID, a.Key, string(a.Kind), string(a.Target), a.Value, a.Detail,
			string(fd.Family), fd.Project, a.After); err != nil {
			return err
		}
	}
	return nil
}

// dropWithDependents removes the action keyed key and every action that
// waits for it, transitively: a run another worker took is claimed by
// that worker's decision, not this one's.
func dropWithDependents(actions []action.Action, key string) []action.Action {
	gone := map[string]bool{key: true}
	for changed := true; changed; {
		changed = false
		for _, a := range actions {
			if !gone[a.Key] && a.After != "" && gone[a.After] {
				gone[a.Key], changed = true, true
			}
		}
	}
	return slices.DeleteFunc(slices.Clone(actions), func(a action.Action) bool { return gone[a.Key] })
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
			(name, family, project, request_key, inbox_id, identity, profile, config_version, state, task)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (name) DO UPDATE SET family = EXCLUDED.family, project = EXCLUDED.project,
			request_key = EXCLUDED.request_key, inbox_id = EXCLUDED.inbox_id,
			identity = EXCLUDED.identity, profile = EXCLUDED.profile,
			config_version = EXCLUDED.config_version, state = EXCLUDED.state, task = EXCLUDED.task,
			task_uid = '', phase = '', limit_restart = '',
			task_exists = false, generation = runs.generation + 1, observed_at = '-infinity',
			updated_at = now()
		WHERE runs.state = 'finished' AND NOT runs.task_exists
		RETURNING name`,
		rn.Name, string(rn.Identity.Family), rn.Identity.Project, requestKey, inboxID,
		rn.Identity, rn.Profile, rn.Profile.ConfigVersion, runSubmitting, rn.Task).Scan(&name)
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
