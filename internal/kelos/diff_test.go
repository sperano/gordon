package kelos_test

import (
	"encoding/json"
	"fmt"
	"slices"
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

func TestDiffKelosLabelsLeftOutOnBothSides(t *testing.T) {
	want := kelos.Task{Metadata: kelos.Metadata{Labels: map[string]string{"kelos.dev/taskspawner": "mine", "role": "x"}}}
	got := raw(t, map[string]any{"metadata": map[string]any{"labels": map[string]any{"role": "x"}},
		"spec": map[string]any{"prompt": ""}})
	if diff, err := kelos.Diff(want, got); err != nil || len(diff) != 0 {
		t.Fatalf("Diff = %v, %v; want a kelos.dev/ label of want left out", diff, err)
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
