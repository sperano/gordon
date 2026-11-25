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
	want := Costs{APIURL: "http://sillage.sillage.svc", Wait: costs.DefaultWait, Deadline: costs.DefaultDeadline,
		Refresh: costs.DefaultRefresh, LiveFor: costs.DefaultLiveFor}
	if c.Costs != want {
		t.Fatalf("cost defaults %+v, want %+v", c.Costs, want)
	}
	env := workerEnv(t)
	env["GORDON_SILLAGE_API_URL"] = "http://sillage.example/"
	env["GORDON_COST_WAIT"] = "1m"
	env["GORDON_COST_DEADLINE"] = "2h"
	env["GORDON_COST_REFRESH"] = "5m"
	env["GORDON_COST_LIVE_FOR"] = "72h"
	if c, err = LoadWorker(envOf(env)); err != nil {
		t.Fatal(err)
	}
	want = Costs{APIURL: "http://sillage.example", Wait: time.Minute, Deadline: 2 * time.Hour,
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
