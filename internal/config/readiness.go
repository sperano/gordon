package config

import (
	"slices"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
)

// Readiness is the worker's PR readiness configuration (Gordon 08c).
type Readiness struct {
	// ReconcileInterval paces the readiness backstop (0: off), the legacy
	// */5 cron scan and event waiter's polling.
	ReconcileInterval time.Duration
	// The ntfy publisher of a ready head (prReadiness.notify): an HTTPS
	// base URL, the topic and the bearer token.
	NtfyURL, NtfyTopic, NtfyToken string
}

func (r *reader) readiness() Readiness {
	c := Readiness{ReconcileInterval: r.offDuration("GORDON_READINESS_RECONCILE_INTERVAL"),
		NtfyURL: r.optionalURL("GORDON_NTFY_URL"), NtfyTopic: r.str("GORDON_NTFY_TOPIC", ""),
		NtfyToken: r.secretFile("GORDON_NTFY_TOKEN_FILE")}
	if c.NtfyURL != "" && !strings.HasPrefix(c.NtfyURL, "https://") {
		r.fail("GORDON_NTFY_URL: want an https URL: the token must not travel in clear")
	}
	return c
}

// readinessRules are the ownership rules that let Gordon decide the
// readiness family (shadow or Gordon).
func readinessRules(c Worker) []rollout.Rule {
	var out []rollout.Rule
	for _, rule := range c.Ownership.Rules {
		if rule.Family == run.PRReadiness && rule.Owner != rollout.Legacy {
			out = append(out, rule)
		}
	}
	return out
}

// checkReadiness refuses a worker that would decide the readiness family
// without what its gate reads (the App, the only token with Checks read,
// and the Kelos Task list) or without its backstop scan, or execute it
// without the ntfy publisher its policy turns on; and a merge method
// GitHub does not take.
func (r *reader) checkReadiness(c Worker) {
	p := c.Routing.Policy.Readiness
	if p.MergeMethod != "" && !slices.Contains(profile.MergeMethods, p.MergeMethod) {
		r.fail("GORDON_ROUTING_CONFIG_FILE policy readiness merge_method %q: want one of %v", p.MergeMethod, profile.MergeMethods)
	}
	if p.SettleSeconds < 0 || p.MissingCheckMinutes < 0 {
		r.fail("GORDON_ROUTING_CONFIG_FILE policy readiness settle_seconds and missing_check_minutes must not be negative")
	}
	for _, rule := range readinessRules(c) {
		r.checkReadinessRule(c, rule)
	}
}

func (r *reader) checkReadinessRule(c Worker, rule rollout.Rule) {
	scope := "ownership rule " + rule.Project + "/" + string(rule.Family)
	if c.GitHub.AppDir == "" {
		r.fail("%s reads check runs: GORDON_GITHUB_APP_DIR is required", scope)
	}
	if c.Kelos == nil {
		r.fail("%s reads the Kelos Tasks: GORDON_DISPATCH_CONFIG_FILE is required", scope)
	}
	if c.Readiness.ReconcileInterval <= 0 {
		r.fail("%s: GORDON_READINESS_RECONCILE_INTERVAL > 0 is required (the scan that finds ready heads)", scope)
	}
	if c.Shadow || rule.Owner != rollout.Gordon {
		return
	}
	if !c.Routing.Policy.PRReadiness {
		r.fail("%s: GORDON_ROUTING_CONFIG_FILE policy pr_readiness must be true (the pr-open notice's promise)", scope)
	}
	n := c.Readiness
	if c.Routing.Policy.Readiness.Notify && (n.NtfyURL == "" || n.NtfyTopic == "" || n.NtfyToken == "") {
		r.fail("%s notifies: GORDON_NTFY_URL, GORDON_NTFY_TOPIC and GORDON_NTFY_TOKEN_FILE are required", scope)
	}
}
