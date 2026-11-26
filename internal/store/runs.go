package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// ErrNoRun is a run the store does not have.
var ErrNoRun = errors.New("no such run")

// SubmitRun is what a submission needs: the generation's inbox row (the
// marker its Task carries), the run's state and the Task built for it.
type SubmitRun struct {
	Name    string
	InboxID int64
	Variant string
	Family  run.Family
	State   string
	Task    *kelos.Task
	// Project and TaskID locate the run on its Vikunja task (0 for a run
	// of none), for the notice of a held submission.
	Project string
	TaskID  int64
}

// Submitted reports a run no longer submitting: its Task created by this
// generation, taken by another submitter's Task, or the run finished.
// Either way nothing is left to submit.
func (r SubmitRun) Submitted() bool { return r.State != runSubmitting }

// Stopped reports a run finished before its Task was created by this
// submission (a drag-cancel, or the observer finishing it).
func (r SubmitRun) Stopped() bool { return r.State == runFinished }

// RunToSubmit reads a run by Task name; ErrNoRun when there is none.
func (s *Store) RunToSubmit(ctx context.Context, name string) (SubmitRun, error) {
	r := SubmitRun{Name: name}
	err := s.pool.QueryRow(ctx, `SELECT inbox_id, coalesce(identity->>'variant', ''), family, state, task,
			project, coalesce((identity->>'task_id')::bigint, 0)
		FROM runs WHERE name = $1`, name).Scan(&r.InboxID, &r.Variant, &r.Family, &r.State, &r.Task, &r.Project, &r.TaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubmitRun{}, ErrNoRun
	}
	return r, err
}

// RunSubmitted records the Task of this generation (inboxID) as created;
// a newer generation's row is left alone. Its uid is recorded whatever
// the state; its state and phase only while the run is still
// submitting: an observer that already followed the Task knows better.
// A Task found already finished (a recovered submission) is recorded
// active with no phase, so the observer sees it finish and tells its
// hook (the failure notice) as for any other run.
func (s *Store) RunSubmitted(ctx context.Context, name string, inboxID int64, uid, phase string) error {
	if stateOf(phase) == runFinished {
		phase = ""
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET task_uid = $3,
			state = CASE WHEN state = 'submitting' THEN '`+runActive+`' ELSE state END,
			task_exists = task_exists OR state = 'submitting',
			phase = CASE WHEN state = 'submitting' THEN $4 ELSE phase END,
			updated_at = now()
		WHERE name = $1 AND inbox_id = $2`,
		name, inboxID, uid, phase)
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
	// Family, Project and TaskID locate the run on its Vikunja task (0
	// for a run of none), for the lifecycle's failure notices.
	Family  run.Family
	Project string
	TaskID  int64
	// PR is a PR follow-up's or conflict run's pull request (0 for none),
	// for their failure notices.
	PR int
}

// ObservedRuns takes the runs the observer follows, at most limit, the
// least recently observed first, and marks them observed now: unchanged
// runs go to the back, and concurrent observers take disjoint batches.
func (s *Store) ObservedRuns(ctx context.Context, limit int) ([]ObservedRun, error) {
	rows, err := s.pool.Query(ctx, `WITH taken AS (
			UPDATE runs SET observed_at = now()
			WHERE name IN (SELECT name FROM runs WHERE state <> 'finished' OR task_exists
				ORDER BY observed_at, name LIMIT $1 FOR UPDATE SKIP LOCKED)
			RETURNING name, inbox_id, state, task_exists, phase, family, project,
				coalesce((identity->>'task_id')::bigint, 0) AS task_id, coalesce((identity->>'pr')::int, 0) AS pr)
		SELECT r.name, r.inbox_id, r.state, r.task_exists, r.phase,
			coalesce((SELECT a.state FROM actions a WHERE a.inbox_id = r.inbox_id AND a.key = $2 || r.name), ''),
			r.family, r.project, r.task_id, r.pr
		FROM taken r ORDER BY r.name`, limit, submitKeyPrefix())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ObservedRun, error) {
		var o ObservedRun
		var family string
		err := r.Scan(&o.Name, &o.InboxID, &o.State, &o.Exists, &o.Phase, &o.SubmitState, &family, &o.Project, &o.TaskID, &o.PR)
		o.Family = run.Family(family)
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
// comments) of runs finished more than retention ago.
func (s *Store) PruneTasks(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET task = NULL
		WHERE task IS NOT NULL AND state = 'finished' AND updated_at < now() - make_interval(secs => $1)`,
		retention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// HeldTasks returns which of the Vikunja tasks have a run of families
// still submitting: a run Gordon may hold back from Kelos.
func (s *Store) HeldTasks(ctx context.Context, ids []int64, families []run.Family) (map[int64]bool, error) {
	fs := make([]string, len(families))
	for i, f := range families {
		fs[i] = string(f)
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT (identity->>'task_id')::bigint FROM runs
		WHERE state = '`+runSubmitting+`' AND family = ANY($2)
		  AND identity->>'task_id' = ANY(SELECT unnest($1::bigint[])::text)`, ids, fs)
	if err != nil {
		return nil, err
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(held))
	for _, id := range held {
		out[id] = true
	}
	return out, nil
}

// TaskHeldBesides reports a run of families other than name, of the
// Vikunja task, still submitting: one more run Gordon holds back on it.
func (s *Store) TaskHeldBesides(ctx context.Context, task int64, name string, families []run.Family) (bool, error) {
	fs := make([]string, len(families))
	for i, f := range families {
		fs[i] = string(f)
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs
		WHERE state = '`+runSubmitting+`' AND family = ANY($3) AND name <> $2
		  AND identity->>'task_id' = $1::bigint::text)`, task, name, fs).Scan(&held)
	return held, err
}
