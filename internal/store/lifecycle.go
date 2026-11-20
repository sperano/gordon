package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// PhaseCancelled is the phase of a run stopped before its Task existed.
const PhaseCancelled = "Cancelled"

// RunStopped is why a stopped run's submission was cancelled, after the
// cancelled prefix in the action's last error.
const RunStopped = "run stopped"

// CancelRuns stops the named runs that are not finished: each becomes
// finished (a queued submission is never created), its pending or
// ambiguous submission and every action waiting for it are cancelled. It
// returns the names it stopped. The caller holds the admission lock, so
// no submission of these runs is between its check and its create.
func (s *Store) CancelRuns(ctx context.Context, names []string) ([]string, error) {
	var stopped []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE runs SET state = 'finished', updated_at = now(),
				phase = CASE WHEN task_exists THEN phase ELSE '`+PhaseCancelled+`' END
			WHERE name = ANY($1) AND state <> 'finished' RETURNING name, inbox_id`, names)
		if err != nil {
			return err
		}
		type gen struct {
			name    string
			inboxID int64
		}
		gens, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (gen, error) {
			var g gen
			err := r.Scan(&g.name, &g.inboxID)
			return g, err
		})
		if err != nil {
			return err
		}
		for _, g := range gens {
			submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, g.name), "").Key
			if _, err := tx.Exec(ctx, `UPDATE actions SET state = 'done', finished_at = now(),
					last_error = '`+cancelledPrefix+RunStopped+`'
				WHERE inbox_id = $1 AND (key = $2 OR after_key = $2) AND state IN ('pending', 'ambiguous')`,
				g.inboxID, submit); err != nil {
				return err
			}
			stopped = append(stopped, g.name)
		}
		return nil
	})
	return stopped, err
}

// RunInfo is what a run's notices name.
type RunInfo struct {
	Name     string
	Family   run.Family
	Identity run.Identity
	Model    string
	Effort   string
	TaskUID  string
	Phase    string
	State    string
}

// ErrUnknownRun is a run name the store does not have.
var ErrUnknownRun = errors.New("unknown run")

// Run reads a run's identity, profile and Kelos state.
func (s *Store) RunInfo(ctx context.Context, name string) (RunInfo, error) {
	r := RunInfo{Name: name}
	var family string
	err := s.pool.QueryRow(ctx, `SELECT family, identity, coalesce(profile->>'model', ''),
			coalesce(profile->>'effort', ''), task_uid, phase, state
		FROM runs WHERE name = $1`, name).Scan(&family, &r.Identity, &r.Model, &r.Effort, &r.TaskUID, &r.Phase, &r.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunInfo{}, ErrUnknownRun
	}
	r.Family = run.Family(family)
	return r, err
}

// NewerRun reports whether a run of the same Vikunja task in one of
// families was created after the named run: the owner asked again since.
func (s *Store) NewerRun(ctx context.Context, name string, families []run.Family) (bool, error) {
	fs := make([]string, len(families))
	for i, f := range families {
		fs[i] = string(f)
	}
	var newer bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs o, runs r
		WHERE r.name = $1 AND o.name <> r.name AND o.family = ANY($2)
		  AND o.identity->>'task_id' = r.identity->>'task_id' AND o.created_at > r.created_at)`,
		name, fs).Scan(&newer)
	return newer, err
}

// QueueRunActions queues a run generation's lifecycle actions (its
// failure notice and labels) on its inbox row, each key at most once.
func (s *Store) QueueRunActions(ctx context.Context, inboxID int64, family run.Family, project string, actions []action.Action) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		fd := route.FamilyDecision{Family: family, Project: project, Actions: actions}
		return insertActions(ctx, tx, inboxID, &fd)
	})
}
