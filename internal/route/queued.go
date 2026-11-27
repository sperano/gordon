package route

import (
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// ReasonRunHeld is a queued task's cleanup with a run still held.
const ReasonRunHeld = "run-held"

// DetailHeld prefixes a held run's queued label (DetailHeld + its Task
// name): the label is not added once the run is no longer held, nor to a
// done task.
const DetailHeld = "held:"

// Key suffixes of the queued label's removals: once the held run's Task
// is created, once its submission ended without one (dead-lettered),
// both followed by the run's name, once a drag-cancel stopped the task's
// runs, and the cleanup of a task with no run held.
const (
	queuedCreated  = ":created"
	queuedReleased = ":released"
	queuedStopped  = ":stopped"
	queuedCleared  = ":cleared"
)

// DetailUnheld prefixes the queued label's removal once a held run's
// Task is created or its submission ended (DetailUnheld + its Task
// name): the label stays while another run of the task is still held.
const DetailUnheld = "unheld:"

// holdSeqSep separates a hold notice's key from its place among the
// run's hold notices: a hold key's fields never contain it, and a hold
// key has a fixed number of fields, so no seq makes it another's.
const holdSeqSep = ":"

// HeldRun is the run a DetailHeld detail names ("" for another detail).
func HeldRun(detail string) string { return detailRun(detail, DetailHeld) }

// UnheldRun is the run a DetailUnheld detail names ("" for another
// detail).
func UnheldRun(detail string) string { return detailRun(detail, DetailUnheld) }

func detailRun(detail, prefix string) string {
	name, ok := strings.CutPrefix(detail, prefix)
	if !ok {
		return ""
	}
	return name
}

// reactionHourglass is the ⏳ a held follow-up's request gets.
const reactionHourglass = "hourglass"

// HoldActions are the writes of a run held back from Kelos on its Vikunja
// task, queued on the run's generation: the queued label, the notice of
// the hold, the label's removal once the run's Task is created and, for a
// follow-up (comment, the owner's request; 0 for another run), the ⏳ on
// that request, keyed by the comment so that every hold adds it once. seq
// is the notice's place among the run's hold notices (0 for the first):
// a deferral for the reason of the newest one passes that one's seq, so
// it queues no other; a changed reason, even back to an earlier one,
// passes the next and posts a new notice. A submission cancelled or
// dead-lettered cancels that removal; the drag-cancel and Release take
// the label off then. None without a queued label.
func HoldActions(p profile.Policy, task, comment int64, h notice.Hold, seq int) []action.Action {
	if p.QueuedLabel == "" {
		return nil
	}
	target := action.VikunjaTask(task)
	label := action.New(action.AddLabel, target, p.QueuedLabel)
	label.Key += ":" + h.Run
	label.Detail = DetailHeld + h.Run
	note := action.Notice(target, notice.Queued, h.Key())
	note.Detail = h.Detail()
	if seq > 0 {
		note.Key += holdSeqSep + strconv.Itoa(seq)
	}
	submit := action.New(action.SubmitTask, action.KelosTask(KelosNamespace, h.Run), "")
	actions := []action.Action{label, note, unheld(p, task, h.Run, queuedCreated).WaitFor(submit)}
	if comment > 0 {
		actions = append(actions, action.New(action.React, action.VikunjaComment(comment), reactionHourglass))
	}
	return actions
}

// ReleaseAction takes the queued label off a task whose held run (name)
// ended its submission without a Task (it was dead-lettered).
func ReleaseAction(p profile.Policy, task int64, name string) (action.Action, bool) {
	if p.QueuedLabel == "" {
		return action.Action{}, false
	}
	return unheld(p, task, name, queuedReleased), true
}

// unheld is the queued label's removal once the held run name is no
// longer held, keyed by the run: another held run of the task keeps it.
func unheld(p profile.Policy, task int64, name, suffix string) action.Action {
	a := queuedRemoval(p, task, suffix+":"+name)
	a.Detail = DetailUnheld + name
	return a
}

func queuedRemoval(p profile.Policy, task int64, suffix string) action.Action {
	a := action.New(action.RemoveLabel, action.VikunjaTask(task), p.QueuedLabel)
	a.Key += suffix
	return a
}

// stopQueued takes the queued label off a dragged task once its runs are
// cancelled; nothing when the task does not carry it (a held run's label
// not added yet is not added once its run stopped: DetailHeld).
func (r Router) stopQueued(fd *FamilyDecision, t event.Task, cancel action.Action) {
	p := r.Config.Policy
	if p.QueuedLabel != "" && t.HasLabel(p.QueuedLabel) {
		fd.Actions = append(fd.Actions, queuedRemoval(p, t.ID, queuedStopped).WaitFor(cancel))
	}
}

// held reports a run of the task still waiting for its Task: active, its
// Task not created yet.
func held(t event.Task, led Ledger) bool {
	for _, rec := range led.TaskRuns(t.ID, run.TaskFamilies) {
		if rec.Active && !rec.Exists {
			return true
		}
	}
	return false
}

// queuedCleanup takes a stale queued label off a task none of whose runs
// is held: a hold whose removal was lost (a submission dead-lettered
// before its release was queued, a label written after the run started).
// The reconciliation's deliveries bring such a task back once no run of
// it is held.
func (r Router) queuedCleanup(proj profile.Project, t event.Task, led Ledger) (FamilyDecision, bool) {
	p := r.Config.Policy
	if p.QueuedLabel == "" || !t.HasLabel(p.QueuedLabel) {
		return FamilyDecision{}, false
	}
	fd := r.family(run.VikunjaQueued, proj.Name)
	if held(t, led) {
		fd.Outcome, fd.Reason = Ignore, ReasonRunHeld
		return fd, true
	}
	fd.Outcome = Act
	fd.Actions = append(fd.Actions, queuedRemoval(p, t.ID, queuedCleared))
	return fd, true
}
