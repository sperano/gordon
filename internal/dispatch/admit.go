package dispatch

import (
	"bytes"
	"encoding/json"
	"errors"
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
	referenced, err := referencedSecrets(t)
	if err != nil {
		return err
	}
	var denied []string
	for _, s := range referenced {
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

type secretRef struct {
	Name string `json:"name"`
}

type envVar struct {
	ValueFrom *struct {
		SecretKeyRef *secretRef `json:"secretKeyRef"`
	} `json:"valueFrom"`
}

type container struct {
	Env     []envVar `json:"env"`
	EnvFrom []struct {
		SecretRef *secretRef `json:"secretRef"`
	} `json:"envFrom"`
}

type volume struct {
	Secret *struct {
		SecretName string `json:"secretName"`
	} `json:"secret"`
	Projected *struct {
		Sources []struct {
			Secret *secretRef `json:"secret"`
		} `json:"sources"`
	} `json:"projected"`
	CSI *struct {
		NodePublishSecretRef *secretRef `json:"nodePublishSecretRef"`
	} `json:"csi"`
	// The in-tree volume types that take a Secret reference.
	CephFS     *withSecretRef `json:"cephfs"`
	RBD        *withSecretRef `json:"rbd"`
	ISCSI      *withSecretRef `json:"iscsi"`
	Cinder     *withSecretRef `json:"cinder"`
	FlexVolume *withSecretRef `json:"flexVolume"`
	ScaleIO    *withSecretRef `json:"scaleIO"`
	StorageOS  *withSecretRef `json:"storageos"`
	AzureFile  *struct {
		SecretName string `json:"secretName"`
	} `json:"azureFile"`
}

// withSecretRef is an in-tree volume source's Secret reference.
type withSecretRef struct {
	SecretRef *secretRef `json:"secretRef"`
}

// referencedSecrets lists the Secrets a Task's worker references: its
// credentials, env, Secret, projected, CSI and in-tree volumes, and init
// containers' env and envFrom. An entry that is not an object of its kind
// is refused: its references could not be checked.
func referencedSecrets(t kelos.Task) ([]string, error) {
	w := t.Spec.Worker
	var out []string
	if w.Credentials != nil && w.Credentials.SecretRef != nil {
		out = append(out, w.Credentials.SecretRef.Name)
	}
	p := w.PodOverrides
	if p == nil {
		return out, nil
	}
	for _, raw := range p.Env {
		var e envVar
		if err := decodeObject(raw, &e); err != nil {
			return nil, refuse(CodePod, "env entry: %v", err)
		}
		out = appendEnvSecret(out, e)
	}
	for _, raw := range p.Volumes {
		var v volume
		if err := decodeObject(raw, &v); err != nil {
			return nil, refuse(CodePod, "volume: %v", err)
		}
		out = append(out, volumeSecrets(v)...)
	}
	for _, raw := range p.ExtraInitContainers {
		var c container
		if err := decodeObject(raw, &c); err != nil {
			return nil, refuse(CodePod, "init container: %v", err)
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
	return out, nil
}

// errNotObject is a pod entry that is no JSON object (null included,
// which json.Unmarshal would take as an empty entry).
var errNotObject = errors.New("not a JSON object")

// decodeObject decodes raw into v, refusing anything but an object.
func decodeObject(raw json.RawMessage, v any) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return errNotObject
	}
	return json.Unmarshal(raw, v)
}

func volumeSecrets(v volume) []string {
	var out []string
	if v.Secret != nil {
		out = append(out, v.Secret.SecretName)
	}
	if v.Projected != nil {
		for _, src := range v.Projected.Sources {
			if src.Secret != nil {
				out = append(out, src.Secret.Name)
			}
		}
	}
	if v.CSI != nil && v.CSI.NodePublishSecretRef != nil {
		out = append(out, v.CSI.NodePublishSecretRef.Name)
	}
	for _, src := range []*withSecretRef{v.CephFS, v.RBD, v.ISCSI, v.Cinder, v.FlexVolume, v.ScaleIO, v.StorageOS} {
		if src != nil && src.SecretRef != nil {
			out = append(out, src.SecretRef.Name)
		}
	}
	if v.AzureFile != nil {
		out = append(out, v.AzureFile.SecretName)
	}
	return out
}

func appendEnvSecret(out []string, e envVar) []string {
	if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
		out = append(out, e.ValueFrom.SecretKeyRef.Name)
	}
	return out
}
