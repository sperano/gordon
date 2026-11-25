package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
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
	e := newEnv(t)
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

func TestCancelledWriteIsDoneAsCancelled(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return Cancelled("run stopped") }
	e, o := outboxEnv(t, p)
	perform(t, o)
	if st, attempts, msg := e.actionRow(t); st != store.ActionDone || attempts != 1 || msg != "cancelled: run stopped" {
		t.Fatalf("state %s, %d attempts, %q; want done, 1, cancelled: run stopped", st, attempts, msg)
	}
	if e.results(o, ResultCancelled) != 1 || e.results(o, ResultDone) != 0 {
		t.Fatal("cancellation not counted as cancelled")
	}
	if perform(t, o) != 0 || p.executes != 1 {
		t.Fatalf("a cancelled action ran again (%d executes)", p.executes)
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

// inboxScope is the scope of the inbox row the queued action came from:
// the key the processor holds while it decides that row.
func (e *env) inboxScope(t *testing.T) string {
	t.Helper()
	var scope string
	storetest.QueryRow(t, e.dbURL, `SELECT i.scope FROM inbox i JOIN actions a ON a.inbox_id = i.id
		WHERE a.kind = 'submit-task'`, nil, &scope)
	if scope == "" {
		t.Fatal("queued action's inbox row has no scope")
	}
	return scope
}

func TestProjectLimitSharedWithInboxDefers(t *testing.T) {
	p := newProvider()
	e, o := outboxEnv(t, p)
	o.Projects = &Limiter{Default: 1}
	o.Projects.TryAcquire(e.inboxScope(t)) // the processor deciding the same project
	perform(t, o)
	if st, attempts, _ := e.actionRow(t); st != store.ActionPending || attempts != 0 || p.executes != 0 {
		t.Fatalf("state %s attempts %d executes %d, want deferred untouched", st, attempts, p.executes)
	}
	if v := testutil.ToFloat64(o.Metrics.Deferred.WithLabelValues("actions", LimitProject)); v != 1 {
		t.Fatalf("deferred metric = %v", v)
	}
}

func TestPerformedActionReleasesProjectSlot(t *testing.T) {
	p := newProvider()
	e, o := outboxEnv(t, p)
	o.Projects = &Limiter{Default: 1}
	scope := e.inboxScope(t)
	var held int
	p.execute = func(action.Action) error {
		o.Projects.mu.Lock()
		held = o.Projects.inUse[scope]
		o.Projects.mu.Unlock()
		return nil
	}
	perform(t, o)
	if st, _, _ := e.actionRow(t); st != store.ActionDone || held != 1 || o.Projects.inUse[scope] != 0 {
		t.Fatalf("state %s, slot held %d during the write, %v after; want done, 1, then freed", st, held, o.Projects.inUse)
	}
	if !o.Projects.TryAcquire(scope) {
		t.Fatal("project slot not free after the action was performed")
	}
}

func TestActionWithoutScopeTakesNoProjectSlot(t *testing.T) {
	p := newProvider()
	e, o := outboxEnv(t, p)
	storetest.Exec(t, e.dbURL, `UPDATE inbox SET scope = ''`)
	o.Projects = &Limiter{Default: 1}
	perform(t, o) // released an untaken "" slot (a nil-map write) before the fix
	if st, _, _ := e.actionRow(t); st != store.ActionDone || len(o.Projects.inUse) != 0 {
		t.Fatalf("state %s, project slots %v; want done with no slot held", st, o.Projects.inUse)
	}
}

func TestAmbiguousAtMaxAttemptsIsDeadLetteredAsUnknown(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return Ambiguous(errors.New("timeout")) }
	p.recoverEr = errors.New("kelos api unavailable")
	e, o := outboxEnv(t, p)
	for range testMaxAttempts {
		e.due(t)
		perform(t, o)
	}
	want := OutcomeUnknown + ": ambiguous: kelos api unavailable"
	if st, attempts, msg := e.actionRow(t); st != store.ActionFailed || attempts != testMaxAttempts || msg != want {
		t.Fatalf("state %s after %d attempts %q, want failed after %d with %q", st, attempts, msg, testMaxAttempts, want)
	}
	if p.executes != 1 || e.results(o, ResultFailed) != 1 {
		t.Fatalf("%d executes, %v failures; want one write and one dead letter", p.executes, e.results(o, ResultFailed))
	}
}

func TestErrorClassification(t *testing.T) {
	base := errors.New("x")
	if !isAmbiguous(Ambiguous(base)) || IsPermanent(Ambiguous(base)) || !errors.Is(Ambiguous(base), base) {
		t.Fatal("Ambiguous misclassified or does not unwrap")
	}
	if !IsPermanent(Permanent(base)) || isAmbiguous(Permanent(base)) || !errors.Is(Permanent(base), base) {
		t.Fatal("Permanent misclassified or does not unwrap")
	}
	if isAmbiguous(base) || IsPermanent(base) {
		t.Fatal("plain error classified")
	}
	if provider(action.KelosTask("ns", "n")) != "kelos" || provider(action.VikunjaTask(1)) != "vikunja" {
		t.Fatal("provider of target")
	}
}

// hangingExecutor blocks every call until its context ends, as a provider
// that never answers, and records each call's deadline.
type hangingExecutor struct {
	executes  int
	deadlines []time.Time
}

func (h *hangingExecutor) wait(ctx context.Context) error {
	d, ok := ctx.Deadline()
	if !ok {
		return errors.New("attempt has no deadline")
	}
	h.deadlines = append(h.deadlines, d)
	<-ctx.Done()
	return ctx.Err()
}

func (h *hangingExecutor) Execute(ctx context.Context, _ action.Action) error {
	h.executes++
	return h.wait(ctx)
}

func (h *hangingExecutor) Recover(ctx context.Context, _ action.Action) (bool, error) {
	return false, h.wait(ctx)
}

// attemptTTL is a lease short enough for a hung call to be cut quickly.
const attemptTTL = 40 * time.Millisecond

func TestHungWriteIsCutWithinTheLeaseAsAmbiguous(t *testing.T) {
	h := &hangingExecutor{}
	o := &Outbox{LeaseTTL: attemptTTL}
	start := time.Now()
	result, err := o.attempt(ctx, h, store.ClaimedAction{})
	if result != ResultAmbiguous || !isAmbiguous(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung write = %s, %v; want ambiguous wrapping the deadline", result, err)
	}
	if len(h.deadlines) != 1 || !h.deadlines[0].Before(start.Add(attemptTTL)) {
		t.Fatalf("write deadlines %v, want one before the lease ends at %v", h.deadlines, start.Add(attemptTTL))
	}
}

func TestHungRecoveryIsCutWithinTheLeaseAndNeverWrites(t *testing.T) {
	h := &hangingExecutor{}
	o := &Outbox{LeaseTTL: attemptTTL}
	start := time.Now()
	result, err := o.attempt(ctx, h, store.ClaimedAction{Recover: true})
	if result != ResultAmbiguous || !errors.Is(err, context.DeadlineExceeded) || h.executes != 0 {
		t.Fatalf("hung recovery = %s, %v after %d writes; want ambiguous, no write", result, err, h.executes)
	}
	if len(h.deadlines) != 1 || !h.deadlines[0].Before(start.Add(attemptTTL)) {
		t.Fatalf("recovery deadlines %v, want one before the lease ends at %v", h.deadlines, start.Add(attemptTTL))
	}
}

func TestRefusedWriteBeforeTheDeadlineRetries(t *testing.T) {
	p := newProvider()
	refused := errors.New("rate limited")
	p.execute = func(action.Action) error { return refused }
	o := &Outbox{LeaseTTL: testTTL}
	if result, err := o.attempt(ctx, p, store.ClaimedAction{}); result != ResultRetry || !errors.Is(err, refused) || isAmbiguous(err) {
		t.Fatalf("refused write = %s, %v; want a plain retry", result, err)
	}
}

func (e *env) nextAttemptIn(t *testing.T) time.Duration {
	t.Helper()
	var secs float64
	storetest.QueryRow(t, e.dbURL, `SELECT extract(epoch FROM next_attempt_at - now())::float8 FROM actions
		WHERE kind = 'submit-task'`, nil, &secs)
	return time.Duration(secs * float64(time.Second))
}

func TestDeferredWriteStaysQueuedWithoutAnAttempt(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error {
		return Deferred(errors.New("variant claude-opus runs 4 of 4"), "variant", 5*time.Minute)
	}
	e, o := outboxEnv(t, p)
	perform(t, o)
	if st, attempts, msg := e.actionRow(t); st != store.ActionPending || attempts != 0 || msg != "variant claude-opus runs 4 of 4" {
		t.Fatalf("deferred action: %s, %d attempts, %q; want pending, 0, the reason", st, attempts, msg)
	}
	if d := e.nextAttemptIn(t); d < 4*time.Minute || d > 5*time.Minute {
		t.Fatalf("due in %s, want the deferral's 5m", d)
	}
	if e.results(o, ResultDeferred) != 1 ||
		testutil.ToFloat64(o.Metrics.Deferred.WithLabelValues(store.QueueActions, "variant")) != 1 {
		t.Fatal("deferral not counted under its limit")
	}
}

func TestDeferredWithoutDelayTakesDeferDelay(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return Deferred(errors.New("branch held"), "branch", 0) }
	e, o := outboxEnv(t, p)
	perform(t, o)
	if d := e.nextAttemptIn(t); d <= 0 || d > o.DeferDelay {
		t.Fatalf("due in %s, want within DeferDelay %s", d, o.DeferDelay)
	}
}

func TestDeferredAfterAnAmbiguousWriteStillRecovers(t *testing.T) {
	p := newProvider()
	p.execute = func(action.Action) error { return Ambiguous(errors.New("timeout")) }
	e, o := outboxEnv(t, p)
	perform(t, o)
	p.execute = func(action.Action) error { return Deferred(errors.New("branch held"), "branch", time.Second) }
	e.due(t)
	perform(t, o)
	if st, attempts, _ := e.actionRow(t); st != store.ActionAmbiguous || attempts != 1 || p.recovers != 1 {
		t.Fatalf("state %s attempts %d recovers %d; want still ambiguous, the deferral uncounted", st, attempts, p.recovers)
	}
}

// heldLines counts the info lines of a first deferral.
func heldLines(e *env) int {
	n := 0
	for line := range strings.SplitSeq(e.logs.String(), "\n") {
		if strings.Contains(line, `"level":"INFO"`) && strings.Contains(line, `"msg":"action held back"`) {
			n++
		}
	}
	return n
}

// TestFirstDeferralIsLoggedAtInfo: a hold is logged at info with its
// reason once; its retries for the same reason are not, a new reason is.
func TestFirstDeferralIsLoggedAtInfo(t *testing.T) {
	p := newProvider()
	reason := "variant claude-opus runs 4 of 4"
	p.execute = func(action.Action) error { return Deferred(errors.New(reason), "variant", time.Second) }
	e, o := outboxEnv(t, p)
	perform(t, o)
	if n := heldLines(e); n != 1 || !strings.Contains(e.logs.String(), `"reason":"variant claude-opus runs 4 of 4"`) {
		t.Fatalf("%d held lines in %s, want one with the reason", n, e.logs.String())
	}
	e.due(t)
	perform(t, o)
	if n := heldLines(e); n != 1 {
		t.Fatalf("%d held lines after a retry for the same reason, want still one", n)
	}
	reason = "branch b held by x"
	e.due(t)
	perform(t, o)
	if n := heldLines(e); n != 2 {
		t.Fatalf("%d held lines after a new reason, want two", n)
	}
}
