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
// objects, every list entry is an object with a name, OmitIfEmptyKey is
// true and only on an env entry, and every template in them parses. The
// API server still checks the rest of the schema.
func (p Pod) validate() error {
	for _, f := range []struct {
		field string
		raw   json.RawMessage
	}{{"resources", p.Resources}, {"affinity", p.Affinity}} {
		if f.raw == nil {
			continue
		}
		if err := checkObject(f.field, f.raw, false, takeNoMark); err != nil {
			return err
		}
	}
	for _, l := range []struct {
		field string
		list  []json.RawMessage
		take  markTaker
	}{{"env", p.Env, takeMark}, {"extraInitContainers", p.ExtraInitContainers, takeEnvMarks},
		{"lastInitContainers", p.LastInitContainers, takeEnvMarks},
		{"volumes", p.Volumes, takeNoMark}, {"volumeMounts", p.VolumeMounts, takeNoMark}} {
		for i, raw := range l.list {
			if err := checkObject(fmt.Sprintf("%s[%d]", l.field, i), raw, true, l.take); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkObject refuses raw unless it is a JSON object (with a non-empty
// string name when named) whose templates parse and whose only
// OmitIfEmptyKey marks are the ones take accepts.
func checkObject(field string, raw json.RawMessage, named bool, take markTaker) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return fmt.Errorf("pod %s: want an object", field)
	}
	if name, ok := m["name"].(string); named && (!ok || name == "") {
		return fmt.Errorf("pod %s: want a name", field)
	}
	what := "pod " + field
	if err := take(what, m); err != nil {
		return err
	}
	if err := checkNoMark(what, m); err != nil {
		return err
	}
	return parseValue(what, m)
}

// markTaker removes from a decoded list entry the OmitIfEmptyKey marks
// expandPod acts on, refusing one that is not true; checkNoMark then
// refuses any left, since nothing would remove them from the Task.
type markTaker func(what string, m map[string]any) error

func takeNoMark(string, map[string]any) error { return nil }

// takeMark takes the mark of an env entry.
func takeMark(what string, m map[string]any) error {
	mark, ok := m[OmitIfEmptyKey]
	if !ok {
		return nil
	}
	if mark != true {
		return fmt.Errorf("%s: %s must be true", what, OmitIfEmptyKey)
	}
	delete(m, OmitIfEmptyKey)
	return nil
}

// takeEnvMarks takes the marks of an init container's env entries.
func takeEnvMarks(what string, m map[string]any) error {
	env, _ := m["env"].([]any)
	for i, e := range env {
		if entry, ok := e.(map[string]any); ok {
			if err := takeMark(fmt.Sprintf("%s env[%d]", what, i), entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkNoMark refuses OmitIfEmptyKey anywhere in v.
func checkNoMark(what string, v any) error {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if err := checkNoMark(what, item); err != nil {
				return err
			}
		}
	case map[string]any:
		if _, ok := x[OmitIfEmptyKey]; ok {
			return fmt.Errorf("%s: %s is only accepted on an env entry", what, OmitIfEmptyKey)
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if err := checkNoMark(what, x[k]); err != nil {
				return err
			}
		}
	}
	return nil
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
