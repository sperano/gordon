package profile

import (
	"slices"
	"strings"
	"testing"
)

// The ops mode of values-hollingsworth.yaml (clusterOperator and
// executionProfiles.system.ops), written out.
const (
	opsLabel          = "ops"
	opsOwner          = "eric"
	opsProfile        = "cluster-operator"
	opsServiceAccount = "kelos-cluster-operator"
	opsProject        = "hollingsworth"
)

// opsConfig is Defaults with the ops mode on for hollingsworth.
func opsConfig() Config {
	c := Defaults()
	c.OpsProfile = opsProfile
	c.Profiles[opsProfile] = Profile{ServiceAccount: opsServiceAccount,
		Capabilities: []Capability{GitHubWrite, GitHubReadOnly, ClusterRead, ClusterWrite}}
	c.Policy.OpsLabel, c.Policy.OpsOwner, c.Policy.OpsProjects = opsLabel, opsOwner, []string{opsProject}
	return c
}

func TestResolveOps(t *testing.T) {
	got, err := opsConfig().Resolve(Request{Project: opsProject, Variant: "claude-opus", Mode: ModeOps})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeOps || got.Profile != opsProfile || got.ServiceAccount != opsServiceAccount ||
		got.Workspace != opsProject || got.ReadOnly || !slices.Contains(got.Capabilities, ClusterWrite) {
		t.Fatalf("ops resolved %+v", got)
	}
}

func TestResolveOpsRefusals(t *testing.T) {
	c := opsConfig()
	c.Profiles["rogue"] = Profile{Capabilities: []Capability{GitHubWrite, ClusterWrite}}
	for i := range c.Projects {
		if c.Projects[i].Name == opsProject {
			c.Projects[i].Profiles[ModeOps] = ProfileDefault
			c.Projects[i].Allowed = map[Mode][]string{ModeOps: {ProfileDefault}, ModeImplement: {opsProfile, "rogue"}}
		}
	}
	off := Defaults()
	cases := map[string]struct {
		c    Config
		req  Request
		want string
	}{
		"ops off":                   {off, Request{Project: opsProject, Variant: "claude-opus", Mode: ModeOps}, CodeOpsOff},
		"project uncovered":         {c, Request{Project: "vigie", Variant: "claude-opus", Mode: ModeOps}, CodeOpsOff},
		"ops override":              {c, Request{Project: opsProject, Variant: "claude-opus", Mode: ModeOps, Profile: ProfileDefault}, CodeProfileNotAllowed},
		"ops profile for implement": {c, Request{Project: opsProject, Variant: "claude-opus", Mode: ModeImplement, Profile: opsProfile}, CodeOpsOnly},
		"cluster write elsewhere":   {c, Request{Project: opsProject, Variant: "claude-opus", Mode: ModeImplement, Profile: "rogue"}, CodeOpsOnly},
	}
	for name, x := range cases {
		_, err := x.c.Resolve(x.req)
		if got := code(err); got != x.want {
			t.Errorf("%s: code %q (%v), want %q", name, got, err, x.want)
		}
	}
	// The project's own ops entry is ignored: the ops profile is the
	// configuration's.
	if got, err := c.Resolve(Request{Project: opsProject, Variant: "claude-opus", Mode: ModeOps}); err != nil || got.Profile != opsProfile {
		t.Fatalf("ops with a project entry: %+v %v", got, err)
	}
}

func TestOpsCovers(t *testing.T) {
	p := opsConfig().Policy
	if !p.OpsCovers(opsProject) || p.OpsCovers("vigie") {
		t.Fatal("OpsCovers by project")
	}
	p.OpsLabel = ""
	if p.OpsCovers(opsProject) {
		t.Fatal("an empty label covers a project")
	}
}

func TestValidateOps(t *testing.T) {
	if err := Defaults().ValidateOps(); err != nil {
		t.Fatalf("defaults (ops off): %v", err)
	}
	if err := opsConfig().ValidateOps(); err != nil {
		t.Fatalf("ops on: %v", err)
	}
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"no owner":          {func(c *Config) { c.Policy.OpsOwner = "" }, "ops_owner is required"},
		"discuss label":     {func(c *Config) { c.Policy.OpsLabel = c.Policy.DiscussLabel }, `"discuss" is already policy discuss_label`},
		"in progress":       {func(c *Config) { c.Policy.OpsLabel = c.Policy.InProgressLabel }, "in_progress_label"},
		"unknown project":   {func(c *Config) { c.Policy.OpsProjects = []string{"nope"} }, `"nope" is not a configured project`},
		"no profile":        {func(c *Config) { c.OpsProfile = "" }, "ops_profile is required"},
		"unknown profile":   {func(c *Config) { c.OpsProfile = "ghost" }, `ops_profile "ghost" is not a profile`},
		"read-only profile": {func(c *Config) { c.OpsProfile = ProfileDiscuss }, "is read-only"},
		"no account": {func(c *Config) {
			c.Profiles[opsProfile] = Profile{Capabilities: []Capability{GitHubWrite, ClusterWrite}}
		}, "names no service_account"},
		"shared account": {func(c *Config) {
			c.Profiles["twin"] = Profile{ServiceAccount: opsServiceAccount, Capabilities: []Capability{GitHubWrite}}
		}, `profile "twin" shares`},
		"project names it": {func(c *Config) { c.Projects[0].Profiles[ModeImplement] = opsProfile }, "implement profile"},
		"project allows it": {func(c *Config) {
			c.Projects[0].Allowed = map[Mode][]string{ModeFollowUp: {opsProfile}}
		}, "followup allows the ops profile"},
		"cluster write elsewhere": {func(c *Config) {
			c.Profiles["rogue"] = Profile{Capabilities: []Capability{GitHubWrite, ClusterWrite}}
		}, `profile "rogue" grants cluster-write`},
		"cluster write with ops off": {func(c *Config) { c.Policy.OpsLabel, c.OpsProfile = "", "" }, "grants cluster-write"},
	}
	for name, x := range cases {
		c := opsConfig()
		x.edit(&c)
		err := c.ValidateOps()
		if err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("%s: %v, want %q", name, err, x.want)
		}
	}
}

// TestNamespaceReadResolves: a namespaced reader's profile (writing or
// read-only) resolves.
func TestNamespaceReadResolves(t *testing.T) {
	c := Defaults()
	c.Profiles["reader"] = Profile{ServiceAccount: "kelos-read-vigie",
		Capabilities: []Capability{GitHubWrite, GitHubReadOnly, NamespaceRead}}
	c.Profiles["reader-discuss"] = Profile{ServiceAccount: "kelos-read-vigie", ReadOnly: true,
		Capabilities: []Capability{GitHubReadOnly, NamespaceRead}}
	for i := range c.Projects {
		if c.Projects[i].Name == "vigie" {
			c.Projects[i].Profiles = map[Mode]string{ModeImplement: "reader", ModeDiscuss: "reader-discuss"}
		}
	}
	for mode, want := range map[Mode]string{ModeImplement: "reader", ModeDiscuss: "reader-discuss", ModeFollowUp: "reader"} {
		got, err := c.Resolve(Request{Project: "vigie", Variant: "claude-opus", Mode: mode})
		if err != nil || got.Profile != want || !slices.Contains(got.Capabilities, NamespaceRead) {
			t.Errorf("%s: %+v, %v", mode, got, err)
		}
	}
}
