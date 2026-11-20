package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/route"
)

// Ledger answers the router's questions about handled work from the
// store. The router's Ledger cannot fail, so the first query error is kept
// in Err and every later answer is false: a caller that sees Err must not
// act on the decision (it read an incomplete ledger) and retries instead.
type Ledger struct {
	ctx context.Context
	q   querier
	Err error
}

// Ledger returns a ledger reading the store under ctx.
func (s *Store) Ledger(ctx context.Context) *Ledger { return &Ledger{ctx: ctx, q: s.pool} }

func (l *Ledger) exists(sql string, arg any) bool {
	if l.Err != nil {
		return false
	}
	var ok bool
	if err := l.q.QueryRow(l.ctx, sql, arg).Scan(&ok); err != nil {
		l.Err = err
		return false
	}
	return ok
}

// SeenDelivery reports whether a delivery with this key was already
// decided. The inbox stores each key once, so the row being decided is
// the only one and is still pending.
func (l *Ledger) SeenDelivery(key string) bool {
	return l.exists(`SELECT EXISTS (SELECT 1 FROM inbox WHERE delivery_key = $1 AND state <> 'pending')`, key)
}

// HandledRequest reports whether the owner request was already handled.
func (l *Ledger) HandledRequest(key string) bool {
	return l.exists(`SELECT EXISTS (SELECT 1 FROM requests WHERE request_key = $1)`, key)
}

// Run returns the run record of a Task name.
func (l *Ledger) Run(name string) (route.RunRecord, bool) {
	if l.Err != nil {
		return route.RunRecord{}, false
	}
	rec, ok, err := runRecord(l.ctx, l.q, name, false)
	if err != nil {
		l.Err = err
	}
	return rec, ok
}

// runRecord reads a run; lock takes its row lock inside a transaction.
func runRecord(ctx context.Context, q querier, name string, lock bool) (route.RunRecord, bool, error) {
	sql := `SELECT state, task_exists FROM runs WHERE name = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var state string
	rec := route.RunRecord{Name: name}
	err := q.QueryRow(ctx, sql, name).Scan(&state, &rec.Exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return route.RunRecord{}, false, nil
	}
	if err != nil {
		return route.RunRecord{}, false, err
	}
	rec.Active = state != runFinished
	return rec, true, nil
}
