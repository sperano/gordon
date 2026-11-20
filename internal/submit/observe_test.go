package submit

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/store/storetest"
)

func (e *env) observer() *Observer {
	return &Observer{Store: e.store, Kelos: e.api, Metrics: e.metrics, Log: logging.New(e.logs, slog.LevelDebug)}
}

func (e *env) observe(t *testing.T) {
	t.Helper()
	if err := e.observer().Once(ctx); err != nil {
		t.Fatal(err)
	}
}

func (e *env) observations(state string) float64 {
	return testutil.ToFloat64(e.metrics.RunObservations.WithLabelValues(state))
}

// TestRunLifecycleFromKelos follows a run from its queued submission to
// its Task's cleanup, after which the ledger frees its name for the
// owner's next request.
func TestRunLifecycleFromKelos(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.observe(t) // queued, no Task yet: left alone
	if st, exists := e.run(t, name); st != "submitting" || exists {
		t.Fatalf("queued run %s exists %v, want submitting", st, exists)
	}
	e.perform(t)
	e.kelos.SetPhase(name, kelos.PhaseRunning)
	e.observe(t)
	var phase string
	storetest.QueryRow(t, e.dbURL, `SELECT phase FROM runs WHERE name = $1`, []any{name}, &phase)
	if st, exists := e.run(t, name); st != "active" || !exists || phase != kelos.PhaseRunning || e.observations(ObservedActive) != 1 {
		t.Fatalf("running Task: run %s exists %v phase %s", st, exists, phase)
	}
	e.observe(t) // unchanged: nothing recorded
	if e.observations(ObservedActive) != 1 {
		t.Fatal("an unchanged run was recorded again")
	}
	e.kelos.SetPhase(name, kelos.PhaseSucceeded)
	e.observe(t)
	if st, exists := e.run(t, name); st != "finished" || !exists || e.observations(ObservedFinished) != 1 {
		t.Fatalf("succeeded Task: run %s exists %v", st, exists)
	}
	if rec, _ := e.store.Ledger(ctx).Run(name); rec.Active || !rec.Exists {
		t.Fatalf("ledger %+v: a finished run whose Task exists is still a duplicate", rec)
	}
	e.kelos.Delete(name) // Kelos's TTL cleanup
	e.observe(t)
	if st, exists := e.run(t, name); st != "finished" || exists || e.observations(ObservedGone) != 1 {
		t.Fatalf("cleaned-up Task: run %s exists %v", st, exists)
	}
	if rec, _ := e.store.Ledger(ctx).Run(name); rec.Active || rec.Exists {
		t.Fatalf("ledger %+v: the name is not free for a new request", rec)
	}
}

func TestDeletedTaskOfAnActiveRunIsGone(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	e.kelos.Delete(name) // the owner cancelled it
	e.observe(t)
	if st, exists := e.run(t, name); st != "finished" || exists {
		t.Fatalf("deleted Task: run %s exists %v, want finished and gone", st, exists)
	}
}

type failingList struct{ TaskAPI }

func (failingList) List(context.Context, string) ([]kelos.Task, error) {
	return nil, errors.New("kelos unavailable")
}

func TestObserverListFailureChangesNothing(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	o := e.observer()
	o.Kelos = failingList{e.api}
	if err := o.Once(ctx); err == nil {
		t.Fatal("a failed list was not reported")
	}
	if st, exists := e.run(t, name); st != "active" || !exists {
		t.Fatalf("run %s exists %v after a failed list, want unchanged", st, exists)
	}
}

func TestObserverWithNoRunsListsNothing(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	o := e.observer()
	o.Kelos = failingList{e.api}
	if err := o.Once(ctx); err != nil {
		t.Fatalf("Once without runs = %v, want no list at all", err)
	}
}
