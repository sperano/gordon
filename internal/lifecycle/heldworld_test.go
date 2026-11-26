package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/submit"
	"github.com/sperano/gordon/internal/worker"
)

// The held-run world: the gordon project's task heldID, assigned to
// claude-opus in Doing, whose run waits behind the variant limit of one.
const (
	gordonProject = 147
	heldID        = 1624
	queuedLabel   = "queued"
	queuedLabelID = 7
	heldTTL       = time.Minute
	heldBusyDelay = 5 * time.Minute
	// maxDrainTicks bounds a drain: a loop of actions would never end.
	maxDrainTicks = 20
	otherTask     = "other-1"
)

// vikunjaTasks reads the fake's tasks as the router's fresh state.
type vikunjaTasks struct{ f *fakeVikunja }

func (v vikunjaTasks) Task(_ context.Context, id int64) (event.Task, error) {
	t := v.f.get(id)
	out := event.Task{ID: t.ID, ProjectID: t.ProjectID, Title: "Ship it", Description: "<p>now</p>", Done: t.Done,
		Labels: labels(t), Buckets: []string{}}
	for _, u := range t.Assignees {
		out.Assignees = append(out.Assignees, u.Username)
	}
	for _, b := range t.Buckets {
		if !slices.Contains(out.Buckets, b.Title) {
			out.Buckets = append(out.Buckets, b.Title)
		}
	}
	return out, nil
}

func (vikunjaTasks) TaskComments(context.Context, int64) ([]event.TaskComment, error) {
	return nil, nil
}

func (vikunjaTasks) Comments(context.Context, int64, int) (json.RawMessage, error) {
	return json.RawMessage("[]"), nil
}

type noPRs struct{}

func (noPRs) PullRequest(context.Context, string, int, bool) (event.PullRequest, error) {
	return event.PullRequest{}, errors.New("no pull requests here")
}

// heldWorld is a worker with every executor the held run's lifecycle
// takes, against Postgres, a fake Kelos and a fake Vikunja.
type heldWorld struct {
	store *store.Store
	dbURL string
	kelos *kelostest.Server
	api   *kelos.Client
	vik   *fakeVikunja
	proc  *worker.Processor
	out   *worker.Outbox
	holds *Holds
	m     *metrics.Metrics
	log   *slog.Logger
}

func newHeldWorld(t *testing.T) *heldWorld {
	t.Helper()
	s, dbURL := storetest.Open(t)
	srv := kelostest.New(route.KelosNamespace)
	t.Cleanup(srv.Close)
	w := &heldWorld{store: s, dbURL: dbURL, kelos: srv, vik: newFake(t), m: metrics.New(),
		log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}
	w.api = &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	w.board()
	routing := profile.Defaults()
	routing.Policy.QueuedLabel = queuedLabel
	cfg := dispatchFixture(t)
	cfg.Limits = dispatch.Limits{Default: 1}
	backoff := worker.Backoff{Base: time.Second, Max: time.Minute}
	w.proc = &worker.Processor{Store: s, Router: route.Router{Config: routing},
		State: worker.StateReader{Config: routing, Tasks: vikunjaTasks{w.vik}, PRs: noPRs{}},
		Tasks: dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}, Comments: vikunjaTasks{w.vik}},
		Owner: "w", Concurrency: 4, LeaseTTL: heldTTL, MaxAttempts: 3, Backoff: backoff,
		Projects: &worker.Limiter{}, DeferDelay: time.Second, Metrics: w.m, Log: w.log}
	w.holds = &Holds{Store: s, Policy: routing.Policy}
	vik := &Executor{W: w.vik.writer(), Policy: routing.Policy, Agents: []string{"claude-opus"}, Runs: s, Lock: s,
		Namespace: route.KelosNamespace}
	ex := map[action.Kind]worker.Executor{
		action.SubmitTask: &submit.Executor{Runs: s, Kelos: w.api, Limits: cfg.Limits, BusyDelay: heldBusyDelay,
			Holds: w.holds, Metrics: w.m, Log: w.log},
		action.CancelRun: &submit.Canceller{Store: s, Kelos: w.api},
	}
	for _, k := range []action.Kind{action.AddLabel, action.RemoveLabel, action.MoveBucket, action.PostNotice} {
		ex[k] = vik
	}
	w.out = &worker.Outbox{Store: s, Executors: ex, Owner: "w", Concurrency: 8, LeaseTTL: heldTTL, MaxAttempts: 3,
		Backoff: backoff, Providers: &worker.Limiter{}, Projects: &worker.Limiter{}, DeferDelay: time.Second,
		NoExecutorDelay: time.Hour, Metrics: w.m, Log: w.log}
	w.kelos.Put(kelos.Task{Metadata: kelos.Metadata{Name: otherTask, Labels: map[string]string{dispatch.VariantLabel: "claude-opus"}},
		Spec:   kelos.Spec{Prompt: "x", Worker: &kelos.Worker{Type: "claude-code", WorkspaceRef: &kelos.Ref{Name: "vigie"}}},
		Status: &kelos.Status{Phase: kelos.PhaseRunning}})
	return w
}

func dispatchFixture(t *testing.T) dispatch.Config {
	t.Helper()
	b, err := os.ReadFile("../dispatch/testdata/hollingsworth.json")
	if err != nil {
		t.Fatal(err)
	}
	var c dispatch.Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// board is the gordon project's Kanban, its labels and task heldID in
// Doing.
func (w *heldWorld) board() {
	f := w.vik
	f.views[gordonProject] = []View{{ID: 1, Kind: "kanban", BucketMode: "manual"}}
	for i, title := range []string{"To-Do", "Doing", "Review"} {
		f.buckets[1] = append(f.buckets[1], Bucket{ID: 10 + int64(i), Title: title, ViewID: 1})
	}
	f.labels = []Label{{ID: 1, Title: "in progress"}, {ID: queuedLabelID, Title: queuedLabel}}
	f.put(Task{ID: heldID, ProjectID: gordonProject, Assignees: []User{{ID: 7, Username: "claude-opus"}},
		Buckets: []Bucket{f.buckets[1][1]}})
}

// update stores a task.updated of task heldID and decides it.
func (w *heldWorld) update(t *testing.T, bucket string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"event_name": "task.updated", "data": map[string]any{
		"task": map[string]any{"id": heldID, "project_id": gordonProject,
			"assignees": []map[string]string{{"username": "claude-opus"}}, "buckets": []map[string]string{{"title": bucket}}},
		"doer": map[string]string{"username": "eric"}}})
	ev, err := event.Parse(event.Delivery{Source: event.SourceVikunja, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.store.Accept(ctx, store.Delivery{Source: event.SourceVikunja, DeliveryKey: ev.DeliveryKey + bucket,
		RequestKey: ev.RequestKey, Kind: ev.Kind, Scope: ev.Scope(), Body: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.proc.Tick(ctx, ctx); err != nil {
		t.Fatal(err)
	}
}

// drain performs actions until none is due, the orphans of a
// dead-lettered or cancelled action cancelled between passes.
func (w *heldWorld) drain(t *testing.T) {
	t.Helper()
	for range maxDrainTicks {
		if _, err := w.store.CancelOrphans(ctx); err != nil {
			t.Fatal(err)
		}
		n, err := w.out.Tick(ctx, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("actions still due after the drain")
}

// retry makes every deferred action due again: the next 30s retry.
func (w *heldWorld) retry(t *testing.T) {
	t.Helper()
	storetest.Exec(t, w.dbURL, `UPDATE actions SET next_attempt_at = now() WHERE state IN ('pending', 'ambiguous')`)
	w.drain(t)
}

func (w *heldWorld) runName() string {
	return run.Identity{Family: run.VikunjaImplement, Project: "gordon", Variant: "claude-opus", TaskID: heldID}.Name()
}

// queuedNotices are the task's hold notices.
func (w *heldWorld) queuedNotices() []string {
	w.vik.mu.Lock()
	defer w.vik.mu.Unlock()
	var out []string
	for _, c := range w.vik.comments[heldID] {
		if strings.Contains(c.Comment, notice.Marker(notice.Queued, w.runName())) {
			out = append(out, c.Comment)
		}
	}
	return out
}

func (w *heldWorld) submitState(t *testing.T) string {
	t.Helper()
	var st string
	storetest.QueryRow(t, w.dbURL, `SELECT state FROM actions WHERE kind = 'submit-task'`, nil, &st)
	return st
}

// held defers the run once: queued label, one notice, no Task.
func (w *heldWorld) held(t *testing.T) {
	t.Helper()
	w.update(t, "Doing")
	w.drain(t)
	if got := labels(w.vik.get(heldID)); !slices.Equal(got, []string{queuedLabel}) {
		t.Fatalf("labels of the held task %v, want only %s", got, queuedLabel)
	}
	want := notice.QueuedText(notice.Hold{Run: w.runName(), Reason: notice.HoldLimit, Variant: "claude-opus",
		Running: 1, Limit: 1}, queuedLabel)
	if ns := w.queuedNotices(); len(ns) != 1 || !strings.HasPrefix(ns[0], want) {
		t.Fatalf("hold notices %q, want one starting %q", ns, want)
	}
	if st := w.submitState(t); st != store.ActionPending || len(w.kelos.Creates()) != 0 {
		t.Fatalf("held submission %s with %d creates, want pending and none", st, len(w.kelos.Creates()))
	}
}

// TestRunDeferredThenSubmitted: a held run's task gets queued and one
// notice however often the submission is deferred again (a busier
// variant included); once its Task is created, queued comes off and in
// progress goes on.
func TestRunDeferredThenSubmitted(t *testing.T) {
	w := newHeldWorld(t)
	w.held(t)
	w.retry(t)
	w.kelos.Put(kelos.Task{Metadata: kelos.Metadata{Name: "other-2", Labels: map[string]string{dispatch.VariantLabel: "claude-opus"}},
		Spec:   kelos.Spec{Prompt: "x", Worker: &kelos.Worker{Type: "claude-code", WorkspaceRef: &kelos.Ref{Name: "vigie"}}},
		Status: &kelos.Status{Phase: kelos.PhaseRunning}})
	w.retry(t) // runs 2 of 1 now: the same hold
	if ns := w.queuedNotices(); len(ns) != 1 {
		t.Fatalf("%d hold notices after three deferrals, want one", len(ns))
	}
	w.kelos.SetPhase(otherTask, kelos.PhaseSucceeded)
	w.kelos.SetPhase("other-2", kelos.PhaseSucceeded)
	w.retry(t)
	if st := w.submitState(t); st != store.ActionDone || len(w.kelos.Creates()) != 1 {
		t.Fatalf("submission %s with %d creates, want done and one", st, len(w.kelos.Creates()))
	}
	if got := labels(w.vik.get(heldID)); !slices.Equal(got, []string{"in progress"}) {
		t.Fatalf("labels of the started task %v, want only in progress", got)
	}
	if ns := w.queuedNotices(); len(ns) != 1 {
		t.Fatalf("%d hold notices after the start, want the one", len(ns))
	}
}

// TestRunDeferredThenCancelled: a held run whose card is dragged back to
// the backlog is cancelled before any Task, and queued comes off.
func TestRunDeferredThenCancelled(t *testing.T) {
	w := newHeldWorld(t)
	w.held(t)
	w.vik.mu.Lock()
	w.vik.tasks[heldID].Buckets = []Bucket{w.vik.buckets[1][0]}
	w.vik.mu.Unlock()
	w.update(t, "To-Do")
	w.drain(t)
	if st, rs := w.submitState(t), w.runState(t); st != store.ActionDone || rs != store.RunFinished || len(w.kelos.Creates()) != 0 {
		t.Fatalf("cancelled run %s, submission %s, %d creates; want finished, done, none", rs, st, len(w.kelos.Creates()))
	}
	if got := labels(w.vik.get(heldID)); len(got) != 0 {
		t.Fatalf("labels of the cancelled task %v, want none", got)
	}
	w.kelos.SetPhase(otherTask, kelos.PhaseSucceeded)
	w.retry(t)
	if got := labels(w.vik.get(heldID)); len(got) != 0 || len(w.kelos.Creates()) != 0 {
		t.Fatalf("after the slot freed: labels %v, %d creates; want none", got, len(w.kelos.Creates()))
	}
}

// TestRunDeferredThenDeadLettered: a held run whose submission is
// refused for good loses queued once the observer finishes it.
func TestRunDeferredThenDeadLettered(t *testing.T) {
	w := newHeldWorld(t)
	w.held(t)
	w.kelos.Admit = func(kelos.Task) error { return errors.New("profile default does not grant Secrets x") }
	w.kelos.SetPhase(otherTask, kelos.PhaseSucceeded)
	w.retry(t)
	if st := w.submitState(t); st != store.ActionFailed {
		t.Fatalf("refused submission %s, want failed", st)
	}
	if got := labels(w.vik.get(heldID)); !slices.Equal(got, []string{queuedLabel}) {
		t.Fatalf("labels before the observer %v, want queued still", got)
	}
	obs := &submit.Observer{Store: w.store, Kelos: w.api, Unsubmitted: w.holds, Metrics: w.m, Log: w.log}
	if err := obs.Once(ctx); err != nil {
		t.Fatal(err)
	}
	w.drain(t)
	if got, rs := labels(w.vik.get(heldID)), w.runState(t); len(got) != 0 || rs != store.RunFinished {
		t.Fatalf("dead-lettered run %s, labels %v; want finished, none", rs, got)
	}
}

func (w *heldWorld) runState(t *testing.T) string {
	t.Helper()
	var st string
	storetest.QueryRow(t, w.dbURL, `SELECT state FROM runs WHERE name = $1`, []any{w.runName()}, &st)
	return st
}
