package route

import (
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// taskComment routes a Vikunja `/gordon` comment: an owner follow-up on a
// task (commentspawners.yaml). The mode follows the fresh labels: discuss
// answers read-only, `in progress` or `pr open` works on the task's
// branch, anything else answers without a branch.
func (r Router) taskComment(ev event.Event, st State, led Ledger) Decision {
	p, c := r.Config.Policy, ev.Vikunja.Comment
	if !isRequest(c.Body, p.Trigger, true) {
		return Decision{Outcome: Ignore, Reason: ReasonNotARequest}
	}
	if ev.Actor != p.VikunjaOwner {
		return Decision{Outcome: Reject, Reason: ReasonNotOwner}
	}
	if led.HandledRequest(ev.RequestKey) {
		return Decision{Outcome: Duplicate, Reason: ReasonRequestHandled}
	}
	if _, known := r.Config.Project(ev.Vikunja.Task.ProjectID); known && st.Task == nil {
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	}
	proj, known, moved := r.projectOf(ev.Vikunja.Task.ProjectID, st.Task)
	family, mode := run.VikunjaFollowUpAnswer, profile.ModeAnswer
	if known {
		family, mode = r.followUpMode(*st.Task)
	}
	fd := r.family(family, proj.Name)
	react := action.New(action.React, action.VikunjaComment(c.ID), reactionEyes)
	if !known {
		o := Ignore
		if moved {
			o = Stale
		}
		unanswered(&fd, o, ReasonProjectNotConfigured, ev.Vikunja.Task.ID, c.ID, react)
		return Decision{Families: []FamilyDecision{fd}}
	}
	if b := r.followUpBlocker(ev.Vikunja.Task, *st.Task); b != nil {
		unanswered(&fd, b.outcome, b.reason, st.Task.ID, c.ID, react)
		return Decision{Families: []FamilyDecision{fd}}
	}
	variant, _ := r.assignment(*st.Task)
	id := run.Identity{Family: family, Project: proj.Name, Variant: variant, TaskID: st.Task.ID, CommentID: c.ID}
	if rn, submit, ok := r.submit(&fd, led, id, profile.Request{Project: proj.Name, Variant: variant, Mode: mode}); ok {
		fd.Actions = append(fd.Actions, react)
		if family == run.VikunjaFollowUpDiscuss {
			fd.Actions = append(fd.Actions, action.Notice(action.VikunjaTask(st.Task.ID), NoticeDiscussStart, itoa(c.ID)))
		}
		r.announce(&fd, *st.Task, rn, submit)
	}
	return Decision{Families: []FamilyDecision{fd}}
}

func (r Router) followUpMode(t event.Task) (run.Family, profile.Mode) {
	p := r.Config.Policy
	switch {
	case t.HasLabel(p.DiscussLabel):
		return run.VikunjaFollowUpDiscuss, profile.ModeDiscuss
	case t.HasLabel(p.InProgressLabel) || t.HasLabel(p.PROpenLabel):
		return run.VikunjaFollowUp, profile.ModeFollowUp
	default:
		return run.VikunjaFollowUpAnswer, profile.ModeAnswer
	}
}

type followUpBlock struct {
	outcome Outcome
	reason  string
}

// followUpBlocker is why no agent answers, in unanswered_notice.reasons()
// order; a task done since the comment was sent is stale.
func (r Router) followUpBlocker(sent, fresh event.Task) *followUpBlock {
	switch {
	case fresh.Done && !sent.Done:
		return &followUpBlock{Stale, ReasonTaskDone}
	case fresh.Done:
		return &followUpBlock{Ignore, ReasonTaskDone}
	case fresh.HasLabel(r.Config.Policy.NoAgentLabel):
		return &followUpBlock{Ignore, ReasonNoAgentLabel}
	}
	if _, b := r.assignment(fresh); b != nil {
		if b.conflict {
			return &followUpBlock{Conflict, b.reason}
		}
		return &followUpBlock{Ignore, b.reason}
	}
	return nil
}

// unanswered records why no agent answers: the owner's request still gets
// its 👀 and an unanswered notice on its task, as legacy does. The
// notice's detail is "<comment id>:<reason>": the comment it links and
// the reason it gives.
func unanswered(fd *FamilyDecision, o Outcome, reason string, task, comment int64, react action.Action) {
	fd.Outcome, fd.Reason = o, reason
	fd.Actions = append(fd.Actions, react,
		action.Notice(action.VikunjaTask(task), NoticeUnanswered, itoa(comment)+":"+reason))
}
