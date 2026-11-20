package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

func TestReconcilePrunesTasksWhenBodyPruneFails(t *testing.T) {
	e := newEnv(t, gordonOwns(run.VikunjaImplement))
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.proc.Tasks = fixedTask{}
	e.tick(t)
	storetest.Exec(t, e.dbURL, `UPDATE runs SET state = 'finished', updated_at = now() - interval '2 hours'`)
	storetest.Exec(t, e.dbURL, `CREATE FUNCTION refuse_body_prune() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'body prune refused'; END $$`)
	storetest.Exec(t, e.dbURL, `CREATE TRIGGER refuse_body_prune BEFORE UPDATE OF body ON inbox
		FOR EACH STATEMENT EXECUTE FUNCTION refuse_body_prune()`)
	r := &Reconciler{Store: e.store, BodyRetention: time.Hour, Metrics: e.metric, Log: logging.New(e.logs, slog.LevelInfo)}
	if err := r.Once(ctx); err == nil || !strings.Contains(err.Error(), "body prune refused") {
		t.Fatalf("Once = %v, want the body prune's error", err)
	}
	if n := e.count(t, `SELECT count(*) FROM runs WHERE task IS NULL`); n != 1 {
		t.Fatal("a failed body prune kept the finished run's Task past retention")
	}
}

// orphanedClaims queues an implementation run whose submission was then
// dead-lettered: its claim actions wait for it for ever.
func orphanedClaims(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, gordonOwns(run.VikunjaImplement))
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.proc.Tasks = fixedTask{}
	e.tick(t)
	if n := e.count(t, `SELECT count(*) FROM actions WHERE after_key <> '' AND state = 'pending'`); n == 0 {
		t.Fatal("the implementation queued no claim action")
	}
	storetest.Exec(t, e.dbURL, `UPDATE actions SET state = 'failed' WHERE kind = 'submit-task'`)
	return e
}

func TestReconcileCancelsTheDependentsOfADeadLetteredAction(t *testing.T) {
	e := orphanedClaims(t)
	r := &Reconciler{Store: e.store, Metrics: e.metric, Log: logging.New(e.logs, slog.LevelInfo)}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM actions WHERE after_key <> '' AND state = 'pending'`); n != 0 {
		t.Fatalf("%d claim actions still wait for the dead-lettered submission", n)
	}
	if n := e.count(t, `SELECT count(*) FROM actions WHERE state = 'done' AND last_error LIKE 'cancelled: waited for %'`); n == 0 {
		t.Fatal("no dependent recorded cancelled")
	}
}

// TestReconcileKeepsUpkeepWhenCancelOrphansFails: a failing orphan
// cancellation is reported, and the queue gauges and the prunes still run.
func TestReconcileKeepsUpkeepWhenCancelOrphansFails(t *testing.T) {
	e := orphanedClaims(t)
	storetest.Exec(t, e.dbURL, `UPDATE runs SET state = 'finished', updated_at = now() - interval '2 hours'`)
	storetest.Exec(t, e.dbURL, `CREATE FUNCTION refuse_cancel() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'orphan cancel refused'; END $$`)
	storetest.Exec(t, e.dbURL, `CREATE TRIGGER refuse_cancel BEFORE UPDATE ON actions FOR EACH ROW
		WHEN (NEW.last_error LIKE 'cancelled: waited for %') EXECUTE FUNCTION refuse_cancel()`)
	r := &Reconciler{Store: e.store, BodyRetention: time.Hour, Metrics: e.metric, Log: logging.New(e.logs, slog.LevelInfo)}
	if err := r.Once(ctx); err == nil || !strings.Contains(err.Error(), "orphan cancel refused") {
		t.Fatalf("Once = %v, want the orphan cancellation's error", err)
	}
	if n := e.count(t, `SELECT count(*) FROM runs WHERE task IS NULL`); n != 1 {
		t.Fatal("a failed orphan cancellation skipped the Task prune")
	}
	if v := testutil.ToFloat64(e.metric.QueueDepth.WithLabelValues("actions", "failed")); v != 1 {
		t.Fatalf("failed actions depth = %v, want 1: the queue gauges were skipped", v)
	}
}
