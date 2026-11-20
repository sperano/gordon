package profile

import (
	"errors"
	"maps"
	"slices"
	"testing"
)

// Legacy runner images and credentials (values-hollingsworth.yaml at
// LegacyVersion), written out so the test does not read the defaults'
// own constants.
const (
	legacyClaudeImage   = "ericsperano/kelos-claude-code:0.64.0-20261120-031945-6f37b61"
	legacyCodexImage    = "ericsperano/kelos-codex:0.64.0-20261120-031945-6f37b61"
	legacyOpenCodeImage = "ericsperano/kelos-opencode:0.64.0-20261120-031945-6f37b61"
)

// legacyRunner is what a variant resolves to: agent, model, effort, and
// the image and credentials that select its container.
type legacyRunner struct {
	agent, model, effort, image, credType, credSecret string
}

// TestDefaultsPreserveLegacyVariants pins each variant to the agent,
// model, effort, image and credentials values-hollingsworth.yaml gives it
// at LegacyVersion.
func TestDefaultsPreserveLegacyVariants(t *testing.T) {
	claude := func(model string) legacyRunner {
		return legacyRunner{"claude-code", model, "medium", legacyClaudeImage, "oauth", "claude-oauth"}
	}
	codex := func(model string) legacyRunner {
		return legacyRunner{"codex", model, "medium", legacyCodexImage, "oauth", "codex-oauth"}
	}
	opencode := func(model string) legacyRunner {
		return legacyRunner{"opencode", model, "", legacyOpenCodeImage, "none", ""}
	}
	want := map[string]legacyRunner{
		"claude-opus": claude("claude-opus-5-5[1m]"), "claude-sonnet": claude("claude-sonnet-5"),
		"claude-fable": claude("claude-fable-5-1"), "claude-haiku": claude("claude-haiku-5"),
		"gpt-sol": codex("gpt-5.6-sol"), "gpt-astra": codex("gpt-6-astra"),
		"deepseek": opencode("deepseek/deepseek-v4-pro"), "qwen-coder": opencode("llamacpp/qwen3-coder"),
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
		g := legacyRunner{got.Agent, got.Model, got.Effort, got.Image, got.CredentialType, got.CredentialSecret}
		if g != w {
			t.Errorf("%s: %+v, want %+v", name, g, w)
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
	if p, ok := c.Project(48); !ok || p.Name != "portal" || p.Title != "Portal" || p.Workspace != "hollingsworth" {
		t.Errorf("Project(48) = %+v", p)
	}
	if p, ok := c.Project(141); !ok || p.Name != "cocovm-3d" || p.Title != "CoCoVM 3D" {
		t.Errorf("Project(141) = %+v", p)
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
		{"puckdb", ModeReview, "pr-review", "puckdb", false},
		{"solitaire", ModeImplement, "", "solitaire", false},
		{"solitaire", ModeDiscuss, "", "solitaire-readonly", true},
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
	profiles := map[string]string{}
	for _, x := range []struct {
		project string
		mode    Mode
	}{{"puckdb", ModeReview}, {"solitaire", ModeImplement}, {"solitaire", ModeDiscuss}, {"vigie", ModeFollowUp}} {
		got, err := c.Resolve(Request{Project: x.project, Variant: "claude-opus", Mode: x.mode})
		if err != nil {
			t.Fatalf("%s/%s: %v", x.project, x.mode, err)
		}
		profiles[x.project+"/"+string(x.mode)] = got.Profile
	}
	wantProfiles := map[string]string{"puckdb/review": ProfilePuckDBReview, "solitaire/implement": ProfileMactest,
		"solitaire/discuss": ProfileMactestDiscuss, "vigie/followup": ProfileDefault}
	if !maps.Equal(profiles, wantProfiles) {
		t.Errorf("profiles %v, want %v", profiles, wantProfiles)
	}
}

// TestResolveCapabilities pins the capability sets of Hollingsworth's
// Gordon 02 admission table.
func TestResolveCapabilities(t *testing.T) {
	c := Defaults()
	cases := []struct {
		project string
		mode    Mode
		want    []Capability
	}{
		{"puckdb", ModeImplement, []Capability{GitHubWrite, GitHubReadOnly, ImageBuildSubmit, ClusterRead, PostgresTest, PuckDBProdReadOnly}},
		{"puckdb", ModeReview, []Capability{GitHubWrite, GitHubReadOnly, PostgresTest, PuckDBProdReadOnly}},
		{"vigie", ModeImplement, []Capability{GitHubWrite, GitHubReadOnly}},
		{"vigie", ModeReview, []Capability{GitHubWrite, GitHubReadOnly}},
		{"solitaire", ModeImplement, []Capability{GitHubWrite, GitHubReadOnly, Mactest}},
		{"solitaire", ModeDiscuss, []Capability{GitHubReadOnly, Mactest}},
	}
	for _, x := range cases {
		got, err := c.Resolve(Request{Project: x.project, Variant: "claude-opus", Mode: x.mode})
		if err != nil || !slices.Equal(got.Capabilities, x.want) {
			t.Errorf("%s/%s capabilities %v (%v), want %v", x.project, x.mode, got.Capabilities, err, x.want)
		}
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
	c.Profiles["no-write"] = Profile{Capabilities: []Capability{GitHubReadOnly, ClusterRead}}
	c.Projects = append(c.Projects, Project{Name: "odd", Workspace: "odd",
		Profiles: map[Mode]string{ModeImplement: ProfileDefault, ModeDiscuss: ProfileDefault},
		Allowed:  map[Mode][]string{ModeImplement: {"leaky", "ghost", ProfileDiscuss, "no-write"}, ModeAnswer: {"unclassified"}}})
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
		"write no grant":    {Request{Project: "odd", Variant: "claude-opus", Mode: ModeImplement, Profile: "no-write"}, CodeReadOnlyViolation},
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
		PuckDBProdReadOnly: {false, true}, Mactest: {false, true}, "vault-admin": {false, false},
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
