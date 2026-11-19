package profile

import (
	"errors"
	"slices"
	"testing"
)

// TestDefaultsPreserveLegacyVariants pins each variant to the model and
// effort values-hollingsworth.yaml gives it at LegacyVersion.
func TestDefaultsPreserveLegacyVariants(t *testing.T) {
	want := map[string][3]string{
		"claude-opus": {"claude-code", "claude-opus-5-5[1m]", "medium"}, "claude-sonnet": {"claude-code", "claude-sonnet-5", "medium"},
		"claude-fable": {"claude-code", "claude-fable-5-1", "medium"}, "claude-haiku": {"claude-code", "claude-haiku-5", "medium"},
		"gpt-sol": {"codex", "gpt-5.6-sol", "medium"}, "gpt-astra": {"codex", "gpt-6-astra", "medium"},
		"deepseek": {"opencode", "deepseek/deepseek-v4-pro", ""}, "qwen-coder": {"opencode", "llamacpp/qwen3-coder", ""},
	}
	c := Defaults()
	if c.DefaultVariant != "claude-opus" || len(c.Variants) != len(want) {
		t.Fatalf("default %q, %d variants", c.DefaultVariant, len(c.Variants))
	}
	for name, w := range want {
		got, err := c.Resolve(Request{Project: "vigie", Variant: name, Mode: ModeImplement})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if [3]string{got.Agent, got.Model, got.Effort} != w {
			t.Errorf("%s: %s %s %q, want %v", name, got.Agent, got.Model, got.Effort, w)
		}
	}
}

func TestDefaultsProjects(t *testing.T) {
	c := Defaults()
	const legacyProjects = 20
	if len(c.Projects) != legacyProjects {
		t.Errorf("%d projects, want %d", len(c.Projects), legacyProjects)
	}
	cases := map[string]string{"sperano/cocovm": "cocovm", "sperano/hollingsworth": "hollingsworth", "sperano/BRIQ": "briq"}
	for repo, want := range cases {
		if p, ok := c.PRProject(repo); !ok || p.Workspace != want {
			t.Errorf("PRProject(%s) = %+v", repo, p)
		}
	}
	if p, ok := c.Project(48); !ok || p.Name != "portal" || p.Workspace != "hollingsworth" {
		t.Errorf("Project(48) = %+v", p)
	}
	if _, ok := c.Project(85); ok {
		t.Error("project 85 has no spawner")
	}
}

func TestResolveServiceAccountsByProjectAndMode(t *testing.T) {
	c := Defaults()
	cases := []struct {
		project string
		mode    Mode
		sa      string
		ws      string
		ro      bool
	}{
		{"hollingsworth", ModeImplement, "image-build-submitter", "hollingsworth", false},
		{"hollingsworth", ModeDiscuss, "kelos-discuss", "hollingsworth-readonly", true},
		{"portal", ModeFollowUp, "image-build-submitter", "hollingsworth", false},
		{"puckdb", ModeDiscuss, "kelos-discuss", "puckdb-readonly", true},
		{"vigie", ModeAnswer, "", "vigie", false},
		{"vigie", ModeDiscuss, "", "vigie-readonly", true},
		{"vigie", ModeReview, "pr-review", "vigie", false},
	}
	for _, x := range cases {
		got, err := c.Resolve(Request{Project: x.project, Variant: "claude-opus", Mode: x.mode})
		if err != nil {
			t.Errorf("%s/%s: %v", x.project, x.mode, err)
			continue
		}
		if got.ServiceAccount != x.sa || got.Workspace != x.ws || got.ReadOnly != x.ro || got.ConfigVersion != LegacyVersion {
			t.Errorf("%s/%s: %+v", x.project, x.mode, got)
		}
	}
	puck, _ := c.Resolve(Request{Project: "puckdb", Variant: "claude-opus", Mode: ModeImplement})
	if !slices.Equal(puck.Capabilities, []Capability{GitHubWrite, ImageBuildSubmit, ClusterRead, PostgresTest, PuckDBProdReadOnly}) {
		t.Errorf("puckdb capabilities %v", puck.Capabilities)
	}
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestResolveRefusals(t *testing.T) {
	c := Defaults()
	c.ReviewProfile = ""
	c.Variants["broken"] = Variant{Runner: "missing"}
	c.Profiles["leaky"] = Profile{ReadOnly: true, Capabilities: []Capability{GitHubReadOnly, ImageBuildSubmit}}
	c.Profiles["unclassified"] = Profile{ReadOnly: true, Capabilities: []Capability{GitHubReadOnly, "vault-admin"}}
	c.Projects = append(c.Projects, Project{Name: "odd", Workspace: "odd",
		Profiles: map[Mode]string{ModeImplement: ProfileDefault, ModeDiscuss: ProfileDefault},
		Allowed:  map[Mode][]string{ModeImplement: {"leaky", "ghost", ProfileDiscuss}, ModeAnswer: {"unclassified"}}})
	cases := map[string]struct {
		req  Request
		want string
	}{
		"unknown project":   {Request{Project: "nope", Variant: "claude-opus", Mode: ModeImplement}, CodeUnknownProject},
		"unknown variant":   {Request{Project: "vigie", Variant: "gpt-9", Mode: ModeImplement}, CodeUnknownVariant},
		"unknown mode":      {Request{Project: "vigie", Variant: "claude-opus", Mode: "admin"}, CodeUnknownMode},
		"no runner":         {Request{Project: "vigie", Variant: "broken", Mode: ModeImplement}, CodeIncompleteVariant},
		"bad effort":        {Request{Project: "vigie", Variant: "claude-opus", Mode: ModeImplement, Effort: "turbo"}, CodeUnsupportedEffort},
		"opencode effort":   {Request{Project: "vigie", Variant: "deepseek", Mode: ModeImplement, Effort: "high"}, CodeUnsupportedEffort},
		"codex max":         {Request{Project: "vigie", Variant: "gpt-sol", Mode: ModeImplement, Effort: "max"}, CodeUnsupportedEffort},
		"payload profile":   {Request{Project: "vigie", Variant: "claude-opus", Mode: ModeImplement, Profile: ProfilePuckDB}, CodeProfileNotAllowed},
		"discuss writes":    {Request{Project: "odd", Variant: "claude-opus", Mode: ModeDiscuss}, CodeReadOnlyViolation},
		"write read-only":   {Request{Project: "odd", Variant: "claude-opus", Mode: ModeImplement, Profile: ProfileDiscuss}, CodeReadOnlyViolation},
		"leaky read-only":   {Request{Project: "odd", Variant: "claude-opus", Mode: ModeImplement, Profile: "leaky"}, CodeConflictingProfile},
		"unknown grant":     {Request{Project: "odd", Variant: "claude-opus", Mode: ModeAnswer, Profile: "unclassified"}, CodeUnknownCapability},
		"ghost profile":     {Request{Project: "odd", Variant: "claude-opus", Mode: ModeImplement, Profile: "ghost"}, CodeUnknownProfile},
		"no review profile": {Request{Project: "odd", Variant: "claude-opus", Mode: ModeReview}, CodeNoProfileForMode},
	}
	for name, x := range cases {
		_, err := c.Resolve(x.req)
		if got := code(err); got != x.want {
			t.Errorf("%s: code %q (%v), want %q", name, got, err, x.want)
		}
	}
}

func TestResolveEffortOverrideAndAllowedProfile(t *testing.T) {
	c := Defaults()
	got, err := c.Resolve(Request{Project: "vigie", Variant: "claude-opus", Mode: ModeImplement, Effort: "high"})
	if err != nil || got.Effort != "high" {
		t.Errorf("effort override: %+v %v", got, err)
	}
	for i := range c.Projects {
		if c.Projects[i].Name == "vigie" {
			c.Projects[i].Allowed = map[Mode][]string{ModeImplement: {ProfileImageBuild}}
		}
	}
	got, err = c.Resolve(Request{Project: "vigie", Variant: "claude-opus", Mode: ModeImplement, Profile: ProfileImageBuild})
	if err != nil || got.Profile != ProfileImageBuild || got.ServiceAccount != "image-build-submitter" {
		t.Errorf("allowed profile: %+v %v", got, err)
	}
	if (&Error{Code: "c", Detail: "d"}).Error() != "c: d" {
		t.Error("Error format")
	}
}

func TestCapabilityWrites(t *testing.T) {
	cases := map[Capability][2]bool{
		GitHubWrite: {true, true}, ImageBuildSubmit: {true, true},
		GitHubReadOnly: {false, true}, ClusterRead: {false, true}, PostgresTest: {false, true},
		PuckDBProdReadOnly: {false, true}, "vault-admin": {false, false},
	}
	for c, want := range cases {
		if writing, known := c.Writes(); writing != want[0] || known != want[1] {
			t.Errorf("%s: writes %v known %v, want %v %v", c, writing, known, want[0], want[1])
		}
	}
}

func TestProjectsDoNotShareProfileMaps(t *testing.T) {
	c := Defaults()
	vigie, _ := c.projectByName("vigie")
	vigie.Profiles[ModeImplement] = ProfilePuckDB
	sillage, _ := c.projectByName("sillage")
	if got := sillage.Profiles[ModeImplement]; got != ProfileDefault {
		t.Errorf("sillage implement profile %q after editing vigie's, want %q", got, ProfileDefault)
	}
	again, _ := Defaults().projectByName("vigie")
	if got := again.Profiles[ModeImplement]; got != ProfileDefault {
		t.Errorf("a later Defaults() vigie implement profile %q, want %q", got, ProfileDefault)
	}
}
