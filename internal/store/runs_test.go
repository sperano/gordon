package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

func builtTask(prompt string) *kelos.Task {
	return &kelos.Task{APIVersion: kelos.APIVersion, Kind: kelos.KindTask,
		Metadata: kelos.Metadata{Name: runName, Namespace: route.KelosNamespace, Labels: map[string]string{"a": "b"}},
		Spec:     kelos.Spec{Prompt: prompt, Branch: "kelos/vikunja-5", Worker: &kelos.Worker{Type: "claude-code"}}}
}

// withTask is submitOnly whose run carries a built Task and a variant.
func withTask(prompt string) route.Decision {
	dec := submitOnly()
	rn := &dec.Families[0].Runs[0]
	rn.Identity.Variant, rn.Task = "claude-opus", builtTask(prompt)
	return dec
}

// recorded completes a fresh delivery with dec and returns its inbox id.
func recorded(t *testing.T, s *store.Store, key, requestKey string, dec route.Decision) int64 {
	t.Helper()
	id := acceptRequest(t, s, key, requestKey)
	if _, err := s.Complete(ctx, claimOne(t, s, "w").Lease, dec, false); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRunToSubmitAndSubmitted(t *testing.T) {
	s, dbURL := storetest.Open(t)
	if _, err := s.RunToSubmit(ctx, runName); !errors.Is(err, store.ErrNoRun) {
		t.Fatalf("RunToSubmit of no run = %v, want ErrNoRun", err)
	}
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("do it"))
	r, err := s.RunToSubmit(ctx, runName)
	if err != nil || r.InboxID != id || r.Variant != "claude-opus" || r.State != "submitting" || r.Submitted() ||
		r.Task == nil || r.Task.Spec.Prompt != "do it" || r.Task.Metadata.Labels["a"] != "b" {
		t.Fatalf("RunToSubmit = %+v, %v", r, err)
	}
	if err := s.RunSubmitted(ctx, runName, id+1, "uid-other", kelos.PhaseRunning); err != nil {
		t.Fatal(err)
	}
	if r, err = s.RunToSubmit(ctx, runName); err != nil || r.Submitted() {
		t.Fatal("another generation's RunSubmitted changed this run")
	}
	if err := s.RunSubmitted(ctx, runName, id, "uid-1", kelos.PhasePending); err != nil {
		t.Fatal(err)
	}
	var state, uid, phase string
	var exists bool
	storetest.QueryRow(t, dbURL, `SELECT state, task_uid, phase, task_exists FROM runs WHERE name = $1`, []any{runName},
		&state, &uid, &phase, &exists)
	if state != "active" || uid != "uid-1" || phase != kelos.PhasePending || !exists {
		t.Fatalf("submitted run state %s uid %s phase %s exists %v", state, uid, phase, exists)
	}
	if r, err = s.RunToSubmit(ctx, runName); err != nil || !r.Submitted() {
		t.Fatal("a created run still reads as unsubmitted")
	}
}

func TestRunSubmittedFinishedTask(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	if err := s.RunSubmitted(ctx, runName, id, "u", kelos.PhaseSucceeded); err != nil {
		t.Fatal(err)
	}
	var state string
	storetest.QueryRow(t, dbURL, `SELECT state FROM runs WHERE name = $1`, []any{runName}, &state)
	if state != "finished" {
		t.Fatalf("run found already succeeded is %s, want finished", state)
	}
}

func TestObservedRunsAndObserveRun(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	runs, err := s.ObservedRuns(ctx, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ObservedRuns = %+v, %v", runs, err)
	}
	if r := runs[0]; r.Name != runName || r.InboxID != id || r.State != "submitting" || r.Exists || r.SubmitState != store.ActionPending {
		t.Fatalf("observed %+v, want the submitting run with its pending submission", r)
	}
	if err := s.ObserveRun(ctx, runName, id, true, kelos.PhaseSucceeded); err != nil {
		t.Fatal(err)
	}
	if runs, err = s.ObservedRuns(ctx, 10); err != nil || len(runs) != 1 || runs[0].State != "finished" || !runs[0].Exists ||
		runs[0].Phase != kelos.PhaseSucceeded {
		t.Fatalf("a finished run whose Task exists is still followed: %+v", runs)
	}
	if err := s.ObserveRun(ctx, runName, id+1, false, ""); err != nil {
		t.Fatal(err)
	}
	if runs, err = s.ObservedRuns(ctx, 10); err != nil || len(runs) != 1 {
		t.Fatal("another generation's observation changed this run")
	}
	if err := s.ObserveRun(ctx, runName, id, false, ""); err != nil {
		t.Fatal(err)
	}
	if runs, err = s.ObservedRuns(ctx, 10); err != nil || len(runs) != 0 {
		t.Fatalf("a finished run whose Task is gone is still followed: %+v", runs)
	}
	if rec, ok := s.Ledger(ctx).Run(runName); !ok || rec.Active || rec.Exists {
		t.Fatalf("ledger run %+v, want finished and gone: a new request may run again", rec)
	}
	var phase string
	storetest.QueryRow(t, dbURL, `SELECT phase FROM runs WHERE name = $1`, []any{runName}, &phase)
	if phase != kelos.PhaseSucceeded {
		t.Fatalf("phase after the Task is gone = %q, want the last observed %q", phase, kelos.PhaseSucceeded)
	}
}

func TestRerunReplacesTaskAndClearsObservation(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("first"))
	if err := s.RunSubmitted(ctx, runName, id, "uid-1", kelos.PhaseRunning); err != nil {
		t.Fatal(err)
	}
	if got := observedNames(t, s, 1); !slices.Equal(got, []string{runName}) {
		t.Fatalf("observed %v, want the run", got)
	}
	if err := s.ObserveRun(ctx, runName, id, false, ""); err != nil {
		t.Fatal(err)
	}
	id2 := recorded(t, s, "k2", "vikunja/comment/2", withTask("second"))
	r, err := s.RunToSubmit(ctx, runName)
	if err != nil || r.InboxID != id2 || r.State != "submitting" || r.Task.Spec.Prompt != "second" {
		t.Fatalf("rerun %+v, %v; want generation 2 with its own Task", r, err)
	}
	var uid, phase string
	storetest.QueryRow(t, dbURL, `SELECT task_uid, phase FROM runs WHERE name = $1`, []any{runName}, &uid, &phase)
	if uid != "" || phase != "" {
		t.Fatalf("rerun kept the old Task's uid %q phase %q", uid, phase)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM runs WHERE name = $1 AND observed_at = '-infinity'`, runName); n != 1 {
		t.Fatal("rerun kept the old generation's observed_at: it would wait behind runs never observed")
	}
}

// TestRunSubmittedAfterTheObserver: a run the observer already followed
// keeps the state it saw, and still gets its Task's uid.
func TestRunSubmittedAfterTheObserver(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	row := func() (state, uid, phase string, exists bool) {
		storetest.QueryRow(t, dbURL, `SELECT state, task_uid, phase, task_exists FROM runs WHERE name = $1`,
			[]any{runName}, &state, &uid, &phase, &exists)
		return
	}
	if err := s.ObserveRun(ctx, runName, id, true, kelos.PhaseRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.RunSubmitted(ctx, runName, id, "uid-1", ""); err != nil {
		t.Fatal(err)
	}
	if st, uid, phase, exists := row(); st != "active" || uid != "uid-1" || phase != kelos.PhaseRunning || !exists {
		t.Fatalf("active run: %s %q %q %v, want active uid-1 Running present", st, uid, phase, exists)
	}
	if err := s.ObserveRun(ctx, runName, id, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RunSubmitted(ctx, runName, id, "uid-2", kelos.PhaseRunning); err != nil {
		t.Fatal(err)
	}
	if st, uid, phase, exists := row(); st != "finished" || uid != "uid-2" || phase != "" || exists {
		t.Fatalf("finished run: %s %q %q %v, want finished uid-2, no phase, gone", st, uid, phase, exists)
	}
	if err := s.RunSubmitted(ctx, runName, id+1, "uid-3", ""); err != nil {
		t.Fatal(err)
	}
	if _, uid, _, _ := row(); uid != "uid-2" {
		t.Fatalf("another generation's submission set uid %q", uid)
	}
}

func TestPruneTasks(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	if n, err := s.PruneTasks(ctx, 0); err != nil || n != 0 {
		t.Fatalf("PruneTasks of an unfinished run = %d, %v; want 0", n, err)
	}
	if err := s.ObserveRun(ctx, runName, id, false, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneTasks(ctx, longDelay); err != nil || n != 0 {
		t.Fatalf("PruneTasks within retention = %d, %v; want 0", n, err)
	}
	storetest.Exec(t, dbURL, `UPDATE runs SET updated_at = now() - interval '2 hours'`)
	if n, err := s.PruneTasks(ctx, longDelay); err != nil || n != 1 {
		t.Fatalf("PruneTasks = %d, %v; want the finished run's Task", n, err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM runs WHERE task IS NULL AND name = $1`, runName); n != 1 {
		t.Fatal("the pruned run lost its row or kept its Task")
	}
}

// copyRun adds a run named name like runName's, in state state.
func copyRun(t *testing.T, dbURL, name, state string) {
	t.Helper()
	storetest.Exec(t, dbURL, `INSERT INTO runs (name, family, inbox_id, identity, profile, state)
		SELECT $2, family, inbox_id, identity, profile, $3 FROM runs WHERE name = $1`, runName, name, state)
}

func observedNames(t *testing.T, s *store.Store, limit int) []string {
	t.Helper()
	runs, err := s.ObservedRuns(ctx, limit)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(runs))
	for _, r := range runs {
		names = append(names, r.Name)
	}
	return names
}

func TestObservedRunsRotateUnchangedRuns(t *testing.T) {
	s, dbURL := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	copyRun(t, dbURL, "run-a", "active")
	copyRun(t, dbURL, "run-b", "active")
	// Nothing observed changes, as for runs whose Task stays Running.
	if got := observedNames(t, s, 2); !slices.Equal(got, []string{"run-a", "run-b"}) {
		t.Fatalf("first batch %v, want the first two by name", got)
	}
	if got := observedNames(t, s, 2); !slices.Equal(got, []string{"run-a", runName}) {
		t.Fatalf("second batch %v, want the run not yet observed, then the oldest observed", got)
	}
}

// lockWaitGuard ends a read that waits on a row lock, so a missing
// SKIP LOCKED fails the test instead of hanging it.
const lockWaitGuard = 5 * time.Second

// TestObservedRunsSkipsRunsAnotherObserverHolds: a run locked by a
// concurrent observer's batch is left out of this batch, not waited on.
func TestObservedRunsSkipsRunsAnotherObserverHolds(t *testing.T) {
	s, dbURL := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	copyRun(t, dbURL, "run-a", "active")
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT name FROM runs WHERE name = 'run-a' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	guarded, cancel := context.WithTimeout(ctx, lockWaitGuard)
	defer cancel()
	runs, err := s.ObservedRuns(guarded, 10)
	if err != nil || len(runs) != 1 || runs[0].Name != runName {
		t.Fatalf("ObservedRuns = %+v, %v; want only the run nobody holds", runs, err)
	}
}

func TestObserveRunNeverReopensAFinishedRun(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	if err := s.ObserveRun(ctx, runName, id, true, kelos.PhaseFailed); err != nil {
		t.Fatal(err)
	}
	state := func() (st, phase string, exists bool) {
		storetest.QueryRow(t, dbURL, `SELECT state, phase, task_exists FROM runs WHERE name = $1`,
			[]any{runName}, &st, &phase, &exists)
		return
	}
	if st, phase, exists := state(); st != "finished" || phase != kelos.PhaseFailed || !exists {
		t.Fatalf("failed Task: %s %s %v, want finished Failed present", st, phase, exists)
	}
	// A replica that read the list before the Task finished.
	if err := s.ObserveRun(ctx, runName, id, true, kelos.PhaseRunning); err != nil {
		t.Fatal(err)
	}
	if st, phase, _ := state(); st != "finished" || phase != kelos.PhaseFailed {
		t.Fatalf("stale Running observation reopened the run: %s %s", st, phase)
	}
	if err := s.ObserveRun(ctx, runName, id, false, ""); err != nil {
		t.Fatal(err)
	}
	// A replica that read the list before Kelos cleaned the Task up.
	if err := s.ObserveRun(ctx, runName, id, true, kelos.PhaseFailed); err != nil {
		t.Fatal(err)
	}
	if st, _, exists := state(); st != "finished" || exists {
		t.Fatalf("stale observation brought a gone Task back: %s exists %v", st, exists)
	}
}
