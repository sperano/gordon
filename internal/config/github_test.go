package config

import (
	"strings"
	"testing"
	"time"
)

const gordonOwnsPRFollowUp = `{"rules":[{"project":"gordon","family":"pr-followup","owner":"gordon",
	"evidence":["replay-parity","shadow-parity","rollback-tested","legacy-disabled"]}]}`

func TestGitHubDefaults(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	g := c.GitHub
	if g.AppDir != "" || g.ReconcileInterval != 0 || g.Lookback != DefaultGitHubLookback || g.Grace != DefaultGitHubGrace ||
		g.ConflictInterval != 0 {
		t.Fatalf("github defaults %+v: no App, no reconciliation, a day's window", g)
	}
	if p := c.Routing.Policy; p.MaxRoundsPerPR != 50 || p.SuggestionsMaxRounds != 4 {
		t.Fatalf("round caps %d/%d, want values-hollingsworth's 50/4", p.MaxRoundsPerPR, p.SuggestionsMaxRounds)
	}
}

func TestGitHubOverrides(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_GITHUB_APP_DIR"] = "/var/run/github-app"
	env["GORDON_GITHUB_RECONCILE_INTERVAL"] = "5m"
	env["GORDON_GITHUB_RECONCILE_LOOKBACK"] = "6h"
	env["GORDON_GITHUB_RECONCILE_GRACE"] = "1m"
	env["GORDON_CONFLICT_RECONCILE_INTERVAL"] = "10m"
	c, err := LoadWorker(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	g := c.GitHub
	if g.AppDir != "/var/run/github-app" || g.ReconcileInterval != 5*time.Minute || g.Lookback != 6*time.Hour || g.Grace != time.Minute ||
		g.ConflictInterval != 10*time.Minute {
		t.Fatalf("github overrides %+v", g)
	}
	if !strings.Contains(c.String(), "github_writer=true github_reconcile=5m0s conflict_reconcile=10m0s") {
		t.Fatalf("start log %q", c.String())
	}
}

func TestGitHubRefusals(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"window inverted": {map[string]string{"GORDON_GITHUB_RECONCILE_LOOKBACK": "1m", "GORDON_GITHUB_RECONCILE_GRACE": "1m"},
			"GORDON_GITHUB_RECONCILE_GRACE 1m0s must be below GORDON_GITHUB_RECONCILE_LOOKBACK 1m0s"},
		"negative reconcile interval": {map[string]string{"GORDON_GITHUB_RECONCILE_INTERVAL": "-1m"},
			`GORDON_GITHUB_RECONCILE_INTERVAL="-1m"`},
		"negative conflict interval": {map[string]string{"GORDON_CONFLICT_RECONCILE_INTERVAL": "-1m"},
			`GORDON_CONFLICT_RECONCILE_INTERVAL="-1m"`},
		"negative round cap": {map[string]string{"GORDON_ROUTING_CONFIG_FILE": `ROUTING:{"policy":{"max_rounds_per_pr":-1}}`},
			"max_rounds_per_pr and suggestions_max_rounds must not be negative"},
		"negative suggestions cap": {map[string]string{"GORDON_ROUTING_CONFIG_FILE": `ROUTING:{"policy":{"suggestions_max_rounds":-1}}`},
			"max_rounds_per_pr and suggestions_max_rounds must not be negative"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := workerEnv(t)
			setFiles(t, env, tc.set)
			if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadWorker = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestGitHubOwnedFamilyNeedsTheApp(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_SHADOW"] = "false"
	env["GORDON_DISPATCH_CONFIG_FILE"] = file(t, "d.json", `{"version":"d-1","namespace":"kelos-agents"}`)
	env["GORDON_OWNERSHIP_FILE"] = file(t, "o.json", gordonOwnsPRFollowUp)
	want := "ownership rule gordon/pr-followup writes to GitHub: GORDON_GITHUB_APP_DIR is required"
	if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadWorker = %v, want %q", err, want)
	}
	env["GORDON_GITHUB_APP_DIR"] = "/var/run/github-app"
	if _, err := LoadWorker(envOf(env)); err != nil {
		t.Fatalf("executing worker with the App: %v", err)
	}
	// In shadow the same rule executes nothing, so needs no App.
	env = workerEnv(t)
	env["GORDON_OWNERSHIP_FILE"] = file(t, "o.json", gordonOwnsPRFollowUp)
	if _, err := LoadWorker(envOf(env)); err != nil {
		t.Fatalf("shadow worker with a Gordon rule: %v", err)
	}
}
