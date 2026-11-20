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
// receiver database role (insert only) and the webhook keys; no API token
// and no Kubernetes right.
func Receiver(ctx context.Context, cfg config.Receiver, log *slog.Logger, onListen OnListen) error {
	s, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer s.Close()
	m := metrics.New()
	admin := &Admin{Store: s, Metrics: m}
	rc := &intake.Receiver{Inbox: s, Slots: intake.NewSlots(cfg.MaxInFlight), Metrics: m, Log: log,
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

// executors are the outbox executors of this release: none yet. The
// dispatch (Kelos Task submission) and lifecycle stages register theirs;
// until then a queued action stays queued (deferred), never dropped.
func executors() map[action.Kind]worker.Executor { return map[action.Kind]worker.Executor{} }

// Worker runs the inbox processor, the outbox (unless shadow) and the
// periodic reconciler until ctx ends.
func Worker(ctx context.Context, cfg config.Worker, log *slog.Logger, onListen OnListen) error {
	s, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer s.Close()
	m := metrics.New()
	admin := &Admin{Store: s, Metrics: m}
	proc := processor(cfg, s, m, log)
	g := newGroup(ctx)
	if err := start(g, log, ServerAdmin, cfg.AdminAddr, adminServer(admin.Handler()), cfg.ShutdownGrace, onListen); err != nil {
		g.cancel()
		return err
	}
	g.Go(func(ctx context.Context) error {
		worker.Loop(ctx, store.QueueInbox, cfg.ShutdownGrace, cfg.PollInterval, log, proc.Tick)
		admin.Drain()
		return nil
	})
	if !cfg.Shadow {
		out := outbox(cfg, s, m, log)
		g.Go(func(ctx context.Context) error {
			worker.Loop(ctx, store.QueueActions, cfg.ShutdownGrace, cfg.PollInterval, log, out.Tick)
			return nil
		})
	}
	rec := &worker.Reconciler{Store: s, BodyRetention: cfg.BodyRetention, Metrics: m, Log: log}
	g.Go(func(ctx context.Context) error {
		worker.Every(ctx, cfg.ReconcileInterval, func(ctx context.Context) { rec.Once(ctx) }) //nolint:errcheck // logged and counted
		return nil
	})
	log.Info("worker started", "config", cfg.String())
	return g.Wait()
}

func processor(cfg config.Worker, s *store.Store, m *metrics.Metrics, log *slog.Logger) *worker.Processor {
	hc := &http.Client{Timeout: cfg.UpstreamTimeout}
	observe := func(provider, result string, d time.Duration) {
		m.Upstream.WithLabelValues(provider, result).Inc()
		m.UpstreamDuration.WithLabelValues(provider).Observe(d.Seconds())
	}
	vikunja := &upstream.Client{Provider: upstream.ProviderVikunja, BaseURL: cfg.VikunjaURL,
		Authorization: "Bearer " + cfg.VikunjaToken, HTTP: hc,
		Limit: upstream.NewLimit(cfg.UpstreamLimit[upstream.ProviderVikunja]), Observe: observe}
	github := &upstream.Client{Provider: upstream.ProviderGitHub, BaseURL: cfg.GitHubAPIURL,
		Authorization: "Bearer " + cfg.GitHubToken, Headers: upstream.GitHubHeaders, HTTP: hc,
		Limit: upstream.NewLimit(cfg.UpstreamLimit[upstream.ProviderGitHub]), Observe: observe}
	return &worker.Processor{
		Store: s, Router: route.Router{Config: cfg.Routing, Ownership: cfg.Ownership},
		State:  worker.StateReader{Config: cfg.Routing, Tasks: upstream.Vikunja{Client: vikunja}, PRs: upstream.GitHub{Client: github}},
		Shadow: cfg.Shadow, Owner: cfg.ID, Concurrency: cfg.Concurrency, LeaseTTL: cfg.LeaseTTL,
		MaxAttempts: cfg.MaxAttempts, Backoff: cfg.Backoff, Projects: &worker.Limiter{Default: cfg.ProjectLimit},
		DeferDelay: cfg.DeferDelay, Metrics: m, Log: log,
	}
}

func outbox(cfg config.Worker, s *store.Store, m *metrics.Metrics, log *slog.Logger) *worker.Outbox {
	return &worker.Outbox{Store: s, Executors: executors(), Owner: cfg.ID, Concurrency: cfg.Concurrency,
		LeaseTTL: cfg.LeaseTTL, MaxAttempts: cfg.MaxAttempts, Backoff: cfg.Backoff,
		Providers: &worker.Limiter{Limits: cfg.UpstreamLimit}, Projects: &worker.Limiter{Default: cfg.ProjectLimit},
		DeferDelay: cfg.DeferDelay, NoExecutorDelay: cfg.Backoff.Max, Metrics: m, Log: log}
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
	log.Info("migrated", "applied", n, "version", v, "receiver_role", cfg.ReceiverRole, "worker_role", cfg.WorkerRole)
	return err
}
