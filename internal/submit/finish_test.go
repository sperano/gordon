package submit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

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
}

func (h *hook) Finished(_ context.Context, r store.ObservedRun, phase string) error {
	h.calls = append(h.calls, r.Name+"="+phase)
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
	o := &Observer{Store: st, Kelos: api, Finished: h, Metrics: metrics.New(), Log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}
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
