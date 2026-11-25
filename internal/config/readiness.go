package config

import (
	"slices"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/profile"
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

// checkReadiness refuses a worker without the readiness backstop scan,
// without the ntfy publisher its policy turns on, or with pr_readiness off
// (the pr-open notice's promise); and a merge method GitHub does not take.
// The App and the Kelos Task list its gate reads are required by
// checkGitHubWriter and kelos.
func (r *reader) checkReadiness(c Worker) {
	p := c.Routing.Policy.Readiness
	if p.MergeMethod != "" && !slices.Contains(profile.MergeMethods, p.MergeMethod) {
		r.fail("GORDON_ROUTING_CONFIG_FILE policy readiness merge_method %q: want one of %v", p.MergeMethod, profile.MergeMethods)
	}
	if p.SettleSeconds < 0 || p.MissingCheckMinutes < 0 {
		r.fail("GORDON_ROUTING_CONFIG_FILE policy readiness settle_seconds and missing_check_minutes must not be negative")
	}
	if c.Readiness.ReconcileInterval <= 0 {
		r.fail("GORDON_READINESS_RECONCILE_INTERVAL > 0 is required (the scan that finds ready heads)")
	}
	if !c.Routing.Policy.PRReadiness {
		r.fail("GORDON_ROUTING_CONFIG_FILE policy pr_readiness must be true (the pr-open notice's promise)")
	}
	n := c.Readiness
	if p.Notify && (n.NtfyURL == "" || n.NtfyTopic == "" || n.NtfyToken == "") {
		r.fail("pr-readiness notifies: GORDON_NTFY_URL, GORDON_NTFY_TOPIC and GORDON_NTFY_TOKEN_FILE are required")
	}
}
