package lifecycle

import (
	"context"
	"errors"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// unlessStillHeld takes a held run's queued label off again when the run
// started or stopped while it was added: its removal, which waits for the
// submission only, may have run before the label landed. A run still
// held after the add leaves that removal to come, and another held run
// of the task keeps the label for its own. A failed read is
// retried, and the retry finds the label there: the reconciliation's
// cleanup takes it off then.
func (e *Executor) unlessStillHeld(ctx context.Context, a action.Action) error {
	name := route.HeldRun(a.Detail)
	if name == "" {
		return nil
	}
	held, err := e.held(ctx, name)
	if err != nil || held {
		return err
	}
	id, _ := taskTarget(a)
	if other, err := e.Runs.TaskHeldBesides(ctx, id, name, run.TaskFamilies); err != nil || other {
		return err
	}
	return e.unheldRemove(ctx, a, name)
}

// heldLabelApplied reports a held run's queued label, absent from t,
// as not wanted: its run started or stopped and no other run of the task
// is held, or the task is done. A label taken off by a removal whose
// restore failed is wanted again while another run is held: the retry
// adds it, and the check after the add keeps it.
func (e *Executor) heldLabelApplied(ctx context.Context, t Task, name string) (bool, error) {
	held, err := e.held(ctx, name)
	if err != nil {
		return false, err
	}
	if held || t.Done {
		return t.Done, nil
	}
	other, err := e.Runs.TaskHeldBesides(ctx, t.ID, name, run.TaskFamilies)
	return !other, err
}

// unheldApplied reports a per-run removal applied when the label matches
// the task's other held runs: on while one is held (its own removal takes
// it off), off while none is. A label off while one is held is one a
// failed restore left off: the write puts it back.
func (e *Executor) unheldApplied(ctx context.Context, t Task, name string, labeled bool) (bool, error) {
	other, err := e.Runs.TaskHeldBesides(ctx, t.ID, name, run.TaskFamilies)
	return err == nil && labeled == other, err
}

// unlabel is the removal write: a removal naming the run it is for
// guards against another run of the task held meanwhile.
func (e *Executor) unlabel(ctx context.Context, a action.Action) error {
	if name := route.UnheldRun(a.Detail); name != "" {
		return e.unheldRemove(ctx, a, name)
	}
	return e.removeLabel(ctx, a)
}

// unheldRemove removes a run's queued label, then puts it back when
// another run of the task is held by then, or when that check fails
// after a label was removed. The check before the removal
// reads the runs outside any lock (the target lock's transaction holds a
// pool connection), so another run can be held, and its label added,
// between that check and the removal; a run held after this second check
// adds its label after the removal, so it keeps one. A failed check or
// restore is retried: the retry's check finds the label, or finds it
// missing while another run is held, and this write puts it back.
func (e *Executor) unheldRemove(ctx context.Context, a action.Action, name string) error {
	removed, err := e.removeLabels(ctx, a)
	if err != nil {
		return err
	}
	id, _ := taskTarget(a)
	other, err := e.Runs.TaskHeldBesides(ctx, id, name, run.TaskFamilies)
	switch {
	case err == nil && !other:
		return nil
	case err != nil && len(removed) == 0:
		return err
	}
	return errors.Join(err, e.restoreLabels(ctx, a, removed))
}

// restoreLabels puts the removed labels back, or the label titled as the
// action's when none was removed while another run is held (a retry
// after a failed restore, or a label already off).
func (e *Executor) restoreLabels(ctx context.Context, a action.Action, removed []int64) error {
	if len(removed) == 0 {
		l, err := e.labelID(ctx, a.Value)
		if err != nil {
			return err
		}
		removed = []int64{l}
	}
	id, _ := taskTarget(a)
	for _, l := range removed {
		if err := e.W.AddLabel(ctx, id, l); err != nil {
			return err
		}
	}
	return nil
}
