package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/store"
)

// Reconcile results, the reconcile_runs_total result label.
const (
	ReconcileOK    = "ok"
	ReconcileError = "error"
)

// Pass is one more step of each reconciliation (the Kelos run observer,
// the shadow comparison).
type Pass interface {
	Once(ctx context.Context) error
}

// Reconciler is the periodic upkeep of the queues: it releases the leases
// of crashed or stalled workers (a running action becomes ambiguous, to be
// recovered), refreshes the queue depth and age gauges, drops stored
// bodies and built Tasks past their retention, then runs its Passes. A
// failed pass does not stop the next one; the run fails if any did.
type Reconciler struct {
	Store         *store.Store
	BodyRetention time.Duration
	Passes        []Pass
	Metrics       *metrics.Metrics
	Log           *slog.Logger
}

// Once runs one reconciliation.
func (r *Reconciler) Once(ctx context.Context) error {
	start := time.Now()
	err := r.run(ctx)
	r.Metrics.ReconcileDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		r.Metrics.Reconciles.WithLabelValues(ReconcileError).Inc()
		r.Log.Warn("reconcile failed", "error", err)
		return err
	}
	r.Metrics.Reconciles.WithLabelValues(ReconcileOK).Inc()
	r.Metrics.ReconcileLastSuccess.SetToCurrentTime()
	return nil
}

func (r *Reconciler) run(ctx context.Context) error {
	errs := []error{r.upkeep(ctx)}
	for _, p := range r.Passes {
		errs = append(errs, p.Once(ctx))
	}
	return errors.Join(errs...)
}

func (r *Reconciler) upkeep(ctx context.Context) error {
	inbox, actions, err := r.Store.ExpireLeases(ctx)
	if err != nil {
		return err
	}
	r.Metrics.LeasesExpired.WithLabelValues(store.QueueInbox).Add(float64(inbox))
	r.Metrics.LeasesExpired.WithLabelValues(store.QueueActions).Add(float64(actions))
	if inbox+actions > 0 {
		r.Log.Warn("released expired leases", "inbox", inbox, "actions", actions)
	}
	stats, err := r.Store.QueueStats(ctx)
	if err != nil {
		return err
	}
	r.Metrics.SetQueues(stats)
	if r.BodyRetention <= 0 {
		return nil
	}
	// Each prune runs whatever the other's outcome.
	n, bodiesErr := r.Store.PruneBodies(ctx, r.BodyRetention)
	r.Metrics.BodiesPruned.Add(float64(n))
	n, tasksErr := r.Store.PruneTasks(ctx, r.BodyRetention)
	r.Metrics.BodiesPruned.Add(float64(n))
	return errors.Join(bodiesErr, tasksErr)
}
