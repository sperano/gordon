package config

import (
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/restart"
)

func TestRestartDefaultsAndOverrides(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Restart{Max: restart.DefaultMaxRestarts, Grace: restart.DefaultGrace, Fallback: restart.DefaultFallback}); c.Restart != want {
		t.Fatalf("restart defaults %+v, want %+v", c.Restart, want)
	}
	env := workerEnv(t)
	env["GORDON_LIMIT_RESTART_MAX"] = "5"
	env["GORDON_LIMIT_RESTART_GRACE"] = "30s"
	env["GORDON_LIMIT_RESTART_FALLBACK"] = "2h"
	if c, err = LoadWorker(envOf(env)); err != nil {
		t.Fatal(err)
	}
	if want := (Restart{Max: 5, Grace: 30 * time.Second, Fallback: 2 * time.Hour}); c.Restart != want {
		t.Fatalf("restart overrides %+v, want %+v", c.Restart, want)
	}
}

func TestRestartRefusals(t *testing.T) {
	cases := map[string]struct{ name, value, want string }{
		"no restarts":       {"GORDON_LIMIT_RESTART_MAX", "0", `GORDON_LIMIT_RESTART_MAX="0": want an integer >= 1`},
		"negative grace":    {"GORDON_LIMIT_RESTART_GRACE", "-1s", `GORDON_LIMIT_RESTART_GRACE="-1s"`},
		"fallback not time": {"GORDON_LIMIT_RESTART_FALLBACK", "soon", `GORDON_LIMIT_RESTART_FALLBACK="soon"`},
	}
	for label, c := range cases {
		env := workerEnv(t)
		env[c.name] = c.value
		if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: LoadWorker = %v, want %q", label, err, c.want)
		}
	}
}

// TestNewFamiliesNeedTheirWriters: limit restarts and auto-assignment write
// to Vikunja; conflict runs write to GitHub.
func TestNewFamiliesNeedTheirWriters(t *testing.T) {
	owns := func(family string) string {
		return `{"rules":[{"project":"gordon","family":"` + family + `","owner":"gordon",
			"evidence":["replay-parity","shadow-parity","rollback-tested","legacy-disabled"]}]}`
	}
	cases := map[string]string{
		"limit-restart":       "ownership rule gordon/limit-restart writes to Vikunja: GORDON_VIKUNJA_WRITE_TOKEN_FILE is required",
		"pr-conflict":         "ownership rule gordon/pr-conflict writes to GitHub: GORDON_GITHUB_APP_DIR is required",
		"vikunja-auto-assign": "ownership rule gordon/vikunja-auto-assign writes to Vikunja: GORDON_VIKUNJA_WRITE_TOKEN_FILE is required",
	}
	for family, want := range cases {
		env := workerEnv(t)
		env["GORDON_SHADOW"] = "false"
		env["GORDON_DISPATCH_CONFIG_FILE"] = file(t, "d.json", `{"version":"d-1","namespace":"kelos-agents"}`)
		env["GORDON_OWNERSHIP_FILE"] = file(t, "o.json", owns(family))
		if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: LoadWorker = %v, want %q", family, err, want)
		}
	}
}
