package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store/storetest"
)

func TestReconcileReleasesLeasesAndReportsQueues(t *testing.T) {
	e := newEnv(t, rollout.Table{})
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	if _, err := e.store.Claim(ctx, "dead", 1, testTTL); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, e.dbURL, `UPDATE inbox SET lease_until = now() - interval '1 second',
		received_at = now() - interval '2 minutes'`)
	r := &Reconciler{Store: e.store, BodyRetention: time.Hour, Metrics: e.metric, Log: logging.New(e.logs, slog.LevelInfo)}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	m := e.metric
	if v := testutil.ToFloat64(m.LeasesExpired.WithLabelValues("inbox")); v != 1 {
		t.Fatalf("expired inbox leases = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.QueueDepth.WithLabelValues("inbox", "pending")); v != 1 {
		t.Fatalf("inbox pending depth = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.QueueOldest.WithLabelValues("inbox", "pending")); v < (2 * time.Minute).Seconds() {
		t.Fatalf("inbox oldest age = %v, want at least 120s", v)
	}
	if v := testutil.ToFloat64(m.QueueDepth.WithLabelValues("actions", "ambiguous")); v != 0 {
		t.Fatalf("empty state depth = %v, want 0", v)
	}
	if testutil.ToFloat64(m.Reconciles.WithLabelValues(ReconcileOK)) != 1 || testutil.ToFloat64(m.ReconcileLastSuccess) == 0 {
		t.Fatal("successful run not recorded")
	}
	e.store.Close()
	if err := r.Once(ctx); err == nil {
		t.Fatal("reconcile on a closed store succeeded")
	}
	if testutil.ToFloat64(m.Reconciles.WithLabelValues(ReconcileError)) != 1 {
		t.Fatal("failed run not counted")
	}
}

// countingPass counts its runs and fails with err.
type countingPass struct {
	runs int
	err  error
}

func (p *countingPass) Once(context.Context) error {
	p.runs++
	return p.err
}

func TestReconcileRunsEveryPassAndReportsAFailure(t *testing.T) {
	e := newEnv(t, rollout.Table{})
	failing, next := &countingPass{err: errors.New("kelos unavailable")}, &countingPass{}
	r := &Reconciler{Store: e.store, Passes: []Pass{failing, next}, Metrics: e.metric, Log: logging.New(e.logs, slog.LevelInfo)}
	err := r.Once(ctx)
	if err == nil || err.Error() != "kelos unavailable" || failing.runs != 1 || next.runs != 1 {
		t.Fatalf("Once = %v with runs %d/%d; want the pass error after both ran", err, failing.runs, next.runs)
	}
	if testutil.ToFloat64(e.metric.Reconciles.WithLabelValues(ReconcileError)) != 1 {
		t.Fatal("the failed pass was not counted")
	}
}

func TestReconcilePrunesBuiltTasks(t *testing.T) {
	e := newEnv(t, gordonOwns(run.VikunjaImplement))
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.proc.Tasks = fixedTask{}
	e.tick(t)
	storetest.Exec(t, e.dbURL, `UPDATE runs SET state = 'finished', updated_at = now() - interval '2 hours'`)
	storetest.Exec(t, e.dbURL, `UPDATE inbox SET body = NULL`)
	r := &Reconciler{Store: e.store, BodyRetention: time.Hour, Metrics: e.metric, Log: logging.New(e.logs, slog.LevelInfo)}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM runs WHERE task IS NULL`); n != 1 {
		t.Fatal("the finished run kept its Task past retention")
	}
	if v := testutil.ToFloat64(e.metric.BodiesPruned); v != 1 {
		t.Fatalf("pruned %v, want the one Task", v)
	}
}
