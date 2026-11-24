package route

import (
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// Vikunja eligibility reasons. The ones a follow-up shares are also the
// legacy unanswered-notice reasons (unanswered_notice.reasons()).
const (
	ReasonProjectNotConfigured = "project-not-configured"
	ReasonTaskDone             = "task-done"
	ReasonNoAgentLabel         = "no-agent-label"
	ReasonNoAgentAssignee      = "no-agent-assignee"
	ReasonMultipleAgents       = "multiple-agent-assignees"
	ReasonNotInDoing           = "not-in-doing"
	ReasonInProgress           = "in-progress"
	ReasonChain                = "chain-parent"
	// An ops-labelled task whose creator or starting event is not the
	// ops owner's runs nothing: never its implementation run either.
	ReasonOpsCreator = "ops-creator-not-owner"
	ReasonOpsDoer    = "ops-doer-not-owner"
)

// Notice kinds, the legacy `<!-- kelos:<kind> -->` markers.
const (
	NoticeUnanswered   = notice.Unanswered
	NoticeDiscussStart = notice.DiscussStart
	NoticePROpen       = notice.PROpen
	NoticeMerged       = notice.Merged
)

// reactionEyes is the 👀 a follow-up request gets.
const reactionEyes = "eyes"

// blocked holds why a task cannot start a run, and whether that is a
// conflict rather than plain ineligibility.
type blocked struct {
	reason   string
	conflict bool
}

// assignment returns the task's single agent variant.
func (r Router) assignment(t event.Task) (string, *blocked) {
	switch agents := r.agents(t); len(agents) {
	case 0:
		return "", &blocked{reason: ReasonNoAgentAssignee}
	case 1:
		return agents[0], nil
	default:
		return "", &blocked{reason: ReasonMultipleAgents, conflict: true}
	}
}

// startBlocker is why a task.updated snapshot cannot start an
// implementation or discuss run (taskspawners.yaml's filters).
func (r Router) startBlocker(t event.Task) *blocked {
	p := r.Config.Policy
	switch {
	case t.Done:
		return &blocked{reason: ReasonTaskDone}
	case !t.InBucket(p.DoingBucket):
		return &blocked{reason: ReasonNotInDoing}
	case t.HasLabel(p.InProgressLabel):
		return &blocked{reason: ReasonInProgress}
	case t.HasLabel(p.NoAgentLabel):
		return &blocked{reason: ReasonNoAgentLabel}
	case t.HasLabel(p.ChainLabel) && !t.HasLabel(p.DiscussLabel):
		return &blocked{reason: ReasonChain}
	}
	_, b := r.assignment(t)
	return b
}

// judge turns the blockers of the event's snapshot and of the fresh state
// into an outcome: blocked now but not when sent is stale.
func judge(fd *FamilyDecision, sent, now *blocked) bool {
	if now == nil {
		return true
	}
	fd.Reason = now.reason
	switch {
	case now.conflict:
		fd.Outcome = Conflict
	case sent == nil:
		fd.Outcome = Stale
	default:
		fd.Outcome = Ignore
	}
	return false
}

func (r Router) taskUpdated(ev event.Event, st State, led Ledger) Decision {
	proj, ok := r.Config.Project(ev.Vikunja.Task.ProjectID)
	if !ok {
		return r.unconfiguredDone(ev, st)
	}
	if st.Task == nil {
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	}
	if proj, ok, _ = r.projectOf(ev.Vikunja.Task.ProjectID, st.Task); !ok {
		return Decision{Outcome: Stale, Reason: ReasonProjectNotConfigured}
	}
	fresh := *st.Task
	dec := Decision{Families: []FamilyDecision{r.agentStart(proj, ev, fresh, led)}}
	for _, f := range []func() (FamilyDecision, bool){
		func() (FamilyDecision, bool) { return r.dragCancel(proj, fresh, led) },
		func() (FamilyDecision, bool) { return r.doneCleanup(proj, fresh) },
		func() (FamilyDecision, bool) { return r.chain(proj, st) },
	} {
		if fd, ok := f(); ok {
			dec.Families = append(dec.Families, fd)
		}
	}
	dec.Requeue = r.requeues(ev, fresh)
	return dec
}

// unconfiguredDone ignores a task of an unconfigured project, but a done
// one still requeues the configured tasks it blocked (blockers count in
// any project) and its configured parents. An unread one requeues nothing:
// the reconciliation is the backstop.
func (r Router) unconfiguredDone(ev event.Event, st State) Decision {
	dec := Decision{Outcome: Ignore, Reason: ReasonProjectNotConfigured}
	if st.Task != nil && st.Task.Done {
		dec.Requeue = r.requeues(ev, *st.Task)
	}
	return dec
}

// agentStart decides a task's implementation, discuss or ops run
// (taskspawners.yaml): eligible in the fresh state, and waiting while a
// task blocking it is open; a dispatched run claims the task once
// submitted.
func (r Router) agentStart(proj profile.Project, ev event.Event, fresh event.Task, led Ledger) FamilyDecision {
	sent := ev.Vikunja.Task
	if sent.Buckets == nil {
		sent.Buckets = fresh.Buckets
	}
	family, mode := r.startMode(proj, fresh)
	fd := r.family(family, proj.Name)
	if !judge(&fd, r.startBlocker(sent), r.startBlocker(fresh)) {
		return fd
	}
	if mode == profile.ModeOps {
		if reason := r.opsBlocker(ev.Actor, fresh); reason != "" {
			fd.Outcome, fd.Reason = Ignore, reason
			return fd
		}
	}
	if len(fresh.OpenBlockers()) > 0 {
		fd.Outcome, fd.Reason = Ignore, ReasonBlocked
		return fd
	}
	variant, _ := r.assignment(fresh)
	id := run.Identity{Family: family, Project: proj.Name, Variant: variant, TaskID: fresh.ID, Ops: mode == profile.ModeOps}
	if rn, submit, ok := r.submit(&fd, led, id, profile.Request{Project: proj.Name, Variant: variant, Mode: mode}); ok {
		r.claim(&fd, fresh, rn, submit)
	}
	return fd
}

// startMode is the run a task.updated starts: discuss with its label,
// else ops with the ops label in a project the ops mode covers, else
// implementation. An ops run is the task's implementation run, in the
// implementation family.
func (r Router) startMode(proj profile.Project, t event.Task) (run.Family, profile.Mode) {
	p := r.Config.Policy
	switch {
	case t.HasLabel(p.DiscussLabel):
		return run.VikunjaDiscuss, profile.ModeDiscuss
	case p.OpsCovers(proj.Name) && t.HasLabel(p.OpsLabel):
		return run.VikunjaImplement, profile.ModeOps
	default:
		return run.VikunjaImplement, profile.ModeImplement
	}
}

// opsBlocker is why an ops start is not the owner's
// (kelos.clusterOperator.filters): the task's creator and the event's
// doer must both be the ops owner. A synthetic delivery has no doer, so
// only the owner's own update starts an ops run.
func (r Router) opsBlocker(doer string, t event.Task) string {
	owner := r.Config.Policy.OpsOwner
	switch {
	case t.CreatedBy != owner:
		return ReasonOpsCreator
	case doer != owner:
		return ReasonOpsDoer
	default:
		return ""
	}
}

// projectOf is the task's configured project: the fresh task's when it
// moved since the delivery (into a configured project too), so a run
// takes its new project's profile and Task name. moved reports a move
// from a configured project out of the configured projects.
func (r Router) projectOf(sent int64, fresh *event.Task) (p profile.Project, ok, moved bool) {
	p, ok = r.Config.Project(sent)
	if fresh == nil || fresh.ProjectID == sent {
		return p, ok, false
	}
	wasConfigured := ok
	p, ok = r.Config.Project(fresh.ProjectID)
	return p, ok, wasConfigured && !ok
}

// submit resolves and deduplicates one run; on success fd dispatches it
// and submit is its submission, which the run's claim waits for.
func (r Router) submit(fd *FamilyDecision, led Ledger, id run.Identity, req profile.Request) (Run, action.Action, bool) {
	if reason, dup := existing(led, id.Name()); dup {
		fd.Outcome, fd.Reason = Duplicate, reason
		return Run{}, action.Action{}, false
	}
	rn, ok := r.resolved(fd, id, req)
	if !ok {
		return Run{}, action.Action{}, false
	}
	submit := action.New(action.SubmitTask, action.KelosTask(KelosNamespace, rn.Name), "")
	fd.Outcome = Dispatch
	fd.Runs = append(fd.Runs, rn)
	fd.Actions = append(fd.Actions, submit)
	return rn, submit, true
}
