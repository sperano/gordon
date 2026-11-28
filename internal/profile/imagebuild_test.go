package profile

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// imageBuilds is a valid catalogue on the defaults' runner profile.
func imageBuilds() Config {
	c := Defaults()
	c.ImageBuilds = ImageBuilds{Profile: ProfileImageBuildRunner, Workspace: "merge-close-stub",
		Runner: ImageBuildRunner{Type: "opencode", Image: "builder:1", CredentialType: "none"},
		Images: []ImageBuild{
			{Name: "lucarne", Repository: "sperano/lucarne", Environment: "hollingsworth", Branches: []string{"main"}},
			{Name: "kelos-agent", Repository: "sperano/hollingsworth", Environment: "hollingsworth", Branches: []string{"main"},
				FilePatterns: &FilePatterns{Include: []string{"docker/kelos-agent/**"}, Exclude: []string{"**/*.md"}}},
		}}
	return c
}

func TestDefaultsBuildNoImageOnPush(t *testing.T) {
	c := Defaults()
	if len(c.ImageBuilds.Images) != 0 || c.ImageBuilds.OnRepository("sperano/lucarne") {
		t.Fatalf("defaults catalogue %+v, want empty", c.ImageBuilds)
	}
	p, ok := c.Profiles[ProfileImageBuildRunner]
	if !ok || p.ServiceAccount != "image-build-runner" || p.ReadOnly || !slices.Equal(p.Capabilities, []Capability{ImageBuildRun}) {
		t.Fatalf("image-build-runner profile %+v %v", p, ok)
	}
	if err := c.ValidateImageBuilds(); err != nil {
		t.Fatalf("empty catalogue refused: %v", err)
	}
}

func TestResolveImageBuild(t *testing.T) {
	c := imageBuilds()
	got, err := c.ResolveImageBuild()
	if err != nil {
		t.Fatal(err)
	}
	want := Resolved{ConfigVersion: LegacyVersion, Agent: "opencode", Image: "builder:1", CredentialType: "none",
		Mode: ModeModelFree, Profile: ProfileImageBuildRunner, ServiceAccount: "image-build-runner",
		Workspace: "merge-close-stub", Capabilities: []Capability{ImageBuildRun}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved %+v\nwant %+v", got, want)
	}
}

func TestResolveImageBuildRefusals(t *testing.T) {
	c := imageBuilds()
	c.ImageBuilds.Profile = "ghost"
	if _, err := c.ResolveImageBuild(); code(err) != CodeUnknownProfile {
		t.Errorf("unknown profile: %v", err)
	}
	c.ImageBuilds.Profile = ProfileDiscuss
	if _, err := c.ResolveImageBuild(); code(err) != CodeReadOnlyViolation {
		t.Errorf("read-only profile: %v", err)
	}
}

func TestValidateImageBuilds(t *testing.T) {
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"no profile":    {func(c *Config) { c.ImageBuilds.Profile = "" }, "image_builds: profile is required"},
		"ghost profile": {func(c *Config) { c.ImageBuilds.Profile = "ghost" }, `image_builds: profile "ghost" is not a profile`},
		"no account":    {func(c *Config) { c.ImageBuilds.Profile = ProfileDefault }, `image_builds: profile "default" names no service_account`},
		"read-only":     {func(c *Config) { c.ImageBuilds.Profile = ProfileDiscuss }, "image_builds: read-only-violation: model-free needs a writing profile"},
		"project's":     {func(c *Config) { c.ImageBuilds.Profile = ProfileImageBuild }, `image_builds: profile "image-build-submitter" is project hollingsworth's implement profile`},
		"ops profile":   {func(c *Config) { c.OpsProfile = ProfileImageBuildRunner }, `image_builds: profile "image-build-runner" is the ops profile`},
		"unclassified": {func(c *Config) {
			c.Profiles[ProfileImageBuildRunner] = Profile{ServiceAccount: "r", Capabilities: []Capability{"vault"}}
		}, "unknown-capability"},
		"cluster write": {func(c *Config) {
			c.Profiles[ProfileImageBuildRunner] = Profile{ServiceAccount: "r", Capabilities: []Capability{ClusterWrite}}
		}, "ops-only"},
		"no workspace":   {func(c *Config) { c.ImageBuilds.Workspace = "" }, "image_builds: workspace, runner type, image and credential_type are required"},
		"no image":       {func(c *Config) { c.ImageBuilds.Runner.Image = "" }, "runner type, image and credential_type are required"},
		"twice":          {func(c *Config) { c.ImageBuilds.Images[1].Name = "lucarne" }, `image_builds: image "lucarne" is listed twice`},
		"bad name":       {func(c *Config) { c.ImageBuilds.Images[0].Name = "Lucarne" }, `image "Lucarne": the name must be [a-z0-9-]`},
		"long name":      {func(c *Config) { c.ImageBuilds.Images[0].Name = "abcdefghijklmnopqrstu" }, "at most 20 characters"},
		"dash end":       {func(c *Config) { c.ImageBuilds.Images[0].Name = "lucarne-" }, "the name must be"},
		"reconcile name": {func(c *Config) { c.ImageBuilds.Images[0].Name = "x-reconcile" }, "not ending in -reconcile"},
		"no repository":  {func(c *Config) { c.ImageBuilds.Images[0].Repository = "" }, "repository, environment and branches are required"},
		"no branch":      {func(c *Config) { c.ImageBuilds.Images[0].Branches = nil }, "repository, environment and branches are required"},
		"bad branch":     {func(c *Config) { c.ImageBuilds.Images[0].Branches = []string{"ma[in"} }, `branch pattern "ma[in" is invalid or uses {a,b}`},
		"empty branch":   {func(c *Config) { c.ImageBuilds.Images[0].Branches = []string{""} }, `branch pattern "" is invalid or uses {a,b}`},
		"branch alternation": {func(c *Config) {
			c.ImageBuilds.Images[0].Branches = []string{"main", "{main,release}"}
		}, `image "lucarne": branch pattern "{main,release}" is invalid or uses {a,b}`},
		"empty file pattern": {func(c *Config) {
			c.ImageBuilds.Images[1].FilePatterns.Include = []string{""}
		}, `file pattern "" is invalid or uses {a,b}`},
		"alternation": {func(c *Config) {
			c.ImageBuilds.Images[1].FilePatterns.Include = []string{"docker/{a,b}/**"}
		}, `file pattern "docker/{a,b}/**" is invalid or uses {a,b}`},
		"bad exclude": {func(c *Config) {
			c.ImageBuilds.Images[1].FilePatterns.Exclude = []string{"[x"}
		}, `file pattern "[x" is invalid`},
	}
	if err := imageBuilds().ValidateImageBuilds(); err != nil {
		t.Fatalf("valid catalogue refused: %v", err)
	}
	longest := imageBuilds()
	longest.ImageBuilds.Images[0].Name = strings.Repeat("a", imageNameMax)
	if err := longest.ValidateImageBuilds(); err != nil {
		t.Fatalf("%d-character name refused: %v", imageNameMax, err)
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := imageBuilds()
			tc.edit(&c)
			if err := c.ValidateImageBuilds(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateImageBuilds = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateImageBuildsRefusesAProjectsAllowedProfile(t *testing.T) {
	c := imageBuilds()
	c.Projects[0].Allowed = map[Mode][]string{ModeAnswer: {ProfileImageBuildRunner}}
	want := `image_builds: profile "image-build-runner" is allowed to project hollingsworth's answer runs`
	if err := c.ValidateImageBuilds(); err == nil || err.Error() != want {
		t.Fatalf("ValidateImageBuilds = %v, want %q", err, want)
	}
}
