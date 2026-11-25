package route_test

import (
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// The ops mode of values-hollingsworth.yaml (clusterOperator), written out.
const (
	opsLabel          = "ops"
	opsOwner          = "eric"
	opsProfile        = "cluster-operator"
	opsServiceAccount = "kelos-cluster-operator"
	opsTask           = 5
	// hollingsworthID and vigieID are the Vikunja ids of a covered and
	// an uncovered project.
	hollingsworthID = 3
	vigieID         = 103
)

// opsRouter is router() with the ops mode on for hollingsworth.
func opsRouter() route.Router {
	r := router()
	r.Config.OpsProfile = opsProfile
	r.Config.Profiles[opsProfile] = profile.Profile{ServiceAccount: opsServiceAccount,
		Capabilities: []profile.Capability{profile.GitHubWrite, profile.GitHubReadOnly, profile.ClusterRead, profile.ClusterWrite}}
	r.Config.Policy.OpsLabel, r.Config.Policy.OpsOwner = opsLabel, opsOwner
	r.Config.Policy.OpsProjects = []string{"hollingsworth"}
	return r
}

// startedBy is a task.updated whose doer is doer ("" for none).
func startedBy(t *testing.T, tk map[string]any, doer string) event.Delivery {
	data := map[string]any{"task": tk}
	if doer != "" {
		data["doer"] = map[string]any{"username": doer}
	}
	return signed(t, event.SourceVikunja, nil, map[string]any{"event_name": "task.updated", "data": data})
}

// createdBy is fresh() with its creator.
func createdBy(creator string, project int64, labels ...string) *event.Task {
	t := fresh(opsTask, project, labels, "claude-opus")
	t.CreatedBy = creator
	return t
}

func TestOpsStartDispatchesTheOpsRun(t *testing.T) {
	r := opsRouter()
	d := r.Decide(startedBy(t, task(opsTask, hollingsworthID, []string{opsLabel}, "claude-opus"), opsOwner),
		route.State{Task: createdBy(opsOwner, hollingsworthID, opsLabel)}, replay.Ledger{})
	fd := d.Families[0]
	expectFamily(t, fd, run.VikunjaImplement, route.Dispatch, "")
	if len(fd.Runs) != 1 {
		t.Fatalf("runs %+v", fd.Runs)
	}
	rn := fd.Runs[0]
	if rn.Name != "vikunja-ops-5-hollingsworth-opus" || rn.Branch != "kelos/vikunja-5" || !rn.Identity.Ops ||
		rn.Profile.Mode != profile.ModeOps || rn.Profile.Profile != opsProfile ||
		rn.Profile.ServiceAccount != opsServiceAccount || rn.Profile.Workspace != "hollingsworth" {
		t.Fatalf("ops run %+v", rn)
	}
	// The claim is the implementation run's.
	plain := router().Decide(startedBy(t, task(opsTask, hollingsworthID, nil, "claude-opus"), opsOwner),
		route.State{Task: createdBy(opsOwner, hollingsworthID)}, replay.Ledger{}).Families[0]
	if got, want := kinds(fd.Actions), kinds(plain.Actions); !slices.Equal(got, want) {
		t.Fatalf("ops actions %v, implementation's %v", got, want)
	}
}

func TestOpsStartNeedsTheOwner(t *testing.T) {
	r := opsRouter()
	cases := []struct {
		name, creator, doer, reason string
	}{
		{"agent creator", "claude-opus", opsOwner, route.ReasonOpsCreator},
		{"no creator", "", opsOwner, route.ReasonOpsCreator},
		{"agent doer", opsOwner, "claude-opus", route.ReasonOpsDoer},
		{"synthetic, no doer", opsOwner, "", route.ReasonOpsDoer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.Decide(startedBy(t, task(opsTask, hollingsworthID, []string{opsLabel}, "claude-opus"), c.doer),
				route.State{Task: createdBy(c.creator, hollingsworthID, opsLabel)}, replay.Ledger{})
			fd := d.Families[0]
			// Never the implementation run instead: nothing runs.
			expectFamily(t, fd, run.VikunjaImplement, route.Ignore, c.reason)
			if len(fd.Runs) != 0 || len(fd.Actions) != 0 {
				t.Fatalf("refused ops start has runs %+v actions %v", fd.Runs, keys(fd.Actions))
			}
		})
	}
}

func TestOpsStartPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		r       route.Router
		project int64
		labels  []string
		outcome route.Outcome
		reason  string
		run     string
	}{
		{"discuss wins", opsRouter(), hollingsworthID, []string{opsLabel, "discuss"}, route.Dispatch, "",
			"vikunja-discuss-5-hollingsworth-opus"},
		{"chain wins", opsRouter(), hollingsworthID, []string{opsLabel, "chain"}, route.Ignore, route.ReasonChain, ""},
		{"ops off", router(), hollingsworthID, []string{opsLabel}, route.Dispatch, "", "vikunja-5-hollingsworth-opus"},
		{"project uncovered", opsRouter(), vigieID, []string{opsLabel}, route.Dispatch, "", "vikunja-5-vigie-opus"},
		{"no ops label", opsRouter(), hollingsworthID, nil, route.Dispatch, "", "vikunja-5-hollingsworth-opus"},
		{"in progress", opsRouter(), hollingsworthID, []string{opsLabel, "in progress"}, route.Ignore, route.ReasonInProgress, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A non-owner creator: only an ops start would be refused.
			d := c.r.Decide(startedBy(t, task(opsTask, c.project, c.labels, "claude-opus"), "claude-opus"),
				route.State{Task: createdBy("claude-opus", c.project, c.labels...)}, replay.Ledger{})
			fd := d.Families[0]
			if fd.Outcome != c.outcome || fd.Reason != c.reason {
				t.Fatalf("%s/%q, want %s/%q", fd.Outcome, fd.Reason, c.outcome, c.reason)
			}
			var names []string
			for _, rn := range fd.Runs {
				names = append(names, rn.Name)
			}
			if want := []string{c.run}; c.run != "" && !slices.Equal(names, want) || c.run == "" && len(names) != 0 {
				t.Fatalf("runs %v, want %q", names, c.run)
			}
		})
	}
}

// TestBlockedOpsStartDispatches: the owner's start of a blocked ops task
// submits the ops run at once (its pod waits for the blockers, as
// legacy's); a requeue after a blocker is done has no doer and starts
// nothing; a blocked implementation run still waits in Gordon.
func TestBlockedOpsStartDispatches(t *testing.T) {
	blocked := func(labels ...string) *event.Task {
		tk := createdBy(opsOwner, hollingsworthID, labels...)
		tk.Relations.BlockedBy = []event.Related{{ID: 9, ProjectID: hollingsworthID}}
		return tk
	}
	d := opsRouter().Decide(startedBy(t, task(opsTask, hollingsworthID, []string{opsLabel}, "claude-opus"), opsOwner),
		route.State{Task: blocked(opsLabel)}, replay.Ledger{})
	fd := d.Families[0]
	expectFamily(t, fd, run.VikunjaImplement, route.Dispatch, "")
	if len(fd.Runs) != 1 || fd.Runs[0].Name != "vikunja-ops-5-hollingsworth-opus" {
		t.Fatalf("blocked ops start runs %+v", fd.Runs)
	}
	requeue := synthetic(t, opsTask, hollingsworthID, map[string]any{"requeue": "k", "depth": 1})
	expectFamily(t, opsRouter().Decide(requeue, route.State{Task: blocked(opsLabel)}, replay.Ledger{}).Families[0],
		run.VikunjaImplement, route.Ignore, route.ReasonOpsDoer)
	impl := opsRouter().Decide(startedBy(t, task(opsTask, hollingsworthID, nil, "claude-opus"), opsOwner),
		route.State{Task: blocked()}, replay.Ledger{})
	expectFamily(t, impl.Families[0], run.VikunjaImplement, route.Ignore, route.ReasonBlocked)
}

func TestOpsRunDeduplicates(t *testing.T) {
	led := replay.Ledger{Runs: []route.RunRecord{{Name: "vikunja-ops-5-hollingsworth-opus", Active: true,
		Exists: true, Family: run.VikunjaImplement, TaskID: opsTask}}}
	d := opsRouter().Decide(startedBy(t, task(opsTask, hollingsworthID, []string{opsLabel}, "claude-opus"), opsOwner),
		route.State{Task: createdBy(opsOwner, hollingsworthID, opsLabel)}, led)
	expectFamily(t, d.Families[0], run.VikunjaImplement, route.Duplicate, route.ReasonRunActive)
}

// TestOpsRunDeduplicatesAgainstItsPreviousName: an ops run started
// under the name it had before the task id led it is not submitted again.
func TestOpsRunDeduplicatesAgainstItsPreviousName(t *testing.T) {
	led := replay.Ledger{Runs: []route.RunRecord{{Name: "vikunja-ops-hollingsworth-claude-opus-5", Active: true,
		Exists: true, Family: run.VikunjaImplement, TaskID: opsTask}}}
	d := opsRouter().Decide(startedBy(t, task(opsTask, hollingsworthID, []string{opsLabel}, "claude-opus"), opsOwner),
		route.State{Task: createdBy(opsOwner, hollingsworthID, opsLabel)}, led)
	expectFamily(t, d.Families[0], run.VikunjaImplement, route.Duplicate, route.ReasonRunActive)
}

func TestDragCancelStopsTheOpsRun(t *testing.T) {
	led := replay.Ledger{Runs: []route.RunRecord{{Name: "vikunja-ops-5-hollingsworth-opus", Active: true,
		Exists: true, Family: run.VikunjaImplement, TaskID: opsTask}}}
	dragged := createdBy(opsOwner, hollingsworthID, opsLabel, "in progress")
	dragged.Buckets = []string{"To-Do"}
	d := opsRouter().Decide(startedBy(t, task(opsTask, hollingsworthID, nil), opsOwner), route.State{Task: dragged}, led)
	fd, ok := familyOf(d, run.VikunjaDragCancel)
	if !ok || fd.Outcome != route.Act || len(fd.Actions) == 0 || fd.Actions[0].Kind != action.CancelRun ||
		fd.Actions[0].Detail != "vikunja-ops-5-hollingsworth-opus" {
		t.Fatalf("drag of an ops task: %+v (found %v), want its ops run cancelled", fd, ok)
	}
}

// TestOpsFollowUpRunsWithTheUsualProfile pins the legacy comment
// spawners' behaviour: a follow-up on an ops task is an ordinary
// follow-up, on the task's branch, with the project's profile and no
// cluster write.
func TestOpsFollowUpRunsWithTheUsualProfile(t *testing.T) {
	labels := []string{opsLabel, "in progress"}
	d := opsRouter().Decide(comment(t, task(opsTask, hollingsworthID, labels, "claude-opus"), 9, "<p>/gordon go</p>", opsOwner),
		route.State{Task: createdBy(opsOwner, hollingsworthID, labels...)}, replay.Ledger{})
	fd := family(t, d)
	expectFamily(t, fd, run.VikunjaFollowUp, route.Dispatch, "")
	rn := fd.Runs[0]
	if rn.Name != "vikunja-fu-hollingsworth-5-c9" || rn.Branch != "kelos/vikunja-5" || rn.Identity.Ops ||
		rn.Profile.Mode != profile.ModeFollowUp || rn.Profile.Profile != profile.ProfileImageBuild ||
		slices.Contains(rn.Profile.Capabilities, profile.ClusterWrite) {
		t.Fatalf("follow-up of an ops task %+v", rn)
	}
}

func kinds(actions []action.Action) []action.Kind {
	out := []action.Kind{}
	for _, a := range actions {
		out = append(out, a.Kind)
	}
	return out
}
