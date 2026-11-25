package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/ghlife"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

// kelosTasks lists Kelos Tasks for the readiness gate.
type kelosTasks struct{ k *kelos.Client }

func (l kelosTasks) Tasks(ctx context.Context, selector string) ([]route.KelosTask, error) {
	tasks, err := l.k.List(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make([]route.KelosTask, len(tasks))
	for i, t := range tasks {
		out[i] = route.KelosTask{Name: t.Metadata.Name, Phase: t.Phase(), Labels: t.Metadata.Labels,
			Annotations: t.Metadata.Annotations}
	}
	return out, nil
}

// readinessReaders are the state reader's readiness readers: the App's
// check-run, commit and thread reads, the Kelos Task list, the Vikunja
// reader's task reactions and the read-only GitHub reader's listings. A
// reader missing (no App, no Kelos) leaves its field nil, which fails the
// readiness reads (config refuses a non-legacy readiness rule then).
func readinessReaders(cfg config.Worker, k *kelos.Client, app *githubapp.App, vikunja upstream.Vikunja,
	github upstream.GitHub) worker.Readiness {
	rd := worker.Readiness{Reactions: vikunja, Open: github}
	if app != nil {
		rd.GitHub = ghlife.Reads{W: &ghlife.Writer{App: app, BaseURL: cfg.GitHubAPIURL,
			HTTP: &http.Client{Timeout: cfg.UpstreamTimeout}}}
	}
	if k != nil {
		rd.Kelos = kelosTasks{k}
	}
	return rd
}

// ntfyTimeout is pr_ready_notify.DEFAULT_TIMEOUT_SECONDS.
const ntfyTimeout = 10 * time.Second

// ntfy is the ready-head publisher, nil without a token.
func ntfy(cfg config.Worker) *lifecycle.Ntfy {
	n := cfg.Readiness
	if n.NtfyToken == "" {
		return nil
	}
	return &lifecycle.Ntfy{URL: n.NtfyURL, Topic: n.NtfyTopic, Token: n.NtfyToken, HTTP: &http.Client{Timeout: ntfyTimeout}}
}

// readinessReconciler is the readiness backstop, nil when it is off.
func readinessReconciler(cfg config.Worker, s *store.Store, github upstream.GitHub, m *metrics.Metrics,
	log *slog.Logger) *ghlife.ReadinessReconciler {
	if cfg.Readiness.ReconcileInterval <= 0 {
		return nil
	}
	return &ghlife.ReadinessReconciler{GitHub: github, Inbox: s, Config: cfg.Routing,
		Interval: cfg.Readiness.ReconcileInterval, Metrics: m, Log: log.With("pass", "readiness-reconcile")}
}

// startReadinessReconcile runs the readiness backstop in g, when it is on.
func startReadinessReconcile(g *group, cfg config.Worker, s *store.Store, github upstream.GitHub, m *metrics.Metrics,
	log *slog.Logger) {
	rr := readinessReconciler(cfg, s, github, m, log)
	if rr == nil {
		return
	}
	g.Go(func(ctx context.Context) error {
		worker.Every(ctx, cfg.Readiness.ReconcileInterval, func(ctx context.Context) { rr.Once(ctx) }) //nolint:errcheck // logged and counted
		return nil
	})
}
