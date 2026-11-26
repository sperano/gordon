package lifecycle

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
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

// TestUnheldRemovalCheckReadFails: a failed read of the task's other
// held runs before the removal writes nothing and is retried.
func TestUnheldRemovalCheckReadFails(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	ex, f := heldExecutor(t, "")
	ex.Runs = &scriptedRuns{infos: []func() (store.RunInfo, error){runIn("active")},
		besides: []func() (bool, error){answer(false, errRead)}}
	f.tasks[5].Labels = []Label{{ID: queuedLabelID, Title: queuedLabel}}
	if err := ex.Execute(ctx, removal); !errors.Is(err, errRead) {
		t.Fatalf("Execute: %v, want %v", err, errRead)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes %v, want none", f.writes)
	}
	if got := labels(f.get(5)); !slices.Equal(got, []string{queuedLabel}) {
		t.Fatalf("labels %v, want %v", got, []string{queuedLabel})
	}
}

// TestUnheldRemovalRetriesAFailedRestore: a restore refused after the
// removal, with another run of the task held, leaves the label off and
// returns the refusal; the retry puts the label back.
func TestUnheldRemovalRetriesAFailedRestore(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	label := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[0]
	for name, c := range map[string]struct {
		a       action.Action
		labeled bool
		refuse  []bool
	}{
		"removal":  {removal, true, []bool{true}},
		"post-add": {label, false, []bool{false, true}},
	} {
		t.Run(name, func(t *testing.T) {
			ex, f := heldExecutor(t, "")
			ex.Runs = &scriptedRuns{infos: []func() (store.RunInfo, error){runIn(store.RunSubmitting), runIn("active")},
				besides: []func() (bool, error){answer(false, nil), answer(true, nil)}}
			if c.labeled {
				f.tasks[5].Labels = []Label{{ID: queuedLabelID, Title: queuedLabel}}
			}
			f.refuseLabelAdds = c.refuse
			var status *ErrStatus
			if err := ex.Execute(ctx, c.a); !errors.As(err, &status) || status.Status != http.StatusServiceUnavailable {
				t.Fatalf("Execute: %v, want HTTP %d", err, http.StatusServiceUnavailable)
			}
			if got := labels(f.get(5)); len(got) != 0 {
				t.Fatalf("labels after the refusal %v, want none", got)
			}
			if err := ex.Execute(ctx, c.a); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got := labels(f.get(5)); !slices.Equal(got, []string{queuedLabel}) {
				t.Fatalf("labels after the retry %v, want %v", got, []string{queuedLabel})
			}
		})
	}
}

// TestUnheldLabelGoneNoneHeldWritesNothing: a retried removal or add
// finding the label off, with no run of the task held, is applied.
func TestUnheldLabelGoneNoneHeldWritesNothing(t *testing.T) {
	for name, a := range map[string]action.Action{
		"removal":  route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2],
		"post-add": route.HoldActions(queuedPolicy(), 5, limitHold, 0)[0],
	} {
		t.Run(name, func(t *testing.T) {
			ex, f := heldExecutor(t, "")
			ex.Runs = &scriptedRuns{infos: []func() (store.RunInfo, error){runIn("active")},
				besides: []func() (bool, error){answer(false, nil)}}
			if err := ex.Execute(ctx, a); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(f.writes) != 0 {
				t.Fatalf("writes %v, want none", f.writes)
			}
		})
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

// TestHeldLabelAppliedCheckReadFails: a failed read in the held label's
// applied check writes nothing and is retried.
func TestHeldLabelAppliedCheckReadFails(t *testing.T) {
	label := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[0]
	for name, rs := range map[string]*scriptedRuns{
		"run read": {infos: []func() (store.RunInfo, error){readFails},
			besides: []func() (bool, error){answer(false, nil)}},
		"other-run read": {infos: []func() (store.RunInfo, error){runIn("active")},
			besides: []func() (bool, error){answer(false, errRead)}},
	} {
		t.Run(name, func(t *testing.T) {
			ex, f := heldExecutor(t, "")
			ex.Runs = rs
			if err := ex.Execute(ctx, label); !errors.Is(err, errRead) {
				t.Fatalf("Execute: %v, want %v", err, errRead)
			}
			if len(f.writes) != 0 {
				t.Fatalf("writes %v, want none", f.writes)
			}
			if got := labels(f.get(5)); len(got) != 0 {
				t.Fatalf("labels %v, want none", got)
			}
		})
	}
}

// TestUnheldRemovalNothingRemovedCheckFails: a removal finding the label
// already off, whose check after the removal fails, returns the failure
// without adding the label.
func TestUnheldRemovalNothingRemovedCheckFails(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	ex, f := heldExecutor(t, "")
	ex.Runs = &scriptedRuns{infos: []func() (store.RunInfo, error){runIn("active")},
		besides: []func() (bool, error){answer(true, nil), answer(false, errRead)}}
	if err := ex.Execute(ctx, removal); !errors.Is(err, errRead) {
		t.Fatalf("Execute: %v, want %v", err, errRead)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes %v, want none", f.writes)
	}
	if got := labels(f.get(5)); len(got) != 0 {
		t.Fatalf("labels %v, want none", got)
	}
}

// TestUnheldRestoreLabelMissing: a removal restoring a label already off,
// while another run is held, fails permanently when no label of that
// title is visible.
func TestUnheldRestoreLabelMissing(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	ex, f := heldExecutor(t, "")
	f.labels = slices.DeleteFunc(f.labels, func(l Label) bool { return l.Title == queuedLabel })
	ex.Runs = &scriptedRuns{infos: []func() (store.RunInfo, error){runIn("active")},
		besides: []func() (bool, error){answer(true, nil)}}
	if err := ex.Execute(ctx, removal); !worker.IsPermanent(err) {
		t.Fatalf("Execute: %v, want a permanent error", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes %v, want none", f.writes)
	}
}

// TestUnheldRestoreLookupFails: a removal restoring a label already off,
// while another run is held, returns the label lookup's refusal and
// writes nothing; the retry puts the label back.
func TestUnheldRestoreLookupFails(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	ex, f := heldExecutor(t, "")
	ex.Runs = &scriptedRuns{infos: []func() (store.RunInfo, error){runIn("active")},
		besides: []func() (bool, error){answer(true, nil), answer(true, nil)}}
	f.refuseLabelLists = 1
	var status *ErrStatus
	if err := ex.Execute(ctx, removal); !errors.As(err, &status) || status.Status != http.StatusServiceUnavailable ||
		worker.IsPermanent(err) {
		t.Fatalf("Execute: %v, want a transient HTTP %d", err, http.StatusServiceUnavailable)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes %v, want none", f.writes)
	}
	if err := ex.Execute(ctx, removal); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := labels(f.get(5)); !slices.Equal(got, []string{queuedLabel}) {
		t.Fatalf("labels after the retry %v, want %v", got, []string{queuedLabel})
	}
}
