package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// States of a cost notice (cost_notices.state).
const (
	// CostWaiting: no notice yet; Sillage's estimate is not final.
	CostWaiting = "waiting"
	// CostLive: posted, and its task total is kept current.
	CostLive = "live"
	// CostFrozen: a notice of a newer run of the task carries the total.
	CostFrozen = "frozen"
	// CostSettled: nothing changed for the live window, or Sillage failed
	// to answer through it.
	CostSettled = "settled"
)

// CostRun is a finished run generation whose cost notice is due.
type CostRun struct {
	Name    string
	InboxID int64
	Family  run.Family
	Project string
	TaskID  int64
	TaskUID string
	Phase   string
	// Finished is when its Task completed (or was seen finished).
	Finished time.Time
}

// CostNotice is a cost notice's row, as the pass reads it.
type CostNotice struct {
	CostRun
	State    string
	Rev      int
	Digest   string
	Failures int
	// LiveUntil is when a live notice settles unless it changes; the
	// finish time before the first publication.
	LiveUntil time.Time
}

// ErrNoCostNotice is a run generation with no cost notice row.
var ErrNoCostNotice = errors.New("no cost notice")

// RecordCostRun records a finished run generation's cost notice as
// waiting, once: a second completion event of the generation changes
// nothing.
func (s *Store) RecordCostRun(ctx context.Context, c CostRun) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO cost_notices
			(name, inbox_id, family, project, task_id, task_uid, phase, finished_at, next_check)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) ON CONFLICT (name, inbox_id) DO NOTHING`,
		c.Name, c.InboxID, string(c.Family), c.Project, c.TaskID, c.TaskUID, c.Phase, c.Finished)
	return err
}

// CostDue takes at most limit waiting or live notices due at now, the
// longest due first, and holds them until now+hold: concurrent passes
// take disjoint rows, and one that dies leaves them to the next pass
// after the hold. They are returned oldest run first.
func (s *Store) CostDue(ctx context.Context, now time.Time, hold time.Duration, limit int) ([]CostNotice, error) {
	rows, err := s.pool.Query(ctx, `UPDATE cost_notices SET next_check = $1::timestamptz + make_interval(secs => $2)
		WHERE (name, inbox_id) IN (SELECT name, inbox_id FROM cost_notices
			WHERE state IN ('waiting', 'live') AND next_check <= $1
			ORDER BY next_check, name LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING name, inbox_id, family, project, task_id, task_uid, phase, finished_at, state, rev, digest, failures,
			coalesce(live_until, finished_at)`,
		now, hold.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CostNotice, error) {
		var n CostNotice
		var family string
		err := r.Scan(&n.Name, &n.InboxID, &family, &n.Project, &n.TaskID, &n.TaskUID, &n.Phase, &n.Finished,
			&n.State, &n.Rev, &n.Digest, &n.Failures, &n.LiveUntil)
		n.Family = run.Family(family)
		return n, err
	})
	// UPDATE ... RETURNING keeps no order.
	slices.SortFunc(due, func(a, b CostNotice) int {
		return cmp.Or(a.Finished.Compare(b.Finished), cmp.Compare(a.Name, b.Name), cmp.Compare(a.InboxID, b.InboxID))
	})
	return due, err
}

// CostCheck records a check that published nothing: the next one at
// next; with failed, a failed read of Sillage (counted, its reason kept),
// else a check that found nothing to change (the count reset). It is
// fenced on the state and revision read: false when another pass changed
// the row meanwhile.
func (s *Store) CostCheck(ctx context.Context, n CostNotice, next time.Time, failed bool, reason string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE cost_notices SET next_check = $5,
			failures = CASE WHEN $6 THEN failures + 1 ELSE 0 END, last_error = $7, updated_at = now()
		WHERE name = $1 AND inbox_id = $2 AND state = $3 AND rev = $4`,
		n.Name, n.InboxID, n.State, n.Rev, next, failed, reason)
	return err == nil && tag.RowsAffected() == 1, err
}

// CostPublish stores revision n.Rev+1 of the notice's text and, in the
// same transaction, queues its publish-cost action on the run's inbox
// row; the notice is checked again at next. The task's newest posted
// notice carries the task total and is live until liveUntil: a first
// publication freezes the task's live notices of older runs, and is
// itself posted frozen when a newer run's notice was posted already.
// The task's notices are locked first, so two first publications of one
// task are ordered. Fenced like CostCheck.
func (s *Store) CostPublish(ctx context.Context, n CostNotice, body, digest string, now, liveUntil, next time.Time) (bool, error) {
	published := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM cost_notices WHERE task_id = $1 ORDER BY name, inbox_id FOR UPDATE`,
			n.TaskID); err != nil {
			return err
		}
		var rev int
		err := tx.QueryRow(ctx, `UPDATE cost_notices c SET rev = rev + 1, body = $5, digest = $6,
				state = CASE WHEN c.state = 'waiting' AND EXISTS (SELECT 1 FROM cost_notices o
					WHERE o.task_id = c.task_id AND o.state <> 'waiting'
					AND (o.finished_at, o.name, o.inbox_id) > (c.finished_at, c.name, c.inbox_id))
				THEN 'frozen' ELSE 'live' END,
				published_at = $7, live_until = $8, next_check = $9, failures = 0, last_error = '', updated_at = now()
			WHERE name = $1 AND inbox_id = $2 AND state = $3 AND rev = $4 RETURNING rev`,
			n.Name, n.InboxID, n.State, n.Rev, body, digest, now, liveUntil, next).Scan(&rev)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		published = true
		if n.State == CostWaiting {
			if _, err := tx.Exec(ctx, `UPDATE cost_notices o SET state = 'frozen', updated_at = now()
				FROM cost_notices c WHERE c.name = $1 AND c.inbox_id = $2
				AND o.task_id = c.task_id AND o.state = 'live'
				AND (o.finished_at, o.name, o.inbox_id) < (c.finished_at, c.name, c.inbox_id)`,
				n.Name, n.InboxID); err != nil {
				return err
			}
		}
		fd := route.FamilyDecision{Family: run.VikunjaCost, Project: n.Project,
			Actions: []action.Action{action.CostNotice(n.TaskID, n.Name, n.InboxID, rev)}}
		return insertActions(ctx, tx, n.InboxID, &fd)
	})
	return published, err
}

// CostSettle stops checking a waiting or live notice. Fenced like
// CostCheck.
func (s *Store) CostSettle(ctx context.Context, n CostNotice) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE cost_notices SET state = 'settled', updated_at = now()
		WHERE name = $1 AND inbox_id = $2 AND state = $3 AND rev = $4`, n.Name, n.InboxID, n.State, n.Rev)
	return err == nil && tag.RowsAffected() == 1, err
}

// CostBody is the latest stored text of a generation's cost notice and
// its revision; ErrNoCostNotice when there is none.
func (s *Store) CostBody(ctx context.Context, name string, inboxID int64) (string, int, error) {
	var body string
	var rev int
	err := s.pool.QueryRow(ctx, `SELECT body, rev FROM cost_notices WHERE name = $1 AND inbox_id = $2`,
		name, inboxID).Scan(&body, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrNoCostNotice
	}
	return body, rev, err
}
