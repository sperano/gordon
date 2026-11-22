package costs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

var ctx = context.Background()

type recorded struct {
	runs []store.CostRun
	err  error
}

func (r *recorded) RecordCostRun(_ context.Context, c store.CostRun) error {
	r.runs = append(r.runs, c)
	return r.err
}

func owns(f run.Family) rollout.Table {
	return rollout.Table{Rules: []rollout.Rule{{Project: "gordon", Family: f, Owner: rollout.Gordon,
		Evidence: []rollout.Gate{rollout.ReplayParity, rollout.ShadowParity, rollout.RollbackTested, rollout.LegacyDisabled}}}}
}

var seen = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func finishedTask(phase, completed string) kelos.Task {
	t := kelos.Task{Metadata: kelos.Metadata{Name: runName, UID: "uid-1"}, Status: &kelos.Status{Phase: phase}}
	t.Status.CompletionTime = completed
	return t
}

func TestHookRecordsFinishedRuns(t *testing.T) {
	r := store.ObservedRun{Name: runName, InboxID: 9, Family: run.VikunjaImplement, Project: "gordon", TaskID: 5}
	rec := &recorded{}
	h := &Hook{Store: rec, Ownership: owns(run.VikunjaCost), Now: func() time.Time { return seen }}
	if err := h.Finished(ctx, r, finishedTask(kelos.PhaseFailed, "2026-11-22T11:30:00Z")); err != nil {
		t.Fatal(err)
	}
	want := store.CostRun{Name: runName, InboxID: 9, Family: run.VikunjaImplement, Project: "gordon", TaskID: 5,
		TaskUID: "uid-1", Phase: kelos.PhaseFailed, Finished: time.Date(2026, 11, 22, 11, 30, 0, 0, time.UTC)}
	if len(rec.runs) != 1 || rec.runs[0] != want {
		t.Fatalf("recorded %+v, want %+v", rec.runs, want)
	}
	// No completion time: the time the observer saw it finish.
	rec.runs = nil
	if err := h.Finished(ctx, r, finishedTask(kelos.PhaseSucceeded, "")); err != nil {
		t.Fatal(err)
	}
	if len(rec.runs) != 1 || !rec.runs[0].Finished.Equal(seen) || rec.runs[0].Phase != kelos.PhaseSucceeded {
		t.Fatalf("recorded %+v, want finished at %s", rec.runs, seen)
	}
}

func TestHookSkips(t *testing.T) {
	base := store.ObservedRun{Name: runName, InboxID: 9, Family: run.VikunjaFollowUp, Project: "gordon", TaskID: 5}
	review, untasked, other := base, base, base
	review.Family = run.PRReview
	untasked.TaskID = 0
	other.Project = "vigie"
	for name, c := range map[string]struct {
		r     store.ObservedRun
		owner rollout.Table
	}{
		"a review": {review, owns(run.VikunjaCost)}, "no Vikunja task": {untasked, owns(run.VikunjaCost)},
		"another project": {other, owns(run.VikunjaCost)}, "family not owned": {base, owns(run.VikunjaFailure)},
	} {
		rec := &recorded{}
		h := &Hook{Store: rec, Ownership: c.owner}
		if err := h.Finished(ctx, c.r, finishedTask(kelos.PhaseSucceeded, "")); err != nil || len(rec.runs) != 0 {
			t.Errorf("%s: %v, recorded %+v", name, err, rec.runs)
		}
	}
	rec := &recorded{err: errors.New("db down")}
	h := &Hook{Store: rec, Ownership: owns(run.VikunjaCost)}
	if err := h.Finished(ctx, base, finishedTask(kelos.PhaseSucceeded, "")); !errors.Is(err, rec.err) {
		t.Errorf("store error: %v", err)
	}
}
