package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/template"

	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// Validate refuses a Config that could build a Task admission refuses,
// that names a reserved key, or whose templates or pod pieces cannot be
// read: those fail at startup, not on every run at decision time. Map
// entries are checked in key order, so the error reported is stable.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("dispatch config: version is required")
	}
	if c.Namespace != route.KelosNamespace {
		// Every submit action targets kelos/<route.KelosNamespace>/<name>.
		return fmt.Errorf("dispatch config: namespace %q, want %q", c.Namespace, route.KelosNamespace)
	}
	if c.CommentsMaxBytes < 0 {
		return fmt.Errorf("dispatch config: comments_max_bytes %d is negative", c.CommentsMaxBytes)
	}
	for _, label := range slices.Sorted(maps.Keys(c.CoreLabels)) {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			return fmt.Errorf("dispatch config: core_labels contains an empty label")
		}
		if trimmed != label {
			return fmt.Errorf("dispatch config: core label %q has leading or trailing whitespace", label)
		}
		if cores := c.CoreLabels[label]; cores <= 0 {
			return fmt.Errorf("dispatch config: core label %q has %d cores, want a positive number", label, cores)
		}
	}
	for _, check := range []func() error{c.validateFamilies, c.validateWorkspaces, c.validateVariants,
		c.validateProfiles, c.validateLimits} {
		if err := check(); err != nil {
			return fmt.Errorf("dispatch config: %w", err)
		}
	}
	return nil
}

func (c Config) validateFamilies() error {
	for _, f := range slices.Sorted(maps.Keys(c.Families)) {
		if !slices.Contains(run.Families, f) || (run.Identity{Family: f}).Name() == "" {
			return fmt.Errorf("family %q submits no Task", f)
		}
		if err := c.Families[f].validate(); err != nil {
			return fmt.Errorf("family %s: %w", f, err)
		}
	}
	return nil
}

func (f Family) validate() error {
	if strings.TrimSpace(f.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	if err := checkKeys(f.Labels, f.Annotations); err != nil {
		return err
	}
	if err := f.Pod.checkNoLabels(); err != nil {
		return err
	}
	if err := parse("prompt", f.Prompt); err != nil {
		return err
	}
	for _, set := range []struct {
		kind string
		m    map[string]string
	}{{"label", f.Labels}, {"annotation", f.Annotations}} {
		for _, k := range slices.Sorted(maps.Keys(set.m)) {
			if err := parse(set.kind+" "+k, set.m[k]); err != nil {
				return err
			}
		}
	}
	for _, entry := range f.AgentConfigs {
		if entry != AgentConfigsProject && entry != AgentConfigVikunja {
			if err := parse("agent config", entry); err != nil {
				return err
			}
		}
	}
	return f.Pod.validate()
}

func (c Config) validateWorkspaces() error {
	for _, name := range slices.Sorted(maps.Keys(c.Workspaces)) {
		p := c.Workspaces[name]
		if err := p.checkNoLabels(); err != nil {
			return fmt.Errorf("workspace %s: %w", name, err)
		}
		if err := p.validate(); err != nil {
			return fmt.Errorf("workspace %s: %w", name, err)
		}
	}
	return nil
}

func (c Config) validateVariants() error {
	for _, name := range slices.Sorted(maps.Keys(c.Variants)) {
		p := c.Variants[name].Pod
		if err := p.checkNoLabels(); err != nil {
			return fmt.Errorf("variant %s: %w", name, err)
		}
		if err := p.validate(); err != nil {
			return fmt.Errorf("variant %s: %w", name, err)
		}
	}
	return nil
}

func (c Config) validateProfiles() error {
	for _, name := range slices.Sorted(maps.Keys(c.Profiles)) {
		p := c.Profiles[name].Pod
		for _, check := range []func() error{
			func() error { return checkKeys(p.Labels) },
			func() error { return checkLiteral(p.Labels) },
			p.validate,
		} {
			if err := check(); err != nil {
				return fmt.Errorf("profile %s: %w", name, err)
			}
		}
	}
	return nil
}

// validateLimits refuses a limit for a variant the Config does not know:
// a misspelled name would leave the variant on the default limit.
func (c Config) validateLimits() error {
	for _, name := range slices.Sorted(maps.Keys(c.Limits.Variants)) {
		if _, ok := c.Variants[name]; !ok {
			return fmt.Errorf("limits: variant %q is not in variants", name)
		}
	}
	return nil
}

func checkKeys(sets ...map[string]string) error {
	for _, m := range sets {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if slices.Contains(reservedKeys, k) {
				return fmt.Errorf("%s is set by Gordon", k)
			}
		}
	}
	return nil
}

// checkLiteral refuses a template in a pod label: pod labels grant
// network access, so they are the profile's literal values, never
// rendered from task data.
func checkLiteral(labels map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		if strings.Contains(k, "{{") || strings.Contains(labels[k], "{{") {
			return fmt.Errorf("pod label %s: templates are not rendered in pod labels", k)
		}
	}
	return nil
}

// checkNoLabels refuses pod labels outside a profile: reserved pod labels
// grant network access, which only a profile may.
func (p Pod) checkNoLabels() error {
	if len(p.Labels) > 0 {
		return fmt.Errorf("pod labels are granted by profiles only")
	}
	return nil
}

// validate checks a pod piece's shape: resources and affinity are
// objects, every list entry is an object with a name, and every template
// in them parses. The API server still checks the rest of the schema.
func (p Pod) validate() error {
	for _, f := range []struct {
		field string
		raw   json.RawMessage
	}{{"resources", p.Resources}, {"affinity", p.Affinity}} {
		if f.raw == nil {
			continue
		}
		if err := checkObject(f.field, f.raw, false); err != nil {
			return err
		}
	}
	for _, l := range []struct {
		field string
		list  []json.RawMessage
	}{{"env", p.Env}, {"extraInitContainers", p.ExtraInitContainers}, {"lastInitContainers", p.LastInitContainers},
		{"volumes", p.Volumes}, {"volumeMounts", p.VolumeMounts}} {
		for i, raw := range l.list {
			if err := checkObject(fmt.Sprintf("%s[%d]", l.field, i), raw, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkObject refuses raw unless it is a JSON object (with a non-empty
// string name when named) whose templates parse.
func checkObject(field string, raw json.RawMessage, named bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return fmt.Errorf("pod %s: want an object", field)
	}
	if name, ok := m["name"].(string); named && (!ok || name == "") {
		return fmt.Errorf("pod %s: want a name", field)
	}
	return parseValue("pod "+field, m)
}

// parseValue parses every template string in v.
func parseValue(what string, v any) error {
	switch x := v.(type) {
	case string:
		return parse(what, x)
	case []any:
		for _, item := range x {
			if err := parseValue(what, item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if err := parseValue(what, x[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// parse refuses a template that does not parse; text without "{{" is
// literal.
func parse(what, text string) error {
	if !strings.Contains(text, "{{") {
		return nil
	}
	if _, err := template.New(what).Option("missingkey=error").Parse(text); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}
