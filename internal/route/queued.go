package route

import (
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
// once a drag-cancel stopped it, and the cleanup of a task with no run
// held.
const (
	queuedCreated  = ":created"
	queuedReleased = ":released"
	queuedStopped  = ":stopped"
	queuedCleared  = ":cleared"
)

// HeldRun is the run a DetailHeld detail names ("" for another detail).
func HeldRun(detail string) string {
	name, _ := strings.CutPrefix(detail, DetailHeld)
	if name == detail {
		return ""
	}
	return name
}

// HoldActions are the writes of a run held back from Kelos on its Vikunja
// task, queued on the run's generation: the queued label, the notice of
// the hold (once per hold: a new one only when the reason changes) and
// the label's removal once the run's Task is created. A submission
// cancelled or dead-lettered cancels that removal; the drag-cancel and
// Release take the label off then. None without a queued label.
func HoldActions(p profile.Policy, task int64, h notice.Hold) []action.Action {
	if p.QueuedLabel == "" {
		return nil
	}
	target := action.VikunjaTask(task)
	label := action.New(action.AddLabel, target, p.QueuedLabel)
	label.Key += ":" + h.Run
	label.Detail = DetailHeld + h.Run
	note := action.Notice(target, notice.Queued, h.Key())
	note.Detail = h.Detail()
	submit := action.New(action.SubmitTask, action.KelosTask(KelosNamespace, h.Run), "")
	return []action.Action{label, note, queuedRemoval(p, task, queuedCreated).WaitFor(submit)}
}

// ReleaseAction takes the queued label off a task whose held run's
// submission ended without a Task (it was dead-lettered).
func ReleaseAction(p profile.Policy, task int64) (action.Action, bool) {
	if p.QueuedLabel == "" {
		return action.Action{}, false
	}
	return queuedRemoval(p, task, queuedReleased), true
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
