package worker

import (
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/rollout"
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
