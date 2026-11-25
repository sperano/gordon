package dispatch_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// The ops mode, on for the fixture's gordon project.
const (
	opsLabel          = "ops"
	opsProfile        = "cluster-operator"
	opsServiceAccount = "kelos-cluster-operator"
	opsParagraph      = "The task carries the `ops` label: this Task may change the live cluster."
	opsTaskPattern    = "vikunja-ops-([0-9]+)-{{.project}}-{{.short_variant}}(?:-retry[0-9]+)?"
	implTaskPattern   = "vikunja-([0-9]+)-{{.project}}-{{.short_variant}}(?:-retry[0-9]+)?"
)

func opsRouting() profile.Config {
	c := profile.Defaults()
	c.OpsProfile = opsProfile
	c.Profiles[opsProfile] = profile.Profile{ServiceAccount: opsServiceAccount,
		Capabilities: []profile.Capability{profile.GitHubWrite, profile.GitHubReadOnly, profile.ClusterRead, profile.ClusterWrite}}
	c.Policy.OpsLabel, c.Policy.OpsOwner, c.Policy.OpsProjects = opsLabel, "eric", []string{"gordon"}
	return c
}

// opsFixture is the fixture with the ops family (the implementation
// family's template with the ops paragraph and the ops Task pattern) and
// the ops profile's Secrets.
func opsFixture(t *testing.T) dispatch.Config {
	t.Helper()
	cfg := fixture(t)
	impl := cfg.Families[run.VikunjaImplement]
	ops := impl
	ops.Prompt = impl.Prompt + "\n" + opsParagraph + "\n"
	ops.Pod.LastInitContainers = nil
	for _, c := range impl.Pod.LastInitContainers {
		ops.Pod.LastInitContainers = append(ops.Pod.LastInitContainers,
			[]byte(strings.ReplaceAll(string(c), implTaskPattern, opsTaskPattern)))
	}
	cfg.Families[dispatch.OpsFamily] = ops
	cfg.Profiles[opsProfile] = dispatch.ProfileSpec{Secrets: []string{"github-token", "kelos-github-readonly"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ops fixture refused: %v", err)
	}
	return cfg
}

func opsSample() sample {
	tk := vikunjaTask(gordonID, opsLabel)
	tk.CreatedBy = "eric"
	return taskUpdated(tk, "claude-opus")
}

func TestOpsTask(t *testing.T) {
	cfg := opsFixture(t)
	rn, fd := preparedWith(t, opsRouting(), cfg, opsSample(), run.VikunjaImplement)
	if fd.Outcome != route.Dispatch || rn.Task == nil {
		t.Fatalf("ops run %s %s, task %v", fd.Outcome, fd.Reason, rn.Task)
	}
	task := rn.Task
	labels := task.Metadata.Labels
	if task.Metadata.Name != "vikunja-ops-1624-gordon-opus" || task.Spec.Branch != "kelos/vikunja-1624" ||
		task.WorkspaceName() != "gordon" || task.Spec.Worker.PodOverrides.ServiceAccountName != opsServiceAccount ||
		labels[dispatch.ProfileLabel] != opsProfile || labels[dispatch.ModeLabel] != string(profile.ModeOps) {
		t.Fatalf("ops Task %s branch %q workspace %q sa %q labels %v", task.Metadata.Name, task.Spec.Branch,
			task.WorkspaceName(), task.Spec.Worker.PodOverrides.ServiceAccountName, labels)
	}
	if _, discuss := labels[dispatch.DiscussLabel]; discuss {
		t.Fatal("an ops Task carries the discuss label")
	}
	if !strings.HasSuffix(task.Spec.Prompt, opsParagraph+"\n") {
		t.Fatalf("ops prompt lacks the ops paragraph:\n%s", task.Spec.Prompt)
	}
	if got := prOpenerTaskID(t, task); got != strconv.Itoa(taskID) {
		t.Fatalf("the PR opener reads task %q from the ops Task name %s", got, task.Metadata.Name)
	}
	// The implementation Task of the same configuration is unchanged.
	impl, _ := preparedWith(t, opsRouting(), cfg, taskUpdated(vikunjaTask(gordonID), "claude-opus"), run.VikunjaImplement)
	if strings.Contains(impl.Task.Spec.Prompt, opsParagraph) || impl.Task.Metadata.Labels[dispatch.ProfileLabel] != "default" {
		t.Fatalf("implementation Task took the ops template: %v", impl.Task.Metadata.Labels)
	}
	if got := prOpenerTaskID(t, impl.Task); got != strconv.Itoa(taskID) {
		t.Fatalf("the PR opener reads task %q from the implementation Task name %s", got, impl.Task.Metadata.Name)
	}
}

// TestOpsTaskNeedsItsTemplate: without the ops family's entry an ops run
// is refused, never built from the implementation template.
func TestOpsTaskNeedsItsTemplate(t *testing.T) {
	cfg := fixture(t)
	cfg.Profiles[opsProfile] = dispatch.ProfileSpec{Secrets: []string{"github-token", "kelos-github-readonly"}}
	s := opsSample()
	routing := opsRouting()
	dec := route.Router{Config: routing}.DecideAuthenticated(s.delivery, s.state, ledger{})
	ev, err := event.Parse(s.delivery)
	if err != nil {
		t.Fatal(err)
	}
	dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}}.
		Prepare(context.Background(), inboxID, ev, s.state, &dec)
	if fd := dec.Families[0]; fd.Family != run.VikunjaImplement || fd.Outcome != route.Reject || fd.Reason != dispatch.ReasonPrefix+dispatch.CodeNoTemplate {
		t.Fatalf("ops run without its template: %s %s", fd.Outcome, fd.Reason)
	}
}

// prOpenerTaskID is the task id the pr-opener init container's
// PR_OPENER_TASK_PATTERN reads from the Task's own name, "" when the
// pattern does not match the whole name.
func prOpenerTaskID(t *testing.T, task *kelos.Task) string {
	t.Helper()
	b, err := json.Marshal(task.Spec.Worker.PodOverrides.ExtraInitContainers)
	if err != nil {
		t.Fatal(err)
	}
	var inits []struct {
		Name string `json:"name"`
		Env  []struct{ Name, Value string }
	}
	if err := json.Unmarshal(b, &inits); err != nil {
		t.Fatal(err)
	}
	for _, c := range inits {
		for _, e := range c.Env {
			if c.Name == "pr-opener" && e.Name == "PR_OPENER_TASK_PATTERN" {
				m := regexp.MustCompile("^(?:" + e.Value + ")$").FindStringSubmatch(task.Metadata.Name)
				if m == nil {
					return ""
				}
				return m[1]
			}
		}
	}
	t.Fatalf("Task %s has no pr-opener PR_OPENER_TASK_PATTERN", task.Metadata.Name)
	return ""
}
