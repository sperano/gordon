// Package app runs Gordon's modes: the public receiver, the worker and
// the migrator. Each mode serves its health, readiness and metrics on a
// separate admin listener, so the public port exposes only the webhooks.
package app

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/store"
)

// readyTimeout bounds one readiness check's database round trips.
const readyTimeout = 2 * time.Second

// Schema is what readiness checks on the store.
type Schema interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
}

// Admin serves /healthz, /readyz and /metrics.
type Admin struct {
	Store    Schema
	Metrics  *metrics.Metrics
	draining atomic.Bool
}

// Drain makes readiness fail from now on, so the Service stops routing
// to this pod while it finishes.
func (a *Admin) Drain() { a.draining.Store(true) }

// Handler is the admin mux.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { text(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", a.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(a.Metrics.Registry, promhttp.HandlerOpts{}))
	return mux
}

// ready is 200 while not draining, the database answers and its schema
// is at least this binary's (migrations are additive).
func (a *Admin) ready(w http.ResponseWriter, r *http.Request) {
	if a.draining.Load() {
		text(w, http.StatusServiceUnavailable, "draining")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := a.Store.Ping(ctx); err != nil {
		text(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	v, err := a.Store.SchemaVersion(ctx)
	if err != nil {
		text(w, http.StatusServiceUnavailable, "schema not migrated")
		return
	}
	if want := store.LatestVersion(); v < want {
		text(w, http.StatusServiceUnavailable, fmt.Sprintf("schema version %d, want %d", v, want))
		return
	}
	text(w, http.StatusOK, "ready")
}

func text(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}
