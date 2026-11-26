package lifecycle

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

var errRead = errors.New("read failed")

// scriptedRuns is runs whose reads follow a script: the run reads as
// infos[i] on its i-th RunInfo (the last one after that), and
// TaskHeldBesides answers besides[i] on its i-th call.
type scriptedRuns struct {
	infos   []func() (store.RunInfo, error)
	besides []func() (bool, error)
	ri, bi  int
}

func (r *scriptedRuns) RunInfo(context.Context, string) (store.RunInfo, error) {
	f := r.infos[min(r.ri, len(r.infos)-1)]
	r.ri++
	return f()
}

func (r *scriptedRuns) TaskHeldBesides(context.Context, int64, string, []run.Family) (bool, error) {
	f := r.besides[min(r.bi, len(r.besides)-1)]
	r.bi++
	return f()
}

func runIn(state string) func() (store.RunInfo, error) {
	return func() (store.RunInfo, error) {
		return store.RunInfo{Name: runName, Family: run.VikunjaImplement, State: state}, nil
	}
}

func readFails() (store.RunInfo, error) { return store.RunInfo{}, errRead }

func answer(held bool, err error) func() (bool, error) {
	return func() (bool, error) { return held, err }
}

// TestHeldLabelPostAddReadFailures: a failed read after the queued
// label's add is retried and leaves the label on.
func TestHeldLabelPostAddReadFailures(t *testing.T) {
	label := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[0]
	for name, rs := range map[string]*scriptedRuns{
		"run read": {infos: []func() (store.RunInfo, error){runIn(store.RunSubmitting), readFails},
			besides: []func() (bool, error){answer(false, nil)}},
		"other-run read": {infos: []func() (store.RunInfo, error){runIn(store.RunSubmitting), runIn("active")},
			besides: []func() (bool, error){answer(false, errRead)}},
	} {
		t.Run(name, func(t *testing.T) {
			ex, f := heldExecutor(t, "")
			ex.Runs = rs
			if err := ex.Execute(ctx, label); !errors.Is(err, errRead) {
				t.Fatalf("Execute: %v, want %v", err, errRead)
			}
			if got := labels(f.get(5)); !slices.Equal(got, []string{queuedLabel}) {
				t.Fatalf("labels %v, want %v", got, []string{queuedLabel})
			}
		})
	}
}

// TestUnheldRemovalOtherRunReadFails: a failed read of the task's other
// held runs leaves the queued label on and is retried.
func TestUnheldRemovalOtherRunReadFails(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	ex, f := heldExecutor(t, "")
	ex.Runs = &scriptedRuns{infos: []func() (store.RunInfo, error){runIn("active")},
		besides: []func() (bool, error){answer(false, errRead)}}
	f.tasks[5].Labels = []Label{{ID: queuedLabelID, Title: queuedLabel}}
	if err := ex.Execute(ctx, removal); !errors.Is(err, errRead) {
		t.Fatalf("Execute: %v, want %v", err, errRead)
	}
	if got := labels(f.get(5)); !slices.Equal(got, []string{queuedLabel}) {
		t.Fatalf("labels %v, want %v", got, []string{queuedLabel})
	}
}

// TestUnheldRemovalRestoresForARunHeldMeanwhile: another run of the task
// held between the check and the removal (its label added before the
// removal) gets the label back; a failed second check puts it back and
// is retried.
func TestUnheldRemovalRestoresForARunHeldMeanwhile(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	release, _ := route.ReleaseAction(queuedPolicy(), 5, runName)
	label := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[0]
	started := []func() (store.RunInfo, error){runIn(store.RunSubmitting), runIn("active")}
	for name, c := range map[string]struct {
		a       action.Action
		labeled bool
		second  func() (bool, error)
		wantErr error
		want    []string
	}{
		"removal, other held":  {removal, true, answer(true, nil), nil, []string{queuedLabel}},
		"release, other held":  {release, true, answer(true, nil), nil, []string{queuedLabel}},
		"removal, none held":   {removal, true, answer(false, nil), nil, nil},
		"removal, read fails":  {removal, true, answer(false, errRead), errRead, []string{queuedLabel}},
		"post-add, other held": {label, false, answer(true, nil), nil, []string{queuedLabel}},
		"post-add, read fails": {label, false, answer(false, errRead), errRead, []string{queuedLabel}},
	} {
		t.Run(name, func(t *testing.T) {
			ex, f := heldExecutor(t, "")
			ex.Runs = &scriptedRuns{infos: started, besides: []func() (bool, error){answer(false, nil), c.second}}
			if c.labeled {
				f.tasks[5].Labels = []Label{{ID: queuedLabelID, Title: queuedLabel}}
			}
			if err := ex.Execute(ctx, c.a); !errors.Is(err, c.wantErr) {
				t.Fatalf("Execute: %v, want %v", err, c.wantErr)
			}
			if got := labels(f.get(5)); !slices.Equal(got, c.want) {
				t.Fatalf("labels %v, want %v", got, c.want)
			}
		})
	}
}
