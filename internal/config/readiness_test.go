package config

import (
	"strings"
	"testing"
	"time"
)

func TestReadinessLoads(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := Readiness{ReconcileInterval: time.Minute, NtfyURL: "https://ntfy.example", NtfyTopic: "pr-ready", NtfyToken: "ntoken"}
	if c.Readiness != want {
		t.Errorf("readiness %+v, want %+v", c.Readiness, want)
	}
	if s := c.String(); !strings.Contains(s, "readiness_reconcile=1m0s ntfy=true") || strings.Contains(s, "ntoken") {
		t.Errorf("String() = %q", s)
	}
}

func TestReadinessRefusals(t *testing.T) {
	cases := map[string]struct {
		change func(map[string]string)
		want   string
	}{
		"without the scan": {func(e map[string]string) { e["GORDON_READINESS_RECONCILE_INTERVAL"] = "0" },
			"GORDON_READINESS_RECONCILE_INTERVAL > 0 is required"},
		"scan unset": {func(e map[string]string) { delete(e, "GORDON_READINESS_RECONCILE_INTERVAL") },
			"GORDON_READINESS_RECONCILE_INTERVAL > 0 is required"},
		"without pr_readiness": {func(e map[string]string) {
			e["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r2.json", `{"policy":{}}`)
		}, "GORDON_ROUTING_CONFIG_FILE policy pr_readiness must be true"},
		"without the ntfy token": {func(e map[string]string) { delete(e, "GORDON_NTFY_TOKEN_FILE") },
			"pr-readiness notifies: GORDON_NTFY_URL, GORDON_NTFY_TOPIC and GORDON_NTFY_TOKEN_FILE are required"},
		"cleartext ntfy": {func(e map[string]string) { e["GORDON_NTFY_URL"] = "http://ntfy.example" },
			"GORDON_NTFY_URL: want an https URL"},
		"unknown merge method": {func(e map[string]string) {
			e["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r3.json", `{"policy":{"pr_readiness":true,"readiness":{"merge_method":"fast-forward"}}}`)
		}, `merge_method "fast-forward"`},
		"negative settle": {func(e map[string]string) {
			e["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r4.json", `{"policy":{"pr_readiness":true,"readiness":{"settle_seconds":-1}}}`)
		}, "settle_seconds and missing_check_minutes must not be negative"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := workerEnv(t)
			c.change(env)
			if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("LoadWorker = %v, want %q", err, c.want)
			}
		})
	}
	// ntfy off in the policy needs no publisher.
	env := workerEnv(t)
	delete(env, "GORDON_NTFY_TOKEN_FILE")
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r5.json", `{"policy":{"pr_readiness":true,"readiness":{"notify":false}}}`)
	if _, err := LoadWorker(envOf(env)); err != nil {
		t.Fatalf("ntfy off: %v", err)
	}
}
