package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/store"
)

// Executor performs one kind of action against its provider.
//
// Execute makes the write. It returns nil when the provider confirmed it,
// an error wrapped by Ambiguous when the write may have reached the
// provider without a confirmation (a timeout or a lost connection after
// sending, a 5xx to a non-idempotent call), an error wrapped by Permanent
// when retrying cannot help (the target is gone, the provider refused the
// request as invalid), and any other error when the write certainly did
// not happen (refused before sending, rate limited).
//
// Recover looks for the write's external marker (the reaction, the label,
// the notice's marker comment, the Task of that name) after an attempt
// that may have reached the provider: applied is true when the write is
// already there, so it is not made twice. Any Recover error leaves the
// outcome unknown: the action stays ambiguous and is never written blind.
//
// A write the provider cannot take yet (a full limit, a held branch)
// returns Deferred: the action stays queued without counting an attempt.
type Executor interface {
	Execute(ctx context.Context, a action.Action) error
	Recover(ctx context.Context, a action.Action) (applied bool, err error)
}

type ambiguousError struct{ err error }

func (e ambiguousError) Error() string { return "ambiguous: " + e.err.Error() }
func (e ambiguousError) Unwrap() error { return e.err }

type permanentError struct{ err error }

func (e permanentError) Error() string { return "permanent: " + e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// deferredError is a write not attempted because the provider is busy
// for it (a limit or a lock outside this process).
type deferredError struct {
	err   error
	limit string
	delay time.Duration
}

func (e deferredError) Error() string { return "deferred: " + e.err.Error() }
func (e deferredError) Unwrap() error { return e.err }

// Deferred marks err as a write not made because the provider cannot
// take it now (a full Kelos concurrency limit, a branch held by another
// Task): the action stays queued with its attempts unchanged, due again
// after delay (the outbox's DeferDelay when <= 0), and limit names the
// deferred_total limit label.
func Deferred(err error, limit string, delay time.Duration) error {
	return deferredError{err: err, limit: limit, delay: delay}
}

// cancelledError is a write not made because what it served was stopped
// meanwhile (a run cancelled under the admission lock).
type cancelledError struct{ reason string }

func (e cancelledError) Error() string { return "cancelled: " + e.reason }

// Cancelled reports a write not made, and never to be made, because what
// it served was stopped: the action is done as cancelled, which cancels
// the actions waiting for it.
func Cancelled(reason string) error { return cancelledError{reason} }

// Ambiguous marks err as an outcome that may have applied the write.
func Ambiguous(err error) error { return ambiguousError{err} }

// Permanent marks err as a refusal retrying cannot change.
func Permanent(err error) error { return permanentError{err} }

// IsCancelled reports whether err, or an error it wraps, is Cancelled.
func IsCancelled(err error) bool { return errors.As(err, new(cancelledError)) }

func isAmbiguous(err error) bool { return errors.As(err, new(ambiguousError)) }

// IsAmbiguous reports an error marked Ambiguous: the write may have applied.
func IsAmbiguous(err error) bool { return isAmbiguous(err) }

// IsPermanent reports whether err, or an error it wraps, is Permanent.
func IsPermanent(err error) bool { return errors.As(err, new(permanentError)) }

// Action results, the action_attempts_total result label.
const (
	ResultDone      = "done"
	ResultRecovered = "recovered"
	ResultRetry     = "retry"
	ResultAmbiguous = "ambiguous"
	ResultFailed    = "failed"
	ResultDeferred  = "deferred"
	ResultCancelled = "cancelled"
)

// ReasonNoExecutor defers an action whose kind has no executor in this
// release: it stays queued for the release that brings one.
const ReasonNoExecutor = "no executor for this action kind"

// OutcomeUnknown prefixes the last error of an action dead-lettered while
// ambiguous: an operator checks the provider before replaying it.
const OutcomeUnknown = "outcome unknown, the write may have applied"

// attemptShareOfLease is the part of the lease (1/attemptShareOfLease)
// one attempt (recovery and write together) may take, so a hung provider
// call is cut while this worker still holds the lease: another worker
// never starts a second write while the first is in flight, and the rest
// of the lease is left for recording the outcome.
const attemptShareOfLease = 2

// LimitProvider is the deferred_total limit label of a full provider.
const LimitProvider = "provider"

// Outbox performs queued actions.
type Outbox struct {
	Store     *store.Store
	Executors map[action.Kind]Executor
	Owner     string

	Concurrency int
	LeaseTTL    time.Duration
	MaxAttempts int
	Backoff     Backoff
	// Providers limits actions in flight per provider (the target's
	// first segment: vikunja, github, kelos); Projects per project, keyed
	// on the inbox scope like the processor's, so they share one cap.
	Providers  *Limiter
	Projects   *Limiter
	DeferDelay time.Duration
	// NoExecutorDelay is how long an action without an executor waits.
	NoExecutorDelay time.Duration

	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Tick claims up to Concurrency due actions and performs them
// concurrently; it returns how many it claimed.
func (o *Outbox) Tick(ctx, work context.Context) (int, error) {
	claimed, err := o.Store.ClaimActions(ctx, o.Owner, o.Concurrency, o.LeaseTTL)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, c := range claimed {
		wg.Go(func() { o.Perform(work, c) })
	}
	wg.Wait()
	return len(claimed), nil
}

// provider is the provider an action writes to.
func provider(t action.Target) string {
	p, _, _ := strings.Cut(string(t), "/")
	return p
}

// Perform runs one claimed action: recovery first when an earlier attempt
// may have applied it, then the write.
func (o *Outbox) Perform(ctx context.Context, c store.ClaimedAction) {
	kind := metrics.Bound(string(c.Action.Kind), metrics.ActionKinds)
	log := o.Log.With(logging.KeyActionID, c.ID, logging.KeyAction, c.Action.Key, logging.KeyInbox, c.InboxID,
		logging.KeyKind, string(c.Action.Kind), "attempt", c.Attempts, "recover", c.Recover)
	ex, ok := o.Executors[c.Action.Kind]
	if !ok {
		o.deferAction(ctx, log, c, kind, "", o.NoExecutorDelay, ReasonNoExecutor)
		return
	}
	prov := provider(c.Action.Target)
	if !o.Providers.TryAcquire(prov) {
		o.deferAction(ctx, log, c, kind, LimitProvider, o.DeferDelay, "provider limit full")
		return
	}
	defer o.Providers.Release(prov)
	if c.Scope != "" {
		if !o.Projects.TryAcquire(c.Scope) {
			o.deferAction(ctx, log, c, kind, LimitProject, o.DeferDelay, "project limit full")
			return
		}
		defer o.Projects.Release(c.Scope)
	}
	start := time.Now()
	result, err := o.attempt(ctx, ex, c)
	o.Metrics.ActionDuration.WithLabelValues(kind).Observe(time.Since(start).Seconds())
	if d, ok := errors.AsType[deferredError](err); ok && result == ResultDeferred {
		delay := d.delay
		if delay <= 0 {
			delay = o.DeferDelay
		}
		reason := logging.Scrub(d.err.Error())
		if reason != c.LastError {
			// The first deferral of this hold (a new reason since the
			// last one): later retries for the same reason stay at debug.
			log.Info("action held back", "reason", reason, "limit", d.limit, "delay", delay.String())
		}
		o.deferAction(ctx, log, c, kind, d.limit, delay, reason)
		return
	}
	o.finish(ctx, log, c, kind, result, err)
}

// attempt recovers (when needed) and executes; it returns the result
// label and, unless done or recovered, the error. A write cut by the
// attempt deadline is ambiguous, as any timeout after sending is.
func (o *Outbox) attempt(ctx context.Context, ex Executor, c store.ClaimedAction) (string, error) {
	if o.LeaseTTL > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.LeaseTTL/attemptShareOfLease)
		defer cancel()
	}
	if c.Recover {
		applied, err := ex.Recover(ctx, c.Action)
		switch {
		case err != nil:
			// Still unknown whether the write happened: never write blind.
			return ResultAmbiguous, Ambiguous(err)
		case applied:
			return ResultRecovered, nil
		}
	}
	err := ex.Execute(ctx, c.Action)
	switch {
	case err == nil:
		return ResultDone, nil
	case errors.As(err, new(deferredError)):
		return ResultDeferred, err
	case IsCancelled(err):
		return ResultCancelled, err
	case IsPermanent(err):
		return ResultFailed, err
	case isAmbiguous(err):
		return ResultAmbiguous, err
	case ctx.Err() != nil:
		// Cut mid-call: the request may have reached the provider.
		return ResultAmbiguous, Ambiguous(err)
	default:
		return ResultRetry, err
	}
}

func (o *Outbox) finish(ctx context.Context, log *slog.Logger, c store.ClaimedAction, kind, result string, err error) {
	var werr error
	switch {
	case result == ResultDone || result == ResultRecovered:
		werr = o.Store.ActionDone(ctx, c.Lease)
		o.Metrics.ActionQueueLatency.WithLabelValues(kind).Observe(time.Since(c.CreatedAt).Seconds())
		log.Info("action " + result)
	case result == ResultCancelled:
		ce, _ := errors.AsType[cancelledError](err)
		werr = o.Store.ActionCancelled(ctx, c.Lease, ce.reason)
		log.Info("action cancelled", "reason", ce.reason)
	case result == ResultFailed || c.Attempts >= o.MaxAttempts:
		msg := err.Error()
		if result == ResultAmbiguous {
			// Not "never applied": the write may be at the provider.
			msg = OutcomeUnknown + ": " + msg
		}
		result = ResultFailed
		werr = o.Store.ActionFailed(ctx, c.Lease, logging.Scrub(msg))
		log.Error("action dead-lettered", "error", msg)
	default:
		delay := o.Backoff.Delay(c.Attempts)
		werr = o.Store.ActionRetry(ctx, c.Lease, result == ResultAmbiguous, delay, logging.Scrub(err.Error()))
		log.Warn("action "+result, "error", err, "delay", delay.String())
	}
	o.Metrics.ActionResults.WithLabelValues(kind, result).Inc()
	if werr != nil {
		// The lease expires and the action is recovered before a retry.
		log.Warn("action outcome not recorded", "error", werr)
	}
}

func (o *Outbox) deferAction(ctx context.Context, log *slog.Logger, c store.ClaimedAction, kind, limit string, delay time.Duration, reason string) {
	o.Metrics.ActionResults.WithLabelValues(kind, ResultDeferred).Inc()
	if limit != "" {
		o.Metrics.Deferred.WithLabelValues(store.QueueActions, limit).Inc()
	}
	if err := o.Store.ActionDefer(ctx, c.Lease, c.Recover, delay, reason); err != nil {
		log.Warn("defer not recorded", "error", err)
	}
	log.Debug("action deferred", "reason", reason, "delay", delay.String())
}
