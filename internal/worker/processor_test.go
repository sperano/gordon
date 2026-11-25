package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/upstream"
)

// An implementation request on Vikunja project 3, Hollingsworth in the
// default configuration, assigned to one agent.
const (
	implementTaskID = 4101
	implementBody   = `{"event_name":"task.updated","data":{"task":{"id":4101,"project_id":3,"done":false,` +
		`"labels":[{"title":"feature"}],"assignees":[{"username":"claude-opus"}]},"doer":{"username":"eric"}}}`
	implementRun = "vikunja-4101-hollingsworth-opus"
	// unconfiguredProject is a Vikunja project no profile configures.
	unconfiguredProject = 999999
)

func eligible() event.Task {
	return event.Task{ID: implementTaskID, ProjectID: 3, Labels: []string{"feature"},
		Assignees: []string{"claude-opus"}, Buckets: []string{"Doing"}}
}

func implementDelivery(t *testing.T) (*env, int64) {
	t.Helper()
	e := newEnv(t)
	return e, e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
}

func recorded(t *testing.T, e *env, id int64) route.Decision {
	t.Helper()
	var raw []byte
	storetest.QueryRow(t, e.dbURL, `SELECT decision FROM decisions WHERE inbox_id = $1`, []any{id}, &raw)
	var d route.Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDecisionQueuesItsActions(t *testing.T) {
	e := newEnv(t)
	id := e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.tick(t)
	if n := e.count(t, `SELECT count(*) FROM actions WHERE inbox_id = $1 AND kind = 'submit-task'`, id); n != 1 {
		t.Fatalf("%d submit actions, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM runs WHERE name = $1`, implementRun); n != 1 {
		t.Fatal("run not recorded")
	}
	if v := testutil.ToFloat64(e.metric.Decisions.WithLabelValues("vikunja", "vikunja-implement", "dispatch", "true")); v != 1 {
		t.Fatalf("executed decisions metric = %v, want 1", v)
	}
	if !strings.Contains(e.logs.String(), `"executed":true`) {
		t.Fatalf("decision log does not say executed: %s", e.logs.String())
	}
}

func TestDependencyOutageRetriesThenDecides(t *testing.T) {
	e, id := implementDelivery(t)
	e.tasks.down = true
	e.tick(t)
	st, attempts, lastErr := e.inboxRow(t, id)
	if st != "pending" || attempts != 1 || !strings.HasPrefix(lastErr, RetryState+": vikunja: unavailable") {
		t.Fatalf("after an outage: %s, %d attempts, %q; want pending for a retry with the reason", st, attempts, lastErr)
	}
	if n := e.tick(t); n != 0 {
		t.Fatal("retried before its backoff")
	}
	if n := e.count(t, `SELECT count(*) FROM inbox WHERE next_attempt_at > now()`); n != 1 {
		t.Fatal("no backoff recorded")
	}
	e.tasks.down = false
	e.tasks.tasks[implementTaskID] = eligible()
	e.due(t)
	e.tick(t)
	if d := recorded(t, e, id); d.Outcome != route.Dispatch {
		t.Fatalf("after recovery: %s, want dispatch", d.Outcome)
	}
	if v := testutil.ToFloat64(e.metric.InboxRetries.WithLabelValues("vikunja", RetryState)); v != 1 {
		t.Fatalf("retries metric = %v, want 1", v)
	}
}

func TestOutageDeadLettersAfterMaxAttempts(t *testing.T) {
	e, id := implementDelivery(t)
	e.tasks.down = true
	for range testMaxAttempts {
		e.due(t)
		e.tick(t)
	}
	st, attempts, _ := e.inboxRow(t, id)
	if st != "failed" || attempts != testMaxAttempts {
		t.Fatalf("after %d failed attempts: %s, %d; want failed", testMaxAttempts, st, attempts)
	}
	if n := e.count(t, `SELECT count(*) FROM decisions`); n != 0 {
		t.Fatal("a decision was recorded for a dead letter")
	}
	if v := testutil.ToFloat64(e.metric.InboxFailures.WithLabelValues("vikunja")); v != 1 {
		t.Fatalf("failures metric = %v, want 1", v)
	}
}

func TestStateIsNotReadForDeliveriesTheRouterRefusesFirst(t *testing.T) {
	e := newEnv(t)
	unconfigured := strings.Replace(implementBody, `"project_id":3`, `"project_id":999999`, 1)
	notOwner := `{"event_name":"task.comment.created","data":{"task":{"id":5,"project_id":3},` +
		`"comment":{"id":77,"comment":"/gordon go"},"doer":{"username":"mallory"}}}`
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, unconfigured))
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, notOwner))
	e.tick(t)
	if e.tasks.reads != 0 {
		t.Fatalf("%d task reads for deliveries refused before state, want 0", e.tasks.reads)
	}
	if n := e.count(t, `SELECT count(*) FROM decisions WHERE outcome IN ('ignore', 'reject')`); n != 2 {
		t.Fatalf("%d ignore/reject decisions, want 2", n)
	}
}

func TestProjectLimitDefersWithoutConsumingAnAttempt(t *testing.T) {
	e, id := implementDelivery(t)
	e.tasks.tasks[implementTaskID] = eligible()
	e.proc.Projects = &Limiter{Default: 1}
	var scope string
	storetest.QueryRow(t, e.dbURL, `SELECT scope FROM inbox WHERE id = $1`, []any{id}, &scope)
	if !strings.HasPrefix(scope, "vikunja/project/") || !e.proc.Projects.TryAcquire(scope) {
		t.Fatalf("could not hold the project's slot %q", scope)
	}
	e.tick(t)
	st, attempts, _ := e.inboxRow(t, id)
	if st != "pending" || attempts != 0 || e.tasks.reads != 0 {
		t.Fatalf("deferred row %s with %d attempts after %d reads, want pending, 0, 0", st, attempts, e.tasks.reads)
	}
	if v := testutil.ToFloat64(e.metric.Deferred.WithLabelValues("inbox", LimitProject)); v != 1 {
		t.Fatalf("deferred metric = %v", v)
	}
	e.proc.Projects.Release(scope)
	e.due(t)
	e.tick(t)
	if st, _, _ := e.inboxRow(t, id); st != "done" {
		t.Fatalf("row %s once the slot freed, want done", st)
	}
}

func TestLeaseLostRecordsNothing(t *testing.T) {
	e, id := implementDelivery(t)
	e.tasks.tasks[implementTaskID] = eligible()
	items, err := e.store.Claim(ctx, "slow", 1, testTTL)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	storetest.Exec(t, e.dbURL, `UPDATE inbox SET lease_until = now() - interval '1 second'`)
	if n := e.tick(t); n != 1 {
		t.Fatal("expired lease not reclaimed")
	}
	e.proc.Process(ctx, items[0]) // the slow worker finishes late
	if n := e.count(t, `SELECT count(*) FROM decisions WHERE inbox_id = $1`, id); n != 1 {
		t.Fatalf("%d decisions, want exactly the reclaiming worker's", n)
	}
	if v := testutil.ToFloat64(e.metric.InboxRetries.WithLabelValues("vikunja", RetryLeaseLost)); v != 1 {
		t.Fatalf("lease-lost metric = %v, want 1", v)
	}
}

func TestCrashAfterPersistIsDecidedOnce(t *testing.T) {
	// The receiver committed the row, then died before answering: the
	// sender redelivers the same bytes, which the inbox refuses as a
	// duplicate, and the worker decides the stored row once.
	e, id := implementDelivery(t)
	if _, dup, err := e.store.Accept(ctx, storedDelivery(t, event.SourceVikunja, nil, implementBody)); !dup || err != nil {
		t.Fatalf("redelivery Accept = %v, %v; want dup", dup, err)
	}
	e.tasks.tasks[implementTaskID] = eligible()
	e.tick(t)
	e.due(t)
	if n := e.tick(t); n != 0 {
		t.Fatalf("decided row claimed again (%d)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM decisions`); n != 1 {
		t.Fatalf("%d decisions, want 1", n)
	}
	if d := recorded(t, e, id); d.Outcome != route.Dispatch {
		t.Fatalf("stored row decided %s, want dispatch", d.Outcome)
	}
}

func TestResendOfHandledRequestIsDuplicate(t *testing.T) {
	e := newEnv(t)
	comment := func(redispatch string) string {
		return `{"event_name":"task.comment.created","data":{"task":{"id":4101,"project_id":3},` +
			`"comment":{"id":88,"comment":"<p>/gordon fix it</p>"},"doer":{"username":"eric"}` + redispatch + `}}`
	}
	task := eligible()
	task.Labels = append(task.Labels, "in progress")
	e.tasks.tasks[implementTaskID] = task
	first := e.accept(t, storedDelivery(t, event.SourceVikunja, nil, comment("")))
	e.tick(t)
	if d := recorded(t, e, first); d.Outcome != route.Dispatch {
		t.Fatalf("first request: %s/%s, want dispatch", d.Outcome, d.Reason)
	}
	second := e.accept(t, storedDelivery(t, event.SourceVikunja, nil, comment(`,"redispatch":"2026-11-20T00:00:00Z"`)))
	e.tick(t)
	if d := recorded(t, e, second); d.Outcome != route.Duplicate || d.Reason != route.ReasonRequestHandled {
		t.Fatalf("resend: %s/%s, want duplicate/request-handled", d.Outcome, d.Reason)
	}
	if n := e.count(t, `SELECT count(*) FROM actions WHERE kind = 'submit-task'`); n != 1 {
		t.Fatalf("%d submissions queued, want 1", n)
	}
}

// ownerCommentOn is the owner's /gordon comment on a task of project.
func ownerCommentOn(project int) string {
	return fmt.Sprintf(`{"event_name":"task.comment.created","data":{"task":{"id":4101,"project_id":%d},`+
		`"comment":{"id":91,"comment":"/gordon go"},"doer":{"username":"eric"}}}`, project)
}

func TestTransientReadRetriesEvenWhenTheRouterWouldDecide(t *testing.T) {
	// An unconfigured project's request is answered without the task, but
	// the read looks for a move into a configured project: an outage must
	// not turn it into an unanswered decision.
	e := newEnv(t)
	id := e.accept(t, storedDelivery(t, event.SourceVikunja, nil, ownerCommentOn(unconfiguredProject)))
	e.tasks.down = true
	e.tick(t)
	st, attempts, lastErr := e.inboxRow(t, id)
	if st != "pending" || attempts != 1 || !strings.HasPrefix(lastErr, RetryState+": vikunja: unavailable") ||
		e.count(t, `SELECT count(*) FROM decisions`) != 0 {
		t.Fatalf("after a task outage: %s, %d attempts, %q; want pending for a retry, undecided", st, attempts, lastErr)
	}
	e.tasks.down = false // the task is gone: a 404 is no reason to wait
	e.due(t)
	e.tick(t)
	if d := recorded(t, e, id); d.Outcome != route.Ignore {
		t.Fatalf("after a not-found read: %s/%s, want the router's ignore", d.Outcome, d.Reason)
	}
}

func TestTransientRead(t *testing.T) {
	unavailable := &upstream.Error{Provider: upstream.ProviderVikunja, Class: upstream.Unavailable}
	limited := &upstream.Error{Provider: upstream.ProviderGitHub, Class: upstream.RateLimited}
	notFound := &upstream.Error{Provider: upstream.ProviderGitHub, Class: upstream.NotFound}
	invalid := &upstream.Error{Provider: upstream.ProviderVikunja, Class: upstream.Invalid}
	cases := map[string]struct {
		err  error
		want bool
	}{
		"none":                  {nil, false},
		"unavailable":           {unavailable, true},
		"rate limited":          {limited, true},
		"not found":             {notFound, false},
		"invalid":               {invalid, false},
		"joined with an outage": {errors.Join(notFound, unavailable), true},
		"joined permanent":      {errors.Join(notFound, invalid), false},
	}
	for name, tc := range cases {
		if got := transientRead(tc.err); got != tc.want {
			t.Errorf("%s: transientRead = %v, want %v", name, got, tc.want)
		}
	}
}

// deadlineTasks records the deadline its read was given.
type deadlineTasks struct {
	at   time.Time
	left time.Duration
	has  bool
}

func (d *deadlineTasks) Task(ctx context.Context, _ int64) (event.Task, error) {
	d.at, d.has = ctx.Deadline()
	d.left = time.Until(d.at)
	return eligible(), nil
}

func (d *deadlineTasks) TaskComments(context.Context, int64) ([]event.TaskComment, error) {
	return nil, nil
}

// deadlinePreparer is fixedTask recording the deadline it was given.
type deadlinePreparer struct {
	fixedTask
	at  time.Time
	has bool
}

func (d *deadlinePreparer) Prepare(ctx context.Context, id int64, ev event.Event, st route.State, dec *route.Decision) {
	d.at, d.has = ctx.Deadline()
	d.fixedTask.Prepare(ctx, id, ev, st, dec)
}

func TestStateReadsGetPartOfTheLease(t *testing.T) {
	e, id := implementDelivery(t)
	reads, prep := &deadlineTasks{}, &deadlinePreparer{}
	e.proc.State.Tasks, e.proc.Tasks = reads, prep
	e.tick(t)
	if !reads.has || reads.left > testTTL/readShareOfLease {
		t.Fatalf("read deadline set %v, %v left; want at most %v of the %v lease", reads.has, reads.left,
			testTTL/readShareOfLease, testTTL)
	}
	if !prep.has || !prep.at.Equal(reads.at) {
		t.Fatalf("prepare deadline set %v at %v; want the read's %v, one share for both", prep.has, prep.at, reads.at)
	}
	if d := recorded(t, e, id); d.Outcome != route.Dispatch {
		t.Fatalf("decision %s, want dispatch", d.Outcome)
	}
}

// leaseStealer is a task outage during which another worker takes the row.
func (leaseStealer) TaskComments(context.Context, int64) ([]event.TaskComment, error) {
	return nil, nil
}

type leaseStealer struct {
	t     *testing.T
	dbURL string
}

func (l leaseStealer) Task(context.Context, int64) (event.Task, error) {
	storetest.Exec(l.t, l.dbURL, `UPDATE inbox SET lease_token = gen_random_uuid()`)
	return event.Task{}, &upstream.Error{Provider: upstream.ProviderVikunja, Class: upstream.Unavailable}
}

func TestRetryNotRecordedIsNotCounted(t *testing.T) {
	for _, last := range []bool{false, true} {
		e, _ := implementDelivery(t)
		e.proc.State.Tasks = leaseStealer{t, e.dbURL}
		if last {
			e.proc.MaxAttempts = 1
		}
		e.tick(t)
		retries := testutil.ToFloat64(e.metric.InboxRetries.WithLabelValues("vikunja", RetryState))
		failures := testutil.ToFloat64(e.metric.InboxFailures.WithLabelValues("vikunja"))
		if retries != 0 || failures != 0 {
			t.Fatalf("last attempt %v: %v retries, %v dead letters counted for a lost lease; want none", last, retries, failures)
		}
	}
}

func TestGordonFamilyWithNothingQueuedIsNotExecuted(t *testing.T) {
	e := newEnv(t)
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	idle := eligible()
	idle.Buckets = []string{"To-Do"}
	e.tasks.tasks[implementTaskID] = idle
	e.tick(t)
	if n := e.count(t, `SELECT count(*) FROM actions`) + e.count(t, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("%d actions or runs queued for a task not in Doing", n)
	}
	if strings.Contains(e.logs.String(), `"executed":true`) {
		t.Fatalf("log says executed for a family with nothing queued: %s", e.logs.String())
	}
	if !strings.Contains(e.logs.String(), `"executed":false`) {
		t.Fatalf("decision log lacks the family's executed flag: %s", e.logs.String())
	}
}

// fixedTask gives every run a Task named after it.
type fixedTask struct{}

func (fixedTask) Prepare(_ context.Context, _ int64, _ event.Event, _ route.State, dec *route.Decision) {
	for i := range dec.Families {
		for j := range dec.Families[i].Runs {
			rn := &dec.Families[i].Runs[j]
			rn.Task = &kelos.Task{Metadata: kelos.Metadata{Name: rn.Name}, Spec: kelos.Spec{Prompt: "built"}}
		}
	}
}

// rejecting rejects every family, as an unbuildable Task does.
type rejecting struct{ calls int }

func (r *rejecting) Prepare(_ context.Context, _ int64, _ event.Event, _ route.State, dec *route.Decision) {
	r.calls++
	for i := range dec.Families {
		fd := &dec.Families[i]
		fd.Outcome, fd.Reason, fd.Runs, fd.Actions = route.Reject, "task-spec-x", nil, nil
	}
	dec.Outcome = route.Overall(dec.Families)
}

func TestPreparedTaskIsRecordedWithTheRun(t *testing.T) {
	e := newEnv(t)
	e.proc.Tasks = fixedTask{}
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.tick(t)
	if n := e.count(t, `SELECT count(*) FROM runs WHERE name = $1 AND task->'spec'->>'prompt' = 'built'`, implementRun); n != 1 {
		t.Fatal("run recorded without its prepared Task")
	}
}

func TestPreparerRejectIsRecorded(t *testing.T) {
	e := newEnv(t)
	p := &rejecting{}
	e.proc.Tasks = p
	id := e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.tick(t)
	dec := recorded(t, e, id)
	if p.calls != 1 || dec.Outcome != route.Reject || dec.Families[0].Reason != "task-spec-x" ||
		e.count(t, `SELECT count(*) FROM actions`)+e.count(t, `SELECT count(*) FROM runs`) != 0 {
		t.Fatalf("decision %+v after %d prepares; want the reject recorded, nothing queued", dec, p.calls)
	}
}

func TestPreparerNotCalledOnRetry(t *testing.T) {
	e := newEnv(t)
	p := &rejecting{}
	e.proc.Tasks = p
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.down = true
	e.tick(t)
	if n := e.count(t, `SELECT count(*) FROM inbox WHERE state = 'pending' AND attempts = 1 AND next_attempt_at > now()`); n != 1 {
		t.Fatal("the outage did not schedule a retry")
	}
	if p.calls != 0 {
		t.Fatalf("prepared %d times for an item retried on its state", p.calls)
	}
}
