package dispatch_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

const (
	effortEnv       = `{"name":"X_EFFORT","value":"{{.effort}}","gordonOmitIfEmpty":true}`
	secretEnv       = `{"name":"X_SECRET","value":"","valueFrom":{"secretKeyRef":{"name":"claude-oauth","key":"k"}},"gordonOmitIfEmpty":true}`
	initWithEffort  = `{"name":"x-init","env":[{"name":"KEEP","value":"1"},` + effortEnv + `]}`
	lifecycleVolume = `{"name":"agent-lifecycle","emptyDir":{}}`
	lifecycleMount  = `{"name":"agent-lifecycle","mountPath":"/kelos/agent-lifecycle"}`
	noEffortVariant = "deepseek"
)

// effortTask builds the discuss Task of variant with an effort-bearing
// env var in the family's env and in one of its init containers.
func effortTask(t *testing.T, variant string) *kelos.PodOverrides {
	t.Helper()
	cfg := fixture(t)
	f := cfg.Families[run.VikunjaDiscuss]
	f.Pod.Env = append(f.Pod.Env, json.RawMessage(effortEnv), json.RawMessage(secretEnv))
	f.Pod.ExtraInitContainers = append(f.Pod.ExtraInitContainers, json.RawMessage(initWithEffort))
	cfg.Families[run.VikunjaDiscuss] = f
	if _, ok := cfg.Variants[variant]; !ok {
		cfg.Variants[variant] = dispatch.VariantSpec{}
	}
	rn, _ := prepared(t, cfg, taskUpdated(vikunjaTask(gordonID, "discuss"), variant), run.VikunjaDiscuss)
	return rn.Task.Spec.Worker.PodOverrides
}

func envEntry(env []json.RawMessage, name string) (map[string]any, bool) {
	for _, raw := range env {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && m["name"] == name {
			return m, true
		}
	}
	return nil, false
}

func initEnv(t *testing.T, po *kelos.PodOverrides, name string) []json.RawMessage {
	t.Helper()
	for _, raw := range po.ExtraInitContainers {
		var c struct {
			Name string
			Env  []json.RawMessage
		}
		if json.Unmarshal(raw, &c) == nil && c.Name == name {
			return c.Env
		}
	}
	t.Fatalf("no init container %s", name)
	return nil
}

// TestOmitIfEmptyDropsAnEmptyEffort: a variant without an effort gets
// neither the marked env var nor the init container's copy, as the legacy
// spawner's {{if}} leaves AGENT_EFFORT out; the unmarked sibling stays.
func TestOmitIfEmptyDropsAnEmptyEffort(t *testing.T) {
	po := effortTask(t, noEffortVariant)
	if e, ok := envEntry(po.Env, "X_EFFORT"); ok {
		t.Fatalf("env X_EFFORT = %v, want it omitted", e)
	}
	ie := initEnv(t, po, "x-init")
	if e, ok := envEntry(ie, "X_EFFORT"); ok {
		t.Fatalf("init env X_EFFORT = %v, want it omitted", e)
	}
	if e, ok := envEntry(ie, "KEEP"); !ok || e["value"] != "1" {
		t.Fatalf("init env KEEP = %v, want 1", e)
	}
}

// TestOmitIfEmptyKeepsAValueWithoutTheKey: with an effort the marked
// entries stay, rendered, without Gordon's key.
func TestOmitIfEmptyKeepsAValueWithoutTheKey(t *testing.T) {
	po := effortTask(t, "claude-opus")
	for where, env := range map[string][]json.RawMessage{"env": po.Env, "init env": initEnv(t, po, "x-init")} {
		e, ok := envEntry(env, "X_EFFORT")
		if !ok || e["value"] != "medium" || len(e) != 2 {
			t.Fatalf("%s X_EFFORT = %v, want only name and value medium", where, e)
		}
	}
}

// TestOmitIfEmptyKeepsAValueFrom: an empty value read from a Secret is
// not empty.
func TestOmitIfEmptyKeepsAValueFrom(t *testing.T) {
	po := effortTask(t, noEffortVariant)
	e, ok := envEntry(po.Env, "X_SECRET")
	if _, marked := e[dispatch.OmitIfEmptyKey]; !ok || e["valueFrom"] == nil || marked {
		t.Fatalf("env X_SECRET = %v, want it kept with valueFrom, unmarked", e)
	}
}

// discussPod builds the discuss Task with the family's and the profile's
// volumes and mounts.
func discussPod(t *testing.T, familyVol, profileVol, familyMount, profileMount string) route.FamilyDecision {
	t.Helper()
	cfg := fixture(t)
	f := cfg.Families[run.VikunjaDiscuss]
	f.Pod.Volumes = append(f.Pod.Volumes, json.RawMessage(familyVol))
	f.Pod.VolumeMounts = append(f.Pod.VolumeMounts, json.RawMessage(familyMount))
	cfg.Families[run.VikunjaDiscuss] = f
	p := cfg.Profiles["discuss-default"]
	p.Pod.Volumes = append(p.Pod.Volumes, json.RawMessage(profileVol))
	p.Pod.VolumeMounts = append(p.Pod.VolumeMounts, json.RawMessage(profileMount))
	cfg.Profiles["discuss-default"] = p
	return preparedCoreDecision(t, cfg, taskUpdated(vikunjaTask(gordonID, "discuss"), "claude-opus"))
}

func count(list []json.RawMessage, key, value string) int {
	n := 0
	for _, raw := range list {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && m[key] == value {
			n++
		}
	}
	return n
}

// TestRepeatedVolumesCollapse: a profile may declare the volume its
// sidecar mounts though the family declares it too; the pod gets one.
func TestRepeatedVolumesCollapse(t *testing.T) {
	// Same content, other key order and spacing.
	fd := discussPod(t, lifecycleVolume, `{ "emptyDir": {}, "name": "agent-lifecycle" }`, lifecycleMount, lifecycleMount)
	if len(fd.Runs) != 1 {
		t.Fatalf("%s %s, want one run", fd.Outcome, fd.Reason)
	}
	po := fd.Runs[0].Task.Spec.Worker.PodOverrides
	if n := count(po.Volumes, "name", "agent-lifecycle"); n != 1 {
		t.Fatalf("%d agent-lifecycle volumes, want 1", n)
	}
	if n := count(po.VolumeMounts, "mountPath", "/kelos/agent-lifecycle"); n != 1 {
		t.Fatalf("%d /kelos/agent-lifecycle mounts, want 1", n)
	}
}

// TestConflictingRepeatsRefuseTask: one name or mountPath with two
// contents rejects the Task instead of picking one.
func TestConflictingRepeatsRefuseTask(t *testing.T) {
	cases := map[string][4]string{
		"volume": {lifecycleVolume, `{"name":"agent-lifecycle","configMap":{"name":"x"}}`, lifecycleMount, lifecycleMount},
		"mount": {lifecycleVolume, lifecycleVolume, lifecycleMount,
			`{"name":"agent-lifecycle","mountPath":"/kelos/agent-lifecycle","readOnly":true}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fd := discussPod(t, tc[0], tc[1], tc[2], tc[3])
			if fd.Outcome != route.Reject || fd.Reason != dispatch.ReasonPrefix+dispatch.CodePod || len(fd.Runs) != 0 {
				t.Fatalf("decision = %s %s with %d runs, want a pod rejection", fd.Outcome, fd.Reason, len(fd.Runs))
			}
		})
	}
}

func TestValidateRefusesMisplacedOmitIfEmpty(t *testing.T) {
	const want = "gordonOmitIfEmpty must be true, on a list entry"
	cases := map[string]dispatch.Pod{
		"false":        {Env: []json.RawMessage{json.RawMessage(`{"name":"A","value":"","gordonOmitIfEmpty":false}`)}},
		"on resources": {Resources: json.RawMessage(`{"limits":{},"gordonOmitIfEmpty":true}`)},
		"nested object": {Env: []json.RawMessage{
			json.RawMessage(`{"name":"A","valueFrom":{"secretKeyRef":{"name":"s"},"gordonOmitIfEmpty":true}}`)}},
	}
	for name, pod := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			c.Workspaces = map[string]dispatch.Pod{"gordon": pod}
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "workspace gordon: pod ") ||
				!strings.HasSuffix(err.Error(), want) {
				t.Fatalf("Validate = %v, want %q", err, want)
			}
		})
	}
}
