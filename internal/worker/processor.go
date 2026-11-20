package worker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
)

// Inbox retry reasons, the inbox_retries_total reason label.
const (
	RetryState     = "state-unavailable"
	RetryStore     = "store-unavailable"
	RetryLeaseLost = "lease-lost"
)

// LimitProject is the deferred_total limit label of a full project.
const LimitProject = "project"

// Processor decides inbox items: it reads the fresh upstream state,
// routes the delivery against the ledger and records the decision with
// its executed runs and actions in one fenced transaction.
type Processor struct {
	Store  *store.Store
	Router route.Router
	State  StateReader
	// Shadow records every decision and executes none, whatever the
	// ownership table says: no run and no action is queued.
	Shadow bool
	Owner  string

	Concurrency int
	LeaseTTL    time.Duration
	MaxAttempts int
	Backoff     Backoff
	// Projects limits the items decided at once per scope.
	Projects   *Limiter
	DeferDelay time.Duration

	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Tick claims up to Concurrency due items, decides them concurrently and
// returns how many it claimed. ctx bounds the claim; work bounds the
// items' processing, so a shutdown can let claimed items finish.
func (p *Processor) Tick(ctx, work context.Context) (int, error) {
	items, err := p.Store.Claim(ctx, p.Owner, p.Concurrency, p.LeaseTTL)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Go(func() { p.Process(work, it) })
	}
	wg.Wait()
	return len(items), nil
}

// Process decides one claimed item.
func (p *Processor) Process(ctx context.Context, it store.Item) {
	log := p.Log.With(logging.KeyInbox, it.ID, logging.KeyDelivery, it.DeliveryKey,
		logging.KeyRequest, it.RequestKey, logging.KeySource, string(it.Source), logging.KeyKind, string(it.Kind),
		"attempt", it.Attempts)
	source := metrics.Bound(string(it.Source), metrics.Sources)
	if it.Scope != "" && !p.Projects.TryAcquire(it.Scope) {
		p.Metrics.Deferred.WithLabelValues(store.QueueInbox, LimitProject).Inc()
		if err := p.Store.Defer(ctx, it.Lease, p.DeferDelay); err != nil {
			log.Warn("defer failed; the lease will expire", "error", err)
		}
		return
	}
	defer p.Projects.Release(it.Scope)
	d := it.Delivery()
	var st route.State
	var readErr error
	if ev, err := event.Parse(d); err == nil {
		st, readErr = p.State.Read(ctx, ev)
	}
	led := p.Store.Ledger(ctx)
	dec := p.Router.DecideAuthenticated(d, st, led)
	switch {
	case led.Err != nil:
		p.retry(ctx, log, it, source, RetryStore, led.Err)
	case needsRetry(dec):
		p.retry(ctx, log, it, source, RetryState, readErr)
	default:
		p.complete(ctx, log, it, source, dec)
	}
}

// needsRetry reports a decision that waits for unavailable state: the
// whole item is retried, so no family is recorded half-decided.
func needsRetry(dec route.Decision) bool {
	if dec.Outcome == route.Retry {
		return true
	}
	for _, f := range dec.Families {
		if f.Outcome == route.Retry {
			return true
		}
	}
	return false
}

// retry queues the item again after its backoff, or dead-letters it after
// MaxAttempts.
func (p *Processor) retry(ctx context.Context, log *slog.Logger, it store.Item, source, reason string, cause error) {
	msg := reason
	if cause != nil {
		msg += ": " + logging.Scrub(cause.Error())
	}
	if it.Attempts >= p.MaxAttempts {
		p.Metrics.InboxFailures.WithLabelValues(source).Inc()
		log.Error("dead-lettered after its last attempt", "reason", reason, "error", cause)
		if err := p.Store.Fail(ctx, it.Lease, msg); err != nil {
			log.Warn("dead-letter failed; the lease will expire", "error", err)
		}
		return
	}
	delay := p.Backoff.Delay(it.Attempts)
	p.Metrics.InboxRetries.WithLabelValues(source, reason).Inc()
	log.Warn("retry", "reason", reason, "error", cause, "delay", delay.String())
	if err := p.Store.Retry(ctx, it.Lease, delay, msg); err != nil {
		log.Warn("retry not recorded; the lease will expire", "error", err)
	}
}

func (p *Processor) complete(ctx context.Context, log *slog.Logger, it store.Item, source string, dec route.Decision) {
	rec, err := p.Store.Complete(ctx, it.Lease, dec, p.Shadow)
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		p.Metrics.InboxRetries.WithLabelValues(source, RetryLeaseLost).Inc()
		log.Warn("lease lost before the decision was recorded; another worker decides it")
		return
	case err != nil:
		p.retry(ctx, log, it, source, RetryStore, err)
		return
	}
	p.Metrics.DecisionLatency.WithLabelValues(source).Observe(time.Since(it.ReceivedAt).Seconds())
	families := metrics.Families()
	if len(rec.Families) == 0 {
		p.Metrics.Decisions.WithLabelValues(source, metrics.None, metrics.Bound(string(rec.Outcome), metrics.Outcomes), "false").Inc()
	}
	attrs := []any{"outcome", string(rec.Outcome), "shadow", p.Shadow}
	if rec.Reason != "" {
		attrs = append(attrs, "reason", rec.Reason)
	}
	for _, f := range rec.Families {
		executed := f.Execute && !p.Shadow
		p.Metrics.Decisions.WithLabelValues(source, metrics.Bound(string(f.Family), families),
			metrics.Bound(string(f.Outcome), metrics.Outcomes), strconv.FormatBool(executed)).Inc()
		attrs = append(attrs, slog.Group(string(f.Family), "outcome", string(f.Outcome), "reason", f.Reason,
			"owner", string(f.Owner), "executed", executed, "runs", runNames(f.Runs), "actions", len(f.Actions)))
	}
	log.Info("decided", attrs...)
}

func runNames(runs []route.Run) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.Name
	}
	return out
}
