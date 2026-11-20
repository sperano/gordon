package dispatch_test

import (
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/run"
)

func TestValidateRefusals(t *testing.T) {
	family := func(f dispatch.Family) func(*dispatch.Config) {
		return func(c *dispatch.Config) { c.Families[run.VikunjaImplement] = f }
	}
	cases := map[string]struct {
		edit func(*dispatch.Config)
		want string
	}{
		"other namespace": {func(c *dispatch.Config) { c.Namespace = "default" }, `namespace "default", want "kelos-agents"`},
		"negative cap":    {func(c *dispatch.Config) { c.CommentsMaxBytes = -1 }, "comments_max_bytes -1 is negative"},
		"family without Task": {func(c *dispatch.Config) { c.Families[run.PROpen] = dispatch.Family{Prompt: "x"} },
			"family \"pr-open\" submits no Task"},
		"unknown family": {func(c *dispatch.Config) { c.Families["wiki-followup"] = dispatch.Family{Prompt: "x"} }, `family "wiki-followup"`},
		"empty prompt":   {family(dispatch.Family{Prompt: " \n"}), "family vikunja-implement: prompt is required"},
		"reserved label": {family(dispatch.Family{Prompt: "x", Labels: map[string]string{dispatch.ModeLabel: "implement"}}), dispatch.ModeLabel + " is set by Gordon"},
		"variant label":  {family(dispatch.Family{Prompt: "x", Labels: map[string]string{dispatch.VariantLabel: "x"}}), dispatch.VariantLabel + " is set by Gordon"},
		"reserved annotation": {family(dispatch.Family{Prompt: "x", Annotations: map[string]string{dispatch.InboxAnnotation: "1"}}),
			dispatch.InboxAnnotation + " is set by Gordon"},
		"family pod label": {family(dispatch.Family{Prompt: "x", Pod: dispatch.Pod{Labels: map[string]string{"a": "b"}}}),
			"family vikunja-implement: pod labels are granted by profiles only"},
		"workspace pod label": {func(c *dispatch.Config) {
			c.Workspaces = map[string]dispatch.Pod{"gordon": {Labels: map[string]string{"a": "b"}}}
		}, "workspace gordon: pod labels"},
		"variant pod label": {func(c *dispatch.Config) {
			c.Variants["claude-opus"] = dispatch.VariantSpec{Pod: dispatch.Pod{Labels: map[string]string{"a": "b"}}}
		}, "variant claude-opus: pod labels"},
		"profile reserved pod label": {func(c *dispatch.Config) {
			c.Profiles["default"] = dispatch.ProfileSpec{Pod: dispatch.Pod{Labels: map[string]string{dispatch.ManagedLabel: "x"}}}
		}, "profile default: " + dispatch.ManagedLabel},
		"profile pod label template": {func(c *dispatch.Config) {
			c.Profiles["default"] = dispatch.ProfileSpec{Pod: dispatch.Pod{Labels: map[string]string{"net": "{{.project}}"}}}
		}, "profile default: pod label net: templates are not rendered in pod labels"},
		"profile pod label key template": {func(c *dispatch.Config) {
			c.Profiles["default"] = dispatch.ProfileSpec{Pod: dispatch.Pod{Labels: map[string]string{"{{.project}}": "true"}}}
		}, "profile default: pod label {{.project}}: templates are not rendered"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			tc.edit(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProfilePodLabelsAccepted(t *testing.T) {
	c := fixture(t)
	if got := c.Profiles["puckdb"].Pod.Labels["capability.kelos.spe.quebec/puckdb-prod-readonly"]; got != "true" {
		t.Fatalf("fixture puckdb pod labels %v", c.Profiles["puckdb"].Pod.Labels)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLimitsFor(t *testing.T) {
	l := dispatch.Limits{Default: 4, Variants: map[string]int{"claude-sonnet": 2, "qwen-coder": 0}}
	for variant, want := range map[string]int{"claude-sonnet": 2, "claude-opus": 4, "qwen-coder": 0} {
		if got := l.For(variant); got != want {
			t.Errorf("For(%s) = %d, want %d", variant, got, want)
		}
	}
}
