package route

import (
	"slices"
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// Lifecycle reasons.
const (
	// ReasonBlocked holds a run (or a chain) while a task it waits for is
	// open: Gordon waits before submitting, where legacy started a pod
	// that waited in its wait-for-blockers init container.
	ReasonBlocked       = "blocked"
	ReasonPROpen        = "pr-open"
	ReasonNoLiveAgent   = "no-live-agent"
	ReasonChainPaused   = "chain-paused"
	ReasonChainHeld     = "chain-held"
	ReasonChainWaiting  = "subtask-in-flight"
	ReasonChainNoTasks  = "no-subtasks"
	ReasonChainComplete = "chain-complete"
)

// Action details the Vikunja executor reads.
const (
	// DetailClaim marks a claim's in-progress label and Doing move: not
	// written when the card went back to the backlog or the task is done
	// meanwhile.
	DetailClaim = "claim"
	// DetailIfDone removes a label only while the task is still done.
	DetailIfDone = "if-done"
	// DetailUnlessDone adds a label only while the task is not done.
	DetailUnlessDone = "unless-done"
)

// MaxRequeueDepth bounds requeues: a webhook delivery (depth 0) requeues
// its task's parents and, once done, the tasks it blocks; their decisions
// (depth 1) requeue their own parents; depth 2 requeues nothing, so a
// relation cycle cannot loop.
const MaxRequeueDepth = 2

// claim is the start of an implementation or discuss run, once its Task
// is submitted (wait_for_blockers.py after the wait): the in-progress
// label, the start-cleared labels, the Doing bucket in every manual view
// and the start notice. Each waits for the submission, so a run that is
// never created claims nothing.
func (r Router) claim(fd *FamilyDecision, t event.Task, rn Run, submit action.Action) {
	p, target := r.Config.Policy, action.VikunjaTask(t.ID)
	label := action.New(action.AddLabel, target, p.InProgressLabel)
	label.Detail = DetailClaim
	fd.Actions = append(fd.Actions, label.WaitFor(submit))
	r.clearStartLabels(fd, t, submit)
	move := action.New(action.MoveBucket, target, p.DoingBucket)
	move.Detail = DetailClaim
	fd.Actions = append(fd.Actions, move.WaitFor(submit),
		action.Notice(target, notice.Start, rn.Name).WaitFor(submit))
}

// announce is a follow-up's start (announce_start.py): the start-cleared
// labels and the start notice, after the submission.
func (r Router) announce(fd *FamilyDecision, t event.Task, rn Run, submit action.Action) {
	r.clearStartLabels(fd, t, submit)
	fd.Actions = append(fd.Actions, action.Notice(action.VikunjaTask(t.ID), notice.Start, rn.Name).WaitFor(submit))
}

// clearStartLabels removes the failure reporter's and the owner-inbox
// labels a new attempt makes stale (START_CLEARED_LABELS).
func (r Router) clearStartLabels(fd *FamilyDecision, t event.Task, submit action.Action) {
	p := r.Config.Policy
	for _, l := range []string{p.AgentErrorLabel, p.ActionNeededLabel} {
		if l != "" && t.HasLabel(l) {
			fd.Actions = append(fd.Actions, action.New(action.RemoveLabel, action.VikunjaTask(t.ID), l).WaitFor(submit))
		}
	}
}

// dragged reports an undone card in the backlog bucket and in no Doing
// bucket (drag_cancel.dragged_to_backlog).
func (r Router) dragged(t event.Task) bool {
	p := r.Config.Policy
	return p.BacklogBucket != "" && !t.Done && t.InBucket(p.BacklogBucket) && !t.InBucket(p.DoingBucket)
}

// dragCancel stops a task's live implementation and discuss runs when its
// card is dragged back to the backlog (drag_cancel.py): the runs are
// cancelled (a queued submission never created, a live Task deleted),
// then the in-progress label goes and the stop notice is posted. A task
// with its PR open is finished, not stopped.
func (r Router) dragCancel(proj profile.Project, t event.Task, led Ledger) (FamilyDecision, bool) {
	if !r.dragged(t) {
		return FamilyDecision{}, false
	}
	p, fd := r.Config.Policy, r.family(run.VikunjaDragCancel, proj.Name)
	if p.PROpenLabel != "" && t.HasLabel(p.PROpenLabel) {
		fd.Outcome, fd.Reason = Ignore, ReasonPROpen
		return fd, true
	}
	var live []string
	for _, rec := range led.TaskRuns(t.ID, run.AgentFamilies) {
		if rec.Active {
			live = append(live, rec.Name)
		}
	}
	if len(live) == 0 {
		fd.Outcome, fd.Reason = Ignore, ReasonNoLiveAgent
		return fd, true
	}
	target := action.VikunjaTask(t.ID)
	cancel := action.New(action.CancelRun, target, "")
	cancel.Detail = strings.Join(live, ",")
	fd.Actions = append(fd.Actions, cancel)
	// The notice comes last: it says whether the label could be removed.
	last := cancel
	if t.HasLabel(p.InProgressLabel) {
		last = action.New(action.RemoveLabel, target, p.InProgressLabel).WaitFor(cancel)
		fd.Actions = append(fd.Actions, last)
	}
	fd.Actions = append(fd.Actions, action.Notice(target, notice.Stop, cancel.Detail).WaitFor(last))
	fd.Outcome = Act
	return fd, true
}

// doneCleanup takes the in-progress and action-needed labels off a done
// task (done_unlabel.py, action_needed.on_update): each removal re-reads
// the task and removes only while it is still done. Like the legacy
// hooks it says nothing about a task that has neither label.
func (r Router) doneCleanup(proj profile.Project, t event.Task) (FamilyDecision, bool) {
	p := r.Config.Policy
	if !t.Done {
		return FamilyDecision{}, false
	}
	fd := r.family(run.VikunjaDoneCleanup, proj.Name)
	for _, l := range []string{p.InProgressLabel, p.ActionNeededLabel} {
		if l != "" && t.HasLabel(l) {
			a := action.New(action.RemoveLabel, action.VikunjaTask(t.ID), l)
			a.Detail = DetailIfDone
			fd.Actions = append(fd.Actions, a)
		}
	}
	fd.Outcome = Act
	return fd, len(fd.Actions) > 0
}

// requeues are the related tasks whose decision a change of t may move
// (subtask_chain.on_update): its parents' chains and, when t is done,
// each task it blocked, which a blocker getting done notifies of nothing.
// Only tasks of configured projects; never past MaxRequeueDepth.
func (r Router) requeues(ev event.Event, t event.Task) []Requeue {
	depth := 0
	if s := ev.Vikunja.Synthetic; s != nil {
		depth = s.Depth
	}
	if depth >= MaxRequeueDepth {
		return nil
	}
	related := slices.Clone(t.Relations.Parents)
	if t.Done && depth == 0 {
		related = append(related, t.Relations.Blocking...)
	}
	var out []Requeue
	for _, rel := range related {
		if _, ok := r.Config.Project(rel.ProjectID); !ok || rel.ID == t.ID {
			continue
		}
		if !slices.ContainsFunc(out, func(q Requeue) bool { return q.TaskID == rel.ID }) {
			out = append(out, Requeue{TaskID: rel.ID, ProjectID: rel.ProjectID, Depth: depth + 1})
		}
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
