package config

import (
	"strings"
	"testing"
	"time"
)

const gordonOwnsImplement = `{"rules":[{"project":"gordon","family":"vikunja-implement","owner":"gordon",
	"evidence":["replay-parity","shadow-parity","rollback-tested","legacy-disabled"]}]}`

func TestLifecycleDefaults(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	l := c.Lifecycle
	if l.WriteToken != "" || l.ReconcileInterval != 0 || l.Lookback != DefaultFollowUpLookback ||
		l.RedispatchAfter != DefaultRedispatchAfter || l.LucarneURL != "" || l.SillageURL != "" {
		t.Fatalf("lifecycle defaults %+v: no writer, no reconciliation, the legacy window", l)
	}
}

func TestLifecycleOverrides(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_VIKUNJA_WRITE_TOKEN_FILE"] = file(t, "w", "wtoken\n")
	env["GORDON_VIKUNJA_RECONCILE_INTERVAL"] = "5m"
	env["GORDON_FOLLOWUP_LOOKBACK"] = "6h"
	env["GORDON_FOLLOWUP_REDISPATCH_AFTER"] = "1m"
	env["GORDON_LUCARNE_URL"] = "https://lucarne.example/"
	env["GORDON_LUCARNE_CLUSTER"] = "hollingsworth"
	env["GORDON_SILLAGE_URL"] = "https://sillage.example"
	c, err := LoadWorker(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	l := c.Lifecycle
	if l.WriteToken != "wtoken" || l.ReconcileInterval != 5*time.Minute || l.Lookback != 6*time.Hour ||
		l.RedispatchAfter != time.Minute || l.LucarneURL != "https://lucarne.example" || l.LucarneCluster != "hollingsworth" ||
		l.SillageURL != "https://sillage.example" {
		t.Fatalf("lifecycle overrides %+v", l)
	}
	if !strings.Contains(c.String(), "vikunja_writer=true vikunja_reconcile=5m0s") || strings.Contains(c.String(), "wtoken") {
		t.Fatalf("start log %q", c.String())
	}
}

func TestLifecycleReconcileIntervalZeroIsOff(t *testing.T) {
	for _, v := range []string{"0", "0s"} {
		env := workerEnv(t)
		env["GORDON_VIKUNJA_RECONCILE_INTERVAL"] = v
		c, err := LoadWorker(envOf(env))
		if err != nil {
			t.Fatalf("GORDON_VIKUNJA_RECONCILE_INTERVAL=%s: %v", v, err)
		}
		if c.Lifecycle.ReconcileInterval != 0 {
			t.Fatalf("GORDON_VIKUNJA_RECONCILE_INTERVAL=%s: interval %s, want off", v, c.Lifecycle.ReconcileInterval)
		}
	}
}

func TestLifecycleOwnedWithAWriterLoads(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_SHADOW"] = "false"
	env["GORDON_DISPATCH_CONFIG_FILE"] = file(t, "d.json", `{"version":"d-1","namespace":"kelos-agents"}`)
	env["GORDON_OWNERSHIP_FILE"] = file(t, "o.json", gordonOwnsImplement)
	env["GORDON_VIKUNJA_WRITE_TOKEN_FILE"] = file(t, "w", "wtoken\n")
	c, err := LoadWorker(envOf(env))
	if err != nil {
		t.Fatalf("executing worker with a Gordon rule and a writer: %v", err)
	}
	if c.Shadow || c.Lifecycle.WriteToken != "wtoken" {
		t.Fatalf("shadow %v, write token %q: want executing with the writer", c.Shadow, c.Lifecycle.WriteToken)
	}
}

func TestLifecycleRefusals(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"window inverted": {map[string]string{"GORDON_FOLLOWUP_LOOKBACK": "1m", "GORDON_FOLLOWUP_REDISPATCH_AFTER": "2m"},
			"must be below GORDON_FOLLOWUP_LOOKBACK"},
		"negative reconcile interval": {map[string]string{"GORDON_VIKUNJA_RECONCILE_INTERVAL": "-1m"},
			`GORDON_VIKUNJA_RECONCILE_INTERVAL="-1m"`},
		"bad lucarne url": {map[string]string{"GORDON_LUCARNE_URL": "lucarne"}, `GORDON_LUCARNE_URL="lucarne"`},
		"owned without a writer": {map[string]string{"GORDON_SHADOW": "false", "GORDON_DISPATCH_CONFIG_FILE": "DISPATCH",
			"GORDON_OWNERSHIP_FILE": "OWNED"}, "vikunja-implement writes to Vikunja: GORDON_VIKUNJA_WRITE_TOKEN_FILE is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := workerEnv(t)
			for k, v := range tc.set {
				switch v {
				case "DISPATCH":
					v = file(t, "d.json", `{"version":"d-1","namespace":"kelos-agents"}`)
				case "OWNED":
					v = file(t, "o.json", gordonOwnsImplement)
				}
				env[k] = v
			}
			if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadWorker = %v, want %q", err, tc.want)
			}
		})
	}
	// In shadow the same rule executes nothing, so needs no writer.
	env := workerEnv(t)
	env["GORDON_OWNERSHIP_FILE"] = file(t, "o.json", gordonOwnsImplement)
	if _, err := LoadWorker(envOf(env)); err != nil {
		t.Fatalf("shadow worker with a Gordon rule: %v", err)
	}
}
