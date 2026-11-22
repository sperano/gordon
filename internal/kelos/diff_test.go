package kelos_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/kelos"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDiff(t *testing.T) {
	want := kelos.Task{Metadata: kelos.Metadata{Name: "a",
		Labels:      map[string]string{"role": "x", "mine": "1"},
		Annotations: map[string]string{"repo": "sperano/gordon", "inbox": "4"}},
		Spec: kelos.Spec{Prompt: "p", Branch: "b", Worker: &kelos.Worker{Type: "claude-code", PodOverrides: &kelos.PodOverrides{
			Env: []json.RawMessage{json.RawMessage(`{"name":"A","value":"1"}`), json.RawMessage(`{"name":"B","value":"2"}`)}}}}}
	same := map[string]any{
		"apiVersion": kelos.APIVersion, "kind": "Task",
		"metadata": map[string]any{"name": "a", "uid": "u", "resourceVersion": "9",
			// An ignored key is no difference on either side.
			"labels":      map[string]any{"role": "x", "kelos.dev/taskspawner": "s"},
			"annotations": map[string]any{"repo": "sperano/gordon", "inbox": "7"}},
		// Env in another order is the same env.
		"spec": map[string]any{"prompt": "p", "branch": "b", "worker": map[string]any{"type": "claude-code",
			"podOverrides": map[string]any{"env": []any{map[string]any{"name": "B", "value": "2"},
				map[string]any{"name": "A", "value": "1"}}}}},
		"status": map[string]any{"phase": "Running"},
	}
	diff, err := kelos.Diff(want, raw(t, same), "mine", "inbox")
	if err != nil || len(diff) != 0 {
		t.Fatalf("Diff = %v, %v; want none", diff, err)
	}
	other := raw(t, map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"role": "y", "extra": "1"}},
		"spec": map[string]any{"prompt": "q", "worker": map[string]any{"type": "claude-code",
			"podOverrides": map[string]any{"env": []any{map[string]any{"name": "A", "value": "9"}}}}},
	})
	diff, err = kelos.Diff(want, other)
	wantPaths := []string{"metadata.annotations.inbox", "metadata.annotations.repo", "metadata.labels.extra",
		"metadata.labels.mine", "metadata.labels.role", "spec.branch", "spec.prompt",
		"spec.worker.podOverrides.env[name=A].value", "spec.worker.podOverrides.env[name=B]"}
	if err != nil || !slices.Equal(diff, wantPaths) {
		t.Fatalf("Diff = %v, %v\nwant %v", diff, err, wantPaths)
	}
}

func TestDiffListsAndCap(t *testing.T) {
	want := kelos.Task{Spec: kelos.Spec{PodFailurePolicy: json.RawMessage(`{"rules":[1,2]}`)}}
	got := raw(t, map[string]any{"spec": map[string]any{"podFailurePolicy": map[string]any{"rules": []any{1, 3}}}})
	diff, _ := kelos.Diff(want, got)
	if !slices.Equal(diff, []string{"spec.podFailurePolicy.rules[1]", "spec.prompt"}) {
		t.Fatalf("unnamed list of equal length: %v", diff)
	}
	got = raw(t, map[string]any{"spec": map[string]any{"prompt": "", "podFailurePolicy": map[string]any{"rules": []any{1}}}})
	if diff, _ := kelos.Diff(want, got); !slices.Equal(diff, []string{"spec.podFailurePolicy.rules"}) {
		t.Fatalf("lists of different lengths: %v", diff)
	}
	labels := map[string]string{}
	for i := range 60 {
		labels[fmt.Sprintf("l%02d", i)] = "x"
	}
	diff, _ = kelos.Diff(kelos.Task{Metadata: kelos.Metadata{Labels: labels}}, raw(t, map[string]any{"spec": map[string]any{"prompt": ""}}))
	if len(diff) != 51 || diff[50] != "…" || diff[0] != "metadata.labels.l00" {
		t.Fatalf("capped diff has %d paths ending %q", len(diff), diff[len(diff)-1])
	}
	if _, err := kelos.Diff(kelos.Task{}, json.RawMessage(`[]`)); err == nil {
		t.Fatal("a non-object Task compared")
	}
}

func TestDiffKelosTypedDefaults(t *testing.T) {
	want := kelos.Task{Spec: kelos.Spec{
		PodFailurePolicy: json.RawMessage(`{"rules":[{"onPodConditions":[{"type":"DisruptionTarget"}]}]}`),
		Worker: &kelos.Worker{Type: "claude-code", PodOverrides: &kelos.PodOverrides{
			ExtraInitContainers: []json.RawMessage{json.RawMessage(`{"name":"wait"}`)},
			VolumeMounts:        []json.RawMessage{json.RawMessage(`{"name":"skip","mountPath":"/kelos-skip"}`)},
		}},
	}}
	base := `{"spec":{"prompt":"","podFailurePolicy":{"rules":[{"onPodConditions":[{"type":"DisruptionTarget","status":""}]}]},"worker":{"type":"claude-code","podOverrides":{"extraInitContainers":[{"name":"wait","resources":{}}],"volumeMounts":[{"name":"skip","mountPath":"/kelos-skip","readOnly":false}]}}}}`
	if diff, err := kelos.Diff(want, json.RawMessage(base)); err != nil || len(diff) != 0 {
		t.Fatalf("typed defaults: Diff = %v, %v", diff, err)
	}
	for _, tc := range []struct {
		name, old, new, path string
	}{
		{"condition status", `"status":""`, `"status":"True"`, "spec.podFailurePolicy.rules[0].onPodConditions[0].status"},
		{"init resources", `"resources":{}`, `"resources":{"requests":{"cpu":"10m"}}`, "spec.worker.podOverrides.extraInitContainers[0].resources"},
		{"mount readOnly", `"readOnly":false`, `"readOnly":true`, "spec.worker.podOverrides.volumeMounts[mountPath=/kelos-skip].readOnly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := json.RawMessage(strings.Replace(base, tc.old, tc.new, 1))
			if diff, err := kelos.Diff(want, got); err != nil || !slices.Equal(diff, []string{tc.path}) {
				t.Fatalf("Diff = %v, %v; want %s", diff, err, tc.path)
			}
		})
	}
}

func TestDiffKelosLabelsLeftOutOnBothSides(t *testing.T) {
	want := kelos.Task{Metadata: kelos.Metadata{Labels: map[string]string{"kelos.dev/taskspawner": "mine", "role": "x"}}}
	got := raw(t, map[string]any{"metadata": map[string]any{"labels": map[string]any{"role": "x"}},
		"spec": map[string]any{"prompt": ""}})
	if diff, err := kelos.Diff(want, got); err != nil || len(diff) != 0 {
		t.Fatalf("Diff = %v, %v; want a kelos.dev/ label of want left out", diff, err)
	}
	want = kelos.Task{Metadata: kelos.Metadata{Labels: map[string]string{"role": "x"}}}
	got = raw(t, map[string]any{"metadata": map[string]any{"labels": map[string]any{"role": "x", "kelos.dev/taskspawner": "s"}},
		"spec": map[string]any{"prompt": ""}})
	if diff, err := kelos.Diff(want, got); err != nil || len(diff) != 0 {
		t.Fatalf("Diff = %v, %v; want a kelos.dev/ label of got left out", diff, err)
	}
}

func TestDiffVolumesByName(t *testing.T) {
	want := kelos.Task{Spec: kelos.Spec{Worker: &kelos.Worker{Type: "claude-code", PodOverrides: &kelos.PodOverrides{
		Volumes: []json.RawMessage{json.RawMessage(`{"name":"a"}`), json.RawMessage(`{"name":"b"}`)}}}}}
	reversed := []any{map[string]any{"name": "b"}, map[string]any{"name": "a"}}
	got := raw(t, map[string]any{"spec": map[string]any{"prompt": "", "worker": map[string]any{"type": "claude-code",
		"podOverrides": map[string]any{"volumes": reversed}}}})
	if diff, err := kelos.Diff(want, got); err != nil || len(diff) != 0 {
		t.Fatalf("Diff = %v, %v; want volumes in another order to be the same", diff, err)
	}
	changed := []any{map[string]any{"name": "a", "readOnly": true}}
	got = raw(t, map[string]any{"spec": map[string]any{"prompt": "", "worker": map[string]any{"type": "claude-code",
		"podOverrides": map[string]any{"volumes": changed}}}})
	diff, err := kelos.Diff(want, got)
	wantPaths := []string{"spec.worker.podOverrides.volumes[name=a].readOnly", "spec.worker.podOverrides.volumes[name=b]"}
	if err != nil || !slices.Equal(diff, wantPaths) {
		t.Fatalf("Diff = %v, %v\nwant %v: a changed or dropped volume is a difference", diff, err, wantPaths)
	}
}

// TestDiffVolumeMountsByMountPath: mounts merge by mountPath, so one
// volume mounted twice still compares regardless of order.
func TestDiffVolumeMountsByMountPath(t *testing.T) {
	want := kelos.Task{Spec: kelos.Spec{Worker: &kelos.Worker{Type: "claude-code", PodOverrides: &kelos.PodOverrides{
		VolumeMounts: []json.RawMessage{json.RawMessage(`{"name":"a","mountPath":"/x"}`),
			json.RawMessage(`{"name":"a","mountPath":"/y"}`), json.RawMessage(`{"name":"b","mountPath":"/z"}`)}}}}}
	reversed := []any{map[string]any{"name": "b", "mountPath": "/z"}, map[string]any{"name": "a", "mountPath": "/y"},
		map[string]any{"name": "a", "mountPath": "/x"}}
	got := raw(t, map[string]any{"spec": map[string]any{"prompt": "", "worker": map[string]any{"type": "claude-code",
		"podOverrides": map[string]any{"volumeMounts": reversed}}}})
	if diff, err := kelos.Diff(want, got); err != nil || len(diff) != 0 {
		t.Fatalf("Diff = %v, %v; want mounts in another order to be the same", diff, err)
	}
	changed := []any{map[string]any{"name": "a", "mountPath": "/x", "readOnly": true},
		map[string]any{"name": "a", "mountPath": "/y"}}
	got = raw(t, map[string]any{"spec": map[string]any{"prompt": "", "worker": map[string]any{"type": "claude-code",
		"podOverrides": map[string]any{"volumeMounts": changed}}}})
	diff, err := kelos.Diff(want, got)
	wantPaths := []string{"spec.worker.podOverrides.volumeMounts[mountPath=/x].readOnly",
		"spec.worker.podOverrides.volumeMounts[mountPath=/z]"}
	if err != nil || !slices.Equal(diff, wantPaths) {
		t.Fatalf("Diff = %v, %v\nwant %v: a changed or dropped mount is a difference", diff, err, wantPaths)
	}
}

func TestDiffOrderedNamedLists(t *testing.T) {
	want := kelos.Task{Spec: kelos.Spec{Worker: &kelos.Worker{Type: "claude-code",
		AgentConfigRefs: []kelos.Ref{{Name: "a"}, {Name: "b"}},
		PodOverrides: &kelos.PodOverrides{ExtraInitContainers: []json.RawMessage{
			json.RawMessage(`{"name":"x"}`), json.RawMessage(`{"name":"y"}`)}}}}}
	got := raw(t, map[string]any{"spec": map[string]any{"prompt": "", "worker": map[string]any{"type": "claude-code",
		"agentConfigRefs": []any{map[string]any{"name": "b"}, map[string]any{"name": "a"}},
		"podOverrides": map[string]any{"extraInitContainers": []any{map[string]any{"name": "y"},
			map[string]any{"name": "x"}}}}}})
	diff, err := kelos.Diff(want, got)
	wantPaths := []string{"spec.worker.agentConfigRefs[0].name", "spec.worker.agentConfigRefs[1].name",
		"spec.worker.podOverrides.extraInitContainers[0].name", "spec.worker.podOverrides.extraInitContainers[1].name"}
	if err != nil || !slices.Equal(diff, wantPaths) {
		t.Fatalf("Diff = %v, %v\nwant %v", diff, err, wantPaths)
	}
}

// TestDiffDependentEnvInOrder: an env referencing another variable
// expands from the variables before it, so its order is compared.
func TestDiffDependentEnvInOrder(t *testing.T) {
	env := func(items ...string) []json.RawMessage {
		out := make([]json.RawMessage, len(items))
		for i, s := range items {
			out[i] = json.RawMessage(s)
		}
		return out
	}
	a, url := `{"name":"HOST","value":"db"}`, `{"name":"URL","value":"postgres://$(HOST)"}`
	want := kelos.Task{Spec: kelos.Spec{Worker: &kelos.Worker{PodOverrides: &kelos.PodOverrides{Env: env(a, url)}}}}
	swapped, err := json.Marshal(kelos.Task{Spec: kelos.Spec{Worker: &kelos.Worker{
		PodOverrides: &kelos.PodOverrides{Env: env(url, a)}}}})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := kelos.Diff(want, swapped)
	wantPaths := []string{"spec.worker.podOverrides.env[0].name", "spec.worker.podOverrides.env[0].value",
		"spec.worker.podOverrides.env[1].name", "spec.worker.podOverrides.env[1].value"}
	if err != nil || !slices.Equal(diff, wantPaths) {
		t.Fatalf("Diff = %v, %v\nwant %v", diff, err, wantPaths)
	}
}

// TestDiffEnvInOrderWhenOneSideReferences: a reference on either side
// alone makes the env's order part of the comparison.
func TestDiffEnvInOrderWhenOneSideReferences(t *testing.T) {
	host, plain, ref := `{"name":"HOST","value":"db"}`, `{"name":"URL","value":"postgres://db"}`,
		`{"name":"URL","value":"postgres://$(HOST)"}`
	wantPaths := []string{"spec.worker.podOverrides.env[0].name", "spec.worker.podOverrides.env[0].value",
		"spec.worker.podOverrides.env[1].name", "spec.worker.podOverrides.env[1].value"}
	for name, tc := range map[string]struct{ want, got []json.RawMessage }{
		"got references":  {[]json.RawMessage{json.RawMessage(host), json.RawMessage(plain)}, []json.RawMessage{json.RawMessage(ref), json.RawMessage(host)}},
		"want references": {[]json.RawMessage{json.RawMessage(ref), json.RawMessage(host)}, []json.RawMessage{json.RawMessage(host), json.RawMessage(plain)}},
	} {
		want := kelos.Task{Spec: kelos.Spec{Worker: &kelos.Worker{PodOverrides: &kelos.PodOverrides{Env: tc.want}}}}
		got, err := json.Marshal(kelos.Task{Spec: kelos.Spec{Worker: &kelos.Worker{PodOverrides: &kelos.PodOverrides{Env: tc.got}}}})
		if err != nil {
			t.Fatal(err)
		}
		if diff, err := kelos.Diff(want, got); err != nil || !slices.Equal(diff, wantPaths) {
			t.Errorf("%s: Diff = %v, %v\nwant %v", name, diff, err, wantPaths)
		}
	}
}
