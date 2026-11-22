package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// What the limit restart pass decided about a failed run
// (runs.limit_restart): limit_reaper.py's SKIPPED reasons, the restart,
// and LimitNone for a run that failed on anything but a limit.
const (
	LimitRestarted   = "restarted"
	LimitSuperseded  = "superseded"
	LimitGaveUp      = "gave-up"
	LimitNameTooLong = "name-too-long"
	LimitNone        = "none"
)

// FailedRun is a run of Gordon's whose Task failed and is still present,
// which the limit restart pass has not decided about yet.
type FailedRun struct {
	Name       string
	InboxID    int64
	RequestKey string
	Project    string
	Identity   run.Identity
	Profile    profile.Resolved
}

// FailedUndecided returns at most limit undecided failed runs of the
// projects, the least recently updated first.
func (s *Store) FailedUndecided(ctx context.Context, projects []string, limit int) ([]FailedRun, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, inbox_id, request_key, project, identity, profile FROM runs
		WHERE state = 'finished' AND task_exists AND phase = 'Failed' AND limit_restart = '' AND project = ANY($1)
		ORDER BY updated_at, name LIMIT $2`, projects, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (FailedRun, error) {
		var f FailedRun
		err := r.Scan(&f.Name, &f.InboxID, &f.RequestKey, &f.Project, &f.Identity, &f.Profile)
		return f, err
	})
}

// DecideFailed records the pass's decision about a failed run and, in the
// same transaction, the restart run it submits (nil for none) and fd's
// actions, queued on the failed run's inbox row: the restart is a run of
// that generation, its Task carrying the same inbox marker. A restart
// whose name another run holds drops its submission and what waits for
// it. It reports false, writing nothing, when the run was decided
// already (another replica) or is no longer this generation.
func (s *Store) DecideFailed(ctx context.Context, f FailedRun, mark string, restart *route.Run, fd route.FamilyDecision) (bool, error) {
	decided := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE runs SET limit_restart = $3, updated_at = now()
			WHERE name = $1 AND inbox_id = $2 AND limit_restart = ''`, f.Name, f.InboxID, mark)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		decided = true
		if restart != nil {
			taken, _, err := recordRun(ctx, tx, f.InboxID, f.RequestKey, *restart)
			if err != nil {
				return err
			}
			if taken {
				submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, restart.Name), "").Key
				fd.Actions = dropWithDependents(fd.Actions, submit)
			}
		}
		return insertActions(ctx, tx, f.InboxID, &fd)
	})
	if err != nil {
		return false, err
	}
	return decided, nil
}

// Completion is one finished run generation, as its Task reported it.
type Completion struct {
	Name    string
	InboxID int64
	Family  run.Family
	Project string
	TaskUID string
	Phase   string
	// Completed is the Task's completion time, nil when it has none.
	Completed *time.Time
	// Results are the Task's outputs, the agent's answer left out.
	Results map[string]string
}

// RecordCompletion records a finished run generation once.
func (s *Store) RecordCompletion(ctx context.Context, c Completion) error {
	results := c.Results
	if results == nil {
		results = map[string]string{}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO run_completions
			(name, inbox_id, family, project, task_uid, phase, completed_at, results)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (name, inbox_id) DO NOTHING`,
		c.Name, c.InboxID, string(c.Family), c.Project, c.TaskUID, c.Phase, c.Completed, results)
	return err
}

// RunsTaken returns which of names are runs still active or whose Task
// is still present: a new request of the same name is a duplicate.
func (s *Store) RunsTaken(ctx context.Context, names []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM runs WHERE name = ANY($1) AND (state <> 'finished' OR task_exists)`, names)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := make(map[string]bool, len(found))
	for _, n := range found {
		out[n] = true
	}
	return out, err
}
