package dispatch

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/sperano/gordon/internal/kelos"
)

// OmitIfEmptyKey marks a pod env entry (top-level or in an init
// container's env) that Gordon drops when its rendered "value" is empty
// and it has no "valueFrom", as a legacy spawner's {{if}} leaves it out;
// a kept entry loses the key. Only true is accepted, and only on those
// env entries: Kubernetes has no such field, so it never reaches a Task.
const OmitIfEmptyKey = "gordonOmitIfEmpty"

// expandPod renders every pod string that holds a template and drops
// the env entries OmitIfEmptyKey marks empty.
func expandPod(p *kelos.PodOverrides, d Data) error {
	var err error
	if p.Resources, err = expandOne(p.Resources, d); err != nil {
		return err
	}
	if p.Affinity, err = expandOne(p.Affinity, d); err != nil {
		return err
	}
	for _, l := range []struct {
		list  *[]json.RawMessage
		prune pruner
	}{{&p.Env, pruneEnvEntry}, {&p.ExtraInitContainers, pruneContainerEnv},
		{&p.Volumes, keepAll}, {&p.VolumeMounts, keepAll}} {
		if *l.list, err = expandList(*l.list, d, l.prune); err != nil {
			return err
		}
	}
	return nil
}

// pruner drops the OmitIfEmptyKey entries of one rendered list entry:
// it reports whether the entry itself goes and whether it found a mark,
// which changes the entry.
type pruner func(v any) (drop, marked bool)

func keepAll(any) (bool, bool) { return false, false }

// pruneEnvEntry handles an env entry: a marked one goes when its value
// is empty and it has no valueFrom; a kept one loses the key.
func pruneEnvEntry(v any) (bool, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return false, false
	}
	if _, marked := m[OmitIfEmptyKey]; !marked {
		return false, false
	}
	delete(m, OmitIfEmptyKey)
	value, _ := m["value"].(string)
	_, from := m["valueFrom"]
	return value == "" && !from, true
}

// pruneContainerEnv prunes the env entries of an init container; the
// container itself always stays.
func pruneContainerEnv(v any) (bool, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return false, false
	}
	env, ok := m["env"].([]any)
	if !ok {
		return false, false
	}
	out, changed := env[:0], false
	for _, e := range env {
		drop, marked := pruneEnvEntry(e)
		changed = changed || marked
		if !drop {
			out = append(out, e)
		}
	}
	m["env"] = out
	return false, changed
}

func expandOne(raw json.RawMessage, d Data) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	out, _, err := expand(raw, d, keepAll)
	return out, err
}

func expandList(list []json.RawMessage, d Data, prune pruner) ([]json.RawMessage, error) {
	if list == nil {
		return nil, nil
	}
	out := list[:0]
	for _, raw := range list {
		e, drop, err := expand(raw, d, prune)
		if err != nil {
			return nil, err
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out, nil
}

// expand renders the template strings of one raw JSON value and reports
// whether prune drops it; a value with neither a template nor a mark is
// kept byte for byte. The mark is looked up on the decoded value, so an
// escaped key is found too.
func expand(raw json.RawMessage, d Data, prune pruner) (json.RawMessage, bool, error) {
	v, err := decodeAny(raw)
	if err != nil {
		return nil, false, refuse(CodePod, "%v", err)
	}
	templated := bytes.Contains(raw, []byte("{{"))
	if templated {
		if v, err = expandValue(v, d); err != nil {
			return nil, false, err
		}
	}
	drop, marked := prune(v)
	if drop {
		return nil, true, nil
	}
	if !templated && !marked {
		return raw, false, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false, refuse(CodePod, "%v", err)
	}
	return out, false, nil
}

func expandValue(v any, d Data) (any, error) {
	var err error
	switch x := v.(type) {
	case string:
		if containsTemplate(x) {
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

func containsTemplate(s string) bool { return strings.Contains(s, "{{") }

// dedupePod collapses the volumes repeated by name and the volume mounts
// repeated by mountPath across pieces, so a profile can declare what its
// sidecar mounts even where the family declares it too. Kubernetes
// refuses either repeated; two entries under one key that differ are
// refused here, as neither can be chosen safely.
func dedupePod(p *kelos.PodOverrides) error {
	var err error
	if p.Volumes, err = dedupe("volume", "name", p.Volumes); err != nil {
		return err
	}
	p.VolumeMounts, err = dedupe("volume mount", "mountPath", p.VolumeMounts)
	return err
}

// dedupe keeps the first entry for each key's value, drops later equal
// ones and refuses a later one that differs; an entry without the key is
// kept for the API server to judge.
func dedupe(what, key string, list []json.RawMessage) ([]json.RawMessage, error) {
	seen := map[string]any{}
	out := list[:0]
	for _, raw := range list {
		v, err := decodeAny(raw)
		if err != nil {
			return nil, refuse(CodePod, "%s: %v", what, err)
		}
		k, ok := keyOf(v, key)
		if !ok {
			out = append(out, raw)
			continue
		}
		first, dup := seen[k]
		if !dup {
			seen[k] = v
			out = append(out, raw)
			continue
		}
		if !reflect.DeepEqual(first, v) {
			return nil, refuse(CodePod, "%s %s %q declared twice with different content", what, key, k)
		}
	}
	return out, nil
}

func decodeAny(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func keyOf(v any, key string) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	k, ok := m[key].(string)
	return k, ok && k != ""
}
