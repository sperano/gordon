package config

import (
	"time"
)

// GitHub is the worker's GitHub lifecycle configuration (Gordon 08).
type GitHub struct {
	// AppDir holds the GitHub App's keys (appID, installationID,
	// privateKey); LoadWorker requires it.
	AppDir string
	// ReconcileInterval paces the GitHub reconciliation (0: off).
	ReconcileInterval time.Duration
	// Lookback and Grace bound what it recovers: younger than Lookback,
	// older than Grace (the webhook may still be on its way).
	Lookback, Grace time.Duration
	// ConflictInterval paces the conflict reconciliation (0: off).
	ConflictInterval time.Duration
}

// GitHub reconciliation defaults (reconcile_merge_close.py's 24 h window,
// poll_review_bot.py's 600 s grace).
const (
	DefaultGitHubLookback = 24 * time.Hour
	DefaultGitHubGrace    = 10 * time.Minute
)

func (r *reader) github() GitHub {
	g := GitHub{AppDir: r.str("GORDON_GITHUB_APP_DIR", ""),
		ReconcileInterval: r.offDuration("GORDON_GITHUB_RECONCILE_INTERVAL"),
		Lookback:          r.duration("GORDON_GITHUB_RECONCILE_LOOKBACK", DefaultGitHubLookback),
		Grace:             r.duration("GORDON_GITHUB_RECONCILE_GRACE", DefaultGitHubGrace),
		ConflictInterval:  r.offDuration("GORDON_CONFLICT_RECONCILE_INTERVAL")}
	if g.Grace >= g.Lookback {
		r.fail("GORDON_GITHUB_RECONCILE_GRACE %s must be below GORDON_GITHUB_RECONCILE_LOOKBACK %s", g.Grace, g.Lookback)
	}
	return g
}

// checkGitHubWriter refuses a worker without the App to write to GitHub
// as (and to read check runs with): its GitHub writes would queue for ever.
func (r *reader) checkGitHubWriter(c Worker) {
	if c.GitHub.AppDir == "" {
		r.fail("GORDON_GITHUB_APP_DIR is required: Gordon's GitHub writes and check run reads need it")
	}
}
