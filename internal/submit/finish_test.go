package submit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
)

type observed struct {
	runs     []store.ObservedRun
	recorded []string
}

func (o *observed) ObservedRuns(context.Context, int) ([]store.ObservedRun, error) {
	return o.runs, nil
}

func (o *observed) ObserveRun(_ context.Context, name string, _ int64, _ bool, phase string) error {
	o.recorded = append(o.recorded, name+"="+phase)
	return nil
}

type hook struct {
	calls []string
	err   error
	// order, when set, is shared by several hooks: each appends its id.
	order *[]string
	id    string
}

func (h *hook) Finished(_ context.Context, r store.ObservedRun, t kelos.Task) error {
	h.calls = append(h.calls, r.Name+"="+t.Phase())
	if h.order != nil {
		*h.order = append(*h.order, h.id+":"+r.Name)
	}
	return h.err
}

func TestObserverTellsTheHookOfEachFinish(t *testing.T) {
	srv := kelostest.New(route.KelosNamespace)
	t.Cleanup(srv.Close)
	api := &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "failed"}, Spec: kelos.Spec{Prompt: "x"}, Status: &kelos.Status{Phase: kelos.PhaseFailed}})
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "seen"}, Spec: kelos.Spec{Prompt: "x"}, Status: &kelos.Status{Phase: kelos.PhaseFailed}})
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "recovered"}, Spec: kelos.Spec{Prompt: "x"}, Status: &kelos.Status{Phase: kelos.PhaseFailed}})
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "running"}, Spec: kelos.Spec{Prompt: "x"}, Status: &kelos.Status{Phase: kelos.PhaseRunning}})
	st := &observed{runs: []store.ObservedRun{
		{Name: "failed", State: "active", Exists: true, Phase: kelos.PhaseRunning},
		// Finished and still present: observed before, not told again.
		{Name: "seen", State: store.RunFinished, Exists: true, Phase: kelos.PhaseRunning},
		{Name: "running", State: "active", Exists: true, Phase: kelos.PhasePending},
		// Found already finished by its submission (store.RunSubmitted).
		{Name: "recovered", State: "active", Exists: true},
	}}
	h := &hook{}
	o := &Observer{Store: st, Kelos: api, Finished: []FinishHook{h}, Metrics: metrics.New(), Log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}
	if err := o.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"failed=Failed", "recovered=Failed"}; !slices.Equal(h.calls, want) {
		t.Fatalf("hook told %v, want %v: the runs that finished now", h.calls, want)
	}
	// A failing hook leaves the observation unrecorded: the next pass
	// observes the finish again, so the hook's actions are not lost.
	st.recorded, h.calls, h.err = nil, nil, errors.New("store down")
	if err := o.Once(context.Background()); err == nil {
		t.Fatal("a failed hook was not reported")
	}
	for _, r := range st.recorded {
		if r == "failed=Failed" {
			t.Fatal("the finish was recorded although its hook failed")
		}
	}
	if want := []string{"failed=Failed"}; !slices.Equal(h.calls, want) {
		t.Fatalf("failing hook told %v, want %v (the pass stops at its error)", h.calls, want)
	}
	// The next pass, its hook working again, tells it of the finish again.
	st.recorded, h.calls, h.err = nil, nil, nil
	if err := o.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"failed=Failed", "recovered=Failed"}; !slices.Equal(h.calls, want) {
		t.Fatalf("retry told %v, want %v", h.calls, want)
	}
	if !slices.Contains(st.recorded, "failed=Failed") {
		t.Fatalf("retry recorded %v, want the finish recorded", st.recorded)
	}
}

// TestObserverTellsEveryHookInOrder: each hook hears of each finish, in
// their order; a failing first hook keeps the later ones from hearing of
// it, and the finish from being recorded, until a later pass.
func TestObserverTellsEveryHookInOrder(t *testing.T) {
	srv := kelostest.New(route.KelosNamespace)
	t.Cleanup(srv.Close)
	api := &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "done"}, Spec: kelos.Spec{Prompt: "x"}, Status: &kelos.Status{Phase: kelos.PhaseSucceeded}})
	st := &observed{runs: []store.ObservedRun{{Name: "done", State: "active", Exists: true, Phase: kelos.PhaseRunning}}}
	var order []string
	first, second := &hook{order: &order, id: "first"}, &hook{order: &order, id: "second"}
	o := &Observer{Store: st, Kelos: api, Finished: []FinishHook{first, second}, Metrics: metrics.New(),
		Log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}
	first.err = errors.New("store down")
	if err := o.Once(context.Background()); err == nil {
		t.Fatal("a failed first hook was not reported")
	}
	if want := []string{"first:done"}; !slices.Equal(order, want) || len(st.recorded) != 0 {
		t.Fatalf("hooks told %v, recorded %v; want %v and nothing recorded", order, st.recorded, want)
	}
	order, first.err = nil, nil
	if err := o.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"first:done", "second:done"}; !slices.Equal(order, want) {
		t.Fatalf("hooks told %v, want %v", order, want)
	}
	if want := []string{"done=Succeeded"}; !slices.Equal(st.recorded, want) {
		t.Fatalf("recorded %v, want %v", st.recorded, want)
	}
}

type completions struct{ got []store.Completion }

func (c *completions) RecordCompletion(_ context.Context, rec store.Completion) error {
	c.got = append(c.got, rec)
	return nil
}

func TestCompletionsRecordTheTaskWithoutItsAnswer(t *testing.T) {
	c := &completions{}
	r := store.ObservedRun{Name: "vikunja-gordon-claude-opus-5", InboxID: 7, Family: "vikunja-implement", Project: "gordon"}
	task := kelos.Task{Metadata: kelos.Metadata{UID: "uid-9"}, Status: &kelos.Status{Phase: kelos.PhaseSucceeded,
		CompletionTime: "2026-11-20T10:00:00Z", Results: map[string]string{"response": "c2VjcmV0", "cost-usd": "1.25"}}}
	if err := (&Completions{Store: c}).Finished(context.Background(), r, task); err != nil {
		t.Fatal(err)
	}
	if len(c.got) != 1 {
		t.Fatalf("recorded %d completions", len(c.got))
	}
	got := c.got[0]
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if got.Name != r.Name || got.InboxID != 7 || got.Family != "vikunja-implement" || got.Project != "gordon" ||
		got.TaskUID != "uid-9" || got.Phase != kelos.PhaseSucceeded || got.Completed == nil || !got.Completed.Equal(at) {
		t.Errorf("completion %+v", got)
	}
	if want := map[string]string{"cost-usd": "1.25"}; !maps.Equal(got.Results, want) {
		t.Errorf("results %v, want %v", got.Results, want)
	}
	if task.Status.Results["response"] != "c2VjcmV0" {
		t.Error("the hook changed the Task's own results")
	}
	// A Task without a status or completion time records no time.
	c.got = nil
	if err := (&Completions{Store: c}).Finished(context.Background(), r, kelos.Task{}); err != nil {
		t.Fatal(err)
	}
	if c.got[0].Completed != nil || c.got[0].Phase != "" || c.got[0].Results != nil {
		t.Errorf("completion of a bare Task %+v", c.got[0])
	}
}

type unsubmitted struct {
	calls []string
	err   error
}

func (u *unsubmitted) Unsubmitted(_ context.Context, r store.ObservedRun) error {
	u.calls = append(u.calls, r.Name)
	return u.err
}

// TestObserverTellsOfRunsFinishedUnsubmitted: only a submitting run whose
// submission ended without a Task is told, before it is recorded; a
// failed hook leaves it unrecorded, to be told again.
func TestObserverTellsOfRunsFinishedUnsubmitted(t *testing.T) {
	srv := kelostest.New(route.KelosNamespace)
	t.Cleanup(srv.Close)
	api := &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	runs := []store.ObservedRun{
		{Name: "dead-lettered", State: store.RunSubmitting, SubmitState: store.ActionFailed},
		{Name: "still-queued", State: store.RunSubmitting, SubmitState: store.ActionPending},
		{Name: "gone", State: "active", Exists: true, Phase: kelos.PhaseRunning},
	}
	st := &observed{runs: runs}
	u := &unsubmitted{}
	obs := &Observer{Store: st, Kelos: api, Unsubmitted: u, Metrics: metrics.New(),
		Log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}
	if err := obs.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(u.calls, []string{"dead-lettered"}) || !slices.Equal(st.recorded, []string{"dead-lettered=", "gone="}) {
		t.Fatalf("told %v, recorded %v; want the dead-lettered run told and both finished recorded", u.calls, st.recorded)
	}
	st, u = &observed{runs: runs[:1]}, &unsubmitted{err: errors.New("store down")}
	obs.Store, obs.Unsubmitted = st, u
	if err := obs.Once(ctx); err == nil || err.Error() != "lifecycle of unsubmitted run dead-lettered: store down" {
		t.Fatalf("a failed hook: %v", err)
	}
	if len(st.recorded) != 0 {
		t.Fatalf("recorded %v after a failed hook, want nothing", st.recorded)
	}
}
