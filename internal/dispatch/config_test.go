package dispatch_test

import (
	"encoding/json"
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
		"empty core label": {func(c *dispatch.Config) { c.CoreLabels = map[string]int{" ": 2} },
			"core_labels contains an empty label"},
		"padded core label": {func(c *dispatch.Config) { c.CoreLabels = map[string]int{" cores:2 ": 2} },
			`core label " cores:2 " has leading or trailing whitespace`},
		"zero cores": {func(c *dispatch.Config) { c.CoreLabels = map[string]int{"cores:0": 0} },
			`core label "cores:0" has 0 cores, want a positive number`},
		"negative cores": {func(c *dispatch.Config) { c.CoreLabels = map[string]int{"cores:-2": -2} },
			`core label "cores:-2" has -2 cores, want a positive number`},
		"family without Task": {func(c *dispatch.Config) { c.Families[run.PROpen] = dispatch.Family{Prompt: "x"} },
			"family \"pr-open\" submits no Task"},
		"unknown family": {func(c *dispatch.Config) { c.Families["wiki-followup"] = dispatch.Family{Prompt: "x"} }, `family "wiki-followup"`},
		"empty prompt":   {family(dispatch.Family{Prompt: " \n"}), "family vikunja-implement: prompt is required"},
		"zero deadline": {family(dispatch.Family{Prompt: "x", Pod: dispatch.Pod{ActiveDeadlineSeconds: ptr(int64(0))}}),
			"family vikunja-implement: pod activeDeadlineSeconds 0 is not positive"},
		"negative profile deadline": {func(c *dispatch.Config) {
			c.Profiles["default"] = dispatch.ProfileSpec{Pod: dispatch.Pod{ActiveDeadlineSeconds: ptr(int64(-5))}}
		}, "profile default: pod activeDeadlineSeconds -5 is not positive"},
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

func TestConfigDecodesCoreLabels(t *testing.T) {
	var cfg dispatch.Config
	if err := json.Unmarshal([]byte(`{"core_labels":{"cores:2":2,"cores:8":8}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.CoreLabels; len(got) != 2 || got["cores:2"] != 2 || got["cores:8"] != 8 {
		t.Fatalf("core_labels = %v, want cores:2=2 and cores:8=8", got)
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
	for variant, want := range map[string]int{"claude-sonnet": 2, "claude-opus": 4, "qwen-coder": 0, "": 0} {
		if got := l.For(variant); got != want {
			t.Errorf("For(%s) = %d, want %d", variant, got, want)
		}
	}
}

func TestValidateRefusesUnreadableEntries(t *testing.T) {
	family := func(edit func(*dispatch.Family)) func(*dispatch.Config) {
		return func(c *dispatch.Config) {
			f := c.Families[run.VikunjaImplement]
			edit(&f)
			c.Families[run.VikunjaImplement] = f
		}
	}
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := map[string]struct {
		edit func(*dispatch.Config)
		want string
	}{
		"no version": {func(c *dispatch.Config) { c.Version = " " }, "dispatch config: version is required"},
		"unknown limit variant": {func(c *dispatch.Config) { c.Limits.Variants = map[string]int{"claude-opsu": 1} },
			`dispatch config: limits: variant "claude-opsu" is not in variants`},
		"prompt template": {family(func(f *dispatch.Family) { f.Prompt = "{{.task" }),
			"family vikunja-implement: prompt: template: prompt:1: unclosed action"},
		"label template": {family(func(f *dispatch.Family) { f.Labels = map[string]string{"a": "{{end}}"} }),
			"family vikunja-implement: label a: template: label a:1: unexpected {{end}}"},
		"annotation template": {family(func(f *dispatch.Family) { f.Annotations = map[string]string{"a": "{{"} }),
			"family vikunja-implement: annotation a: template: annotation a:1: unclosed action"},
		"agent config template": {family(func(f *dispatch.Family) { f.AgentConfigs = []string{"x-{{.y"} }),
			"family vikunja-implement: agent config: template: agent config:1: unclosed action"},
		"pod value template": {family(func(f *dispatch.Family) {
			f.Pod.Env = []json.RawMessage{raw(`{"name":"A","value":"{{.x"}`)}
		}), "family vikunja-implement: pod env[0]: template: pod env[0]:1: unclosed action"},
		"resources not an object": {func(c *dispatch.Config) {
			c.Workspaces = map[string]dispatch.Pod{"gordon": {Resources: raw(`["cpu"]`)}}
		}, "dispatch config: workspace gordon: pod resources: want an object"},
		"affinity null": {func(c *dispatch.Config) {
			c.Variants["claude-opus"] = dispatch.VariantSpec{Pod: dispatch.Pod{Affinity: raw(`null`)}}
		}, "dispatch config: variant claude-opus: pod affinity: want an object"},
		"variants sharing a short name": {func(c *dispatch.Config) {
			c.Variants["gpt-opus"] = dispatch.VariantSpec{}
		}, `dispatch config: variants claude-opus and gpt-opus share the run name part "opus"`},
		"short name taken by a whole variant": {func(c *dispatch.Config) {
			c.Variants["opus"] = dispatch.VariantSpec{}
		}, `dispatch config: variants claude-opus and opus share the run name part "opus"`},
		"unnamed volume": {func(c *dispatch.Config) {
			c.Profiles["default"] = dispatch.ProfileSpec{Pod: dispatch.Pod{Volumes: []json.RawMessage{raw(`{"emptyDir":{}}`)}}}
		}, "dispatch config: profile default: pod volumes[0]: want a name"},
		"init container not an object": {family(func(f *dispatch.Family) {
			f.Pod.LastInitContainers = []json.RawMessage{raw(`"pr-opener"`)}
		}), "family vikunja-implement: pod lastInitContainers[0]: want an object"},
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

// TestValidateReportsTheFirstInvalidEntryByName: with several invalid
// entries the error names the first by key, at every run.
func TestValidateReportsTheFirstInvalidEntryByName(t *testing.T) {
	const runs = 20
	bad := dispatch.Pod{Labels: map[string]string{"a": "b"}}
	for range runs {
		c := fixture(t)
		c.Workspaces = map[string]dispatch.Pod{"zeta": bad, "alpha": bad, "mu": bad}
		c.Variants["zz"], c.Variants["aa"] = dispatch.VariantSpec{Pod: bad}, dispatch.VariantSpec{Pod: bad}
		if err := c.Validate(); err == nil || err.Error() != "dispatch config: workspace alpha: pod labels are granted by profiles only" {
			t.Fatalf("Validate = %v, want workspace alpha's error", err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
