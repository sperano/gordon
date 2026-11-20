// Package store is Gordon's durable state in Postgres: the inbox of
// verified deliveries, the decisions taken on them, the ledger of handled
// requests and submitted runs, and the outbox of intended writes.
//
// Every worker claim takes a lease: a random token and an expiry. A write
// that completes a claimed row names the token, so a worker whose lease
// expired (and whose row another worker took) changes nothing: ErrLeaseLost.
// Lease expiry and backoff use the database clock, so pods with skewed
// clocks agree.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLeaseLost refuses a write on a row whose lease this worker no longer
// holds: it expired and another claim took the row, or the row finished.
var ErrLeaseLost = errors.New("lease lost")

// Store is the Postgres store.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to url and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx's message can quote the URL, password included.
		return nil, errors.New("database url invalid")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connections.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
