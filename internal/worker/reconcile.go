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
// the limit restarts, the cost notices).
type Pass interface {
	Once(ctx context.Context) error
}

// Reconciler is the periodic upkeep of the queues: it releases the leases
// of crashed or stalled workers (a running action becomes ambiguous, to be
// recovered), cancels the pending actions whose prerequisite failed or was
// cancelled, refreshes the queue depth and age gauges, drops stored
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

// upkeep runs each housekeeping step whatever another's outcome, so one
// failing query skips neither the metrics nor retention; it returns every
// step's error.
func (r *Reconciler) upkeep(ctx context.Context) error {
	return errors.Join(r.expireLeases(ctx), r.cancelOrphans(ctx), r.queueStats(ctx), r.prune(ctx))
}

func (r *Reconciler) expireLeases(ctx context.Context) error {
	inbox, actions, err := r.Store.ExpireLeases(ctx)
	if err != nil {
		return err
	}
	r.Metrics.LeasesExpired.WithLabelValues(store.QueueInbox).Add(float64(inbox))
	r.Metrics.LeasesExpired.WithLabelValues(store.QueueActions).Add(float64(actions))
	if inbox+actions > 0 {
		r.Log.Warn("released expired leases", "inbox", inbox, "actions", actions)
	}
	return nil
}

// cancelOrphans cancels the actions waiting for one that will never be
// done.
func (r *Reconciler) cancelOrphans(ctx context.Context) error {
	n, err := r.Store.CancelOrphans(ctx)
	if n > 0 {
		r.Log.Info("cancelled actions whose prerequisite failed or was cancelled", "actions", n)
	}
	return err
}

func (r *Reconciler) queueStats(ctx context.Context) error {
	stats, err := r.Store.QueueStats(ctx)
	if err != nil {
		return err
	}
	r.Metrics.SetQueues(stats)
	return nil
}

func (r *Reconciler) prune(ctx context.Context) error {
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
