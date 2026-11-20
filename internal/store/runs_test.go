package store_test

import (
	"errors"
	"testing"

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
	if r, _ = s.RunToSubmit(ctx, runName); r.Submitted() {
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
	if r, _ = s.RunToSubmit(ctx, runName); !r.Submitted() {
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
	s, _ := storetest.Open(t)
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
	if runs, _ = s.ObservedRuns(ctx, 10); len(runs) != 1 || runs[0].State != "finished" || !runs[0].Exists ||
		runs[0].Phase != kelos.PhaseSucceeded {
		t.Fatalf("a finished run whose Task exists is still followed: %+v", runs)
	}
	if err := s.ObserveRun(ctx, runName, id+1, false, ""); err != nil {
		t.Fatal(err)
	}
	if runs, _ = s.ObservedRuns(ctx, 10); len(runs) != 1 {
		t.Fatal("another generation's observation changed this run")
	}
	if err := s.ObserveRun(ctx, runName, id, false, ""); err != nil {
		t.Fatal(err)
	}
	if runs, _ = s.ObservedRuns(ctx, 10); len(runs) != 0 {
		t.Fatalf("a finished run whose Task is gone is still followed: %+v", runs)
	}
	if rec, ok := s.Ledger(ctx).Run(runName); !ok || rec.Active || rec.Exists {
		t.Fatalf("ledger run %+v, want finished and gone: a new request may run again", rec)
	}
}

func TestRerunReplacesTaskAndClearsObservation(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("first"))
	if err := s.RunSubmitted(ctx, runName, id, "uid-1", kelos.PhaseRunning); err != nil {
		t.Fatal(err)
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
