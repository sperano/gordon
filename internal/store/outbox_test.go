package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

// queued completes one delivery whose decision queues the follow-up's
// two actions, and returns the database URL.
func queued(t *testing.T) (*store.Store, string) {
	t.Helper()
	s, dbURL := storetest.Open(t)
	accept(t, s, "k")
	it := claimOne(t, s, "w")
	if _, err := s.Complete(ctx, it.Lease, dispatch(true), false); err != nil {
		t.Fatal(err)
	}
	return s, dbURL
}

func claimAction(t *testing.T, s *store.Store, owner string) store.ClaimedAction {
	t.Helper()
	got, err := s.ClaimActions(ctx, owner, 1, testTTL)
	if err != nil || len(got) != 1 {
		t.Fatalf("ClaimActions = %d, %v; want 1", len(got), err)
	}
	return got[0]
}

// reclaim claims every due action and returns the one with id.
func reclaim(t *testing.T, s *store.Store, id int64) store.ClaimedAction {
	t.Helper()
	got, err := s.ClaimActions(ctx, "w", 2, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("action %d not claimable again (claimed %d others)", id, len(got))
	return store.ClaimedAction{}
}

func actionState(t *testing.T, dbURL string, id int64) (state string, attempts int, lastErr string) {
	t.Helper()
	storetest.QueryRow(t, dbURL, `SELECT state, attempts, last_error FROM actions WHERE id = $1`,
		[]any{id}, &state, &attempts, &lastErr)
	return state, attempts, lastErr
}

func TestClaimActionsCarriesTheIntent(t *testing.T) {
	s, _ := queued(t)
	a := claimAction(t, s, "w")
	want := action.New(action.SubmitTask, action.KelosTask("kelos-agents", runName), "")
	if a.Action != want || a.Family != "vikunja-followup" || a.Project != "gordon" || a.Recover || a.Attempts != 1 {
		t.Fatalf("claimed %+v, want the submit action, fresh", a)
	}
}

func TestActionAmbiguousIsRecoveredNextAttempt(t *testing.T) {
	s, dbURL := queued(t)
	a := claimAction(t, s, "w")
	if err := s.ActionRetry(ctx, a.Lease, true, 0, "timeout after send"); err != nil {
		t.Fatal(err)
	}
	if st, _, msg := actionState(t, dbURL, a.ID); st != store.ActionAmbiguous || msg != "timeout after send" {
		t.Fatalf("state %s (%q), want ambiguous with the reason", st, msg)
	}
	b := reclaim(t, s, a.ID)
	if !b.Recover || b.Attempts != 2 {
		t.Fatalf("reclaimed %+v, want the same action flagged for recovery on attempt 2", b)
	}
	if err := s.ActionDone(ctx, b.Lease); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := actionState(t, dbURL, a.ID); st != store.ActionDone {
		t.Fatalf("state %s, want done", st)
	}
	if err := s.ActionDone(ctx, b.Lease); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("second ActionDone = %v, want ErrLeaseLost", err)
	}
}

func TestActionPlainRetryNeedsNoRecovery(t *testing.T) {
	s, _ := queued(t)
	a := claimAction(t, s, "w")
	if err := s.ActionRetry(ctx, a.Lease, false, 0, "refused before send"); err != nil {
		t.Fatal(err)
	}
	if b := reclaim(t, s, a.ID); b.Recover {
		t.Fatalf("reclaimed %+v, want the same action without recovery", b)
	}
}

func TestActionCrashedWorkerIsRecovered(t *testing.T) {
	s, dbURL := queued(t)
	a := claimAction(t, s, "dead")
	storetest.Exec(t, dbURL, `UPDATE actions SET lease_until = now() - interval '1 second' WHERE id = $1`, a.ID)
	got, err := s.ClaimActions(ctx, "w", 2, testTTL)
	if err != nil || len(got) != 2 {
		t.Fatalf("ClaimActions = %d, %v", len(got), err)
	}
	for _, b := range got {
		if (b.ID == a.ID) != b.Recover {
			t.Fatalf("action %d recover=%v; only the crashed one needs recovery", b.ID, b.Recover)
		}
	}
	if err := s.ActionFailed(ctx, a.Lease, "late"); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("the dead worker's write = %v, want ErrLeaseLost", err)
	}
}

func TestActionDeferKeepsRecoveryAndAttempts(t *testing.T) {
	s, dbURL := queued(t)
	a := claimAction(t, s, "w")
	if err := s.ActionRetry(ctx, a.Lease, true, 0, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	b := reclaim(t, s, a.ID)
	if err := s.ActionDefer(ctx, b.Lease, b.Recover, 0, "no executor"); err != nil {
		t.Fatal(err)
	}
	if st, attempts, msg := actionState(t, dbURL, a.ID); st != store.ActionAmbiguous || attempts != 1 || msg != "no executor" {
		t.Fatalf("after defer: %s attempts %d %q, want ambiguous, 1, no executor", st, attempts, msg)
	}
}

func TestActionFailedIsNeverClaimed(t *testing.T) {
	s, dbURL := queued(t)
	a := claimAction(t, s, "w")
	if err := s.ActionFailed(ctx, a.Lease, "permanent: 422"); err != nil {
		t.Fatal(err)
	}
	b := claimAction(t, s, "w")
	if b.ID == a.ID {
		t.Fatal("failed action claimed again")
	}
	if st, _, msg := actionState(t, dbURL, a.ID); st != store.ActionFailed || msg != "permanent: 422" {
		t.Fatalf("state %s %q, want failed with the reason", st, msg)
	}
}

func TestQueueStatsAndExpireLeases(t *testing.T) {
	s, dbURL := queued(t)
	accept(t, s, "pending")
	claimAction(t, s, "dead")
	storetest.Exec(t, dbURL, `UPDATE actions SET lease_until = now() - interval '1 second' WHERE state = 'running'`)
	storetest.Exec(t, dbURL, `UPDATE inbox SET received_at = now() - interval '90 seconds' WHERE delivery_key = 'pending'`)
	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]store.QueueStat{}
	for _, q := range stats {
		byKey[q.Queue+"/"+q.State] = q
	}
	if q := byKey["inbox/pending"]; q.Count != 1 || q.OldestAge < 90*time.Second {
		t.Fatalf("inbox/pending = %+v, want 1 row at least 90s old", q)
	}
	if byKey["actions/pending"].Count != 1 || byKey["actions/running"].Count != 1 || len(byKey) != 3 {
		t.Fatalf("stats = %+v, want inbox/pending, actions/pending and actions/running only", stats)
	}
	inbox, actions, err := s.ExpireLeases(ctx)
	if err != nil || inbox != 0 || actions != 1 {
		t.Fatalf("ExpireLeases = %d, %d, %v; want 0, 1", inbox, actions, err)
	}
	var state string
	storetest.QueryRow(t, dbURL, `SELECT state FROM actions WHERE attempts = 1`, nil, &state)
	if state != store.ActionAmbiguous {
		t.Fatalf("expired running action is %s, want ambiguous", state)
	}
}

func TestExpireInboxLeases(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k")
	claimOne(t, s, "dead")
	storetest.Exec(t, dbURL, `UPDATE inbox SET lease_until = now() - interval '1 second'`)
	if inbox, _, err := s.ExpireLeases(ctx); inbox != 1 || err != nil {
		t.Fatalf("ExpireLeases = %d, %v; want 1", inbox, err)
	}
	var owner *string
	storetest.QueryRow(t, dbURL, `SELECT lease_owner FROM inbox`, nil, &owner)
	if owner != nil {
		t.Fatalf("lease owner %q kept, want released", *owner)
	}
}

func TestPruneBodiesKeepsKeys(t *testing.T) {
	s, dbURL := queued(t)
	accept(t, s, "pending")
	storetest.Exec(t, dbURL, `UPDATE inbox SET finished_at = now() - interval '2 hours' WHERE state = 'done'`)
	n, err := s.PruneBodies(ctx, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("PruneBodies = %d, %v; want the one finished row", n, err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM inbox WHERE body IS NULL AND delivery_key = 'k'`); n != 1 {
		t.Fatal("finished body kept")
	}
	if n := count(t, dbURL, `SELECT count(*) FROM inbox WHERE body IS NOT NULL AND delivery_key = 'pending'`); n != 1 {
		t.Fatal("pending body pruned")
	}
	if _, dup, _ := s.Accept(ctx, delivery("k")); !dup {
		t.Fatal("pruned delivery no longer deduplicated")
	}
}
