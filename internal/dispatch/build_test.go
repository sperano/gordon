package dispatch_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// The fixture: testdata/hollingsworth.json is a dispatch Config made from
// the TaskSpawners Hollingsworth bf18aa5 renders for the gordon project
// (claude-opus, the pr-challenger reviewer) and the puckdb profile, with
// the project, variant, model, effort and repository turned into template
// variables; testdata/legacy/ holds the Task each legacy spawner makes for
// the sample requests below, rendered from the same chart.
const (
	inboxID   = 42
	taskID    = 1624
	gordonID  = 147
	puckdbID  = 6
	mactestID = 137 // solitaire, a mactest project
	commentID = 9001
	prNumber  = 7
	headSHA   = "0123456789abcdef0123456789abcdef01234567"
	comments  = `[{"id":1,"comment":"<p>earlier</p>","author":{"username":"eric"}}]`
	title     = "Ship the thing"
	desc      = "<p>Make it so.</p>"
)

// gordonKeys are the metadata keys only Gordon sets.
var gordonKeys = []string{dispatch.ManagedLabel, dispatch.InboxAnnotation, dispatch.VersionAnnotation}

func fixture(t *testing.T) dispatch.Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hollingsworth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c dispatch.Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("fixture refused: %v", err)
	}
	return c
}

func legacy(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "legacy", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type ledger struct{}

func (ledger) SeenDelivery(string) bool                       { return false }
func (ledger) HandledRequest(string) bool                     { return false }
func (ledger) Run(string) (route.RunRecord, bool)             { return route.RunRecord{}, false }
func (ledger) TaskRuns(int64, []run.Family) []route.RunRecord { return nil }
func (f fakeComments) Comments(context.Context, int64, int) (json.RawMessage, error) {
	return json.RawMessage(f), nil
}

type fakeComments string

// sample is one request, routed by the real router with the legacy
// routing defaults.
type sample struct {
	delivery event.Delivery
	state    route.State
}

func vikunjaTask(project int64, labels ...string) event.Task {
	return event.Task{ID: taskID, ProjectID: project, Title: title, Description: desc,
		Labels: labels, Assignees: []string{"claude-opus"}, Buckets: []string{"Doing"}}
}

func taskUpdated(t event.Task, variant string) sample {
	t.Assignees = []string{variant}
	labels := make([]map[string]string, 0, len(t.Labels))
	for _, l := range t.Labels {
		labels = append(labels, map[string]string{"title": l})
	}
	body, _ := json.Marshal(map[string]any{"event_name": "task.updated", "data": map[string]any{
		"task": map[string]any{"id": t.ID, "project_id": t.ProjectID, "title": t.Title, "description": t.Description,
			"labels": labels, "assignees": []map[string]string{{"username": variant}},
			"buckets": []map[string]string{{"title": "Doing"}}},
		"doer": map[string]string{"username": "eric"}}})
	return sample{event.Delivery{Source: event.SourceVikunja, Body: body}, route.State{Task: &t}}
}

func commentCreated(t event.Task) sample {
	body, _ := json.Marshal(map[string]any{"event_name": "task.comment.created", "data": map[string]any{
		"task":    map[string]any{"id": t.ID, "project_id": t.ProjectID, "title": t.Title, "description": t.Description},
		"comment": map[string]any{"id": commentID, "comment": "<p>/gordon please fix</p>"},
		"doer":    map[string]string{"username": "eric"}}})
	return sample{event.Delivery{Source: event.SourceVikunja, Body: body}, route.State{Task: &t}}
}

func freshPR() event.PullRequest {
	return event.PullRequest{Repository: "sperano/gordon", Number: prNumber, Title: "Add dispatch",
		URL: "https://github.com/sperano/gordon/pull/7", State: event.PullRequestOpen, Author: "gordon-cole-ai[bot]",
		HeadRef: "kelos/vikunja-1624", HeadSHA: headSHA, HeadRepo: "sperano/gordon", BaseRef: "main",
		Files: []string{"README.md"}}
}

func github(kind string, payload map[string]any) event.Delivery {
	return githubIn(freshPR().Repository, kind, payload)
}

func githubIn(repo, kind string, payload map[string]any) event.Delivery {
	payload["repository"] = map[string]string{"full_name": repo}
	body, _ := json.Marshal(payload)
	return event.Delivery{Source: event.SourceGitHub, Body: body, Headers: map[string]string{
		event.GitHubEventHeader: kind, event.GitHubDeliveryHeader: "guid-" + kind}}
}

func prComment() sample {
	pr := freshPR()
	return sample{github("issue_comment", map[string]any{"action": "created", "sender": map[string]string{"login": "sperano"},
		"issue": map[string]any{"number": prNumber, "pull_request": map[string]any{}},
		"comment": map[string]any{"id": 555, "node_id": "IC_555", "body": "/gordon rebase please",
			"html_url": "https://github.com/sperano/gordon/pull/7#issuecomment-555"}}), route.State{PullRequest: &pr}}
}

func prOpened() sample { return prOpenedIn(freshPR().Repository) }

// prOpenedIn is prOpened in the repository repo.
func prOpenedIn(repo string) sample {
	pr := freshPR()
	pr.Repository, pr.HeadRepo = repo, repo
	return sample{githubIn(repo, "pull_request", map[string]any{"action": "opened", "sender": map[string]string{"login": pr.Author},
		"pull_request": map[string]any{"number": prNumber, "title": pr.Title, "html_url": pr.URL, "state": "open",
			"user": map[string]string{"login": pr.Author},
			"head": map[string]any{"ref": pr.HeadRef, "sha": headSHA, "repo": map[string]string{"full_name": pr.HeadRepo}},
			"base": map[string]any{"ref": "main"}}}), route.State{PullRequest: &pr}}
}

// prepared routes and prepares a sample, returning its one run.
func prepared(t *testing.T, cfg dispatch.Config, s sample, family run.Family) (route.Run, route.FamilyDecision) {
	t.Helper()
	routing := profile.Defaults()
	dec := route.Router{Config: routing}.DecideAuthenticated(s.delivery, s.state, ledger{})
	ev, err := event.Parse(s.delivery)
	if err != nil {
		t.Fatal(err)
	}
	p := dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}, Comments: fakeComments(comments)}
	p.Prepare(context.Background(), inboxID, ev, s.state, &dec)
	for _, fd := range dec.Families {
		if fd.Family == family {
			if len(fd.Runs) != 1 {
				t.Fatalf("%s: %s %s with %d runs", family, fd.Outcome, fd.Reason, len(fd.Runs))
			}
			return fd.Runs[0], fd
		}
	}
	t.Fatalf("no %s family in %+v", family, dec)
	return route.Run{}, route.FamilyDecision{}
}

// TestTasksMatchLegacySpawners builds each permission mode's Task and
// compares it with the Task the legacy spawner makes for that request:
// same name, labels, annotations and spec, Gordon's own keys aside.
func TestTasksMatchLegacySpawners(t *testing.T) {
	cfg := fixture(t)
	cases := []struct {
		legacy string
		sample sample
		family run.Family
		mode   profile.Mode
	}{
		{"implement", taskUpdated(vikunjaTask(gordonID), "claude-opus"), run.VikunjaImplement, profile.ModeImplement},
		{"implement-sonnet", taskUpdated(vikunjaTask(gordonID), "claude-sonnet"), run.VikunjaImplement, profile.ModeImplement},
		{"implement-puckdb", taskUpdated(vikunjaTask(puckdbID), "claude-opus"), run.VikunjaImplement, profile.ModeImplement},
		{"discuss", taskUpdated(vikunjaTask(gordonID, "discuss"), "claude-opus"), run.VikunjaDiscuss, profile.ModeDiscuss},
		{"followup", commentCreated(vikunjaTask(gordonID, "in progress")), run.VikunjaFollowUp, profile.ModeFollowUp},
		{"followup-answer", commentCreated(vikunjaTask(gordonID)), run.VikunjaFollowUpAnswer, profile.ModeAnswer},
		{"followup-discuss", commentCreated(vikunjaTask(gordonID, "discuss")), run.VikunjaFollowUpDiscuss, profile.ModeDiscuss},
		{"pr-followup", prComment(), run.PRFollowUp, profile.ModeFollowUp},
		{"pr-review", prOpened(), run.PRReview, profile.ModeReview},
	}
	for _, tc := range cases {
		t.Run(tc.legacy, func(t *testing.T) {
			rn, _ := prepared(t, cfg, tc.sample, tc.family)
			if rn.Task == nil {
				t.Fatal("run has no Task")
			}
			want := legacy(t, tc.legacy)
			var meta struct {
				Metadata kelos.Metadata `json:"metadata"`
			}
			if err := json.Unmarshal(want, &meta); err != nil {
				t.Fatal(err)
			}
			if rn.Task.Metadata.Name != meta.Metadata.Name || rn.Name != meta.Metadata.Name {
				t.Fatalf("name %q (run %q), legacy %q", rn.Task.Metadata.Name, rn.Name, meta.Metadata.Name)
			}
			diff, err := kelos.Diff(*rn.Task, want, gordonKeys...)
			if err != nil {
				t.Fatal(err)
			}
			if len(diff) > 0 {
				t.Fatalf("differs from the legacy Task at %v", diff)
			}
			labels := rn.Task.Metadata.Labels
			if labels[dispatch.ModeLabel] != string(tc.mode) || labels[dispatch.ManagedLabel] != "true" ||
				rn.Task.Metadata.Annotations[dispatch.InboxAnnotation] != "42" ||
				rn.Task.Metadata.Annotations[dispatch.VersionAnnotation] != profile.LegacyVersion+"/"+cfg.Version {
				t.Fatalf("Gordon keys %v %v", labels, rn.Task.Metadata.Annotations)
			}
		})
	}
}

// TestPermissionModes checks what each mode's Task may do: discuss runs
// read-only with the discuss label, the writing modes on the writing
// Workspace, each as its profile's ServiceAccount.
func TestPermissionModes(t *testing.T) {
	cfg := fixture(t)
	cases := []struct {
		sample                 sample
		family                 run.Family
		profile, sa, workspace string
		discuss                bool
		branch                 string
	}{
		{taskUpdated(vikunjaTask(gordonID), "claude-opus"), run.VikunjaImplement, "default", "", "gordon", false, "kelos/vikunja-1624"},
		{taskUpdated(vikunjaTask(gordonID, "discuss"), "claude-opus"), run.VikunjaDiscuss, "discuss-default", "", "gordon-readonly", true, "kelos/vikunja-1624"},
		{commentCreated(vikunjaTask(gordonID, "pr open")), run.VikunjaFollowUp, "default", "", "gordon", false, "kelos/vikunja-1624"},
		{commentCreated(vikunjaTask(gordonID)), run.VikunjaFollowUpAnswer, "default", "", "gordon", false, ""},
		{commentCreated(vikunjaTask(gordonID, "discuss")), run.VikunjaFollowUpDiscuss, "discuss-default", "", "gordon-readonly", true, ""},
		{prComment(), run.PRFollowUp, "default", "", "gordon", false, "kelos/vikunja-1624"},
		{prOpened(), run.PRReview, "review", "pr-review", "gordon", false, ""},
		{taskUpdated(vikunjaTask(puckdbID), "claude-opus"), run.VikunjaImplement, "puckdb", "image-build-submitter", "puckdb", false, "kelos/vikunja-1624"},
		{taskUpdated(vikunjaTask(puckdbID, "discuss"), "claude-opus"), run.VikunjaDiscuss, "puckdb-discuss", "kelos-discuss", "puckdb-readonly", true, "kelos/vikunja-1624"},
		{prOpenedIn("sperano/puckdb"), run.PRReview, "puckdb-review", "pr-review", "puckdb", false, ""},
		{taskUpdated(vikunjaTask(mactestID), "claude-opus"), run.VikunjaImplement, "mactest", "", "solitaire", false, "kelos/vikunja-1624"},
		{taskUpdated(vikunjaTask(mactestID, "discuss"), "claude-opus"), run.VikunjaDiscuss, "mactest-discuss", "", "solitaire-readonly", true, "kelos/vikunja-1624"},
	}
	for _, tc := range cases {
		t.Run(string(tc.family)+"/"+tc.profile, func(t *testing.T) {
			rn, _ := prepared(t, cfg, tc.sample, tc.family)
			task := rn.Task
			sa := ""
			if task.Spec.Worker.PodOverrides != nil {
				sa = task.Spec.Worker.PodOverrides.ServiceAccountName
			}
			_, discuss := task.Metadata.Labels[dispatch.DiscussLabel]
			if task.Metadata.Labels[dispatch.ProfileLabel] != tc.profile || sa != tc.sa ||
				task.WorkspaceName() != tc.workspace || discuss != tc.discuss || task.Spec.Branch != tc.branch {
				t.Fatalf("profile %q sa %q workspace %q discuss %v branch %q", task.Metadata.Labels[dispatch.ProfileLabel],
					sa, task.WorkspaceName(), discuss, task.Spec.Branch)
			}
		})
	}
}

// TestNewVariantIsConfiguration: a variant the routing configuration
// gains (a new model and effort) dispatches with one more Variants entry
// and no new family template.
func TestNewVariantIsConfiguration(t *testing.T) {
	cfg := fixture(t)
	routing := profile.Defaults()
	routing.Variants["claude-next"] = profile.Variant{Runner: "claude-code", Model: "claude-next-6", Effort: "high"}
	cfg.Variants["claude-next"] = dispatch.VariantSpec{VikunjaAgentConfig: "vikunja-claude-next"}
	// Its Vikunja and Authentik identities, as the admission table lists
	// every variant's.
	cfg.CommonSecrets = append(cfg.CommonSecrets, "vikunja-agent-claude-next", "authentik-agent-claude-next")
	s := taskUpdated(vikunjaTask(gordonID), "claude-next")
	dec := route.Router{Config: routing}.DecideAuthenticated(s.delivery, s.state, ledger{})
	ev, _ := event.Parse(s.delivery)
	dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}}.Prepare(context.Background(), inboxID, ev, s.state, &dec)
	if fd := dec.Families[0]; len(fd.Runs) != 1 {
		t.Fatalf("%s %s, want one run", fd.Outcome, fd.Reason)
	}
	rn := dec.Families[0].Runs[0]
	w := rn.Task.Spec.Worker
	refs := make([]string, len(w.AgentConfigRefs))
	for i, r := range w.AgentConfigRefs {
		refs[i] = r.Name
	}
	if rn.Name != "vikunja-gordon-claude-next-1624" || w.Model != "claude-next-6" || w.Effort != "high" ||
		!slices.Equal(refs, []string{"base", "vikunja-claude-next"}) || !containsEnv(w.PodOverrides.Env, "AGENT_MODEL", "claude-next-6") ||
		rn.Task.Metadata.Labels[dispatch.VariantLabel] != "claude-next" {
		t.Fatalf("run %s worker %+v refs %v", rn.Name, w, refs)
	}
}

func containsEnv(env []json.RawMessage, name, value string) bool {
	for _, raw := range env {
		var e struct{ Name, Value string }
		if json.Unmarshal(raw, &e) == nil && e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}

func TestFreshTaskDescriptionIsAuthoritative(t *testing.T) {
	s := taskUpdated(vikunjaTask(gordonID), "claude-opus")
	fresh := *s.state.Task
	fresh.Description = "" // the owner cleared it after the webhook
	s.state.Task = &fresh
	rn, _ := prepared(t, fixture(t), s, run.VikunjaImplement)
	if strings.Contains(rn.Task.Spec.Prompt, "Make it so") {
		t.Fatalf("prompt quotes the delivery's stale description: %s", rn.Task.Spec.Prompt)
	}
	if !strings.Contains(rn.Task.Spec.Prompt, title) {
		t.Fatalf("prompt lacks the fresh title: %s", rn.Task.Spec.Prompt)
	}
}

func envNamed(name string) json.RawMessage {
	return json.RawMessage(`{"name":"` + name + `","value":"1"}`)
}

// TestPodPiecesComposeInOrder: env is the family's, the Workspace's, the
// profile's then the variant's; a later piece's resources or affinity
// wins. A discuss run takes its project's Workspace pieces, keyed by the
// writing Workspace, though its Task names the read-only copy.
func TestPodPiecesComposeInOrder(t *testing.T) {
	cfg := fixture(t)
	f := cfg.Families[run.VikunjaDiscuss]
	f.Pod.Env = append(f.Pod.Env, envNamed("FROM_FAMILY"))
	f.Pod.Resources, f.Pod.Affinity = json.RawMessage(`{"limits":{"cpu":"1"}}`), json.RawMessage(`{"a":"family"}`)
	cfg.Families[run.VikunjaDiscuss] = f
	cfg.Workspaces = map[string]dispatch.Pod{"gordon": {Env: []json.RawMessage{envNamed("FROM_WORKSPACE")}}}
	p := cfg.Profiles["discuss-default"]
	p.Pod.Env = append(p.Pod.Env, envNamed("FROM_PROFILE"))
	p.Pod.Resources = json.RawMessage(`{"limits":{"cpu":"2"}}`)
	cfg.Profiles["discuss-default"] = p
	v := cfg.Variants["claude-opus"]
	v.Pod.Env = append(v.Pod.Env, envNamed("FROM_VARIANT"))
	v.Pod.Affinity = json.RawMessage(`{"a":"variant"}`)
	cfg.Variants["claude-opus"] = v
	rn, _ := prepared(t, cfg, taskUpdated(vikunjaTask(gordonID, "discuss"), "claude-opus"), run.VikunjaDiscuss)
	po := rn.Task.Spec.Worker.PodOverrides
	var order []string
	for _, e := range po.Env {
		var named struct{ Name string }
		if err := json.Unmarshal(e, &named); err == nil && strings.HasPrefix(named.Name, "FROM_") {
			order = append(order, named.Name)
		}
	}
	if want := []string{"FROM_FAMILY", "FROM_WORKSPACE", "FROM_PROFILE", "FROM_VARIANT"}; !slices.Equal(order, want) {
		t.Fatalf("env pieces %v, want %v", order, want)
	}
	if string(po.Resources) != `{"limits":{"cpu":"2"}}` || string(po.Affinity) != `{"a":"variant"}` {
		t.Fatalf("resources %s affinity %s, want the profile's and the variant's", po.Resources, po.Affinity)
	}
	if rn.Task.WorkspaceName() != "gordon-readonly" {
		t.Fatalf("workspace %s, want the read-only copy", rn.Task.WorkspaceName())
	}
}

// TestLastInitContainersFollowEveryPiece: a family's last init containers
// start after every piece's extra ones, a later piece's included.
func TestLastInitContainersFollowEveryPiece(t *testing.T) {
	cfg := fixture(t)
	f := cfg.Families[run.VikunjaDiscuss]
	f.Pod.ExtraInitContainers = append(f.Pod.ExtraInitContainers, json.RawMessage(`{"name":"from-family"}`))
	f.Pod.LastInitContainers = append(f.Pod.LastInitContainers, json.RawMessage(`{"name":"from-family-last"}`))
	cfg.Families[run.VikunjaDiscuss] = f
	p := cfg.Profiles["discuss-default"]
	p.Pod.ExtraInitContainers = append(p.Pod.ExtraInitContainers, json.RawMessage(`{"name":"from-profile"}`))
	cfg.Profiles["discuss-default"] = p
	rn, _ := prepared(t, cfg, taskUpdated(vikunjaTask(gordonID, "discuss"), "claude-opus"), run.VikunjaDiscuss)
	var order []string
	for _, c := range rn.Task.Spec.Worker.PodOverrides.ExtraInitContainers {
		var named struct{ Name string }
		if err := json.Unmarshal(c, &named); err == nil && strings.HasPrefix(named.Name, "from-") {
			order = append(order, named.Name)
		}
	}
	if want := []string{"from-family", "from-profile", "from-family-last"}; !slices.Equal(order, want) {
		t.Fatalf("init containers %v, want %v", order, want)
	}
}
