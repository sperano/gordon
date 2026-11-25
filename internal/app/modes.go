package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/intake"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

// OnListen reports a server's bound address (tests bind port 0).
type OnListen func(server string, addr net.Addr)

// Server names.
const (
	ServerPublic = "public"
	ServerAdmin  = "admin"
)

// start binds addr and serves srv in g until g's context ends.
func start(g *group, log *slog.Logger, name, addr string, srv *http.Server, grace time.Duration, onListen OnListen) error {
	ln, err := listen(addr)
	if err != nil {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	if onListen != nil {
		onListen(name, ln.Addr())
	}
	g.Go(func(ctx context.Context) error { return serve(ctx, log, name, srv, ln, grace) })
	return nil
}

// Receiver runs the public webhook receiver until ctx ends. It holds the
// receiver database role (it inserts deliveries and reads only their keys
// and the schema version) and the webhook keys; no API token
// and no Kubernetes right.
func Receiver(ctx context.Context, cfg config.Receiver, log *slog.Logger, onListen OnListen) error {
	s, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer s.Close()
	m := metrics.New()
	admin := &Admin{Store: s, Metrics: m}
	rc := &intake.Receiver{Inbox: s, Slots: intake.NewSlots(cfg.MaxInFlight), Proxies: cfg.TrustedProxies,
		Metrics: m, Log: log,
		Deadline: cfg.RequestDeadline, Keys: intake.FileKeys(cfg.KeyFiles, func(src event.Source, err error) {
			log.Error("key file unreadable; refusing the source", "source", string(src), "error", err)
		})}
	mux := http.NewServeMux()
	mux.Handle(intake.Path, rc)
	public := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: cfg.RequestDeadline,
		WriteTimeout: cfg.RequestDeadline + writeSlack, IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes}
	g := newGroup(ctx)
	if err := start(g, log, ServerAdmin, cfg.AdminAddr, adminServer(admin.Handler()), cfg.ShutdownGrace, onListen); err != nil {
		g.cancel()
		return err
	}
	if err := start(g, log, ServerPublic, cfg.ListenAddr, public, cfg.ShutdownGrace, onListen); err != nil {
		g.cancel()
		g.Wait() //nolint:errcheck // reporting the listener error
		return err
	}
	g.Go(func(ctx context.Context) error {
		<-ctx.Done()
		admin.Drain()
		return nil
	})
	log.Info("receiver started", "sources", len(cfg.KeyFiles), "max_in_flight", cfg.MaxInFlight)
	return g.Wait()
}

// Worker runs the inbox processor, the outbox and the periodic
// reconciler until ctx ends.
func Worker(ctx context.Context, cfg config.Worker, log *slog.Logger, onListen OnListen) error {
	s, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer s.Close()
	m := metrics.New()
	admin := &Admin{Store: s, Metrics: m}
	// One project limiter for the inbox and the outbox: the per-project
	// cap bounds all of a project's work in this process.
	projects := &worker.Limiter{Default: cfg.ProjectLimit}
	k, err := newKelos(cfg, m)
	if err != nil {
		return err
	}
	app, err := newGitHubApp(cfg)
	if err != nil {
		return err
	}
	autoAssignUsers, err := resolveAutoAssignUsers(ctx, cfg)
	if err != nil {
		return err
	}
	// One Vikunja reader for the router and the reconciliation, so the
	// Vikunja upstream limit bounds their reads together (the lifecycle
	// writer reads through its own client, under the outbox's limiter).
	vikunja, github := vikunjaReader(cfg, m), githubReader(cfg, m)
	proc := processor(cfg, s, m, log, projects, vikunja, github)
	proc.State.Readiness = readinessReaders(cfg, k, app, vikunja, github)
	g := newGroup(ctx)
	if err := start(g, log, ServerAdmin, cfg.AdminAddr, adminServer(admin.Handler()), cfg.ShutdownGrace, onListen); err != nil {
		g.cancel()
		return err
	}
	g.Go(func(ctx context.Context) error {
		<-ctx.Done()
		admin.Drain()
		return nil
	})
	g.Go(func(ctx context.Context) error {
		worker.Loop(ctx, store.QueueInbox, cfg.ShutdownGrace, cfg.PollInterval, log, proc.Tick)
		return nil
	})
	out := outbox(cfg, s, m, log, projects, executors(cfg, s, k, app, m, log, autoAssignUsers))
	g.Go(func(ctx context.Context) error {
		worker.Loop(ctx, store.QueueActions, cfg.ShutdownGrace, cfg.PollInterval, log, out.Tick)
		return nil
	})
	rec := &worker.Reconciler{Store: s, BodyRetention: cfg.BodyRetention, Passes: passes(cfg, s, k, m, log),
		Metrics: m, Log: log}
	g.Go(func(ctx context.Context) error {
		worker.Every(ctx, cfg.ReconcileInterval, func(ctx context.Context) { rec.Once(ctx) }) //nolint:errcheck // logged and counted
		return nil
	})
	startVikunjaReconcile(g, cfg, s, vikunja, m, log)
	startGitHubReconcile(g, cfg, s, github, m, log)
	startConflictReconcile(g, cfg, s, github, m, log)
	startReadinessReconcile(g, cfg, s, github, m, log)
	if len(cfg.Deprecated) > 0 {
		log.Warn("ignoring removed settings: Gordon executes every family", "settings", cfg.Deprecated)
	}
	log.Info("worker started", "config", cfg.String())
	return g.Wait()
}

// upstreamObserver counts and times upstream reads.
func upstreamObserver(m *metrics.Metrics) upstream.Observer {
	return func(provider, result string, d time.Duration) {
		m.Upstream.WithLabelValues(provider, result).Inc()
		m.UpstreamDuration.WithLabelValues(provider).Observe(d.Seconds())
	}
}

// vikunjaReader is the GET-only Vikunja client of the worker's token.
func vikunjaReader(cfg config.Worker, m *metrics.Metrics) upstream.Vikunja {
	return upstream.Vikunja{Client: &upstream.Client{Provider: upstream.ProviderVikunja, BaseURL: cfg.VikunjaURL,
		TokenFile: cfg.VikunjaTokenFile, HTTP: &http.Client{Timeout: cfg.UpstreamTimeout},
		Limit: upstream.NewLimit(cfg.UpstreamLimit[upstream.ProviderVikunja]), Observe: upstreamObserver(m)}}
}

// githubReader is the GET-only GitHub client of the worker's read-only
// token, shared by the router and the reconciliation so the GitHub
// upstream limit bounds their reads together.
func githubReader(cfg config.Worker, m *metrics.Metrics) upstream.GitHub {
	return upstream.GitHub{Client: &upstream.Client{Provider: upstream.ProviderGitHub, BaseURL: cfg.GitHubAPIURL,
		TokenFile: cfg.GitHubTokenFile, Headers: upstream.GitHubHeaders,
		HTTP:  &http.Client{Timeout: cfg.UpstreamTimeout},
		Limit: upstream.NewLimit(cfg.UpstreamLimit[upstream.ProviderGitHub]), Observe: upstreamObserver(m)}}
}

func processor(cfg config.Worker, s *store.Store, m *metrics.Metrics, log *slog.Logger, projects *worker.Limiter,
	vikunja upstream.Vikunja, github upstream.GitHub) *worker.Processor {
	p := &worker.Processor{
		Store: s, Router: route.Router{Config: cfg.Routing},
		State: worker.StateReader{Config: cfg.Routing, Tasks: vikunja, PRs: github, History: github},
		Owner: cfg.ID, Concurrency: cfg.Concurrency, LeaseTTL: cfg.LeaseTTL,
		MaxAttempts: cfg.MaxAttempts, Backoff: cfg.Backoff, Projects: projects,
		DeferDelay: cfg.DeferDelay, Metrics: m, Log: log,
	}
	if cfg.Kelos != nil {
		// The task comments a prompt quotes are one more Vikunja read.
		p.Tasks = dispatch.Preparer{Builder: dispatch.Builder{Routing: cfg.Routing, Config: cfg.Kelos.Dispatch},
			Comments: vikunja}
	}
	return p
}

// outbox builds the action loop. Its provider limiter bounds writes; the
// upstream clients' limits bound reads, separately.
func outbox(cfg config.Worker, s *store.Store, m *metrics.Metrics, log *slog.Logger, projects *worker.Limiter,
	executors map[action.Kind]worker.Executor) *worker.Outbox {
	return &worker.Outbox{Store: s, Executors: executors, Owner: cfg.ID, Concurrency: cfg.Concurrency,
		LeaseTTL: cfg.LeaseTTL, MaxAttempts: cfg.MaxAttempts, Backoff: cfg.Backoff,
		Providers: actionProviders(cfg), Projects: projects,
		DeferDelay: cfg.DeferDelay, NoExecutorDelay: cfg.Backoff.Max, Metrics: m, Log: log}
}

// actionProviders limits outbox actions in flight per provider: the
// configured limit, and DefaultUpstreamLimit for a provider that has none
// (kelos), so no provider's writes are unbounded.
func actionProviders(cfg config.Worker) *worker.Limiter {
	return &worker.Limiter{Default: config.DefaultUpstreamLimit, Limits: cfg.UpstreamLimit}
}

// Migrate applies the schema migrations and grants the mode roles.
func Migrate(ctx context.Context, cfg config.Migrate, log *slog.Logger) error {
	s, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer s.Close()
	n, err := s.Migrate(ctx)
	if err != nil {
		return err
	}
	if err := s.Grant(ctx, store.Roles{Receiver: cfg.ReceiverRole, Worker: cfg.WorkerRole}); err != nil {
		return err
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	log.Info("migrated", "applied", n, "version", v, "receiver_role", cfg.ReceiverRole, "worker_role", cfg.WorkerRole)
	return nil
}
