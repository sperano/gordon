package route

import (
	"cmp"
	"slices"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// chain runs the subtasks of a task carrying the chain label one at a
// time, in index order (subtask_chain.advance), from the parent's fresh
// state, its fresh subtasks and its comments. Every write is idempotent
// and every notice posted once (its marker on a comment of Gordon's own),
// so concurrent or repeated decisions of one parent converge, and a
// subtask the chain already started is never moved again: the owner
// stopping it (dragging it back) holds the chain.
func (r Router) chain(proj profile.Project, st State) (FamilyDecision, bool) {
	p, parent := r.Config.Policy, *st.Task
	if p.ChainLabel == "" || !parent.HasLabel(p.ChainLabel) {
		return FamilyDecision{}, false
	}
	fd := r.family(run.VikunjaChain, proj.Name)
	target := action.VikunjaTask(parent.ID)
	switch {
	case parent.Done:
		r.dropInProgress(&fd, parent, ReasonTaskDone)
	case p.BacklogBucket != "" && parent.InBucket(p.BacklogBucket) && !parent.InBucket(p.DoingBucket):
		r.dropInProgress(&fd, parent, ReasonChainPaused)
	case !parent.InBucket(p.DoingBucket):
		fd.Outcome, fd.Reason = Ignore, ReasonNotInDoing
	case parent.HasLabel(p.DiscussLabel) || parent.HasLabel(p.NoAgentLabel):
		fd.Outcome, fd.Reason = Ignore, ReasonChainHeld
	case len(parent.OpenBlockers()) > 0:
		fd.Outcome, fd.Reason = Ignore, ReasonBlocked
	default:
		agent, b := r.assignment(parent)
		if b != nil {
			fd.Outcome, fd.Reason = Act, b.reason
			fd.Actions = append(fd.Actions, action.Notice(target, notice.ChainNoAgent, ""))
			return fd, true
		}
		if !parent.HasLabel(p.InProgressLabel) {
			fd.Actions = append(fd.Actions, action.New(action.AddLabel, target, p.InProgressLabel))
		}
		r.chainStep(&fd, parent, agent, st)
	}
	return fd, true
}

// dropInProgress removes a stopped or finished chain's in-progress label.
func (r Router) dropInProgress(fd *FamilyDecision, t event.Task, reason string) {
	fd.Outcome, fd.Reason = Ignore, reason
	if l := r.Config.Policy.InProgressLabel; t.HasLabel(l) {
		fd.Outcome = Act
		fd.Actions = append(fd.Actions, action.New(action.RemoveLabel, action.VikunjaTask(t.ID), l))
	}
}

// chainStep waits, starts the next subtask, or finishes (run_chain).
func (r Router) chainStep(fd *FamilyDecision, parent event.Task, agent string, st State) {
	p := r.Config.Policy
	subs := slices.SortedFunc(slices.Values(st.Subtasks), func(a, b event.Task) int {
		return cmp.Or(cmp.Compare(a.Index, b.Index), cmp.Compare(a.ID, b.ID))
	})
	pending := slices.DeleteFunc(slices.Clone(subs), func(t event.Task) bool { return t.Done })
	fd.Outcome = Act
	switch {
	case len(subs) == 0:
		fd.Reason = ReasonChainNoTasks
	case len(pending) == 0:
		fd.Reason = ReasonChainComplete
		target := action.VikunjaTask(parent.ID)
		fd.Actions = append(fd.Actions, action.Notice(target, notice.ChainDone, ""))
		if p.ReviewBucket != "" && !parent.InBucket(p.ReviewBucket) {
			fd.Actions = append(fd.Actions, action.New(action.MoveBucket, target, p.ReviewBucket))
		}
	case slices.ContainsFunc(pending, r.inFlight):
		fd.Reason = ReasonChainWaiting
	default:
		ready := slices.DeleteFunc(pending, func(t event.Task) bool { return len(t.OpenBlockers()) > 0 })
		if len(ready) == 0 {
			fd.Reason = ReasonBlocked
			break
		}
		r.next(fd, parent, ready[0], agent, st.Comments)
	}
	if len(fd.Actions) == 0 {
		fd.Outcome = Ignore
	}
}

// inFlight reports an open subtask running, in review or stuck on its way.
func (r Router) inFlight(t event.Task) bool {
	p := r.Config.Policy
	for _, b := range []string{p.DoingBucket, p.ReviewBucket} {
		if b != "" && t.InBucket(b) {
			return true
		}
	}
	for _, l := range []string{p.InProgressLabel, p.PROpenLabel} {
		if l != "" && t.HasLabel(l) {
			return true
		}
	}
	return false
}

// next hands the subtask to its agent (subtask_chain.start), or holds the
// chain on it: waiting for the owner on a no-agent subtask, stopped when
// the chain had already started it. The subtask gets the chain label
// first when it has subtasks of its own, then its agent and the parent's
// auto-merge label, and only then moves to Doing: the move is what starts
// its run, which must see all three.
func (r Router) next(fd *FamilyDecision, parent, sub event.Task, agent string, comments []event.TaskComment) {
	p, ptarget, starget := r.Config.Policy, action.VikunjaTask(parent.ID), action.VikunjaTask(sub.ID)
	detail := itoa(sub.ID)
	switch {
	case p.NoAgentLabel != "" && sub.HasLabel(p.NoAgentLabel):
		fd.Actions = append(fd.Actions, action.Notice(ptarget, notice.ChainWaiting, detail))
		return
	case notice.ChainStartedFor(comments, p.VikunjaServiceUser, p.VikunjaWebURL, sub.ID):
		fd.Actions = append(fd.Actions, action.Notice(ptarget, notice.ChainStopped, detail))
		return
	}
	var steps []action.Action
	if len(sub.Relations.Subtasks) > 0 && !sub.HasLabel(p.ChainLabel) {
		steps = append(steps, action.New(action.AddLabel, starget, p.ChainLabel))
	}
	if own := r.agents(sub); len(own) == 0 {
		steps = append(steps, action.New(action.Assign, starget, agent))
	}
	if p.AutoMergeLabel != "" && p.AutoMergeLabelCreator != "" {
		sync := action.New(action.SyncLabel, starget, p.AutoMergeLabel)
		sync.Detail = itoa(parent.ID)
		steps = append(steps, sync)
	}
	steps = append(steps, action.New(action.MoveBucket, starget, p.DoingBucket),
		action.Notice(ptarget, notice.ChainStarted, detail))
	for i := 1; i < len(steps); i++ {
		steps[i] = steps[i].Then(steps[i-1])
	}
	fd.Actions = append(fd.Actions, steps...)
}
