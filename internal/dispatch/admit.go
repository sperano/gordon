package dispatch

import (
	"encoding/json"
	"slices"
	"sort"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/profile"
)

// admit refuses a Task referencing a Secret its profile does not grant,
// the check Hollingsworth's execution-profile admission makes: building
// it here turns a Task Kelos would refuse into an explicit reject at
// decision time instead of a dead-lettered submission.
func (b Builder) admit(t kelos.Task, prof profile.Resolved) error {
	granted := append(slices.Clone(b.Config.CommonSecrets), b.Config.Profiles[prof.Profile].Secrets...)
	var denied []string
	for _, s := range referencedSecrets(t) {
		if !slices.Contains(granted, s) && !slices.Contains(denied, s) {
			denied = append(denied, s)
		}
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		return refuse(CodeSecretNotGranted, "profile %q does not grant %v", prof.Profile, denied)
	}
	return nil
}

type envVar struct {
	ValueFrom *struct {
		SecretKeyRef *struct {
			Name string `json:"name"`
		} `json:"secretKeyRef"`
	} `json:"valueFrom"`
}

type container struct {
	Env     []envVar `json:"env"`
	EnvFrom []struct {
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}

type volume struct {
	Secret *struct {
		SecretName string `json:"secretName"`
	} `json:"secret"`
}

// referencedSecrets lists the Secrets a Task's worker references: its
// credentials, env, Secret volumes and init containers' env and envFrom,
// as the admission policy's referencedSecrets.
func referencedSecrets(t kelos.Task) []string {
	w := t.Spec.Worker
	var out []string
	if w.Credentials != nil && w.Credentials.SecretRef != nil {
		out = append(out, w.Credentials.SecretRef.Name)
	}
	p := w.PodOverrides
	if p == nil {
		return out
	}
	out = append(out, envSecrets(p.Env)...)
	for _, raw := range p.Volumes {
		var v volume
		if json.Unmarshal(raw, &v) == nil && v.Secret != nil {
			out = append(out, v.Secret.SecretName)
		}
	}
	for _, raw := range p.ExtraInitContainers {
		var c container
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		for _, e := range c.Env {
			out = appendEnvSecret(out, e)
		}
		for _, f := range c.EnvFrom {
			if f.SecretRef != nil {
				out = append(out, f.SecretRef.Name)
			}
		}
	}
	return out
}

func envSecrets(env []json.RawMessage) []string {
	var out []string
	for _, raw := range env {
		var e envVar
		if json.Unmarshal(raw, &e) == nil {
			out = appendEnvSecret(out, e)
		}
	}
	return out
}

func appendEnvSecret(out []string, e envVar) []string {
	if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
		out = append(out, e.ValueFrom.SecretKeyRef.Name)
	}
	return out
}
