package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
)

// SyntheticHeader marks an inbox row the worker queued itself; the
// receiver strips nothing it does not store, and no sender sets it.
const SyntheticHeader = event.SyntheticHeader

// Synthetic kinds, the header's value.
const (
	SyntheticRequeue   = "requeue"
	SyntheticReconcile = "reconcile"
)

type synthTask struct {
	ID        int64 `json:"id"`
	ProjectID int64 `json:"project_id"`
}

type synthComment struct {
	ID      int64  `json:"id"`
	Comment string `json:"comment"`
}

type synthUser struct {
	Username string `json:"username"`
}

type synthData struct {
	Task    synthTask        `json:"task"`
	Comment *synthComment    `json:"comment,omitempty"`
	Doer    *synthUser       `json:"doer,omitempty"`
	Gordon  *event.Synthetic `json:"gordon,omitempty"`
	// Redispatch makes a reconciled comment's delivery key differ from
	// its webhook's, so a lost webhook does not shadow its recovery.
	Redispatch string `json:"redispatch,omitempty"`
}

// TaskUpdate is a synthetic task.updated: the router re-reads the task.
// It fails on ids the router would refuse (a task id that is not
// positive), which come from Vikunja's data.
func TaskUpdate(task, project int64, s event.Synthetic) (Delivery, error) {
	return synthetic(SyntheticKind(s), "task.updated", synthData{Task: synthTask{task, project}, Gordon: &s})
}

// CommentCreated is a synthetic task.comment.created for an owner request
// whose webhook never arrived; its author is the comment's, as Vikunja's
// comment listing reports it. It fails as TaskUpdate does, and on a
// comment id that is not positive.
func CommentCreated(task, project int64, c event.TaskComment, s event.Synthetic) (Delivery, error) {
	return synthetic(SyntheticKind(s), "task.comment.created", synthData{Task: synthTask{task, project},
		Comment: &synthComment{c.ID, c.Body}, Doer: &synthUser{c.Author}, Gordon: &s, Redispatch: SyntheticReconcile})
}

// SyntheticKind is the header value of a synthetic delivery.
func SyntheticKind(s event.Synthetic) string {
	if s.Reconcile != "" {
		return SyntheticReconcile
	}
	return SyntheticRequeue
}

// synthetic parses the body it builds, so its keys are the router's.
func synthetic(kind, name string, data synthData) (Delivery, error) {
	body, err := json.Marshal(map[string]any{"event_name": name, "data": data})
	if err != nil {
		panic(err) // plain structs always marshal
	}
	ev, err := event.ParseVikunja(body)
	if err != nil {
		return Delivery{}, fmt.Errorf("synthetic %s: %w", name, err)
	}
	return Delivery{Source: event.SourceVikunja, DeliveryKey: ev.DeliveryKey, RequestKey: ev.RequestKey,
		Kind: ev.Kind, Scope: ev.Scope(), Headers: map[string]string{SyntheticHeader: kind}, Body: body}, nil
}

// GitHubSynthetic is a synthetic GitHub delivery of eventName with body,
// for a webhook the GitHub reconciliation found missing: guid makes its
// delivery key (github/delivery/<guid>), so the same recovery is queued
// once. It parses the body it builds, so its keys are the router's.
func GitHubSynthetic(eventName, guid string, body any) (Delivery, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Delivery{}, fmt.Errorf("synthetic github %s: %w", eventName, err)
	}
	headers := map[string]string{event.GitHubEventHeader: eventName, event.GitHubDeliveryHeader: guid,
		SyntheticHeader: SyntheticReconcile}
	ev, err := event.ParseGitHub(event.Delivery{Source: event.SourceGitHub, Headers: headers, Body: raw})
	if err != nil {
		return Delivery{}, fmt.Errorf("synthetic github %s: %w", eventName, err)
	}
	return Delivery{Source: event.SourceGitHub, DeliveryKey: ev.DeliveryKey, RequestKey: ev.RequestKey,
		Kind: ev.Kind, Scope: ev.Scope(), Headers: headers, Body: raw}, nil
}

// Enqueue stores synthetic deliveries, each unless its key is stored
// already; it returns how many it stored, none when it fails (the
// transaction rolled back).
func (s *Store) Enqueue(ctx context.Context, ds []Delivery) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		n, err = enqueue(ctx, tx, ds)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func enqueue(ctx context.Context, tx pgx.Tx, ds []Delivery) (int, error) {
	n := 0
	for _, d := range ds {
		tag, err := tx.Exec(ctx, `INSERT INTO inbox (source, delivery_key, request_key, kind, scope, headers, body)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			string(d.Source), d.DeliveryKey, d.RequestKey, string(d.Kind), d.Scope, d.Headers, d.Body)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// requeues are the synthetic deliveries a decision asks for, each naming
// the delivery that asked. A requeue of a task id the router would refuse
// (Vikunja's relation data) is skipped: retrying the decision cannot fix
// it.
func requeues(dec route.Decision) []Delivery {
	out := make([]Delivery, 0, len(dec.Requeue))
	for _, q := range dec.Requeue {
		d, err := TaskUpdate(q.TaskID, q.ProjectID, event.Synthetic{Requeue: dec.DeliveryKey, Depth: q.Depth})
		if err == nil {
			out = append(out, d)
		}
	}
	return out
}

// KnownDeliveries returns which of keys the inbox holds, in any state.
func (s *Store) KnownDeliveries(ctx context.Context, keys []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT delivery_key FROM inbox WHERE delivery_key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := make(map[string]bool, len(found))
	for _, k := range found {
		out[k] = true
	}
	return out, err
}

// HandledRequests returns which of keys a decision handled.
func (s *Store) HandledRequests(ctx context.Context, keys []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT request_key FROM requests WHERE request_key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := make(map[string]bool, len(found))
	for _, k := range found {
		out[k] = true
	}
	return out, err
}
