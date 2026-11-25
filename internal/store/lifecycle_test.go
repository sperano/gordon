package store_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

const submitKey = "submit-task:kelos/kelos-agents/" + runName

// claimed is dispatch() whose run's claim waits for its submission:
// a label after the submit, a notice after the label.
func claimed() route.Decision {
	dec := dispatch()
	fd := &dec.Families[0]
	label := action.New(action.AddLabel, action.VikunjaTask(5), "in progress").WaitFor(fd.Actions[0])
	fd.Actions = append(fd.Actions, label, action.Notice(action.VikunjaTask(5), "start", runName).WaitFor(label))
	return dec
}

func claimedKinds(t *testing.T, s *store.Store) []string {
	t.Helper()
	cs, err := s.ClaimActions(ctx, "w", 10, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, string(c.Action.Kind))
		if err := s.ActionDone(ctx, c.Lease); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(out)
	return out
}

// TestActionsWaitForTheirPrerequisite: a claim is not claimed before the
// submission it waits for is done, then each level in turn.
func TestActionsWaitForTheirPrerequisite(t *testing.T) {
	s, _ := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", claimed())
	if got := claimedKinds(t, s); !slices.Equal(got, []string{"react", "submit-task"}) {
		t.Fatalf("first claim %v, want only the actions waiting for nothing", got)
	}
	if got := claimedKinds(t, s); !slices.Equal(got, []string{"add-label"}) {
		t.Fatalf("second claim %v, want the label once the submission is done", got)
	}
	if got := claimedKinds(t, s); !slices.Equal(got, []string{"post-notice"}) {
		t.Fatalf("third claim %v, want the notice once the label is done", got)
	}
}

// TestDeadLetteredPrerequisiteCancelsItsDependents: a submission that
// failed for good claims nothing; its claim chain is cancelled, level by
// level, never claimed.
func TestDeadLetteredPrerequisiteCancelsItsDependents(t *testing.T) {
	s, dbURL := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", claimed())
	cs, err := s.ClaimActions(ctx, "w", 10, testTTL)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Action.Kind == action.SubmitTask {
			err = s.ActionFailed(ctx, c.Lease, "refused")
		} else {
			err = s.ActionDone(ctx, c.Lease)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CancelOrphans(ctx)
	if err != nil || n != 2 {
		t.Fatalf("CancelOrphans = %d, %v, want the label and the notice", n, err)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM actions WHERE state = 'done' AND last_error LIKE 'cancelled: %'`); got != 2 {
		t.Fatalf("%d cancelled actions, want 2", got)
	}
	if cs, _ := s.ClaimActions(ctx, "w", 10, testTTL); len(cs) != 0 {
		t.Fatalf("claimed %d actions after the cancellation", len(cs))
	}
}

// TestCancelRunsStopsAQueuedSubmission: a run cancelled before its Task
// exists is finished, its submission and its claim are cancelled, and a
// run already finished is left alone.
func TestCancelRunsStopsAQueuedSubmission(t *testing.T) {
	s, dbURL := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", claimed())
	stopped, err := s.CancelRuns(ctx, []string{runName, "never-recorded"})
	if err != nil || !slices.Equal(stopped, []string{runName}) {
		t.Fatalf("CancelRuns = %v, %v", stopped, err)
	}
	r, err := s.RunToSubmit(ctx, runName)
	if err != nil || !r.Submitted() {
		t.Fatalf("cancelled run %+v (%v) still submits", r, err)
	}
	var phase string
	storetest.QueryRow(t, dbURL, `SELECT phase FROM runs WHERE name = $1`, []any{runName}, &phase)
	if phase != store.PhaseCancelled {
		t.Errorf("phase %q, want %s", phase, store.PhaseCancelled)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM actions WHERE last_error = 'cancelled: run stopped'`); got != 2 {
		t.Fatalf("%d actions cancelled, want the submission and the label", got)
	}
	if again, err := s.CancelRuns(ctx, []string{runName}); err != nil || len(again) != 0 {
		t.Fatalf("second CancelRuns = %v, %v, want nothing to stop", again, err)
	}
}

// TestCompleteDropsTheClaimOfARunTakenConcurrently: a run another worker
// recorded meanwhile loses its submission and every action waiting for it.
func TestCompleteDropsTheClaimOfARunTakenConcurrently(t *testing.T) {
	s, dbURL := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", submitOnly())
	dec := claimed()
	dec.RequestKey = "vikunja/comment/2"
	acceptRequest(t, s, "k2", "vikunja/comment/2")
	rec, err := s.Complete(ctx, claimOne(t, s, "w").Lease, dec)
	if err != nil {
		t.Fatal(err)
	}
	if fd := rec.Families[0]; fd.Outcome != route.Act || len(fd.Actions) != 1 || fd.Actions[0].Kind != action.React {
		t.Fatalf("recorded %+v, want only the reaction", fd)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM actions WHERE after_key <> ''`); got != 0 {
		t.Fatalf("%d claim actions queued for a taken run", got)
	}
}

// TestCompleteQueuesRequeues: a decision's requeues are synthetic
// deliveries queued once.
func TestCompleteQueuesRequeues(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k1")
	dec := route.Decision{Outcome: route.Ignore, DeliveryKey: "k1",
		Requeue: []route.Requeue{{TaskID: 7, ProjectID: 3, Depth: 1}, {TaskID: 8, ProjectID: 3, Depth: 1}}}
	if _, err := s.Complete(ctx, claimOne(t, s, "w").Lease, dec); err != nil {
		t.Fatal(err)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM inbox WHERE headers->>'X-Gordon-Synthetic' = 'requeue' AND state = 'pending'`); got != 2 {
		t.Fatalf("%d requeues queued, want 2", got)
	}
	it := claimOne(t, s, "w")
	ev, err := event.Parse(it.Delivery())
	if err != nil || ev.Kind != event.VikunjaTaskUpdated || ev.Vikunja.Synthetic == nil ||
		ev.Vikunja.Synthetic.Requeue != "k1" || ev.Vikunja.Synthetic.Depth != 1 || it.Scope != "vikunja/project/3" {
		t.Fatalf("requeued delivery %+v (%v), scope %q", ev, err, it.Scope)
	}
	d, err := store.TaskUpdate(7, 3, event.Synthetic{Requeue: "k1", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Enqueue(ctx, []store.Delivery{d}); err != nil || n != 0 {
		t.Fatalf("the same requeue again stored %d (%v)", n, err)
	}
}

func TestLedgerTaskRuns(t *testing.T) {
	s, _ := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", submitOnly())
	led := s.Ledger(ctx)
	got := led.TaskRuns(5, []run.Family{run.VikunjaFollowUp})
	if led.Err != nil || len(got) != 1 || got[0].Name != runName || !got[0].Active || got[0].Family != run.VikunjaFollowUp {
		t.Fatalf("TaskRuns = %+v (%v)", got, led.Err)
	}
	if other := led.TaskRuns(5, run.AgentFamilies); len(other) != 0 {
		t.Fatalf("runs of other families %+v", other)
	}
	if none := led.TaskRuns(55, []run.Family{run.VikunjaFollowUp}); len(none) != 0 {
		t.Fatalf("task 55 has runs %+v (task id 5 is a prefix)", none)
	}
}

func TestRunInfoAndNewerRun(t *testing.T) {
	s, dbURL := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", submitOnly())
	info, err := s.RunInfo(ctx, runName)
	if err != nil || info.Family != run.VikunjaFollowUp || info.Identity.CommentID != 1 || info.State != "submitting" {
		t.Fatalf("RunInfo = %+v, %v", info, err)
	}
	if newer, err := s.NewerRun(ctx, runName, []run.Family{run.VikunjaFollowUp}); err != nil || newer {
		t.Fatalf("NewerRun alone = %v, %v", newer, err)
	}
	dec := submitOnly()
	fd := &dec.Families[0]
	fd.Runs[0].Name, fd.Runs[0].Identity.CommentID = "vikunja-fu-gordon-5-c2", 2
	fd.Actions = []action.Action{action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, "vikunja-fu-gordon-5-c2"), "")}
	dec.RequestKey = "vikunja/comment/2"
	recorded(t, s, "k2", "vikunja/comment/2", dec)
	storetest.Exec(t, dbURL, `UPDATE runs SET created_at = created_at + interval '1 second' WHERE name = 'vikunja-fu-gordon-5-c2'`)
	if newer, err := s.NewerRun(ctx, runName, []run.Family{run.VikunjaFollowUp}); err != nil || !newer {
		t.Fatalf("NewerRun with a later run = %v, %v", newer, err)
	}
}

func TestQueueRunActionsOnce(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", submitOnly())
	a := []action.Action{action.Notice(action.VikunjaTask(5), "failed", runName)}
	for range 2 {
		if err := s.QueueRunActions(ctx, id, run.VikunjaFailure, "gordon", a); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(t, dbURL, `SELECT count(*) FROM actions WHERE family = 'vikunja-failure'`); got != 1 {
		t.Fatalf("%d failure notices queued, want 1", got)
	}
}

// TestTargetLockSerialises: two holders of one target never overlap.
func TestTargetLockSerialises(t *testing.T) {
	s, _ := storetest.Open(t)
	var mu sync.Mutex
	inside, overlap := 0, false
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			err := s.WithTargetLock(ctx, "vikunja/task/5", func(context.Context) error {
				mu.Lock()
				inside++
				overlap = overlap || inside > 1
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if overlap {
		t.Fatal("two holders of one target lock ran at once")
	}
}

func TestKnownDeliveriesAndHandledRequests(t *testing.T) {
	s, _ := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", submitOnly())
	known, err := s.KnownDeliveries(ctx, []string{"k1", "k9"})
	if err != nil || !known["k1"] || known["k9"] {
		t.Fatalf("KnownDeliveries = %v, %v", known, err)
	}
	handled, err := s.HandledRequests(ctx, []string{"vikunja/comment/1", "vikunja/comment/9"})
	if err != nil || !handled["vikunja/comment/1"] || handled["vikunja/comment/9"] {
		t.Fatalf("HandledRequests = %v, %v", handled, err)
	}
}

// TestHeldTasks: a task is held while a run of the families is still
// submitting; not once its Task exists, not for another family.
func TestHeldTasks(t *testing.T) {
	s, _ := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", submitOnly())
	families := []run.Family{run.VikunjaFollowUp}
	got, err := s.HeldTasks(ctx, []int64{5, 55}, families)
	if err != nil || !maps.Equal(got, map[int64]bool{5: true}) {
		t.Fatalf("HeldTasks = %v (%v), want task 5", got, err)
	}
	if got, err := s.HeldTasks(ctx, []int64{5}, run.AgentFamilies); err != nil || len(got) != 0 {
		t.Fatalf("HeldTasks of other families = %v (%v), want none", got, err)
	}
	if err := s.RunSubmitted(ctx, runName, id, "uid", kelos.PhaseRunning); err != nil {
		t.Fatal(err)
	}
	if got, err := s.HeldTasks(ctx, []int64{5}, families); err != nil || len(got) != 0 {
		t.Fatalf("HeldTasks once created = %v (%v), want none", got, err)
	}
}

// TestClaimedActionCarriesItsLastError: a deferred action is claimed
// again with the reason of its deferral.
func TestClaimedActionCarriesItsLastError(t *testing.T) {
	s, _ := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", submitOnly())
	first, err := s.ClaimActions(ctx, "w", 1, time.Minute)
	if err != nil || len(first) != 1 || first[0].LastError != "" {
		t.Fatalf("first claim %+v (%v), want no last error", first, err)
	}
	if err := s.ActionDefer(ctx, first[0].Lease, false, 0, "variant x runs 1 of 1"); err != nil {
		t.Fatal(err)
	}
	again, err := s.ClaimActions(ctx, "w", 1, time.Minute)
	if err != nil || len(again) != 1 || again[0].LastError != "variant x runs 1 of 1" {
		t.Fatalf("second claim %+v (%v), want the deferral's reason", again, err)
	}
}
