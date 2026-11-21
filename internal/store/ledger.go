package store

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
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

// TaskRuns returns the runs of a Vikunja task in the given families.
func (l *Ledger) TaskRuns(taskID int64, families []run.Family) []route.RunRecord {
	if l.Err != nil {
		return nil
	}
	names := make([]string, len(families))
	for i, f := range families {
		names[i] = string(f)
	}
	rows, err := l.q.Query(l.ctx, `SELECT name, state, task_exists, family FROM runs
		WHERE identity->>'task_id' = $1 AND family = ANY($2) ORDER BY name`, strconv.FormatInt(taskID, 10), names)
	if err != nil {
		l.Err = err
		return nil
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (route.RunRecord, error) {
		rec := route.RunRecord{TaskID: taskID}
		var state, family string
		err := r.Scan(&rec.Name, &state, &rec.Exists, &family)
		rec.Active, rec.Family = state != runFinished, run.Family(family)
		return rec, err
	})
	if err != nil {
		l.Err = err
		return nil
	}
	return out
}

// PRRuns returns the PR follow-up runs of a Workspace's pull request.
func (l *Ledger) PRRuns(workspace string, pr int) []route.RunRecord {
	if l.Err != nil {
		return nil
	}
	rows, err := l.q.Query(l.ctx, `SELECT name, state, task_exists FROM runs
		WHERE family = $1 AND identity->>'workspace' = $2 AND identity->>'pr' = $3 ORDER BY name`,
		string(run.PRFollowUp), workspace, strconv.Itoa(pr))
	if err != nil {
		l.Err = err
		return nil
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (route.RunRecord, error) {
		rec := route.RunRecord{Family: run.PRFollowUp, Workspace: workspace, PR: pr}
		var state string
		err := r.Scan(&rec.Name, &state, &rec.Exists)
		rec.Active = state != runFinished
		return rec, err
	})
	if err != nil {
		l.Err = err
		return nil
	}
	return out
}
