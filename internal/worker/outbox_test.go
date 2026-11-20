package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

// fakeProvider stands for the provider behind an executor: it keeps the
// writes that landed, so Recover answers from what really happened.
type fakeProvider struct {
	mu        sync.Mutex
	applied   map[string]int
	executes  int
	recovers  int
	execute   func(a action.Action) error // nil applies the write
	recoverEr error
}

func newProvider() *fakeProvider { return &fakeProvider{applied: map[string]int{}} }

func (p *fakeProvider) Execute(_ context.Context, a action.Action) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.executes++
	if p.execute != nil {
		return p.execute(a)
	}
	p.applied[a.Key]++
	return nil
}

func (p *fakeProvider) Recover(_ context.Context, a action.Action) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recovers++
	if p.recoverEr != nil {
		return false, p.recoverEr
	}
	return p.applied[a.Key] > 0, nil
}

// outboxEnv queues the submit action of an executed implementation run.
func outboxEnv(t *testing.T, p *fakeProvider) (*env, *Outbox) {
	t.Helper()
	e := newEnv(t, gordonOwns(run.VikunjaImplement))
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.tick(t)
	o := &Outbox{Store: e.store, Executors: map[action.Kind]Executor{action.SubmitTask: p}, Owner: "test-outbox",
		Concurrency: testConcurrency, LeaseTTL: testTTL, MaxAttempts: testMaxAttempts, Backoff: testBackoff,
		Providers: &Limiter{}, Projects: &Limiter{}, DeferDelay: time.Second, NoExecutorDelay: time.Minute,
		Metrics: e.metric, Log: logging.New(e.logs, slog.LevelDebug)}
	return e, o
}

func (e *env) actionRow(t *testing.T) (state string, attempts int, lastErr string) {
	t.Helper()
	storetest.QueryRow(t, e.dbURL, `SELECT state, attempts, last_error FROM actions WHERE kind = 'submit-task'`, nil,
		&state, &attempts, &lastErr)
	return state, attempts, lastErr
}

func perform(t *testing.T, o *Outbox) int {
	t.Helper()
	n, err := o.Tick(ctx, ctx)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) results(o *Outbox, result string) float64 {
	return testutil.ToFloat64(o.Metrics.ActionResults.WithLabelValues(string(action.SubmitTask), result))
}

func TestActionDone(t *testing.T) {
	p := newProvider()
	e, o := outboxEnv(t, p)
	perform(t, o)
	if st, _, _ := e.actionRow(t); st != store.ActionDone || p.executes != 1 || p.recovers != 0 {
		t.Fatalf("state %s after %d executes, %d recovers; want done, 1, 0", st, p.executes, p.recovers)
	}
	if e.results(o, ResultDone) != 1 {
		t.Fatal("done not counted")
	}
}

func TestAmbiguousWriteThatLandedIsNotRepeated(t *testing.T) {
	p := newProvider()
	p.execute = func(a action.Action) error {
		p.applied[a.Key]++ // the Task was created, the response lost
		return Ambiguous(errors.New("timeout reading the response"))
	}
	e, o := outboxEnv(t, p)
	perform(t, o)
	if st, attempts, msg := e.actionRow(t); st != store.ActionAmbiguous || attempts != 1 ||
		msg != "ambiguous: timeout reading the response" {
		t.Fatalf("after an ambiguous write: %s, %d, %q", st, attempts, msg)
	}
	p.execute = nil
	e.due(t)
	perform(t, o)
	if st, _, _ := e.actionRow(t); st != store.ActionDone || p.executes != 1 || p.recovers != 1 {
		t.Fatalf("state %s after %d executes, %d recovers; want done by recovery, no second write", st, p.executes, p.recovers)
	}
	if p.applied[action.New(action.SubmitTask, action.KelosTask("kelos-agents", implementRun), "").Key] != 1 {
		t.Fatal("write applied more than once")
	}
	if e.results(o, ResultRecovered) != 1 || e.results(o, ResultAmbiguous) != 1 {
		t.Fatal("ambiguous then recovered not counted")
	}
}

func TestAmbiguousWriteThatDidNotLandIsMadeAgain(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return Ambiguous(errors.New("connection reset")) }
	e, o := outboxEnv(t, p)
	perform(t, o)
	p.execute = nil
	e.due(t)
	perform(t, o)
	if st, _, _ := e.actionRow(t); st != store.ActionDone || p.executes != 2 || p.recovers != 1 {
		t.Fatalf("state %s after %d executes, %d recovers; want done after recover then execute", st, p.executes, p.recovers)
	}
}

func TestRecoveryFailureNeverWritesBlind(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return Ambiguous(errors.New("timeout")) }
	e, o := outboxEnv(t, p)
	perform(t, o)
	p.recoverEr = errors.New("kelos api unavailable")
	e.due(t)
	perform(t, o)
	if st, _, _ := e.actionRow(t); st != store.ActionAmbiguous || p.executes != 1 {
		t.Fatalf("state %s after %d executes; want still ambiguous without a second write", st, p.executes)
	}
}

func TestCrashedWorkerActionIsRecovered(t *testing.T) {
	p := newProvider()
	e, o := outboxEnv(t, p)
	claimed, err := e.store.ClaimActions(ctx, "dead", 1, testTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	p.applied[claimed[0].Action.Key] = 1 // it wrote, then died before recording
	storetest.Exec(t, e.dbURL, `UPDATE actions SET lease_until = now() - interval '1 second'`)
	perform(t, o)
	if st, _, _ := e.actionRow(t); st != store.ActionDone || p.executes != 0 || p.recovers != 1 {
		t.Fatalf("state %s after %d executes, %d recovers; want recovered without writing", st, p.executes, p.recovers)
	}
}

func TestTransientFailureRetriesWithoutRecovery(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return errors.New("429 rate limited") }
	e, o := outboxEnv(t, p)
	perform(t, o)
	if st, _, msg := e.actionRow(t); st != store.ActionPending || msg != "429 rate limited" {
		t.Fatalf("state %s %q, want pending with the reason", st, msg)
	}
	if n := perform(t, o); n != 0 {
		t.Fatal("retried before its backoff")
	}
	p.execute = nil
	e.due(t)
	perform(t, o)
	if st, _, _ := e.actionRow(t); st != store.ActionDone || p.recovers != 0 {
		t.Fatalf("state %s after %d recovers, want done without recovery", st, p.recovers)
	}
}

func TestPermanentFailureDeadLetters(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return Permanent(errors.New("422 invalid")) }
	e, o := outboxEnv(t, p)
	perform(t, o)
	if st, _, msg := e.actionRow(t); st != store.ActionFailed || msg != "permanent: 422 invalid" {
		t.Fatalf("state %s %q, want failed", st, msg)
	}
}

func TestRetriesStopAtMaxAttempts(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return errors.New("503") }
	e, o := outboxEnv(t, p)
	for range testMaxAttempts {
		e.due(t)
		perform(t, o)
	}
	if st, attempts, _ := e.actionRow(t); st != store.ActionFailed || attempts != testMaxAttempts {
		t.Fatalf("state %s after %d attempts, want failed after %d", st, attempts, testMaxAttempts)
	}
	if e.results(o, ResultFailed) != 1 || e.results(o, ResultRetry) != testMaxAttempts-1 {
		t.Fatal("retry and failure counts wrong")
	}
}

func TestNoExecutorKeepsActionQueued(t *testing.T) {
	e, o := outboxEnv(t, newProvider())
	o.Executors = nil
	perform(t, o)
	if st, attempts, msg := e.actionRow(t); st != store.ActionPending || attempts != 0 || msg != ReasonNoExecutor {
		t.Fatalf("state %s attempts %d %q, want pending, 0, no executor", st, attempts, msg)
	}
}

func TestProviderLimitDefers(t *testing.T) {
	p := newProvider()
	e, o := outboxEnv(t, p)
	o.Providers = &Limiter{Limits: map[string]int{"kelos": 1}}
	o.Providers.TryAcquire("kelos")
	perform(t, o)
	if st, attempts, _ := e.actionRow(t); st != store.ActionPending || attempts != 0 || p.executes != 0 {
		t.Fatalf("state %s attempts %d executes %d, want deferred untouched", st, attempts, p.executes)
	}
	if v := testutil.ToFloat64(o.Metrics.Deferred.WithLabelValues("actions", LimitProvider)); v != 1 {
		t.Fatalf("deferred metric = %v", v)
	}
}

func TestErrorClassification(t *testing.T) {
	base := errors.New("x")
	if !isAmbiguous(Ambiguous(base)) || isPermanent(Ambiguous(base)) || !errors.Is(Ambiguous(base), base) {
		t.Fatal("Ambiguous misclassified or does not unwrap")
	}
	if !isPermanent(Permanent(base)) || isAmbiguous(Permanent(base)) || !errors.Is(Permanent(base), base) {
		t.Fatal("Permanent misclassified or does not unwrap")
	}
	if isAmbiguous(base) || isPermanent(base) {
		t.Fatal("plain error classified")
	}
	if provider(action.KelosTask("ns", "n")) != "kelos" || provider(action.VikunjaTask(1)) != "vikunja" {
		t.Fatal("provider of target")
	}
}
