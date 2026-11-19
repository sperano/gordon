package route

import (
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
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
)

// Notice kinds, the legacy `<!-- kelos:<kind> -->` markers.
const (
	NoticeUnanswered   = "unanswered"
	NoticeDiscussStart = "discuss-start"
	NoticePROpen       = "pr-open"
	NoticeMerged       = "merged"
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
		return Decision{Outcome: Ignore, Reason: ReasonProjectNotConfigured}
	}
	if st.Task == nil {
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	}
	sent, fresh := ev.Vikunja.Task, *st.Task
	if sent.Buckets == nil {
		sent.Buckets = fresh.Buckets
	}
	family, mode := run.VikunjaImplement, profile.ModeImplement
	if fresh.HasLabel(r.Config.Policy.DiscussLabel) {
		family, mode = run.VikunjaDiscuss, profile.ModeDiscuss
	}
	fd := r.family(family, proj.Name)
	if judge(&fd, r.startBlocker(sent), r.startBlocker(fresh)) {
		variant, _ := r.assignment(fresh)
		id := run.Identity{Family: family, Project: proj.Name, Variant: variant, TaskID: fresh.ID}
		r.submit(&fd, led, id, profile.Request{Project: proj.Name, Variant: variant, Mode: mode})
	}
	return Decision{Families: []FamilyDecision{fd}}
}

// submit resolves and deduplicates one run; on success fd dispatches it.
func (r Router) submit(fd *FamilyDecision, led Ledger, id run.Identity, req profile.Request) bool {
	if reason, dup := existing(led, id.Name()); dup {
		fd.Outcome, fd.Reason = Duplicate, reason
		return false
	}
	rn, ok := r.resolved(fd, id, req)
	if !ok {
		return false
	}
	fd.Outcome = Dispatch
	fd.Runs = append(fd.Runs, rn)
	fd.Actions = append(fd.Actions, action.New(action.SubmitTask, action.KelosTask(KelosNamespace, rn.Name), ""))
	return true
}
