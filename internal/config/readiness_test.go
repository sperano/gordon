package config

import (
	"strings"
	"testing"
	"time"
)

func readinessRule(owner string) string {
	evidence := `["replay-parity"]`
	if owner == "gordon" {
		evidence = `["replay-parity","shadow-parity","rollback-tested","legacy-disabled"]`
	}
	return `{"rules":[{"project":"vigie","family":"pr-readiness","owner":"` + owner + `","evidence":` + evidence + `}]}`
}

// readinessEnv is a worker whose pr-readiness rule has owner, with
// everything it needs: the App, Kelos, the scan, a Vikunja writer,
// pr_readiness on and ntfy.
func readinessEnv(t *testing.T, owner string) map[string]string {
	env := workerEnv(t)
	env["GORDON_OWNERSHIP_FILE"] = file(t, "o.json", readinessRule(owner))
	env["GORDON_GITHUB_APP_DIR"] = "/var/run/github-app"
	env["GORDON_DISPATCH_CONFIG_FILE"] = file(t, "d.json", `{"version":"d-1","namespace":"kelos-agents"}`)
	env["GORDON_READINESS_RECONCILE_INTERVAL"] = "1m"
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r.json", `{"policy":{"pr_readiness":true}}`)
	if owner == "gordon" {
		env["GORDON_SHADOW"] = "false"
		env["GORDON_VIKUNJA_WRITE_TOKEN_FILE"] = file(t, "w", "wtoken\n")
		env["GORDON_NTFY_URL"] = "https://ntfy.example/"
		env["GORDON_NTFY_TOPIC"] = "pr-ready"
		env["GORDON_NTFY_TOKEN_FILE"] = file(t, "n", "ntoken\n")
	}
	return env
}

func TestReadinessLoads(t *testing.T) {
	c, err := LoadWorker(envOf(readinessEnv(t, "gordon")))
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
	if _, err := LoadWorker(envOf(readinessEnv(t, "shadow"))); err != nil {
		t.Fatalf("shadow rule: %v", err)
	}
	// A legacy worker needs none of it.
	c, err = LoadWorker(envOf(workerEnv(t)))
	if err != nil || c.Readiness != (Readiness{}) {
		t.Fatalf("defaults %+v, %v", c.Readiness, err)
	}
}

func TestReadinessRefusals(t *testing.T) {
	cases := map[string]struct {
		owner  string
		change func(map[string]string)
		want   string
	}{
		"shadow without the App": {"shadow", func(e map[string]string) { delete(e, "GORDON_GITHUB_APP_DIR") },
			"ownership rule vigie/pr-readiness reads check runs: GORDON_GITHUB_APP_DIR is required"},
		"shadow without Kelos": {"shadow", func(e map[string]string) { delete(e, "GORDON_DISPATCH_CONFIG_FILE") },
			"ownership rule vigie/pr-readiness reads the Kelos Tasks: GORDON_DISPATCH_CONFIG_FILE is required"},
		"shadow without the scan": {"shadow", func(e map[string]string) { e["GORDON_READINESS_RECONCILE_INTERVAL"] = "0" },
			"GORDON_READINESS_RECONCILE_INTERVAL > 0 is required"},
		"owned without pr_readiness": {"gordon", func(e map[string]string) {
			e["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r2.json", `{"policy":{}}`)
		}, "policy pr_readiness must be true"},
		"owned without the ntfy token": {"gordon", func(e map[string]string) { delete(e, "GORDON_NTFY_TOKEN_FILE") },
			"GORDON_NTFY_URL, GORDON_NTFY_TOPIC and GORDON_NTFY_TOKEN_FILE are required"},
		"owned without a Vikunja writer": {"gordon", func(e map[string]string) { delete(e, "GORDON_VIKUNJA_WRITE_TOKEN_FILE") },
			"ownership rule vigie/pr-readiness writes to Vikunja: GORDON_VIKUNJA_WRITE_TOKEN_FILE is required"},
		"cleartext ntfy": {"gordon", func(e map[string]string) { e["GORDON_NTFY_URL"] = "http://ntfy.example" },
			"GORDON_NTFY_URL: want an https URL"},
		"unknown merge method": {"shadow", func(e map[string]string) {
			e["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r3.json", `{"policy":{"readiness":{"merge_method":"fast-forward"}}}`)
		}, `merge_method "fast-forward"`},
		"negative settle": {"shadow", func(e map[string]string) {
			e["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r4.json", `{"policy":{"readiness":{"settle_seconds":-1}}}`)
		}, "settle_seconds and missing_check_minutes must not be negative"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := readinessEnv(t, c.owner)
			c.change(env)
			if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("LoadWorker = %v, want %q", err, c.want)
			}
		})
	}
	// ntfy off in the policy needs no publisher.
	env := readinessEnv(t, "gordon")
	delete(env, "GORDON_NTFY_TOKEN_FILE")
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "r5.json", `{"policy":{"pr_readiness":true,"readiness":{"notify":false}}}`)
	if _, err := LoadWorker(envOf(env)); err != nil {
		t.Fatalf("ntfy off: %v", err)
	}
}
