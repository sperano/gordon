package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/submit"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

// vikunjaKinds are the action kinds the Vikunja writer performs.
var vikunjaKinds = []action.Kind{action.React, action.AddLabel, action.RemoveLabel, action.MoveBucket,
	action.MarkDone, action.PostNotice, action.Assign, action.SyncLabel}

// agentUsers are the routing configuration's agent variants, sorted.
func agentUsers(cfg config.Worker) []string {
	out := make([]string, 0, len(cfg.Routing.Variants))
	for v := range cfg.Routing.Variants {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// lifecycleExecutors registers the run cancellation and the review
// supersede (with Kelos), and under each kind the Vikunja writer (with a
// write token) and the GitHub writer (with the App) perform, routed by
// the target's provider: a provider without its writer defers the
// action, which stays queued.
func lifecycleExecutors(out map[action.Kind]worker.Executor, cfg config.Worker, s *store.Store, k *kelos.Client,
	app *githubapp.App) {
	if k != nil {
		out[action.CancelRun] = &submit.Canceller{Store: s, Kelos: k}
		out[action.SupersedeReviews] = &submit.Superseder{Store: s, Kelos: k}
	}
	providers := map[action.Kind]map[string]worker.Executor{}
	register := func(provider string, ex worker.Executor, kinds []action.Kind) {
		for _, kind := range kinds {
			if providers[kind] == nil {
				providers[kind] = map[string]worker.Executor{}
			}
			providers[kind][provider] = ex
		}
	}
	if ex := vikunjaExecutor(cfg, s, k); ex != nil {
		register(upstream.ProviderVikunja, ex, vikunjaKinds)
	}
	if app != nil {
		register(upstream.ProviderGitHub, githubExecutor(cfg, s, k, app), githubKinds)
	}
	for kind, byProvider := range providers {
		out[kind] = worker.ByProvider{Executors: byProvider, Delay: cfg.Backoff.Max}
	}
}

// vikunjaExecutor is the Vikunja writer's executor, nil without a write
// token.
func vikunjaExecutor(cfg config.Worker, s *store.Store, k *kelos.Client) *lifecycle.Executor {
	l := cfg.Lifecycle
	if l.WriteToken == "" {
		return nil
	}
	ex := &lifecycle.Executor{
		W:      &lifecycle.Writer{BaseURL: cfg.VikunjaURL, Authorization: "Bearer " + l.WriteToken, HTTP: &http.Client{Timeout: cfg.UpstreamTimeout}},
		Policy: cfg.Routing.Policy, Agents: agentUsers(cfg), Runs: s, Lock: s, Namespace: route.KelosNamespace,
		MaxRestarts: cfg.Restart.Max,
		Links: lifecycle.Links{Lucarne: l.LucarneURL, Cluster: l.LucarneCluster, Namespace: route.KelosNamespace,
			Sillage: l.SillageURL, VikunjaWeb: cfg.Routing.Policy.VikunjaWebURL},
	}
	if k != nil {
		ex.Messages, ex.Limits = taskMessages(k), taskLimits(k)
	}
	return ex
}

// taskLimits reads the limit that stopped a Task for its restart notices:
// "" for a Task gone or without a status, the error of a failed read.
func taskLimits(k *kelos.Client) lifecycle.TaskLimits {
	return func(ctx context.Context, name string) (string, error) {
		t, _, err := k.Get(ctx, name)
		switch {
		case kelos.IsNotFound(err):
			return "", nil
		case err != nil:
			return "", fmt.Errorf("limit of Task %s: %w", name, err)
		}
		return t.LimitType(), nil
	}
}

// taskMessages reads a Task's status message for its failure notice: ""
// for a Task gone or without a status, the error of a failed read (the
// notice retries rather than lose the message).
func taskMessages(k *kelos.Client) lifecycle.TaskMessages {
	return func(ctx context.Context, name string) (string, error) {
		t, _, err := k.Get(ctx, name)
		switch {
		case kelos.IsNotFound(err):
			return "", nil
		case err != nil:
			return "", fmt.Errorf("status message of Task %s: %w", name, err)
		case t.Status == nil:
			return "", nil
		}
		return t.Status.Message, nil
	}
}

// finishHooks are the observer's hooks: the completion record, then the
// lifecycle's failure notices.
func finishHooks(cfg config.Worker, s *store.Store) []submit.FinishHook {
	return []submit.FinishHook{&submit.Completions{Store: s},
		&lifecycle.Failures{Store: s, Policy: cfg.Routing.Policy, Ownership: cfg.Ownership, Projects: cfg.Routing.Projects}}
}

// vikunjaReconciler is the Vikunja reconciliation, nil when it is off.
func vikunjaReconciler(cfg config.Worker, s *store.Store, vikunja upstream.Vikunja, m *metrics.Metrics,
	log *slog.Logger) *lifecycle.Reconciler {
	l := cfg.Lifecycle
	if l.ReconcileInterval <= 0 {
		return nil
	}
	return &lifecycle.Reconciler{Tasks: vikunja, Inbox: s, Config: cfg.Routing,
		Lookback: l.Lookback, After: l.RedispatchAfter, Metrics: m, Log: log.With("pass", "vikunja-reconcile")}
}

// startVikunjaReconcile runs the Vikunja reconciliation in g, when it is on.
func startVikunjaReconcile(g *group, cfg config.Worker, s *store.Store, vikunja upstream.Vikunja, m *metrics.Metrics,
	log *slog.Logger) {
	vr := vikunjaReconciler(cfg, s, vikunja, m, log)
	if vr == nil {
		return
	}
	g.Go(func(ctx context.Context) error {
		worker.Every(ctx, cfg.Lifecycle.ReconcileInterval, func(ctx context.Context) { vr.Once(ctx) }) //nolint:errcheck // logged and counted
		return nil
	})
}
