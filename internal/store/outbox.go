package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
)

// Action states.
const (
	ActionPending   = "pending"
	ActionRunning   = "running"
	ActionAmbiguous = "ambiguous"
	ActionDone      = "done"
	ActionFailed    = "failed"
)

// ClaimedAction is a leased outbox row.
type ClaimedAction struct {
	Lease
	Action  action.Action
	Family  string
	Project string
	// Scope is the inbox row's scope (event.Scope), the key the
	// per-project limit shares with the inbox processor.
	Scope   string
	InboxID int64
	// Attempts counts claims, this one included.
	Attempts int
	// Recover is set when an earlier attempt may have reached the
	// provider: it ended ambiguous, or its worker lost the lease while
	// running. The executor must look for the write's external marker
	// before writing again.
	Recover   bool
	CreatedAt time.Time
	// LastError is the row's last error before this claim, "" for none:
	// the reason of its previous deferral, or the error that left it to
	// retry, ambiguous or with its lease expired.
	LastError string
}

// ClaimActions leases up to limit due actions to owner for ttl: pending
// or ambiguous rows past their backoff, and running rows whose lease
// expired (a worker died mid-write). A pending row with an after_key also
// waits until the action it names is done; one finished as cancelled
// does not count.
func (s *Store) ClaimActions(ctx context.Context, owner string, limit int, ttl time.Duration) ([]ClaimedAction, error) {
	rows, err := s.pool.Query(ctx, `WITH due AS (
			SELECT id, state, last_error FROM actions a
			WHERE ((state IN ('pending', 'ambiguous') AND next_attempt_at <= now())
			   OR (state = 'running' AND lease_until < now()))
			  AND (after_key = '' OR state <> 'pending' OR EXISTS (SELECT 1 FROM actions d
				WHERE d.inbox_id = a.inbox_id AND d.key = a.after_key AND d.state = 'done'
				  AND d.last_error NOT LIKE '`+cancelledPrefix+`%'))
			ORDER BY next_attempt_at, id LIMIT $1
			FOR UPDATE SKIP LOCKED)
		UPDATE actions a SET state = 'running', attempts = a.attempts + 1,
			lease_token = gen_random_uuid(), lease_owner = $2,
			lease_until = now() + make_interval(secs => $3)
		FROM due WHERE a.id = due.id
		RETURNING a.id, a.lease_token::text, a.key, a.kind, a.target, a.value, a.detail, a.after_key,
			a.family, a.project,
			coalesce((SELECT i.scope FROM inbox i WHERE i.id = a.inbox_id), ''), a.inbox_id, a.attempts, due.state <> 'pending', a.created_at, due.last_error`,
		limit, owner, ttl.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClaimedAction, error) {
		var c ClaimedAction
		var kind, target string
		err := r.Scan(&c.ID, &c.Token, &c.Action.Key, &kind, &target, &c.Action.Value, &c.Action.Detail, &c.Action.After,
			&c.Family, &c.Project, &c.Scope, &c.InboxID, &c.Attempts, &c.Recover, &c.CreatedAt, &c.LastError)
		c.Action.Kind, c.Action.Target = action.Kind(kind), action.Target(target)
		return c, err
	})
}

const releaseLease = `lease_token = NULL, lease_owner = NULL, lease_until = NULL`

// ActionDone records the write as made (or found already made).
func (s *Store) ActionDone(ctx context.Context, l Lease) error {
	return execFenced(ctx, s.pool, `UPDATE actions SET state = 'done', finished_at = now(), last_error = '', `+
		releaseLease+` WHERE id = $1 AND lease_token = $2::uuid AND state = 'running'`, l.ID, l.Token)
}

// ActionCancelled records the action finished without being made: its
// run was stopped meanwhile. Its last error starts with the cancelled
// prefix, as a run cancelled before its submission was claimed
// (CancelRuns), so the action's dependents are cancelled too.
func (s *Store) ActionCancelled(ctx context.Context, l Lease, reason string) error {
	return execFenced(ctx, s.pool, `UPDATE actions SET state = 'done', finished_at = now(),
		last_error = '`+cancelledPrefix+`' || $3, `+releaseLease+`
		WHERE id = $1 AND lease_token = $2::uuid AND state = 'running'`, l.ID, l.Token, reason)
}

// ActionRetry queues the action again after delay. ambiguous records
// that the write may have happened, so the next attempt recovers first.
func (s *Store) ActionRetry(ctx context.Context, l Lease, ambiguous bool, delay time.Duration, reason string) error {
	state := ActionPending
	if ambiguous {
		state = ActionAmbiguous
	}
	return execFenced(ctx, s.pool, `UPDATE actions SET state = $3, last_error = $4,
		next_attempt_at = now() + make_interval(secs => $5), `+releaseLease+`
		WHERE id = $1 AND lease_token = $2::uuid AND state = 'running'`,
		l.ID, l.Token, state, reason, delay.Seconds())
}

// ActionDefer returns the action unattempted (no executor yet, or a
// limit was full): due after delay, its state and attempts unchanged.
func (s *Store) ActionDefer(ctx context.Context, l Lease, recover bool, delay time.Duration, reason string) error {
	state := ActionPending
	if recover {
		state = ActionAmbiguous
	}
	return execFenced(ctx, s.pool, `UPDATE actions SET state = $3, last_error = $4,
		attempts = greatest(attempts - 1, 0), next_attempt_at = now() + make_interval(secs => $5), `+releaseLease+`
		WHERE id = $1 AND lease_token = $2::uuid AND state = 'running'`,
		l.ID, l.Token, state, reason, delay.Seconds())
}

// cancelledPrefix starts the last_error of an action finished without
// being made: its run was stopped, or the action it waited for was
// dead-lettered or cancelled. Such an action is done, never retried.
const cancelledPrefix = "cancelled: "

// CancelOrphans cancels the pending actions whose prerequisite was
// dead-lettered or cancelled, level by level, and returns how many.
func (s *Store) CancelOrphans(ctx context.Context) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `UPDATE actions a SET state = 'done', finished_at = now(),
				last_error = '`+cancelledPrefix+`waited for ' || a.after_key
			WHERE a.state = 'pending' AND a.after_key <> '' AND EXISTS (SELECT 1 FROM actions d
				WHERE d.inbox_id = a.inbox_id AND d.key = a.after_key
				  AND (d.state = 'failed' OR (d.state = 'done' AND d.last_error LIKE '`+cancelledPrefix+`%')))`)
		if err != nil || tag.RowsAffected() == 0 {
			return total, err
		}
		total += tag.RowsAffected()
	}
}

// ActionFailed dead-letters the action.
func (s *Store) ActionFailed(ctx context.Context, l Lease, reason string) error {
	return execFenced(ctx, s.pool, `UPDATE actions SET state = 'failed', finished_at = now(), last_error = $3, `+
		releaseLease+` WHERE id = $1 AND lease_token = $2::uuid AND state = 'running'`, l.ID, l.Token, reason)
}
