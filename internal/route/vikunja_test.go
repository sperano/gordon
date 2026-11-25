package route_test

import (
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

func task(id, project int64, labels []string, assignees ...string) map[string]any {
	ls, as := []map[string]any{}, []map[string]any{}
	for _, l := range labels {
		ls = append(ls, map[string]any{"title": l})
	}
	for _, a := range assignees {
		as = append(as, map[string]any{"username": a})
	}
	return map[string]any{"id": id, "project_id": project, "labels": ls, "assignees": as}
}

func fresh(id, project int64, labels []string, assignees ...string) *event.Task {
	return &event.Task{ID: id, ProjectID: project, Labels: labels, Assignees: assignees, Buckets: []string{"Doing"}}
}

func updated(t *testing.T, tk map[string]any) event.Delivery {
	return signed(t, event.SourceVikunja, nil, map[string]any{"event_name": "task.updated", "data": map[string]any{"task": tk}})
}

func created(t *testing.T, tk map[string]any) event.Delivery {
	return signed(t, event.SourceVikunja, nil, map[string]any{"event_name": "task.created", "data": map[string]any{"task": tk}})
}

func TestTaskCreatedAutoAssign(t *testing.T) {
	r := router()
	state := route.State{Task: fresh(5, 136, nil), ParentsRead: true}
	d := r.Decide(created(t, task(5, 136, nil)), state, replay.Ledger{})
	fd := family(t, d)
	expectFamily(t, fd, run.VikunjaAutoAssign, route.Act, "")
	want := action.Action{Key: "assign-if-unassigned:vikunja/task/5:claude-opus", Kind: action.AssignIfUnassigned,
		Target: action.VikunjaTask(5), Value: "claude-opus", Detail: "136"}
	if len(fd.Actions) != 1 || fd.Actions[0] != want {
		t.Fatalf("actions %+v, want %+v", fd.Actions, want)
	}
}

func TestTaskCreatedAutoAssignRetriesUnreadState(t *testing.T) {
	r := router()
	cases := map[string]route.State{
		"task":    {},
		"parents": {Task: fresh(5, 136, nil)},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			d := r.Decide(created(t, task(5, 136, nil)), state, replay.Ledger{})
			expect(t, d, route.Retry, route.ReasonStateUnavailable)
			if len(d.Families) != 0 {
				t.Fatalf("retry decision has families: %+v", d)
			}
		})
	}
}

func TestTaskCreatedAutoAssignGuards(t *testing.T) {
	r := router()
	cases := []struct {
		name    string
		sent    *event.Task
		fresh   *event.Task
		parents []event.Task
		outcome route.Outcome
		reason  string
	}{
		{"sent assignee", fresh(5, 136, nil, "eric"), fresh(5, 136, nil), nil, route.Ignore, route.ReasonAlreadyAssigned},
		{"fresh assignee", fresh(5, 136, nil), fresh(5, 136, nil, "eric"), nil, route.Stale, route.ReasonAlreadyAssigned},
		{"sent no-agent", fresh(5, 136, []string{"no-agent"}), fresh(5, 136, nil), nil, route.Ignore, route.ReasonSkipLabel},
		{"fresh no-agent", fresh(5, 136, nil), fresh(5, 136, []string{"no-agent"}), nil, route.Stale, route.ReasonSkipLabel},
		{"sent discuss", fresh(5, 136, []string{"discuss"}), fresh(5, 136, nil), nil, route.Ignore, route.ReasonSkipLabel},
		{"fresh discuss", fresh(5, 136, nil), fresh(5, 136, []string{"discuss"}), nil, route.Stale, route.ReasonSkipLabel},
		{"sent chain", fresh(5, 136, []string{"chain"}), fresh(5, 136, nil), nil, route.Ignore, route.ReasonSkipLabel},
		{"fresh chain", fresh(5, 136, nil), fresh(5, 136, []string{"chain"}), nil, route.Stale, route.ReasonSkipLabel},
		{"chain parent", fresh(5, 136, nil), fresh(5, 136, nil), []event.Task{{Labels: []string{"chain"}}}, route.Ignore, route.ReasonChainSubtask},
		{"moved", fresh(5, 136, nil), fresh(5, 139, nil), nil, route.Stale, route.ReasonProjectNotConfigured},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.Decide(created(t, task(c.sent.ID, c.sent.ProjectID, c.sent.Labels, c.sent.Assignees...)),
				route.State{Task: c.fresh, Parents: c.parents, ParentsRead: true}, replay.Ledger{})
			fd := family(t, d)
			expectFamily(t, fd, run.VikunjaAutoAssign, c.outcome, c.reason)
			if len(fd.Actions) != 0 {
				t.Fatalf("guarded decision actions %+v", fd.Actions)
			}
		})
	}
}

func TestTaskCreatedAutoAssignOptOut(t *testing.T) {
	r := router()
	for i := range r.Config.Projects {
		if r.Config.Projects[i].VikunjaID == 136 {
			r.Config.Projects[i].AutoAssignVariant = ""
		}
	}
	d := r.Decide(created(t, task(5, 136, nil)), route.State{}, replay.Ledger{})
	expect(t, d, route.Ignore, route.ReasonAutoAssignDisabled)
}

func comment(t *testing.T, tk map[string]any, id int64, text, doer string) event.Delivery {
	return signed(t, event.SourceVikunja, nil, map[string]any{"event_name": "task.comment.created",
		"data": map[string]any{"task": tk, "comment": map[string]any{"id": id, "comment": text}, "doer": map[string]any{"username": doer}}})
}

func TestTaskUpdatedEligibility(t *testing.T) {
	r := router()
	cases := []struct {
		name    string
		sent    []string
		now     []string
		family  run.Family
		outcome route.Outcome
		reason  string
	}{
		{"chain parent", []string{"chain"}, []string{"chain"}, run.VikunjaImplement, route.Ignore, route.ReasonChain},
		{"chain discuss", []string{"chain", "discuss"}, []string{"chain", "discuss"}, run.VikunjaDiscuss, route.Dispatch, ""},
		{"in progress", []string{"in progress"}, []string{"in progress"}, run.VikunjaImplement, route.Ignore, route.ReasonInProgress},
		{"claimed since", nil, []string{"in progress"}, run.VikunjaImplement, route.Stale, route.ReasonInProgress},
		{"no-agent", []string{"no-agent"}, []string{"no-agent"}, run.VikunjaImplement, route.Ignore, route.ReasonNoAgentLabel},
	}
	for _, c := range cases {
		d := r.Decide(updated(t, task(5, 136, c.sent, "claude-opus")), route.State{Task: fresh(5, 136, c.now, "claude-opus")}, replay.Ledger{})
		// The agent family comes first; a chain parent adds its chain's.
		t.Run(c.name, func(t *testing.T) {
			if len(d.Families) == 0 {
				t.Fatalf("no family decided: %+v", d)
			}
			expectFamily(t, d.Families[0], c.family, c.outcome, c.reason)
		})
	}
	none := r.Decide(updated(t, task(5, 136, nil)), route.State{Task: fresh(5, 136, nil)}, replay.Ledger{})
	expectFamily(t, family(t, none), run.VikunjaImplement, route.Ignore, route.ReasonNoAgentAssignee)
	retry := r.Decide(updated(t, task(5, 136, nil, "claude-opus")), route.State{}, replay.Ledger{})
	expect(t, retry, route.Retry, route.ReasonStateUnavailable)
}

// TestFreshProjectWins pins that a task moved since the delivery runs
// under its new project, and one moved out of the configured projects is
// stale.
func TestFreshProjectWins(t *testing.T) {
	r := router()
	moved := r.Decide(updated(t, task(5, 136, nil, "claude-opus")), route.State{Task: fresh(5, 139, nil, "claude-opus")}, replay.Ledger{})
	fd := family(t, moved)
	if fd.Project != "lucarne" || len(fd.Runs) != 1 || fd.Runs[0].Name != "vikunja-5-lucarne-opus" {
		t.Errorf("moved task: project %q runs %+v", fd.Project, fd.Runs)
	}
	gone := r.Decide(updated(t, task(5, 136, nil, "claude-opus")), route.State{Task: fresh(5, 99, nil, "claude-opus")}, replay.Ledger{})
	expect(t, gone, route.Stale, route.ReasonProjectNotConfigured)
	c := r.Decide(comment(t, task(5, 136, nil, "claude-opus"), 9, "<p>/gordon go</p>", "eric"),
		route.State{Task: fresh(5, 139, nil, "claude-opus")}, replay.Ledger{})
	if fd := family(t, c); len(fd.Runs) != 1 || fd.Runs[0].Name != "vikunja-fu-lucarne-5-c9" {
		t.Errorf("moved task comment runs %+v", fd.Runs)
	}
	in := r.Decide(comment(t, task(5, 99, nil, "claude-opus"), 9, "<p>/gordon go</p>", "eric"),
		route.State{Task: fresh(5, 139, nil, "claude-opus")}, replay.Ledger{})
	if fd := family(t, in); fd.Outcome != route.Dispatch || len(fd.Runs) != 1 || fd.Runs[0].Name != "vikunja-fu-lucarne-5-c9" {
		t.Errorf("comment on a task moved into a project: %s runs %+v", fd.Outcome, fd.Runs)
	}
	between := r.Decide(comment(t, task(5, 99, nil, "claude-opus"), 9, "<p>/gordon go</p>", "eric"),
		route.State{Task: fresh(5, 98, nil, "claude-opus")}, replay.Ledger{})
	expectFamily(t, family(t, between), run.VikunjaFollowUpAnswer, route.Ignore, route.ReasonProjectNotConfigured)
}

func TestTaskCommentModes(t *testing.T) {
	r := router()
	const text = "<p>/gordon go</p>"
	busy := r.Decide(comment(t, task(5, 136, []string{"pr open"}, "claude-opus"), 9, text, "eric"),
		route.State{Task: fresh(5, 136, []string{"pr open"}, "claude-opus")}, replay.Ledger{})
	fd := family(t, busy)
	expectFamily(t, fd, run.VikunjaFollowUp, route.Dispatch, "")
	if fd.Runs[0].Branch != "kelos/vikunja-5" {
		t.Errorf("pr open follow-up branch %q", fd.Runs[0].Branch)
	}
	exists := r.Decide(comment(t, task(5, 136, nil, "claude-opus"), 9, text, "eric"), route.State{Task: fresh(5, 136, nil, "claude-opus")},
		replay.Ledger{Runs: []route.RunRecord{{Name: "vikunja-fu-sillage-5-c9", Exists: true}}})
	fd = family(t, exists)
	expectFamily(t, fd, run.VikunjaFollowUpAnswer, route.Duplicate, route.ReasonRunExists)
	if len(fd.Actions) != 0 {
		t.Errorf("duplicate has actions %v", keys(fd.Actions))
	}
	notRequest := r.Decide(comment(t, task(5, 136, nil), 9, "<p>/gordonx</p>", "eric"), route.State{}, replay.Ledger{})
	expect(t, notRequest, route.Ignore, route.ReasonNotARequest)
	retry := r.Decide(comment(t, task(5, 136, nil), 9, text, "eric"), route.State{}, replay.Ledger{})
	expect(t, retry, route.Retry, route.ReasonStateUnavailable)
}

func TestTaskCommentUnanswered(t *testing.T) {
	r := router()
	const text = "<p>/gordon go</p>"
	cases := []struct {
		name    string
		sent    map[string]any
		now     *event.Task
		outcome route.Outcome
		reason  string
	}{
		{"done since", task(5, 136, nil, "claude-opus"), &event.Task{ID: 5, ProjectID: 136, Done: true, Assignees: []string{"claude-opus"}}, route.Stale, route.ReasonTaskDone},
		{"two agents", task(5, 136, nil, "claude-opus", "gpt-sol"), fresh(5, 136, nil, "claude-opus", "gpt-sol"), route.Conflict, route.ReasonMultipleAgents},
		{"no-agent", task(5, 136, nil, "claude-opus"), fresh(5, 136, []string{"no-agent"}, "claude-opus"), route.Ignore, route.ReasonNoAgentLabel},
		{"unknown project", task(5, 99, nil, "claude-opus"), nil, route.Ignore, route.ReasonProjectNotConfigured},
		{"moved out since", task(5, 136, nil, "claude-opus"), fresh(5, 99, nil, "claude-opus"), route.Stale, route.ReasonProjectNotConfigured},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.Decide(comment(t, c.sent, 9, text, "eric"), route.State{Task: c.now}, replay.Ledger{})
			fd := family(t, d)
			if fd.Outcome != c.outcome || fd.Reason != c.reason {
				t.Errorf("%s/%q, want %s/%q", fd.Outcome, fd.Reason, c.outcome, c.reason)
			}
			want := []string{"react:vikunja/comment/9:eyes", "post-notice:vikunja/task/5:unanswered:9:" + c.reason}
			if got := keys(fd.Actions); !slices.Equal(got, want) {
				t.Errorf("actions %v, want %v", got, want)
			}
		})
	}
}

// TestDiscussRunDeduplicatesAgainstItsPreviousName: a discuss run that
// exists under the name it had before the task id led it is not
// submitted again, and the same name without the discuss prefix (the
// implementation run's previous name) does not hold it back.
func TestDiscussRunDeduplicatesAgainstItsPreviousName(t *testing.T) {
	r := router()
	discuss := []string{"discuss"}
	decide := func(name string) route.FamilyDecision {
		led := replay.Ledger{Runs: []route.RunRecord{{Name: name, Exists: true, Family: run.VikunjaDiscuss, TaskID: 5}}}
		return family(t, r.Decide(updated(t, task(5, 136, discuss, "claude-opus")),
			route.State{Task: fresh(5, 136, discuss, "claude-opus")}, led))
	}
	expectFamily(t, decide("vikunja-discuss-sillage-claude-opus-5"), run.VikunjaDiscuss, route.Duplicate, route.ReasonRunExists)
	fd := decide("vikunja-sillage-claude-opus-5")
	expectFamily(t, fd, run.VikunjaDiscuss, route.Dispatch, "")
	if len(fd.Runs) != 1 || fd.Runs[0].Name != "vikunja-5-sillage-opus-discuss" {
		t.Fatalf("discuss runs %+v", fd.Runs)
	}
}
