package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/intake"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

// Receiver is the receiver mode's configuration.
type Receiver struct {
	Common
	ListenAddr string
	// KeyFiles maps each source to its signing key file; a source
	// without one refuses every delivery (403 signed, 401 unsigned).
	KeyFiles        map[event.Source]string
	MaxInFlight     int
	RequestDeadline time.Duration
	ShutdownGrace   time.Duration
	// TrustedProxies are the webhook ingress's addresses: a request from
	// one is charged to the client its X-Forwarded-For names.
	TrustedProxies intake.Proxies
}

// keyVars names each source's key file variable.
var keyVars = map[event.Source]string{
	event.SourceVikunja:     "GORDON_VIKUNJA_KEY_FILE",
	event.SourceGitHub:      "GORDON_GITHUB_KEY_FILE",
	event.SourceWikiComment: "GORDON_WIKI_COMMENT_KEY_FILE",
	event.SourceImageBuild:  "GORDON_IMAGE_BUILD_KEY_FILE",
}

// Receiver defaults: the legacy bounded_http caps.
const (
	DefaultMaxInFlight     = 32
	DefaultRequestDeadline = 30 * time.Second
)

// LoadReceiver reads the receiver's configuration.
func LoadReceiver(env Getenv) (Receiver, error) {
	r := &reader{env: env}
	c := Receiver{Common: r.common(), ListenAddr: r.str("GORDON_LISTEN_ADDR", DefaultListenAddr),
		KeyFiles:        map[event.Source]string{},
		MaxInFlight:     r.integer("GORDON_MAX_IN_FLIGHT", DefaultMaxInFlight, 1),
		RequestDeadline: r.duration("GORDON_REQUEST_DEADLINE", DefaultRequestDeadline),
		ShutdownGrace:   r.duration("GORDON_SHUTDOWN_GRACE", DefaultShutdownGrace)}
	proxies, err := intake.ParseProxies(r.str("GORDON_TRUSTED_PROXIES", ""))
	if err != nil {
		r.fail("GORDON_TRUSTED_PROXIES: %v", err)
	}
	c.TrustedProxies = proxies
	for src, name := range keyVars {
		path := r.str(name, "")
		if path == "" {
			continue
		}
		// Re-read on every request (rotation); a key missing at start
		// is a misconfiguration, not a rotation.
		if b, err := os.ReadFile(path); err != nil {
			r.fail("%s: %v", name, err)
		} else if strings.TrimRight(string(b), "\r\n") == "" {
			r.fail("%s: %s is empty", name, path)
		}
		c.KeyFiles[src] = path
	}
	if len(c.KeyFiles) == 0 {
		r.fail("no source key file set (GORDON_VIKUNJA_KEY_FILE, GORDON_GITHUB_KEY_FILE, ...)")
	}
	return c, r.err()
}

// Worker is the worker mode's configuration.
type Worker struct {
	Common
	// Shadow (the default) records decisions and executes none.
	Shadow    bool
	ID        string
	Routing   profile.Config
	Ownership rollout.Table

	VikunjaURL    string
	VikunjaToken  string
	GitHubAPIURL  string
	GitHubToken   string
	UpstreamLimit map[string]int

	Concurrency       int
	LeaseTTL          time.Duration
	PollInterval      time.Duration
	MaxAttempts       int
	Backoff           worker.Backoff
	ProjectLimit      int
	DeferDelay        time.Duration
	ReconcileInterval time.Duration
	BodyRetention     time.Duration
	UpstreamTimeout   time.Duration
	ShutdownGrace     time.Duration

	// Kelos is nil without a dispatch configuration: runs then carry no
	// Task and the worker can only shadow.
	Kelos *Kelos
	// Lifecycle is the Vikunja lifecycle's writer, reconciliation and links.
	Lifecycle Lifecycle
	// GitHub is the GitHub lifecycle's App and reconciliation.
	GitHub GitHub
	// Restart is the limit restart pass's.
	Restart Restart
}

// Worker defaults.
const (
	DefaultConcurrency       = 4
	DefaultLeaseTTL          = 2 * time.Minute
	DefaultPollInterval      = time.Second
	DefaultMaxAttempts       = 20
	DefaultRetryBase         = 5 * time.Second
	DefaultRetryMax          = 10 * time.Minute
	DefaultProjectLimit      = 2
	DefaultDeferDelay        = 2 * time.Second
	DefaultReconcileInterval = 30 * time.Second
	DefaultBodyRetention     = 7 * 24 * time.Hour
	DefaultUpstreamTimeout   = 10 * time.Second
	DefaultUpstreamLimit     = 4
	DefaultGitHubAPIURL      = "https://api.github.com"
)

// LoadWorker reads the worker's configuration.
func LoadWorker(env Getenv) (Worker, error) {
	r := &reader{env: env}
	host, _ := os.Hostname()
	c := Worker{Common: r.common(), Shadow: r.boolean("GORDON_SHADOW", true), ID: r.str("GORDON_WORKER_ID", host),
		Routing:       profile.Defaults(),
		VikunjaURL:    r.baseURL("GORDON_VIKUNJA_URL", ""),
		VikunjaToken:  r.requiredSecret("GORDON_VIKUNJA_TOKEN_FILE"),
		GitHubAPIURL:  r.baseURL("GORDON_GITHUB_API_URL", DefaultGitHubAPIURL),
		GitHubToken:   r.requiredSecret("GORDON_GITHUB_TOKEN_FILE"),
		UpstreamLimit: r.limits("GORDON_UPSTREAM_LIMITS"),
		Concurrency:   r.integer("GORDON_WORKER_CONCURRENCY", DefaultConcurrency, 1),
		LeaseTTL:      r.duration("GORDON_LEASE_TTL", DefaultLeaseTTL),
		PollInterval:  r.duration("GORDON_POLL_INTERVAL", DefaultPollInterval),
		MaxAttempts:   r.integer("GORDON_MAX_ATTEMPTS", DefaultMaxAttempts, 1),
		Backoff: worker.Backoff{Base: r.duration("GORDON_RETRY_BASE", DefaultRetryBase),
			Max: r.duration("GORDON_RETRY_MAX", DefaultRetryMax)},
		ProjectLimit:      r.integer("GORDON_PROJECT_LIMIT", DefaultProjectLimit, 0),
		DeferDelay:        r.duration("GORDON_DEFER_DELAY", DefaultDeferDelay),
		ReconcileInterval: r.duration("GORDON_RECONCILE_INTERVAL", DefaultReconcileInterval),
		BodyRetention:     r.duration("GORDON_BODY_RETENTION", DefaultBodyRetention),
		UpstreamTimeout:   r.duration("GORDON_UPSTREAM_TIMEOUT", DefaultUpstreamTimeout),
		ShutdownGrace:     r.duration("GORDON_SHUTDOWN_GRACE", DefaultShutdownGrace)}
	if c.ID == "" {
		r.fail("GORDON_WORKER_ID is required (no hostname)")
	}
	if c.Backoff.Max < c.Backoff.Base {
		r.fail("GORDON_RETRY_MAX %s is below GORDON_RETRY_BASE %s", c.Backoff.Max, c.Backoff.Base)
	}
	if c.LeaseTTL <= c.UpstreamTimeout {
		r.fail("GORDON_LEASE_TTL %s must exceed GORDON_UPSTREAM_TIMEOUT %s", c.LeaseTTL, c.UpstreamTimeout)
	}
	r.jsonFile("GORDON_ROUTING_CONFIG_FILE", &c.Routing)
	if p := c.Routing.Policy; p.MaxRoundsPerPR < 0 || p.SuggestionsMaxRounds < 0 {
		r.fail("GORDON_ROUTING_CONFIG_FILE policy max_rounds_per_pr and suggestions_max_rounds must not be negative")
	}
	c.Kelos = r.kelos()
	if !c.Shadow && r.str("GORDON_DISPATCH_CONFIG_FILE", "") == "" {
		r.fail("GORDON_SHADOW=false needs GORDON_DISPATCH_CONFIG_FILE: nothing could be submitted")
	}
	if r.jsonFile("GORDON_OWNERSHIP_FILE", &c.Ownership) {
		if err := c.Ownership.Validate(); err != nil {
			r.fail("GORDON_OWNERSHIP_FILE: %v", err)
		}
	}
	c.Lifecycle = r.lifecycle()
	c.GitHub = r.github()
	c.Restart = r.restart()
	r.checkWriter(c)
	r.checkGitHubWriter(c)
	return c, r.err()
}

// limits parses provider=n,provider=n; a provider not listed takes
// DefaultUpstreamLimit.
func (r *reader) limits(name string) map[string]int {
	out := map[string]int{upstream.ProviderVikunja: DefaultUpstreamLimit, upstream.ProviderGitHub: DefaultUpstreamLimit}
	v := r.str(name, "")
	if v == "" {
		return out
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		k, n, ok := strings.Cut(strings.TrimSpace(part), "=")
		limit, err := strconv.Atoi(n)
		if _, known := out[k]; !ok || !known || err != nil || limit < 1 {
			r.fail("%s: %q: want vikunja=<n>,github=<n> with n >= 1", name, part)
			continue
		}
		if seen[k] {
			r.fail("%s: %s given twice", name, k)
			continue
		}
		seen[k] = true
		out[k] = limit
	}
	return out
}

// Migrate is the migrate mode's configuration.
type Migrate struct {
	Common
	ReceiverRole string
	WorkerRole   string
}

// LoadMigrate reads the migrator's configuration: the schema owner's URL
// and the roles to grant.
func LoadMigrate(env Getenv) (Migrate, error) {
	r := &reader{env: env}
	c := Migrate{Common: r.common(), ReceiverRole: r.str("GORDON_RECEIVER_ROLE", ""),
		WorkerRole: r.str("GORDON_WORKER_ROLE", "")}
	return c, r.err()
}

// String names the mode's settings without any secret, for the start log.
func (c Worker) String() string {
	dispatchVersion, kelosURL := "none", ""
	if c.Kelos != nil {
		dispatchVersion, kelosURL = c.Kelos.Dispatch.Version, c.Kelos.APIURL
	}
	return fmt.Sprintf("shadow=%v id=%s concurrency=%d lease=%s poll=%s attempts=%d backoff=%s..%s project_limit=%d "+
		"upstream=%v reconcile=%s retention=%s config=%s rules=%d dispatch=%s kelos=%s vikunja_writer=%v "+
		"vikunja_reconcile=%s github_writer=%v github_reconcile=%s conflict_reconcile=%s", c.Shadow, c.ID, c.Concurrency,
		c.LeaseTTL, c.PollInterval, c.MaxAttempts, c.Backoff.Base, c.Backoff.Max, c.ProjectLimit, c.UpstreamLimit,
		c.ReconcileInterval, c.BodyRetention, c.Routing.Version, len(c.Ownership.Rules), dispatchVersion, kelosURL,
		c.Lifecycle.WriteToken != "", c.Lifecycle.ReconcileInterval, c.GitHub.AppDir != "", c.GitHub.ReconcileInterval,
		c.GitHub.ConflictInterval)
}
