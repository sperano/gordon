package kelos

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// maxDiffPaths caps the differing paths Diff reports.
const maxDiffPaths = 50

// kelosLabelPrefix marks labels Kelos itself adds to a spawner's Task.
const kelosLabelPrefix = "kelos.dev/"

// Diff lists the paths where got (a Task as the API returns it) differs
// from want, over the metadata labels and annotations and the whole spec.
// Labels under kelos.dev/ (Kelos adds them to a spawner's Task) and the
// ignored metadata keys are left out on both sides. Paths are sorted, at most maxDiffPaths of them,
// followed by a "…" entry when more were cut.
func Diff(want Task, got json.RawMessage, ignore ...string) ([]string, error) {
	w, err := taskMap(want)
	if err != nil {
		return nil, err
	}
	var g map[string]any
	if err := json.Unmarshal(got, &g); err != nil {
		return nil, fmt.Errorf("task: %w", err)
	}
	g = pick(g)
	for _, k := range ignore {
		deleteMeta(w, k)
		deleteMeta(g, k)
	}
	for _, m := range []map[string]any{w, g} {
		if labels, ok := dig(m, "metadata", "labels").(map[string]any); ok {
			maps.DeleteFunc(labels, func(k string, _ any) bool { return strings.HasPrefix(k, kelosLabelPrefix) })
		}
	}
	var out []string
	walk("", w, g, &out)
	slices.Sort(out)
	if len(out) > maxDiffPaths {
		out = append(out[:maxDiffPaths], "…")
	}
	return out, nil
}

// taskMap is t as generic JSON, limited to what Diff compares.
func taskMap(t Task) (map[string]any, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return pick(m), nil
}

func pick(m map[string]any) map[string]any {
	meta, _ := m["metadata"].(map[string]any)
	return map[string]any{
		"metadata": map[string]any{"labels": orEmpty(meta["labels"]), "annotations": orEmpty(meta["annotations"])},
		"spec":     m["spec"],
	}
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

func dig(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

func deleteMeta(m map[string]any, key string) {
	for _, kind := range []string{"labels", "annotations"} {
		if mm, ok := dig(m, "metadata", kind).(map[string]any); ok {
			delete(mm, key)
		}
	}
}

// unorderedLists are the fields whose lists of named objects Kubernetes
// merges by name, so their order is no difference. Any other list
// (agentConfigRefs, extraInitContainers) is compared in order.
var unorderedLists = map[string]bool{"env": true, "volumes": true, "volumeMounts": true}

// walk appends the paths where a and b differ: into objects key by key,
// into the unorderedLists name by name, into other arrays of equal
// length index by index, else the whole value.
func walk(path string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := slices.Collect(maps.Keys(am))
		for k := range bm {
			if _, ok := am[k]; !ok {
				keys = append(keys, k)
			}
		}
		for _, k := range keys {
			walk(join(path, k), am[k], bm[k], out)
		}
		return
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if am, bm, ok := byName(al), byName(bl), aok && bok && unorderedLists[field(path)]; ok && am != nil && bm != nil {
		// Env, volumes and volume mounts: by name, not position.
		keys := slices.Collect(maps.Keys(am))
		for k := range bm {
			if _, ok := am[k]; !ok {
				keys = append(keys, k)
			}
		}
		for _, k := range keys {
			walk(fmt.Sprintf("%s[name=%s]", path, k), am[k], bm[k], out)
		}
		return
	}
	if aok && bok && len(al) == len(bl) {
		for i := range al {
			walk(fmt.Sprintf("%s[%d]", path, i), al[i], bl[i], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
}

// byName indexes a list of objects by their "name"; nil for an empty
// list, and unless every item is an object with a distinct string name.
func byName(list []any) map[string]any {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]any, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		name, named := m["name"].(string)
		if !ok || !named {
			return nil
		}
		if _, dup := out[name]; dup {
			return nil
		}
		out[name] = m
	}
	return out
}

// field is the last key of path.
func field(path string) string {
	return path[strings.LastIndexByte(path, '.')+1:]
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
