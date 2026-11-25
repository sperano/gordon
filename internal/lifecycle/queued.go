package lifecycle

import (
	"context"
	"slices"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

// QueueStore queues a run generation's lifecycle actions.
type QueueStore interface {
	QueueRunActions(ctx context.Context, inboxID int64, family run.Family, project string, actions []action.Action) error
}

// Holds labels and explains the Vikunja task of a run Gordon holds back
// from Kelos (the submit executor's hold hook), and takes the label off
// once its submission ended without a Task (the observer's). Only runs
// that come from a Vikunja task, and only with a queued label configured.
type Holds struct {
	Store  QueueStore
	Policy profile.Policy
}

// fromTask reports a run of a Vikunja task.
func fromTask(family run.Family, task int64) bool {
	return task > 0 && slices.Contains(run.TaskFamilies, family)
}

// Held queues the hold's label, notice and the label's removal once the
// run's Task is created. Each is keyed by the run and, for the notice,
// the hold: a later deferral for the same reason queues nothing.
func (h *Holds) Held(ctx context.Context, r store.SubmitRun, hold notice.Hold) error {
	if !fromTask(r.Family, r.TaskID) {
		return nil
	}
	actions := route.HoldActions(h.Policy, r.TaskID, hold)
	if len(actions) == 0 {
		return nil
	}
	return h.Store.QueueRunActions(ctx, r.InboxID, run.VikunjaQueued, r.Project, actions)
}

// Unsubmitted takes the queued label off the task of a run whose
// submission ended without a Task (dead-lettered): the removal queued
// with the hold waited for that submission and was cancelled with it.
func (h *Holds) Unsubmitted(ctx context.Context, r store.ObservedRun) error {
	if !fromTask(r.Family, r.TaskID) {
		return nil
	}
	a, ok := route.ReleaseAction(h.Policy, r.TaskID)
	if !ok {
		return nil
	}
	return h.Store.QueueRunActions(ctx, r.InboxID, run.VikunjaQueued, r.Project, []action.Action{a})
}
