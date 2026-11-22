package config

import (
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/costs"
)

func TestCostsDefaultsAndOverrides(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := Costs{Wait: costs.DefaultWait, Deadline: costs.DefaultDeadline, Refresh: costs.DefaultRefresh,
		LiveFor: costs.DefaultLiveFor}
	if c.Costs != want {
		t.Fatalf("cost defaults %+v, want %+v", c.Costs, want)
	}
	env := workerEnv(t)
	env["GORDON_SILLAGE_API_URL"] = "http://sillage.sillage.svc/"
	env["GORDON_COST_WAIT"] = "1m"
	env["GORDON_COST_DEADLINE"] = "2h"
	env["GORDON_COST_REFRESH"] = "5m"
	env["GORDON_COST_LIVE_FOR"] = "72h"
	if c, err = LoadWorker(envOf(env)); err != nil {
		t.Fatal(err)
	}
	want = Costs{APIURL: "http://sillage.sillage.svc", Wait: time.Minute, Deadline: 2 * time.Hour,
		Refresh: 5 * time.Minute, LiveFor: 72 * time.Hour}
	if c.Costs != want {
		t.Fatalf("cost overrides %+v, want %+v", c.Costs, want)
	}
	if !strings.Contains(c.String(), " sillage_api=true") {
		t.Errorf("String() %q lacks sillage_api=true", c.String())
	}
}

func TestCostsRefusals(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"wait past the deadline": {map[string]string{"GORDON_COST_WAIT": "2h", "GORDON_COST_DEADLINE": "1h"},
			"GORDON_COST_WAIT 2h0m0s must be below GORDON_COST_DEADLINE 1h0m0s"},
		"bad refresh":  {map[string]string{"GORDON_COST_REFRESH": "often"}, `GORDON_COST_REFRESH="often"`},
		"relative URL": {map[string]string{"GORDON_SILLAGE_API_URL": "sillage"}, "GORDON_SILLAGE_API_URL"},
	}
	for label, c := range cases {
		env := workerEnv(t)
		for k, v := range c.env {
			env[k] = v
		}
		if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: LoadWorker = %v, want %q", label, err, c.want)
		}
	}
}

// TestCostFamilyNeedsSillage: owning vikunja-cost needs Sillage's API
// (and, as every Vikunja family, the write token); in shadow neither.
func TestCostFamilyNeedsSillage(t *testing.T) {
	rule := `{"rules":[{"project":"gordon","family":"vikunja-cost","owner":"gordon",
		"evidence":["replay-parity","shadow-parity","rollback-tested","legacy-disabled"]}]}`
	env := workerEnv(t)
	env["GORDON_SHADOW"] = "false"
	env["GORDON_DISPATCH_CONFIG_FILE"] = file(t, "d.json", `{"version":"d-1","namespace":"kelos-agents"}`)
	env["GORDON_OWNERSHIP_FILE"] = file(t, "o.json", rule)
	_, err := LoadWorker(envOf(env))
	for _, want := range []string{"ownership rule gordon/vikunja-cost reads Sillage: GORDON_SILLAGE_API_URL is required",
		"ownership rule gordon/vikunja-cost writes to Vikunja: GORDON_VIKUNJA_WRITE_TOKEN_FILE is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadWorker = %v, want %q", err, want)
		}
	}
	env["GORDON_SILLAGE_API_URL"] = "http://sillage.sillage.svc"
	if _, err := LoadWorker(envOf(env)); err == nil || strings.Contains(err.Error(), "GORDON_SILLAGE_API_URL") {
		t.Errorf("with the API: LoadWorker = %v, want only the write token refused", err)
	}
	env["GORDON_SHADOW"] = "true"
	delete(env, "GORDON_SILLAGE_API_URL")
	if _, err := LoadWorker(envOf(env)); err != nil {
		t.Errorf("shadow: LoadWorker = %v", err)
	}
}
