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

// unlabel is the removal write: a removal naming the run it is for
// guards against another run of the task held meanwhile.
func (e *Executor) unlabel(ctx context.Context, a action.Action) error {
	if name := route.UnheldRun(a.Detail); name != "" {
		return e.unheldRemove(ctx, a, name)
	}
	return e.removeLabel(ctx, a)
}

// unheldRemove removes a run's queued label, then puts it back when
// another run of the task is held by then. The check before the removal
// reads the runs outside any lock (the target lock's transaction holds a
// pool connection), so another run can be held, and its label added,
// between that check and the removal; a run held after this second check
// adds its label after the removal, so it keeps one. A failed check puts
// the label back too and is retried: the retry finds the label and
// checks again before removing it.
func (e *Executor) unheldRemove(ctx context.Context, a action.Action, name string) error {
	removed, err := e.removeLabels(ctx, a)
	if err != nil || len(removed) == 0 {
		return err
	}
	id, _ := taskTarget(a)
	other, err := e.Runs.TaskHeldBesides(ctx, id, name, run.TaskFamilies)
	if err == nil && !other {
		return nil
	}
	return errors.Join(err, e.restoreLabels(ctx, id, removed))
}

func (e *Executor) restoreLabels(ctx context.Context, id int64, labels []int64) error {
	for _, l := range labels {
		if err := e.W.AddLabel(ctx, id, l); err != nil {
			return err
		}
	}
	return nil
}
