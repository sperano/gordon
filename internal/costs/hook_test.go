package costs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/kelos"
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

var seen = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func finishedTask(phase, completed string) kelos.Task {
	t := kelos.Task{Metadata: kelos.Metadata{Name: runName, UID: "uid-1"}, Status: &kelos.Status{Phase: phase}}
	t.Status.CompletionTime = completed
	return t
}

func TestHookRecordsFinishedRuns(t *testing.T) {
	r := store.ObservedRun{Name: runName, InboxID: 9, Family: run.VikunjaImplement, Project: "gordon", TaskID: 5}
	rec := &recorded{}
	h := &Hook{Store: rec, Now: func() time.Time { return seen }}
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
	review, untasked := base, base
	review.Family = run.PRReview
	untasked.TaskID = 0
	for name, r := range map[string]store.ObservedRun{"a review": review, "no Vikunja task": untasked} {
		rec := &recorded{}
		h := &Hook{Store: rec}
		if err := h.Finished(ctx, r, finishedTask(kelos.PhaseSucceeded, "")); err != nil || len(rec.runs) != 0 {
			t.Errorf("%s: %v, recorded %+v", name, err, rec.runs)
		}
	}
	rec := &recorded{err: errors.New("db down")}
	h := &Hook{Store: rec}
	if err := h.Finished(ctx, base, finishedTask(kelos.PhaseSucceeded, "")); !errors.Is(err, rec.err) {
		t.Errorf("store error: %v", err)
	}
}
