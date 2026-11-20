package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

// digestBytes is how much of a task state's sha256 a reconcile key keeps.
const digestBytes = 16

// Defaults of the follow-up recovery window (reconcile_vikunja_followups.py).
const (
	DefaultLookback      = 12 * time.Hour
	DefaultRedispatchAge = 2 * time.Minute
)

// TaskLister lists Vikunja tasks.
type TaskLister interface {
	Tasks(ctx context.Context, filter string, comments bool) ([]upstream.ListedTask, error)
	NewestComments(ctx context.Context, id int64) ([]event.TaskComment, error)
	LabelID(ctx context.Context, title string) (int64, bool, error)
}

// Inbox is the part of the store the reconciliation queues into.
type Inbox interface {
	Enqueue(ctx context.Context, ds []store.Delivery) (int, error)
	KnownDeliveries(ctx context.Context, keys []string) (map[string]bool, error)
	HandledRequests(ctx context.Context, keys []string) (map[string]bool, error)
}

// Reconciler recovers what webhooks missed (subtask-chain-reconcile,
// reconcile_vikunja_followups.py, the failure reporter's done sweeps):
// every pass lists the configured projects' open tasks and the done ones
// still labelled, and queues a synthetic task.updated for each task whose
// state the router may act on, keyed by that state, so an unchanged task
// is queued once; and a synthetic comment delivery for each owner request
// of the window that no delivery brought. The router decides them as it
// decides webhooks, from the fresh state and the ledger: a request is
// never answered twice and a run never submitted twice, whatever Kelos
// cleaned up since.
type Reconciler struct {
	Tasks  TaskLister
	Inbox  Inbox
	Config profile.Config
	// Lookback and After bound the requests recovered: younger than
	// Lookback, older than After (the webhook may still be on its way).
	Lookback, After time.Duration
	Now             func() time.Time
	Metrics         *metrics.Metrics
	Log             *slog.Logger
}

// Reconcile results, the vikunja_reconcile_runs_total result label.
const (
	ReconcileOK    = "ok"
	ReconcileError = "error"
)

// Once runs one reconciliation pass, counted and logged.
func (r *Reconciler) Once(ctx context.Context) error {
	n, err := r.pass(ctx)
	r.Metrics.VikunjaReconcileQueued.Add(float64(n))
	if err != nil {
		r.Metrics.VikunjaReconciles.WithLabelValues(ReconcileError).Inc()
		r.Log.Warn("vikunja reconciliation failed", "error", err)
		return err
	}
	r.Metrics.VikunjaReconciles.WithLabelValues(ReconcileOK).Inc()
	if n > 0 {
		r.Log.Info("vikunja reconciliation queued", "deliveries", n)
	}
	return nil
}

func (r *Reconciler) pass(ctx context.Context) (int, error) {
	projects := r.projectIDs()
	if projects == "" {
		return 0, nil
	}
	open, err := r.Tasks.Tasks(ctx, "done = false && project in "+projects, true)
	if err != nil {
		return 0, fmt.Errorf("listing open tasks: %w", err)
	}
	done, err := r.doneLabelled(ctx, projects)
	if err != nil {
		return 0, err
	}
	var ds []store.Delivery
	for _, lt := range append(open, done...) {
		if !r.candidate(lt.Task) {
			continue
		}
		d, err := store.TaskUpdate(lt.Task.ID, lt.Task.ProjectID,
			event.Synthetic{Reconcile: digest(lt), Depth: route.MaxRequeueDepth})
		if err != nil {
			r.Log.Warn("vikunja reconciliation skipped a task", "task", lt.Task.ID, "error", err)
			continue
		}
		ds = append(ds, d)
	}
	requests, err := r.requests(ctx, open)
	if err != nil {
		return 0, err
	}
	return r.Inbox.Enqueue(ctx, append(ds, requests...))
}

func (r *Reconciler) projectIDs() string {
	ids := make([]string, 0, len(r.Config.Projects))
	for _, p := range r.Config.Projects {
		ids = append(ids, fmt.Sprint(p.VikunjaID))
	}
	return strings.Join(ids, ", ")
}

// doneLabelled lists the done tasks still carrying in progress or
// action-needed (done_unlabel.sweep, Flagger.sweep_done).
func (r *Reconciler) doneLabelled(ctx context.Context, projects string) ([]upstream.ListedTask, error) {
	p := r.Config.Policy
	var out []upstream.ListedTask
	for _, title := range []string{p.InProgressLabel, p.ActionNeededLabel} {
		if title == "" {
			continue
		}
		id, ok, err := r.Tasks.LabelID(ctx, title)
		if err != nil || !ok {
			if err != nil {
				return nil, fmt.Errorf("label %q: %w", title, err)
			}
			continue
		}
		found, err := r.Tasks.Tasks(ctx, fmt.Sprintf("done = true && labels in %d && project in %s", id, projects), false)
		if err != nil {
			return nil, fmt.Errorf("listing done tasks labelled %q: %w", title, err)
		}
		out = append(out, found...)
	}
	return out, nil
}

// candidate is a task the router may act on: one with an agent assignee
// (a run to start, a chain), a chain label, or the in-progress or
// action-needed label (a drag to stop, a done task to clean).
func (r *Reconciler) candidate(t event.Task) bool {
	p := r.Config.Policy
	for _, a := range t.Assignees {
		if r.Config.IsAgent(a) {
			return true
		}
	}
	for _, l := range []string{p.ChainLabel, p.InProgressLabel, p.ActionNeededLabel} {
		if l != "" && t.HasLabel(l) {
			return true
		}
	}
	return false
}

// digest names a listed task's state: a change of labels, assignees,
// buckets, relations (a blocker done) or its update time is a new state.
func digest(lt upstream.ListedTask) string {
	t := lt.Task
	t.Title, t.Description = "", ""
	b, _ := json.Marshal(struct { //nolint:errcheck // plain structs
		T event.Task `json:"t"`
		U time.Time  `json:"u"`
	}{t, lt.Updated})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:digestBytes])
}
