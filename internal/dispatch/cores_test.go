package dispatch_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

func TestCoreLabelsOverrideOnlyFreshCPURequest(t *testing.T) {
	cfg := fixture(t)
	cfg.CoreLabels = map[string]int{"cores:2": 2, "cores:8": 8, "kelos:cores:6": 6}

	vikunja := taskUpdated(vikunjaTask(gordonID, "cores:2"), "claude-opus")
	fresh := *vikunja.state.Task
	fresh.Labels = []string{"cores:8"}
	vikunja.state.Task = &fresh
	vikunjaRun, _ := prepared(t, cfg, vikunja, run.VikunjaImplement)
	assertResources(t, vikunjaRun.Task.Spec.Worker.PodOverrides.Resources, "8", "4Gi", "8Gi")

	github := prComment()
	pr := *github.state.PullRequest
	pr.Labels = append(pr.Labels, "kelos:cores:6")
	github.state.PullRequest = &pr
	githubRun, _ := prepared(t, cfg, github, run.PRFollowUp)
	assertResources(t, githubRun.Task.Spec.Worker.PodOverrides.Resources, "6", "4Gi", "8Gi")
}

func TestCoreLabelsLeaveDefaultsAndEquivalentLabelsAlone(t *testing.T) {
	plain := fixture(t)
	configured := fixture(t)
	configured.CoreLabels = map[string]int{"cores:4": 4, "kelos:cores:4": 4}

	s := taskUpdated(vikunjaTask(gordonID), "claude-opus")
	defaultRun, _ := prepared(t, plain, s, run.VikunjaImplement)
	configuredRun, _ := prepared(t, configured, s, run.VikunjaImplement)
	if got, want := string(configuredRun.Task.Spec.Worker.PodOverrides.Resources), string(defaultRun.Task.Spec.Worker.PodOverrides.Resources); got != want {
		t.Fatalf("resources without a configured label = %s, want unchanged %s", got, want)
	}

	s = taskUpdated(vikunjaTask(gordonID, "cores:4", "kelos:cores:4"), "claude-opus")
	equivalentRun, _ := prepared(t, configured, s, run.VikunjaImplement)
	assertResources(t, equivalentRun.Task.Spec.Worker.PodOverrides.Resources, "4", "4Gi", "8Gi")
}

func TestConflictingCoreLabelsRefuseTask(t *testing.T) {
	cfg := fixture(t)
	cfg.CoreLabels = map[string]int{"cores:2": 2, "cores:8": 8}
	fd := preparedCoreDecision(t, cfg, taskUpdated(vikunjaTask(gordonID, "cores:2", "cores:8"), "claude-opus"))
	if fd.Outcome != route.Reject || fd.Reason != "task-spec-"+dispatch.CodeCoreLabelConflict || len(fd.Runs) != 0 || len(fd.Actions) != 0 {
		t.Fatalf("decision = %s %s, %d runs, %d actions", fd.Outcome, fd.Reason, len(fd.Runs), len(fd.Actions))
	}
}

func TestCoreLabelRefusesNonObjectRequests(t *testing.T) {
	cfg := fixture(t)
	cfg.CoreLabels = map[string]int{"cores:2": 2}
	f := cfg.Families[run.VikunjaImplement]
	f.Pod.Resources = json.RawMessage(`{"requests":"all"}`)
	cfg.Families[run.VikunjaImplement] = f
	fd := preparedCoreDecision(t, cfg, taskUpdated(vikunjaTask(gordonID, "cores:2"), "claude-opus"))
	if fd.Outcome != route.Reject || fd.Reason != "task-spec-"+dispatch.CodePod {
		t.Fatalf("decision = %s %s, want pod rejection", fd.Outcome, fd.Reason)
	}
}

func preparedCoreDecision(t *testing.T, cfg dispatch.Config, s sample) route.FamilyDecision {
	t.Helper()
	routing := profile.Defaults()
	dec := route.Router{Config: routing}.DecideAuthenticated(s.delivery, s.state, ledger{})
	ev, err := event.Parse(s.delivery)
	if err != nil {
		t.Fatal(err)
	}
	dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}}.
		Prepare(context.Background(), inboxID, ev, s.state, &dec)
	return dec.Families[0]
}

func assertResources(t *testing.T, raw json.RawMessage, cpu, memoryRequest, memoryLimit string) {
	t.Helper()
	var got struct {
		Requests map[string]string `json:"requests"`
		Limits   map[string]string `json:"limits"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Requests["cpu"] != cpu || got.Requests["memory"] != memoryRequest || got.Limits["memory"] != memoryLimit {
		t.Fatalf("resources = %s, want cpu %s, memory request %s, memory limit %s", raw, cpu, memoryRequest, memoryLimit)
	}
}
