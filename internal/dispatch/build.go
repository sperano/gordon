package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"text/template"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/profile"
)

// Error codes of a Task that cannot be built; a family whose run fails to
// build is a reject with ReasonPrefix and the code.
const (
	CodeNoTemplate       = "no-task-template"
	CodeUnknownProject   = "unknown-project"
	CodeUnknownVariant   = "unknown-variant"
	CodeTemplate         = "task-template-invalid"
	CodePod              = "pod-invalid"
	CodeSecretNotGranted = "secret-not-granted"
)

// ReasonPrefix starts the family reason of a run whose Task cannot be built.
const ReasonPrefix = "task-spec-"

// Error is a Task that cannot be built.
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

func refuse(code, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Builder builds Tasks from the routing and dispatch configurations.
type Builder struct {
	Routing profile.Config
	Config  Config
}

// Build returns the Task of one run.
func (b Builder) Build(in Input) (kelos.Task, error) {
	id, prof := in.Run.Identity, in.Run.Profile
	fam, ok := b.Config.Families[id.Family]
	if !ok {
		return kelos.Task{}, refuse(CodeNoTemplate, "family %s", id.Family)
	}
	proj, ok := b.project(id.Project)
	if !ok {
		return kelos.Task{}, refuse(CodeUnknownProject, "project %q", id.Project)
	}
	// A variant's entry may be empty, but it must be there: a missing one
	// would build the Task without its variant's identity and pod pieces.
	variant, ok := b.Config.Variants[id.Variant]
	if !ok {
		return kelos.Task{}, refuse(CodeUnknownVariant, "variant %q", id.Variant)
	}
	d := b.data(in, proj)
	prompt, err := render("prompt", fam.Prompt, d)
	if err != nil {
		return kelos.Task{}, err
	}
	configs, err := b.agentConfigs(fam, proj, variant, d)
	if err != nil {
		return kelos.Task{}, err
	}
	pod, err := b.pod(fam, proj, prof, variant, d)
	if err != nil {
		return kelos.Task{}, err
	}
	meta, err := b.metadata(in, fam, prof, d)
	if err != nil {
		return kelos.Task{}, err
	}
	t := kelos.Task{APIVersion: kelos.APIVersion, Kind: kelos.KindTask, Metadata: meta, Spec: kelos.Spec{
		Prompt: prompt, Branch: in.Run.Branch, TTLSecondsAfterFinished: fam.TTLSecondsAfterFinished,
		PodFailurePolicy: fam.PodFailurePolicy,
		Worker: &kelos.Worker{
			Type: prof.Agent, Image: prof.Image, Model: prof.Model, Effort: prof.Effort,
			Credentials:     credentials(prof),
			WorkspaceRef:    &kelos.Ref{Name: prof.Workspace},
			AgentConfigRefs: configs, PodOverrides: pod,
		},
	}}
	return t, b.admit(t, prof)
}

func (b Builder) project(name string) (profile.Project, bool) {
	for _, p := range b.Routing.Projects {
		if p.Name == name {
			return p, true
		}
	}
	return profile.Project{}, false
}

func credentials(p profile.Resolved) *kelos.Credentials {
	c := &kelos.Credentials{Type: p.CredentialType}
	if p.CredentialSecret != "" {
		c.SecretRef = &kelos.Ref{Name: p.CredentialSecret}
	}
	return c
}

// metadata is the Task's name, namespace, labels and annotations: the
// family's, rendered (an empty value stays, as a spawner's), then
// Gordon's reserved keys.
func (b Builder) metadata(in Input, fam Family, prof profile.Resolved, d Data) (kelos.Metadata, error) {
	m := kelos.Metadata{Name: in.Run.Name, Namespace: b.Config.Namespace, Labels: map[string]string{},
		Annotations: map[string]string{}}
	for k, v := range fam.Labels {
		out, err := render("label "+k, v, d)
		if err != nil {
			return m, err
		}
		m.Labels[k] = out
	}
	for k, v := range fam.Annotations {
		out, err := render("annotation "+k, v, d)
		if err != nil {
			return m, err
		}
		m.Annotations[k] = out
	}
	m.Labels[ProfileLabel], m.Labels[ModeLabel], m.Labels[ManagedLabel] = prof.Profile, string(prof.Mode), "true"
	m.Labels[VariantLabel] = in.Run.Identity.Variant
	if prof.Mode == profile.ModeDiscuss {
		m.Labels[DiscussLabel] = "true"
	}
	m.Annotations[InboxAnnotation] = strconv.FormatInt(in.InboxID, 10)
	m.Annotations[VersionAnnotation] = prof.ConfigVersion + "/" + b.Config.Version
	return m, nil
}

// agentConfigs expands and suffixes the family's agent config list.
func (b Builder) agentConfigs(fam Family, proj profile.Project, v VariantSpec, d Data) ([]kelos.Ref, error) {
	var names []string
	for _, entry := range fam.AgentConfigs {
		switch entry {
		case AgentConfigsProject:
			names = append(names, proj.AgentConfigs...)
		case AgentConfigVikunja:
			if v.VikunjaAgentConfig != "" {
				names = append(names, v.VikunjaAgentConfig)
			}
		default:
			name, err := render("agent config", entry, d)
			if err != nil {
				return nil, err
			}
			names = append(names, name)
		}
	}
	out := make([]kelos.Ref, 0, len(names))
	for _, n := range names {
		out = append(out, kelos.Ref{Name: n + v.AgentConfigSuffix})
	}
	return out, nil
}

// pod composes the family's, Workspace's, profile's and variant's pieces
// and sets the profile's ServiceAccount.
func (b Builder) pod(fam Family, proj profile.Project, prof profile.Resolved, v VariantSpec, d Data) (*kelos.PodOverrides, error) {
	out := &kelos.PodOverrides{ServiceAccountName: prof.ServiceAccount}
	pieces := []Pod{fam.Pod, b.Config.Workspaces[proj.Workspace], b.Config.Profiles[prof.Profile].Pod, v.Pod}
	for _, p := range pieces {
		if len(p.Labels) > 0 {
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			maps.Copy(out.Labels, p.Labels)
		}
		if p.Resources != nil {
			out.Resources = p.Resources
		}
		if p.Affinity != nil {
			out.Affinity = p.Affinity
		}
		out.Env = append(out.Env, p.Env...)
		out.ExtraInitContainers = append(out.ExtraInitContainers, p.ExtraInitContainers...)
		out.Volumes = append(out.Volumes, p.Volumes...)
		out.VolumeMounts = append(out.VolumeMounts, p.VolumeMounts...)
	}
	if err := expandPod(out, d); err != nil {
		return nil, err
	}
	if podEmpty(out) {
		return nil, nil
	}
	return out, nil
}

func podEmpty(p *kelos.PodOverrides) bool {
	return p.ServiceAccountName == "" && len(p.Labels) == 0 && p.Resources == nil && p.Affinity == nil &&
		len(p.Env) == 0 && len(p.ExtraInitContainers) == 0 && len(p.Volumes) == 0 && len(p.VolumeMounts) == 0
}

// expandPod renders every pod string that holds a template.
func expandPod(p *kelos.PodOverrides, d Data) error {
	var err error
	one := func(raw json.RawMessage) json.RawMessage {
		if err != nil || raw == nil {
			return raw
		}
		var out json.RawMessage
		out, err = expand(raw, d)
		return out
	}
	p.Resources, p.Affinity = one(p.Resources), one(p.Affinity)
	for _, list := range [][]json.RawMessage{p.Env, p.ExtraInitContainers, p.Volumes, p.VolumeMounts} {
		for i := range list {
			list[i] = one(list[i])
		}
	}
	return err
}

// expand renders the template strings of one raw JSON value; a value
// without "{{" is kept byte for byte.
func expand(raw json.RawMessage, d Data) (json.RawMessage, error) {
	if !bytes.Contains(raw, []byte("{{")) {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, refuse(CodePod, "%v", err)
	}
	v, err := expandValue(v, d)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, refuse(CodePod, "%v", err)
	}
	return out, nil
}

func expandValue(v any, d Data) (any, error) {
	var err error
	switch x := v.(type) {
	case string:
		if strings.Contains(x, "{{") {
			return render("pod value", x, d)
		}
	case []any:
		for i := range x {
			if x[i], err = expandValue(x[i], d); err != nil {
				return nil, err
			}
		}
	case map[string]any:
		for k := range x {
			if x[k], err = expandValue(x[k], d); err != nil {
				return nil, err
			}
		}
	}
	return v, nil
}

// render executes one template; a missing variable is an error.
func render(what, text string, d Data) (string, error) {
	tmpl, err := template.New(what).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", refuse(CodeTemplate, "%s: %v", what, err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, map[string]any(d)); err != nil {
		return "", refuse(CodeTemplate, "%s: %v", what, err)
	}
	return out.String(), nil
}
