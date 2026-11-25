package config

import (
	"strings"
	"testing"
	"time"
)

func TestLifecycleDefaults(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	l := c.Lifecycle
	if l.WriteTokenFile == "" || l.ReconcileInterval != 0 || l.Lookback != DefaultFollowUpLookback ||
		l.RedispatchAfter != DefaultRedispatchAfter || l.LucarneURL != "" || l.SillageURL != "" {
		t.Fatalf("lifecycle defaults %+v: the writer, no reconciliation, the default window", l)
	}
}

func TestLifecycleOverrides(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_VIKUNJA_WRITE_TOKEN_FILE"] = file(t, "w", "wtoken\n")
	writeTokenFile := env["GORDON_VIKUNJA_WRITE_TOKEN_FILE"]
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
	if l.WriteTokenFile != writeTokenFile || l.ReconcileInterval != 5*time.Minute || l.Lookback != 6*time.Hour ||
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
		"no writer": {map[string]string{"GORDON_VIKUNJA_WRITE_TOKEN_FILE": ""},
			"GORDON_VIKUNJA_WRITE_TOKEN_FILE is required: Gordon's Vikunja writes need it"},
		"no service user": {routingWith(`"vikunja_service_user":""`),
			"GORDON_ROUTING_CONFIG_FILE policy vikunja_service_user is empty"},
		"no web url":         {routingWith(`"vikunja_web_url":""`), "policy vikunja_web_url is empty"},
		"no cancel reaction": {routingWith(`"cancel_reaction":""`), "policy cancel_reaction is empty"},
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
	// A routing file overriding one policy field keeps the others.
	env := workerEnv(t)
	setFiles(t, env, routingWith(`"cancel_reaction":"x"`))
	if _, err := LoadWorker(envOf(env)); err != nil {
		t.Fatalf("worker with a complete policy: %v", err)
	}
}

// routingWith sets a routing file whose policy has pr_readiness on and
// the policy fields fields (a JSON object's members).
func routingWith(fields string) map[string]string {
	return map[string]string{"GORDON_ROUTING_CONFIG_FILE": `ROUTING:{"policy":{"pr_readiness":true,` + fields + `}}`}
}

// setFiles sets set into env, writing the file a placeholder value names:
// ROUTING:<json>.
func setFiles(t *testing.T, env, set map[string]string) {
	t.Helper()
	for k, v := range set {
		if routing, ok := strings.CutPrefix(v, "ROUTING:"); ok {
			v = file(t, "r.json", routing)
		}
		env[k] = v
	}
}
