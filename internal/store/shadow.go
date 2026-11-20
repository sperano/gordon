package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
)

// Shadow comparison results.
const (
	ShadowPending = "pending"
	ShadowMatch   = "match"
	ShadowDiffer  = "differ"
	ShadowMissing = "missing"
)

// recordShadow keeps the Task each run of a family not executed would
// have submitted, once per (name, delivery).
func recordShadow(ctx context.Context, tx pgx.Tx, inboxID int64, fd *route.FamilyDecision) error {
	for _, rn := range fd.Runs {
		if rn.Task == nil {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO shadow_tasks (name, inbox_id, family, task)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, rn.Name, inboxID, string(fd.Family), rn.Task); err != nil {
			return err
		}
	}
	return nil
}

// ShadowTask is a shadow Task waiting for its comparison.
type ShadowTask struct {
	Name    string
	InboxID int64
	Family  string
	Task    kelos.Task
	Age     time.Duration
}

// PendingShadow lists up to limit shadow Tasks not yet compared, oldest
// first.
func (s *Store) PendingShadow(ctx context.Context, limit int) ([]ShadowTask, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, inbox_id, family, task,
			extract(epoch FROM now() - created_at)::float8
		FROM shadow_tasks WHERE result = 'pending' AND task IS NOT NULL
		ORDER BY created_at, name LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ShadowTask, error) {
		var st ShadowTask
		var age float64
		err := r.Scan(&st.Name, &st.InboxID, &st.Family, &st.Task, &age)
		st.Age = time.Duration(age * float64(time.Second))
		return st, err
	})
}

// ShadowCompared records a comparison: its result and the differing
// paths (nil for a match or a missing Task).
func (s *Store) ShadowCompared(ctx context.Context, name string, inboxID int64, result string, diff []string) error {
	var d any
	if diff != nil {
		b, err := json.Marshal(diff)
		if err != nil {
			return err
		}
		d = json.RawMessage(b)
	}
	_, err := s.pool.Exec(ctx, `UPDATE shadow_tasks SET result = $3, diff = $4, compared_at = now()
		WHERE name = $1 AND inbox_id = $2 AND result = 'pending'`, name, inboxID, result, d)
	return err
}
