package config

import (
	"time"

	"github.com/sperano/gordon/internal/profile"
)

// Lifecycle is the worker's Vikunja lifecycle configuration (Gordon 06).
type Lifecycle struct {
	// WriteTokenFile is the file of the Vikunja token the lifecycle's
	// writes use (labels, buckets, assignments, reactions, notices), ""
	// for none: Vikunja actions then stay queued, deferred, as before
	// this stage. Re-read on every write, so a rotated Secret applies
	// without a restart.
	WriteTokenFile string
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

func (r *reader) lifecycle() Lifecycle {
	l := Lifecycle{WriteTokenFile: r.optionalSecretFile("GORDON_VIKUNJA_WRITE_TOKEN_FILE"),
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

// checkWriter refuses a worker without a token to write to Vikunja with
// (its claims, labels and notices would queue for ever) or without the
// policy its notices key on.
func (r *reader) checkWriter(c Worker) {
	if c.Lifecycle.WriteTokenFile == "" {
		r.fail("GORDON_VIKUNJA_WRITE_TOKEN_FILE is required: Gordon's Vikunja writes need it")
	}
	r.checkLifecyclePolicy(c.Routing.Policy)
}

// checkLifecyclePolicy refuses an empty policy field the Vikunja families
// need: Gordon's own user (without it no notice is trusted, so each is
// posted again), the web base its links and chain notices match on, and
// the reaction that withdraws a request.
func (r *reader) checkLifecyclePolicy(p profile.Policy) {
	for _, f := range []struct{ name, value string }{
		{"vikunja_service_user", p.VikunjaServiceUser},
		{"vikunja_web_url", p.VikunjaWebURL},
		{"cancel_reaction", p.CancelReaction},
	} {
		if f.value == "" {
			r.fail("GORDON_ROUTING_CONFIG_FILE policy %s is empty: Gordon's Vikunja notices need it", f.name)
		}
	}
}
