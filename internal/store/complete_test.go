package store_test

import (
	"errors"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

const runName = "vikunja-fu-gordon-5-c1"

func dispatch(execute bool) route.Decision {
	id := run.Identity{Family: run.VikunjaFollowUp, Project: "gordon", TaskID: 5, CommentID: 1}
	submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, runName), "")
	react := action.New(action.React, action.VikunjaComment(1), "eyes")
	return route.Decision{Outcome: route.Dispatch, DeliveryKey: "k", RequestKey: "vikunja/comment/1",
		Families: []route.FamilyDecision{{
			Family: run.VikunjaFollowUp, Project: "gordon", Owner: rollout.Gordon, Execute: execute,
			Outcome: route.Dispatch,
			Runs:    []route.Run{{Name: runName, Identity: id, Profile: profile.Resolved{ConfigVersion: "v1"}}},
			Actions: []action.Action{submit, react},
		}}}
}

// submitOnly is a dispatch whose only action is the run's submission.
func submitOnly() route.Decision {
	dec := dispatch(true)
	dec.Families[0].Actions = dec.Families[0].Actions[:1]
	return dec
}

func count(t *testing.T, dbURL, sql string, args ...any) int {
	t.Helper()
	var n int
	storetest.QueryRow(t, dbURL, sql, args, &n)
	return n
}

func TestCompleteExecutedRecordsRunsActionsAndRequest(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := accept(t, s, "k")
	it := claimOne(t, s, "w")
	got, err := s.Complete(ctx, it.Lease, dispatch(true), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != route.Dispatch {
		t.Fatalf("outcome %s, want dispatch", got.Outcome)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM actions WHERE inbox_id = $1 AND state = 'pending'`, id); n != 2 {
		t.Fatalf("%d pending actions, want 2", n)
	}
	var cfg, state string
	storetest.QueryRow(t, dbURL, `SELECT config_version, state FROM runs WHERE name = $1`, []any{runName}, &cfg, &state)
	if cfg != "v1" || state != "submitting" {
		t.Fatalf("run config_version %q state %q, want v1 submitting", cfg, state)
	}
	var executed bool
	storetest.QueryRow(t, dbURL, `SELECT executed FROM requests WHERE request_key = 'vikunja/comment/1'`, nil, &executed)
	if !executed {
		t.Fatal("request recorded as not executed")
	}
	storetest.QueryRow(t, dbURL, `SELECT executed FROM decisions WHERE inbox_id = $1`, []any{id}, &executed)
	if !executed {
		t.Fatal("decision recorded as not executed")
	}
	led := s.Ledger(ctx)
	if !led.HandledRequest("vikunja/comment/1") || !led.SeenDelivery("k") || led.Err != nil {
		t.Fatalf("ledger misses the handled request or delivery (err %v)", led.Err)
	}
	if rec, ok := led.Run(runName); !ok || !rec.Active || rec.Exists {
		t.Fatalf("ledger run = %+v, %v; want an active run whose Task is not seen yet", rec, ok)
	}
}

func TestCompleteShadowQueuesNothing(t *testing.T) {
	for _, tc := range []struct {
		name            string
		execute, shadow bool
	}{{"global shadow", true, true}, {"family not executed", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, dbURL := storetest.Open(t)
			id := accept(t, s, "k")
			it := claimOne(t, s, "w")
			if _, err := s.Complete(ctx, it.Lease, dispatch(tc.execute), tc.shadow); err != nil {
				t.Fatal(err)
			}
			if n := count(t, dbURL, `SELECT count(*) FROM actions`); n != 0 {
				t.Fatalf("%d actions queued, want none", n)
			}
			if n := count(t, dbURL, `SELECT count(*) FROM runs`); n != 0 {
				t.Fatalf("%d runs recorded, want none", n)
			}
			var executed bool
			storetest.QueryRow(t, dbURL, `SELECT executed FROM requests WHERE request_key = 'vikunja/comment/1'`, nil, &executed)
			if executed {
				t.Fatal("shadow request recorded as executed")
			}
			var outcome string
			storetest.QueryRow(t, dbURL, `SELECT outcome FROM decisions WHERE inbox_id = $1 AND NOT executed`, []any{id}, &outcome)
			if outcome != string(route.Dispatch) {
				t.Fatalf("recorded outcome %q, want dispatch", outcome)
			}
		})
	}
}

func TestCompleteDropsRunTakenConcurrently(t *testing.T) {
	s, dbURL := storetest.Open(t)
	acceptRequest(t, s, "k1", "vikunja/comment/1")
	acceptRequest(t, s, "k2", "vikunja/comment/2")
	items, err := s.Claim(ctx, "w", 2, testTTL)
	if err != nil || len(items) != 2 {
		t.Fatalf("Claim = %d, %v", len(items), err)
	}
	// Both were decided on a ledger without the run.
	if _, err := s.Complete(ctx, items[0].Lease, dispatch(true), false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Complete(ctx, items[1].Lease, dispatch(true), false)
	if err != nil {
		t.Fatal(err)
	}
	fd := got.Families[0]
	if got.Outcome != route.Act || fd.Outcome != route.Act || len(fd.Runs) != 0 ||
		len(fd.Actions) != 1 || fd.Actions[0].Kind != action.React {
		t.Fatalf("second decision = %+v, want act with only the reaction", got)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM actions WHERE kind = 'submit-task'`); n != 1 {
		t.Fatalf("%d submit actions queued, want 1", n)
	}
}

func TestCompleteDropsWholeFamilyAsDuplicate(t *testing.T) {
	s, dbURL := storetest.Open(t)
	acceptRequest(t, s, "k1", "vikunja/comment/1")
	second := acceptRequest(t, s, "k2", "vikunja/comment/2")
	items, _ := s.Claim(ctx, "w", 2, testTTL)
	if _, err := s.Complete(ctx, items[0].Lease, dispatch(true), false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Complete(ctx, items[1].Lease, submitOnly(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != route.Duplicate || got.Families[0].Reason != route.ReasonRunActive {
		t.Fatalf("decision = %s/%s, want duplicate/%s", got.Outcome, got.Families[0].Reason, route.ReasonRunActive)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM actions WHERE inbox_id = $1`, second); n != 0 {
		t.Fatalf("%d actions queued for the duplicate, want none", n)
	}
	var executed bool
	storetest.QueryRow(t, dbURL, `SELECT executed FROM decisions WHERE inbox_id = $1`, []any{second}, &executed)
	if executed {
		t.Fatal("duplicate decision recorded as executed")
	}
}

func TestCompleteSecondDeliveryOfARequestQueuesNothing(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k1")
	second := accept(t, s, "k2") // the same owner request, another delivery
	items, err := s.Claim(ctx, "w", 2, testTTL)
	if err != nil || len(items) != 2 {
		t.Fatalf("Claim = %d, %v", len(items), err)
	}
	// Both were decided on a ledger without the request.
	if _, err := s.Complete(ctx, items[0].Lease, dispatch(true), false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Complete(ctx, items[1].Lease, dispatch(true), false)
	if err != nil {
		t.Fatal(err)
	}
	fd := got.Families[0]
	if got.Outcome != route.Duplicate || got.Reason != route.ReasonRequestHandled || fd.Outcome != route.Duplicate ||
		len(fd.Runs) != 0 || len(fd.Actions) != 0 {
		t.Fatalf("second decision = %+v, want a duplicate of the handled request with nothing queued", got)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM actions WHERE inbox_id = $1`, second); n != 0 {
		t.Fatalf("%d actions queued for the second delivery, want none (no second reaction)", n)
	}
	var holder int64
	storetest.QueryRow(t, dbURL, `SELECT inbox_id FROM requests WHERE request_key = 'vikunja/comment/1'`, nil, &holder)
	if holder != items[0].ID {
		t.Fatalf("request held by row %d, want the first delivery %d", holder, items[0].ID)
	}
}

// unanswered is an owner request no agent answers: ignored, with its
// reaction and notice.
func unanswered(actions ...action.Action) route.Decision {
	return route.Decision{Outcome: route.Ignore, RequestKey: "vikunja/comment/1", Families: []route.FamilyDecision{{
		Family: run.VikunjaFollowUpAnswer, Project: "gordon", Owner: rollout.Gordon, Execute: true,
		Outcome: route.Ignore, Reason: "task-done", Actions: actions}}}
}

func TestCompleteQueuesAnUnansweredRequestsActions(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := accept(t, s, "k")
	it := claimOne(t, s, "w")
	react := action.New(action.React, action.VikunjaComment(1), "eyes")
	notice := action.Notice(action.VikunjaComment(1), route.NoticeUnanswered, "task-done")
	got, err := s.Complete(ctx, it.Lease, unanswered(react, notice), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != route.Ignore || got.Families[0].Outcome != route.Ignore || got.Families[0].Reason != "task-done" {
		t.Fatalf("decision = %s/%s, want the router's ignore/task-done", got.Outcome, got.Families[0].Reason)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM actions WHERE inbox_id = $1 AND kind IN ('react', 'post-notice')`, id); n != 2 {
		t.Fatalf("%d actions queued, want the reaction and the notice", n)
	}
	var executed bool
	var outcome string
	storetest.QueryRow(t, dbURL, `SELECT executed, outcome FROM requests WHERE request_key = 'vikunja/comment/1'`, nil,
		&executed, &outcome)
	if !executed || outcome != string(route.Ignore) {
		t.Fatalf("request recorded executed=%v outcome=%q, want handled by the executed ignore", executed, outcome)
	}
}

func TestCompleteIgnoredFamilyWithoutActionsQueuesNothing(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k")
	it := claimOne(t, s, "w")
	got, err := s.Complete(ctx, it.Lease, unanswered(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != route.Ignore || got.Families[0].Outcome != route.Ignore {
		t.Fatalf("decision = %s/%s, want ignore kept", got.Outcome, got.Families[0].Outcome)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM actions`) + count(t, dbURL, `SELECT count(*) FROM requests`); n != 0 {
		t.Fatalf("%d actions and requests recorded, want none", n)
	}
}

func TestCompleteRerunsFinishedRunWhoseTaskIsGone(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k1")
	it := claimOne(t, s, "w")
	if _, err := s.Complete(ctx, it.Lease, submitOnly(), false); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, dbURL, `UPDATE runs SET state = 'finished', task_exists = true`)
	acceptRequest(t, s, "k2", "vikunja/comment/2") // the owner asks again
	it = claimOne(t, s, "w")
	got, err := s.Complete(ctx, it.Lease, submitOnly(), false)
	if err != nil || got.Families[0].Reason != route.ReasonRunExists {
		t.Fatalf("Complete while the Task exists = %+v, %v; want duplicate/run-exists", got.Families[0], err)
	}
	storetest.Exec(t, dbURL, `UPDATE runs SET task_exists = false`)
	acceptRequest(t, s, "k3", "vikunja/comment/3")
	it = claimOne(t, s, "w")
	got, err = s.Complete(ctx, it.Lease, submitOnly(), false)
	if err != nil || got.Outcome != route.Dispatch {
		t.Fatalf("Complete after cleanup = %s, %v; want dispatch", got.Outcome, err)
	}
	var gen int
	var state string
	storetest.QueryRow(t, dbURL, `SELECT generation, state FROM runs WHERE name = $1`, []any{runName}, &gen, &state)
	if gen != 2 || state != "submitting" {
		t.Fatalf("run generation %d state %s, want 2 submitting", gen, state)
	}
}

func TestCompleteWithLostLeaseWritesNothing(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k")
	stale := claimOne(t, s, "a")
	storetest.Exec(t, dbURL, `UPDATE inbox SET lease_until = now() - interval '1 second'`)
	claimOne(t, s, "b")
	if _, err := s.Complete(ctx, stale.Lease, dispatch(true), false); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("Complete with a lost lease = %v, want ErrLeaseLost", err)
	}
	for _, table := range []string{"decisions", "requests", "runs", "actions"} {
		if n := count(t, dbURL, `SELECT count(*) FROM `+table); n != 0 {
			t.Fatalf("%d rows in %s after a refused completion, want 0", n, table)
		}
	}
}

func TestCompleteRecordsOnlyOwnerRequests(t *testing.T) {
	s, dbURL := storetest.Open(t)
	d := delivery("k")
	d.Kind, d.RequestKey = "vikunja/task.updated", "vikunja/task/5"
	if _, _, err := s.Accept(ctx, d); err != nil {
		t.Fatal(err)
	}
	it := claimOne(t, s, "w")
	if _, err := s.Complete(ctx, it.Lease, dispatch(true), false); err != nil {
		t.Fatal(err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM requests`); n != 0 {
		t.Fatalf("task.updated recorded %d handled requests, want 0: its key names the task", n)
	}
}

func TestLedgerKeepsFirstError(t *testing.T) {
	s, dbURL := storetest.Open(t)
	led := s.Ledger(ctx)
	storetest.Exec(t, dbURL, `ALTER TABLE requests RENAME TO requests_gone`)
	if led.HandledRequest("x") || led.Err == nil {
		t.Fatalf("HandledRequest on a broken table = true or no error (%v)", led.Err)
	}
	first := led.Err
	if _, ok := led.Run("x"); ok || led.Err != first {
		t.Fatal("ledger answered after an error or replaced the first error")
	}
}

func TestSeenDeliveryIgnoresTheRowBeingDecided(t *testing.T) {
	s, _ := storetest.Open(t)
	accept(t, s, "k")
	it := claimOne(t, s, "w")
	if led := s.Ledger(ctx); led.SeenDelivery("k") || led.Err != nil {
		t.Fatalf("pending claimed delivery seen (err %v), want unseen until decided", led.Err)
	}
	if _, err := s.Complete(ctx, it.Lease, dispatch(true), false); err != nil {
		t.Fatal(err)
	}
	if led := s.Ledger(ctx); !led.SeenDelivery("k") || led.Err != nil {
		t.Fatalf("decided delivery not seen (err %v)", led.Err)
	}
}
