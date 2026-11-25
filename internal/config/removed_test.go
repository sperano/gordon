package config

import (
	"slices"
	"strings"
	"testing"
)

// TestWorkerRequiresItsWriters: Gordon executes every family, so the
// worker refuses to start without what their writes and reads need.
func TestWorkerRequiresItsWriters(t *testing.T) {
	cases := map[string]string{
		"GORDON_DISPATCH_CONFIG_FILE":     "GORDON_DISPATCH_CONFIG_FILE is required: nothing could be submitted",
		"GORDON_VIKUNJA_WRITE_TOKEN_FILE": "GORDON_VIKUNJA_WRITE_TOKEN_FILE is required: Gordon's Vikunja writes need it",
		"GORDON_GITHUB_APP_DIR":           "GORDON_GITHUB_APP_DIR is required: Gordon's GitHub writes and check run reads need it",
		"GORDON_SILLAGE_API_URL":          "GORDON_SILLAGE_API_URL is required: the completion cost notices read it",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			env := workerEnv(t)
			delete(env, name)
			if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("LoadWorker = %v, want %q", err, want)
			}
		})
	}
}

// TestWorkerIgnoresRemovedSettings: a deployment still setting shadow
// mode or the ownership table, whatever their values (the ownership
// file is not even opened), starts and lists them as deprecated.
func TestWorkerIgnoresRemovedSettings(t *testing.T) {
	cases := map[string]map[string]string{
		"previous chart": {"GORDON_SHADOW": "false", "GORDON_OWNERSHIP_FILE": file(t, "o.json",
			`{"rules":[{"project":"*","family":"pr-review","owner":"gordon","evidence":["replay-parity"]}]}`)},
		"shadow on":         {"GORDON_SHADOW": "true"},
		"invalid values":    {"GORDON_SHADOW": "maybe", "GORDON_SHADOW_WINDOW": "soon", "GORDON_OWNERSHIP_FILE": "/nonexistent"},
		"unknown rule keys": {"GORDON_OWNERSHIP_FILE": file(t, "o2.json", `{"rules":[],"cutover":true}`)},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			env := workerEnv(t)
			var want []string
			for _, k := range RemovedWorkerVars {
				if v, ok := set[k]; ok {
					env[k] = v
					want = append(want, k)
				}
			}
			c, err := LoadWorker(envOf(env))
			if err != nil {
				t.Fatalf("LoadWorker = %v, want the removed settings ignored", err)
			}
			if !slices.Equal(c.Deprecated, want) {
				t.Fatalf("deprecated %v, want %v", c.Deprecated, want)
			}
		})
	}
}
