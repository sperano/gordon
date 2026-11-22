package route

import (
	"slices"
	"strconv"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/run"
)

const (
	ReasonAutoAssignDisabled = "auto-assign-disabled"
	ReasonAlreadyAssigned    = "already-assigned"
	ReasonSkipLabel          = "auto-assign-skip-label"
	ReasonChainSubtask       = "chain-subtask"
)

func (r Router) taskCreated(ev event.Event, st State) Decision {
	sent := ev.Vikunja.Task
	proj, ok := r.Config.Project(sent.ProjectID)
	if !ok {
		return Decision{Outcome: Ignore, Reason: ReasonProjectNotConfigured}
	}
	if proj.AutoAssignVariant == "" {
		return Decision{Outcome: Ignore, Reason: ReasonAutoAssignDisabled}
	}
	fd := r.family(run.VikunjaAutoAssign, proj.Name)
	if reason := r.autoAssignBlocker(sent); reason != "" {
		fd.Outcome, fd.Reason = Ignore, reason
		return Decision{Families: []FamilyDecision{fd}}
	}
	if st.Task == nil || !st.ParentsRead {
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	}
	fresh := *st.Task
	if fresh.ProjectID != sent.ProjectID {
		fd.Outcome, fd.Reason = Stale, ReasonProjectNotConfigured
		return Decision{Families: []FamilyDecision{fd}}
	}
	if reason := r.autoAssignBlocker(fresh); reason != "" {
		fd.Outcome, fd.Reason = Stale, reason
		return Decision{Families: []FamilyDecision{fd}}
	}
	if slices.ContainsFunc(st.Parents, func(parent event.Task) bool {
		return parent.HasLabel(r.Config.Policy.ChainLabel)
	}) {
		fd.Outcome, fd.Reason = Ignore, ReasonChainSubtask
		return Decision{Families: []FamilyDecision{fd}}
	}
	a := action.New(action.AssignIfUnassigned, action.VikunjaTask(fresh.ID), proj.AutoAssignVariant)
	a.Detail = strconv.FormatInt(sent.ProjectID, 10)
	fd.Outcome, fd.Actions = Act, []action.Action{a}
	return Decision{Families: []FamilyDecision{fd}}
}

func (r Router) autoAssignBlocker(t event.Task) string {
	if len(t.Assignees) != 0 {
		return ReasonAlreadyAssigned
	}
	p := r.Config.Policy
	if t.HasLabel(p.NoAgentLabel) || t.HasLabel(p.ChainLabel) || t.HasLabel(p.DiscussLabel) {
		return ReasonSkipLabel
	}
	return ""
}
