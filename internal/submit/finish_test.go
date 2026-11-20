package submit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "running"}, Spec: kelos.Spec{Prompt: "x"}, Status: &kelos.Status{Phase: kelos.PhaseRunning}})
	st := &observed{runs: []store.ObservedRun{
		{Name: "failed", State: "active", Exists: true, Phase: kelos.PhaseRunning},
		// Finished and still present: observed before, not told again.
		{Name: "seen", State: store.RunFinished, Exists: true, Phase: kelos.PhaseRunning},
		{Name: "running", State: "active", Exists: true, Phase: kelos.PhasePending},
	}}
	h := &hook{}
	o := &Observer{Store: st, Kelos: api, Finished: h, Metrics: metrics.New(), Log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}
	if err := o.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 || h.calls[0] != "failed=Failed" {
		t.Fatalf("hook told %v, want only the run that finished now", h.calls)
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
}
