package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store/storetest"
)

// An implementation request on Vikunja project 3, Hollingsworth in the
// default configuration, assigned to one agent.
const (
	implementTaskID = 4101
	implementBody   = `{"event_name":"task.updated","data":{"task":{"id":4101,"project_id":3,"done":false,` +
		`"labels":[{"title":"feature"}],"assignees":[{"username":"claude-opus"}]},"doer":{"username":"eric"}}}`
	implementRun = "vikunja-hollingsworth-claude-opus-4101"
)

func eligible() event.Task {
	return event.Task{ID: implementTaskID, ProjectID: 3, Labels: []string{"feature"},
		Assignees: []string{"claude-opus"}, Buckets: []string{"Doing"}}
}

func implementDelivery(t *testing.T) (*env, int64) {
	t.Helper()
	e := newEnv(t, rollout.Table{})
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

func TestLegacyOwnedDecisionIsRecordedNotExecuted(t *testing.T) {
	e, id := implementDelivery(t)
	e.tasks.tasks[implementTaskID] = eligible()
	if n := e.tick(t); n != 1 {
		t.Fatalf("claimed %d, want 1", n)
	}
	d := recorded(t, e, id)
	if d.Outcome != route.Dispatch || len(d.Families) != 1 || d.Families[0].Owner != rollout.Legacy ||
		d.Families[0].Execute || d.Families[0].Runs[0].Name != implementRun {
		t.Fatalf("decision %+v, want a legacy-owned dispatch of %s", d, implementRun)
	}
	if n := e.count(t, `SELECT count(*) FROM actions`); n != 0 {
		t.Fatalf("%d actions queued for a legacy-owned family", n)
	}
	if st, _, _ := e.inboxRow(t, id); st != "done" {
		t.Fatalf("inbox row %s, want done", st)
	}
	if v := testutil.ToFloat64(e.metric.Decisions.WithLabelValues("vikunja", "vikunja-implement", "dispatch", "false")); v != 1 {
		t.Fatalf("decisions metric = %v, want 1", v)
	}
}

func TestGordonOwnedDecisionQueuesItsActions(t *testing.T) {
	e := newEnv(t, gordonOwns(run.VikunjaImplement))
	id := e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.tick(t)
	if n := e.count(t, `SELECT count(*) FROM actions WHERE inbox_id = $1 AND kind = 'submit-task'`, id); n != 1 {
		t.Fatalf("%d submit actions, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM runs WHERE name = $1`, implementRun); n != 1 {
		t.Fatal("run not recorded")
	}
}

func TestShadowModeExecutesNothingEvenWhenGordonOwns(t *testing.T) {
	e := newEnv(t, gordonOwns(run.VikunjaImplement))
	e.proc.Shadow = true
	id := e.accept(t, storedDelivery(t, event.SourceVikunja, nil, implementBody))
	e.tasks.tasks[implementTaskID] = eligible()
	e.tick(t)
	if n := e.count(t, `SELECT count(*) FROM actions`) + e.count(t, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("shadow queued %d actions or runs, want none", n)
	}
	if d := recorded(t, e, id); d.Outcome != route.Dispatch || !d.Families[0].Execute {
		t.Fatalf("decision %+v, want the dispatch Gordon would execute, recorded only", d)
	}
	if !strings.Contains(e.logs.String(), `"shadow":true`) {
		t.Fatal("decision log does not say shadow")
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
	e := newEnv(t, rollout.Table{})
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
	e := newEnv(t, gordonOwns(run.VikunjaFollowUp))
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
