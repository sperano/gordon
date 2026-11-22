package app

import (
	"log/slog"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/restart"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/submit"
	"github.com/sperano/gordon/internal/worker"
)

// newKelos is the Kelos Task API client, nil without a dispatch
// configuration.
func newKelos(cfg config.Worker, m *metrics.Metrics) (*kelos.Client, error) {
	if cfg.Kelos == nil {
		return nil, nil
	}
	hc, err := kelos.NewHTTPClient(cfg.Kelos.CAFile, cfg.UpstreamTimeout)
	if err != nil {
		return nil, err
	}
	return &kelos.Client{BaseURL: cfg.Kelos.APIURL, Namespace: cfg.Kelos.Dispatch.Namespace,
		TokenFile: cfg.Kelos.TokenFile, HTTP: hc,
		Observe: func(op, result string, d time.Duration) {
			op = metrics.Bound(op, kelos.Ops)
			m.Kelos.WithLabelValues(op, metrics.Bound(result, kelos.Results)).Inc()
			m.KelosDuration.WithLabelValues(op).Observe(d.Seconds())
		}}, nil
}

// executors are the outbox executors of this release: Kelos Task
// submission, run cancellation and review supersede when Kelos is
// configured, the Vikunja writer with a write token and the GitHub writer
// with the App. A writer missing leaves its actions queued (deferred),
// never dropped.
func executors(cfg config.Worker, s *store.Store, k *kelos.Client, app *githubapp.App, m *metrics.Metrics,
	log *slog.Logger) map[action.Kind]worker.Executor {
	out := map[action.Kind]worker.Executor{}
	if k != nil {
		out[action.SubmitTask] = &submit.Executor{Runs: s, Kelos: k, Limits: cfg.Kelos.Dispatch.Limits,
			BusyDelay: cfg.Kelos.BusyDelay, Metrics: m, Log: log.With("executor", string(action.SubmitTask))}
	}
	lifecycleExecutors(out, cfg, s, k, app)
	return out
}

// passes are the reconciler's Kelos passes: the shadow comparison and,
// outside shadow mode (the only one with runs), the run observer and the
// limit restarts.
func passes(cfg config.Worker, s *store.Store, k *kelos.Client, m *metrics.Metrics, log *slog.Logger) []worker.Pass {
	if k == nil {
		return nil
	}
	out := []worker.Pass{&submit.Shadow{Store: s, Kelos: k, Window: cfg.Kelos.ShadowWindow, Metrics: m,
		Log: log.With("pass", "shadow")}}
	if !cfg.Shadow {
		out = append(out, &submit.Observer{Store: s, Kelos: k, Finished: finishHooks(cfg, s), Metrics: m,
			Log: log.With("pass", "observe")}, limitRestarts(cfg, s, k, m, log))
	}
	return out
}

// limitRestarts is the limit restart pass.
func limitRestarts(cfg config.Worker, s *store.Store, k *kelos.Client, m *metrics.Metrics, log *slog.Logger) *restart.Reaper {
	return &restart.Reaper{Store: s, Kelos: k, Config: cfg.Routing, Ownership: cfg.Ownership,
		MaxRestarts: cfg.Restart.Max, Grace: cfg.Restart.Grace, Fallback: cfg.Restart.Fallback,
		Metrics: m, Log: log.With("pass", "limit-restart")}
}
