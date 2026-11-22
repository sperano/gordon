package config

import (
	"slices"
	"time"

	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
)

// Lifecycle is the worker's Vikunja lifecycle configuration (Gordon 06).
type Lifecycle struct {
	// WriteToken is the Vikunja token the lifecycle's writes use (labels,
	// buckets, assignments, reactions, notices), "" for none: Vikunja
	// actions then stay queued, deferred, as before this stage.
	WriteToken string
	// ReconcileInterval paces the Vikunja reconciliation (0: off).
	ReconcileInterval time.Duration
	// Lookback and RedispatchAfter bound the owner requests recovered.
	Lookback, RedispatchAfter time.Duration
	// The start and failure notices' private links.
	LucarneURL, LucarneCluster, SillageURL string
}

// Lifecycle defaults.
const (
	DefaultFollowUpLookback = 12 * time.Hour
	DefaultRedispatchAfter  = 2 * time.Minute
)

// vikunjaWriting are the families whose actions write to Vikunja.
var vikunjaWriting = []run.Family{run.VikunjaImplement, run.VikunjaDiscuss, run.VikunjaFollowUp,
	run.VikunjaFollowUpAnswer, run.VikunjaFollowUpDiscuss, run.VikunjaDragCancel, run.VikunjaDoneCleanup,
	run.VikunjaChain, run.VikunjaFailure, run.PROpen, run.MergeClose, run.LimitRestart}

func (r *reader) lifecycle() Lifecycle {
	l := Lifecycle{WriteToken: r.secretFile("GORDON_VIKUNJA_WRITE_TOKEN_FILE"),
		ReconcileInterval: r.offDuration("GORDON_VIKUNJA_RECONCILE_INTERVAL"),
		Lookback:          r.duration("GORDON_FOLLOWUP_LOOKBACK", DefaultFollowUpLookback),
		RedispatchAfter:   r.duration("GORDON_FOLLOWUP_REDISPATCH_AFTER", DefaultRedispatchAfter),
		LucarneURL:        r.optionalURL("GORDON_LUCARNE_URL"),
		LucarneCluster:    r.str("GORDON_LUCARNE_CLUSTER", ""),
		SillageURL:        r.optionalURL("GORDON_SILLAGE_URL")}
	if l.RedispatchAfter >= l.Lookback {
		r.fail("GORDON_FOLLOWUP_REDISPATCH_AFTER %s must be below GORDON_FOLLOWUP_LOOKBACK %s", l.RedispatchAfter, l.Lookback)
	}
	return l
}

// offDuration is duration for an interval that zero (or unset) turns off.
func (r *reader) offDuration(name string) time.Duration {
	if d, err := time.ParseDuration(r.str(name, "")); err == nil && d == 0 {
		return 0
	}
	return r.duration(name, 0)
}

// optionalURL is baseURL for a URL that may be unset ("").
func (r *reader) optionalURL(name string) string {
	if r.str(name, "") == "" {
		return ""
	}
	return r.baseURL(name, "")
}

// checkWriter refuses a worker that would execute a Gordon-owned family
// writing to Vikunja without a token to write with (its writes would
// queue for ever while legacy, switched off for that family, writes none)
// or without the policy its notices key on.
func (r *reader) checkWriter(c Worker) {
	if c.Shadow {
		return
	}
	policyChecked := false
	for _, rule := range c.Ownership.Rules {
		if rule.Owner != rollout.Gordon || !slices.Contains(vikunjaWriting, rule.Family) {
			continue
		}
		if c.Lifecycle.WriteToken == "" {
			r.fail("ownership rule %s/%s writes to Vikunja: GORDON_VIKUNJA_WRITE_TOKEN_FILE is required", rule.Project, rule.Family)
		}
		if !policyChecked {
			r.checkLifecyclePolicy(c.Routing.Policy, rule)
			policyChecked = true
		}
	}
}

// checkLifecyclePolicy refuses an empty policy field a Gordon-owned
// Vikunja family needs: Gordon's own user (without it no notice is
// trusted, so each is posted again), the web base its links and chain
// notices match on, and the reaction that withdraws a request.
func (r *reader) checkLifecyclePolicy(p profile.Policy, rule rollout.Rule) {
	for _, f := range []struct{ name, value string }{
		{"vikunja_service_user", p.VikunjaServiceUser},
		{"vikunja_web_url", p.VikunjaWebURL},
		{"cancel_reaction", p.CancelReaction},
	} {
		if f.value == "" {
			r.fail("ownership rule %s/%s writes to Vikunja: GORDON_ROUTING_CONFIG_FILE policy %s is empty", rule.Project, rule.Family, f.name)
		}
	}
}
