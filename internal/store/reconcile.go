package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queues.
const (
	QueueInbox   = "inbox"
	QueueActions = "actions"
)

// QueueStat is the depth and oldest age of one queue state.
type QueueStat struct {
	Queue string
	State string
	Count int64
	// OldestAge is the age of the oldest row in that state.
	OldestAge time.Duration
}

// QueueStats reports every unfinished state (and the dead letters) of
// both queues; done rows are history, not depth.
func (s *Store) QueueStats(ctx context.Context) ([]QueueStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT 'inbox', state, count(*), extract(epoch FROM now() - min(received_at))::float8
		FROM inbox WHERE state <> 'done' GROUP BY state
		UNION ALL
		SELECT 'actions', state, count(*), extract(epoch FROM now() - min(created_at))::float8
		FROM actions WHERE state <> 'done' GROUP BY state
		ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (QueueStat, error) {
		var q QueueStat
		var age float64
		err := r.Scan(&q.Queue, &q.State, &q.Count, &age)
		q.OldestAge = time.Duration(age * float64(time.Second))
		return q, err
	})
}

// ExpireLeases releases the leases of crashed or stalled workers: an
// inbox row returns to its queue, and a running action becomes ambiguous
// (its write may have been sent), so it is recovered before a retry.
func (s *Store) ExpireLeases(ctx context.Context) (inbox, actions int64, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE inbox SET `+releaseLease+`
		WHERE state = 'pending' AND lease_until < now()`)
	if err != nil {
		return 0, 0, err
	}
	inbox = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `UPDATE actions SET state = 'ambiguous',
		last_error = 'lease expired while running', `+releaseLease+`
		WHERE state = 'running' AND lease_until < now()`)
	if err != nil {
		return inbox, 0, err
	}
	return inbox, tag.RowsAffected(), nil
}

// PruneBodies drops the stored bodies of rows finished more than
// retention ago; the row, its keys and its decision stay, so a redelivery
// is still a duplicate. Bodies hold comment text, so they do not linger.
func (s *Store) PruneBodies(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE inbox SET body = NULL
		WHERE state <> 'pending' AND body IS NOT NULL
		  AND finished_at < now() - make_interval(secs => $1)`, retention.Seconds())
	return tag.RowsAffected(), err
}
