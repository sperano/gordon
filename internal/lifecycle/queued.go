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

// QueueStore queues a run generation's lifecycle actions and counts the
// hold notices it queued.
type QueueStore interface {
	QueueRunActions(ctx context.Context, inboxID int64, family run.Family, project string, actions []action.Action) error
	HoldNotices(ctx context.Context, inboxID int64, name, kind string) (int, string, error)
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
// the hold and its place among the run's hold notices: a later deferral
// for the reason of the newest notice queues nothing, a changed reason
// (back to an earlier one included) a new notice.
func (h *Holds) Held(ctx context.Context, r store.SubmitRun, hold notice.Hold) error {
	if !fromTask(r.Family, r.TaskID) || h.Policy.QueuedLabel == "" {
		return nil
	}
	seq, err := h.noticeSeq(ctx, r, hold)
	if err != nil {
		return err
	}
	actions := route.HoldActions(h.Policy, r.TaskID, r.CommentID, hold, seq)
	return h.Store.QueueRunActions(ctx, r.InboxID, run.VikunjaQueued, r.Project, actions)
}

// noticeSeq is the hold notice's place among the run's: the newest one's
// when it is of the same hold, the next one otherwise.
func (h *Holds) noticeSeq(ctx context.Context, r store.SubmitRun, hold notice.Hold) (int, error) {
	n, last, err := h.Store.HoldNotices(ctx, r.InboxID, r.Name, notice.Queued)
	if err != nil || n == 0 {
		return 0, err
	}
	if prev, err := notice.ParseHold(last); err == nil && prev.Key() == hold.Key() {
		return n - 1, nil
	}
	return n, nil
}

// Unsubmitted takes the queued label off the task of a run whose
// submission ended without a Task (dead-lettered): the removal queued
// with the hold waited for that submission and was cancelled with it.
func (h *Holds) Unsubmitted(ctx context.Context, r store.ObservedRun) error {
	if !fromTask(r.Family, r.TaskID) {
		return nil
	}
	a, ok := route.ReleaseAction(h.Policy, r.TaskID, r.Name)
	if !ok {
		return nil
	}
	return h.Store.QueueRunActions(ctx, r.InboxID, run.VikunjaQueued, r.Project, []action.Action{a})
}
