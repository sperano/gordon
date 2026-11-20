package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
)

// ErrNoRun is a run the store does not have.
var ErrNoRun = errors.New("no such run")

// SubmitRun is what a submission needs: the generation's inbox row (the
// marker its Task carries), the run's state and the Task built for it.
type SubmitRun struct {
	Name    string
	InboxID int64
	Variant string
	State   string
	Task    *kelos.Task
}

// Submitted reports a run no longer submitting: its Task created by this
// generation, taken by another submitter's Task, or the run finished.
// Either way nothing is left to submit.
func (r SubmitRun) Submitted() bool { return r.State != runSubmitting }

// RunToSubmit reads a run by Task name; ErrNoRun when there is none.
func (s *Store) RunToSubmit(ctx context.Context, name string) (SubmitRun, error) {
	r := SubmitRun{Name: name}
	err := s.pool.QueryRow(ctx, `SELECT inbox_id, coalesce(identity->>'variant', ''), state, task
		FROM runs WHERE name = $1`, name).Scan(&r.InboxID, &r.Variant, &r.State, &r.Task)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubmitRun{}, ErrNoRun
	}
	return r, err
}

// RunSubmitted records the Task of this generation (inboxID) as created;
// a newer generation's row is left alone. Its uid is recorded whatever
// the state; its state and phase only while the run is still
// submitting: an observer that already followed the Task knows better.
func (s *Store) RunSubmitted(ctx context.Context, name string, inboxID int64, uid, phase string) error {
	_, err := s.pool.Exec(ctx, `UPDATE runs SET task_uid = $4,
			state = CASE WHEN state = 'submitting' THEN $3 ELSE state END,
			task_exists = task_exists OR state = 'submitting',
			phase = CASE WHEN state = 'submitting' THEN $5 ELSE phase END,
			updated_at = now()
		WHERE name = $1 AND inbox_id = $2`,
		name, inboxID, stateOf(phase), uid, phase)
	return err
}

func stateOf(phase string) string {
	if phase == kelos.PhaseSucceeded || phase == kelos.PhaseFailed {
		return runFinished
	}
	return runActive
}

// ObservedRun is a run whose Task the observer follows: not finished, or
// finished with its Task still present.
type ObservedRun struct {
	Name    string
	InboxID int64
	State   string
	Exists  bool
	Phase   string
	// SubmitState is the state of this generation's submit action ("" if
	// none): while it is not done, a missing Task is not yet created.
	SubmitState string
}

// ObservedRuns takes the runs the observer follows, at most limit, the
// least recently observed first, and marks them observed now: unchanged
// runs go to the back, and concurrent observers take disjoint batches.
func (s *Store) ObservedRuns(ctx context.Context, limit int) ([]ObservedRun, error) {
	rows, err := s.pool.Query(ctx, `WITH taken AS (
			UPDATE runs SET observed_at = now()
			WHERE name IN (SELECT name FROM runs WHERE state <> 'finished' OR task_exists
				ORDER BY observed_at, name LIMIT $1 FOR UPDATE SKIP LOCKED)
			RETURNING name, inbox_id, state, task_exists, phase)
		SELECT r.name, r.inbox_id, r.state, r.task_exists, r.phase,
			coalesce((SELECT a.state FROM actions a WHERE a.inbox_id = r.inbox_id AND a.key = $2 || r.name), '')
		FROM taken r ORDER BY r.name`, limit, submitKeyPrefix())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ObservedRun, error) {
		var o ObservedRun
		err := r.Scan(&o.Name, &o.InboxID, &o.State, &o.Exists, &o.Phase, &o.SubmitState)
		return o, err
	})
}

// submitKeyPrefix is the submit action key of a run, less its name.
func submitKeyPrefix() string {
	return action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, ""), "").Key
}

// ObserveRun records what Kelos reports about a run's generation: its
// Task present with a phase, or gone (exists false: finished, the last
// observed phase kept; phase is then ignored). A finished
// run never goes back: an observation read before another replica's
// (a Running phase, a Task since cleaned up) changes nothing.
func (s *Store) ObserveRun(ctx context.Context, name string, inboxID int64, exists bool, phase string) error {
	state := runFinished
	if exists {
		state = stateOf(phase)
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET state = $3, task_exists = $4,
		phase = CASE WHEN $4 THEN $5 ELSE phase END, updated_at = now()
		WHERE name = $1 AND inbox_id = $2
			AND (state <> 'finished' OR ($3 = 'finished' AND (task_exists OR NOT $4)))`,
		name, inboxID, state, exists, phase)
	return err
}

// PruneTasks drops the built Tasks (their prompts quote the task and its
// comments) of runs finished, and of shadow Tasks compared, more than
// retention ago.
func (s *Store) PruneTasks(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET task = NULL
		WHERE task IS NOT NULL AND state = 'finished' AND updated_at < now() - make_interval(secs => $1)`,
		retention.Seconds())
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `UPDATE shadow_tasks SET task = NULL
		WHERE task IS NOT NULL AND result <> 'pending' AND compared_at < now() - make_interval(secs => $1)`,
		retention.Seconds())
	return n + tag.RowsAffected(), err
}
