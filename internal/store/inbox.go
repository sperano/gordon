package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/event"
)

// Delivery is one verified delivery to persist, with the keys and routing
// hints the receiver parsed from it.
type Delivery struct {
	Source      event.Source
	DeliveryKey string
	RequestKey  string
	Kind        event.Kind
	// Scope is the project hint the worker limits concurrency on: a
	// Vikunja project id or a GitHub repository; "" when none.
	Scope   string
	Headers map[string]string
	Body    []byte
}

// Accept persists d, unless a delivery with its key is already stored:
// then dup is true and nothing is written. The insert commits before the
// receiver answers, so an acknowledged delivery survives a crash.
func (s *Store) Accept(ctx context.Context, d Delivery) (id int64, dup bool, err error) {
	headers := d.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO inbox
		(source, delivery_key, request_key, kind, scope, headers, body)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (delivery_key) DO NOTHING RETURNING id`,
		string(d.Source), d.DeliveryKey, d.RequestKey, string(d.Kind), d.Scope, headers, d.Body).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, true, nil
	}
	return id, false, err
}

// Lease is a claim on one row: its id and the token that fences writes.
type Lease struct {
	ID    int64
	Token string
}

// Item is a claimed inbox row.
type Item struct {
	Lease
	Source      event.Source
	DeliveryKey string
	RequestKey  string
	Kind        event.Kind
	Scope       string
	Headers     map[string]string
	Body        []byte
	// Attempts counts claims, this one included.
	Attempts   int
	ReceivedAt time.Time
}

// Delivery rebuilds the event delivery the router decides.
func (it Item) Delivery() event.Delivery {
	return event.Delivery{Source: it.Source, Headers: it.Headers, Body: it.Body, ReceivedAt: it.ReceivedAt}
}

// Claim leases up to limit due pending rows to owner for ttl, oldest due
// first. Rows another worker holds under an unexpired lease are skipped
// without waiting (SKIP LOCKED), so concurrent workers claim disjoint sets.
func (s *Store) Claim(ctx context.Context, owner string, limit int, ttl time.Duration) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `WITH due AS (
			SELECT id FROM inbox
			WHERE state = 'pending' AND next_attempt_at <= now()
			  AND (lease_until IS NULL OR lease_until < now())
			ORDER BY next_attempt_at, id LIMIT $1
			FOR UPDATE SKIP LOCKED)
		UPDATE inbox i SET attempts = i.attempts + 1, lease_token = gen_random_uuid(),
			lease_owner = $2, lease_until = now() + make_interval(secs => $3)
		FROM due WHERE i.id = due.id
		RETURNING i.id, i.lease_token::text, i.source, i.delivery_key, i.request_key,
			i.kind, i.scope, i.headers, coalesce(i.body, ''::bytea), i.attempts, i.received_at`,
		limit, owner, ttl.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		var source, kind string
		err := r.Scan(&it.ID, &it.Token, &source, &it.DeliveryKey, &it.RequestKey,
			&kind, &it.Scope, &it.Headers, &it.Body, &it.Attempts, &it.ReceivedAt)
		it.Source, it.Kind = event.Source(source), event.Kind(kind)
		return it, err
	})
}

// Retry returns the row to the queue, due after delay, recording why.
func (s *Store) Retry(ctx context.Context, l Lease, delay time.Duration, reason string) error {
	return execFenced(ctx, s.pool, `UPDATE inbox SET next_attempt_at = now() + make_interval(secs => $3),
		last_error = $4, lease_token = NULL, lease_owner = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2::uuid AND state = 'pending'`,
		l.ID, l.Token, delay.Seconds(), reason)
}

// Defer returns the row unprocessed (a concurrency limit was full): due
// after delay, and the claim does not count as an attempt.
func (s *Store) Defer(ctx context.Context, l Lease, delay time.Duration) error {
	return execFenced(ctx, s.pool, `UPDATE inbox SET next_attempt_at = now() + make_interval(secs => $3),
		attempts = greatest(attempts - 1, 0), lease_token = NULL, lease_owner = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2::uuid AND state = 'pending'`,
		l.ID, l.Token, delay.Seconds())
}

// Fail dead-letters the row: it is never claimed again.
func (s *Store) Fail(ctx context.Context, l Lease, reason string) error {
	return execFenced(ctx, s.pool, `UPDATE inbox SET state = 'failed', finished_at = now(), last_error = $3,
		lease_token = NULL, lease_owner = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2::uuid AND state = 'pending'`,
		l.ID, l.Token, reason)
}
