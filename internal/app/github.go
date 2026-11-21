package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/ghlife"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

// githubKinds are the action kinds the GitHub writer performs.
var githubKinds = []action.Kind{action.React, action.AddLabel, action.RemoveLabel, action.ConvertDraft,
	action.SkippedChecks, action.PostNotice}

// newGitHubApp loads the GitHub App the writes act as, nil without
// GORDON_GITHUB_APP_DIR: GitHub actions then stay queued (deferred).
func newGitHubApp(cfg config.Worker) (*githubapp.App, error) {
	if cfg.GitHub.AppDir == "" {
		return nil, nil
	}
	return githubapp.Load(cfg.GitHub.AppDir, cfg.GitHubAPIURL, &http.Client{Timeout: cfg.UpstreamTimeout})
}

// githubExecutor is the GitHub writer's executor, acting as the App.
func githubExecutor(cfg config.Worker, s *store.Store, app *githubapp.App) *ghlife.Executor {
	l := cfg.Lifecycle
	return &ghlife.Executor{
		W:      &ghlife.Writer{App: app, BaseURL: cfg.GitHubAPIURL, HTTP: &http.Client{Timeout: cfg.UpstreamTimeout}},
		Policy: cfg.Routing.Policy, Runs: s, Lock: s,
		Links: lifecycle.Links{Lucarne: l.LucarneURL, Cluster: l.LucarneCluster, Namespace: route.KelosNamespace,
			Sillage: l.SillageURL, VikunjaWeb: cfg.Routing.Policy.VikunjaWebURL},
	}
}

// githubReconciler is the GitHub reconciliation, nil when it is off.
func githubReconciler(cfg config.Worker, s *store.Store, github upstream.GitHub, m *metrics.Metrics,
	log *slog.Logger) *ghlife.Reconciler {
	g := cfg.GitHub
	if g.ReconcileInterval <= 0 {
		return nil
	}
	return &ghlife.Reconciler{GitHub: github, Inbox: s, Config: cfg.Routing, Lookback: g.Lookback, Grace: g.Grace,
		Metrics: m, Log: log.With("pass", "github-reconcile")}
}

// startGitHubReconcile runs the GitHub reconciliation in g, when it is on.
func startGitHubReconcile(g *group, cfg config.Worker, s *store.Store, github upstream.GitHub, m *metrics.Metrics,
	log *slog.Logger) {
	gr := githubReconciler(cfg, s, github, m, log)
	if gr == nil {
		return
	}
	g.Go(func(ctx context.Context) error {
		worker.Every(ctx, cfg.GitHub.ReconcileInterval, func(ctx context.Context) { gr.Once(ctx) }) //nolint:errcheck // logged and counted
		return nil
	})
}
