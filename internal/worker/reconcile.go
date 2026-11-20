package worker

import (
	"context"
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

// Reconciler is the periodic upkeep of the queues: it releases the leases
// of crashed or stalled workers (a running action becomes ambiguous, to be
// recovered), refreshes the queue depth and age gauges, and drops stored
// bodies past their retention.
type Reconciler struct {
	Store         *store.Store
	BodyRetention time.Duration
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
	n, err := r.Store.PruneBodies(ctx, r.BodyRetention)
	r.Metrics.BodiesPruned.Add(float64(n))
	return err
}
