package config

import (
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/upstream"
)

func file(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func envOf(m map[string]string) Getenv { return func(k string) string { return m[k] } }

// readinessRouting is the routing file of workerEnv: the defaults with
// pr_readiness on, which the worker requires.
const readinessRouting = `{"policy":{"pr_readiness":true}}`

// workerEnv is the smallest environment a worker loads with: its reads,
// the dispatch configuration and every writer and reader its families
// need.
func workerEnv(t *testing.T) map[string]string {
	return map[string]string{
		"GORDON_DATABASE_URL_FILE":            file(t, "db", "postgres://worker:pw@db/gordon\n"),
		"GORDON_VIKUNJA_URL":                  "https://vikunja.example/api/v1/",
		"GORDON_VIKUNJA_TOKEN_FILE":           file(t, "vt", "vtoken\n"),
		"GORDON_GITHUB_TOKEN_FILE":            file(t, "gt", "gtoken"),
		"GORDON_WORKER_ID":                    "worker-0",
		"GORDON_DISPATCH_CONFIG_FILE":         file(t, "dispatch.json", `{"version":"d-1","namespace":"kelos-agents"}`),
		"GORDON_ROUTING_CONFIG_FILE":          file(t, "routing.json", readinessRouting),
		"GORDON_VIKUNJA_WRITE_TOKEN_FILE":     file(t, "wt", "wtoken\n"),
		"GORDON_GITHUB_APP_DIR":               "/var/run/github-app",
		"GORDON_SILLAGE_API_URL":              "http://sillage.sillage.svc",
		"GORDON_READINESS_RECONCILE_INTERVAL": "1m",
		"GORDON_NTFY_URL":                     "https://ntfy.example/",
		"GORDON_NTFY_TOPIC":                   "pr-ready",
		"GORDON_NTFY_TOKEN_FILE":              file(t, "nt", "ntoken\n"),
	}
}

func TestWorkerDefaults(t *testing.T) {
	env := workerEnv(t)
	c, err := LoadWorker(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.Deprecated != nil {
		t.Fatalf("deprecated settings %v, want none", c.Deprecated)
	}
	if c.DatabaseURL != "postgres://worker:pw@db/gordon" || c.VikunjaTokenFile != env["GORDON_VIKUNJA_TOKEN_FILE"] ||
		c.GitHubTokenFile != env["GORDON_GITHUB_TOKEN_FILE"] {
		t.Fatalf("secrets read as %q %q %q, want the database URL's content and the tokens' paths",
			c.DatabaseURL, c.VikunjaTokenFile, c.GitHubTokenFile)
	}
	if c.VikunjaURL != "https://vikunja.example/api/v1" || c.GitHubAPIURL != DefaultGitHubAPIURL {
		t.Fatalf("urls %q %q", c.VikunjaURL, c.GitHubAPIURL)
	}
	if c.Concurrency != DefaultConcurrency || c.LeaseTTL != DefaultLeaseTTL || c.MaxAttempts != DefaultMaxAttempts ||
		c.Backoff.Base != DefaultRetryBase || c.Backoff.Max != DefaultRetryMax || c.ProjectLimit != DefaultProjectLimit ||
		c.ReconcileInterval != DefaultReconcileInterval || c.BodyRetention != DefaultBodyRetention ||
		c.AdminAddr != DefaultAdminAddr || c.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if !reflect.DeepEqual(c.UpstreamLimit, map[string]int{upstream.ProviderVikunja: DefaultUpstreamLimit,
		upstream.ProviderGitHub: DefaultUpstreamLimit}) {
		t.Fatalf("upstream limits %v", c.UpstreamLimit)
	}
	if c.Routing.Version != profile.Defaults().Version {
		t.Fatal("routing defaults not applied")
	}
	if c.Kelos == nil {
		t.Fatal("Kelos not configured from the dispatch configuration")
	}
	if strings.Contains(c.String(), "pw") || strings.Contains(c.String(), "token") {
		t.Fatalf("String() leaks a secret: %s", c)
	}
}

func TestWorkerOverrides(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_UPSTREAM_LIMITS"] = "github=9, vikunja=1"
	env["GORDON_RETRY_BASE"] = "1s"
	env["GORDON_RETRY_MAX"] = "3s"
	env["GORDON_LOG_LEVEL"] = "debug"
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "routing.json", `{"version":"custom-1","policy":{"pr_readiness":true}}`)
	env["GORDON_DISPATCH_CONFIG_FILE"] = file(t, "dispatch.json", `{"version":"d-1","namespace":"kelos-agents"}`)
	env["GORDON_KELOS_API_URL"] = "https://k8s.example:6443/"
	env["GORDON_KELOS_TOKEN_FILE"] = "/run/token"
	env["GORDON_KELOS_CA_FILE"] = "/run/ca.crt"
	env["GORDON_KELOS_BUSY_DELAY"] = "7s"
	c, err := LoadWorker(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	want := Kelos{Dispatch: dispatch.Config{Version: "d-1", Namespace: "kelos-agents"}, APIURL: "https://k8s.example:6443",
		TokenFile: "/run/token", CAFile: "/run/ca.crt", BusyDelay: 7 * time.Second}
	if c.Kelos == nil || !reflect.DeepEqual(*c.Kelos, want) {
		t.Fatalf("kelos %+v, want %+v", c.Kelos, want)
	}
	if !strings.Contains(c.String(), "dispatch=d-1 kelos=https://k8s.example:6443") {
		t.Fatalf("String() %s lacks the dispatch version and Kelos URL", c)
	}
	if c.UpstreamLimit["github"] != 9 || c.UpstreamLimit["vikunja"] != 1 ||
		c.Backoff.Base != time.Second || c.Backoff.Max != 3*time.Second || c.LogLevel != slog.LevelDebug ||
		c.Routing.Version != "custom-1" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestWorkerRefusesUnknownAutoAssignVariant(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "routing.json",
		`{"projects":[{"name":"hollingsworth","auto_assign_variant":"missing"}]}`)
	_, err := LoadWorker(envOf(env))
	if err == nil || !strings.Contains(err.Error(), `project hollingsworth: auto_assign_variant "missing" is not in variants`) {
		t.Fatalf("error %v", err)
	}
}

func TestWorkerOpsMode(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "routing.json", `{"ops_profile":"cluster-operator",`+
		`"profiles":{"cluster-operator":{"service_account":"kelos-cluster-operator",`+
		`"capabilities":["github-write","github-readonly","cluster-read","cluster-write"]}},`+
		`"policy":{"pr_readiness":true,"ops_label":"ops","ops_owner":"eric","ops_projects":["hollingsworth"]}}`)
	c, err := LoadWorker(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Routing.Policy; !p.OpsCovers("hollingsworth") || p.OpsOwner != "eric" || c.Routing.OpsProfile != "cluster-operator" {
		t.Fatalf("ops policy %+v profile %q", p, c.Routing.OpsProfile)
	}
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "routing.json", `{"policy":{"ops_label":"ops","ops_projects":["hollingsworth"]}}`)
	if _, err := LoadWorker(envOf(env)); err == nil || !strings.Contains(err.Error(),
		"GORDON_ROUTING_CONFIG_FILE: policy ops_owner is required with ops_label") {
		t.Fatalf("ops without an owner: %v", err)
	}
}

func TestWorkerRefusals(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"missing database":      {map[string]string{"GORDON_DATABASE_URL_FILE": ""}, "GORDON_DATABASE_URL_FILE is required"},
		"unreadable token":      {map[string]string{"GORDON_GITHUB_TOKEN_FILE": "/nonexistent"}, "GORDON_GITHUB_TOKEN_FILE: open /nonexistent"},
		"empty token":           {map[string]string{"GORDON_VIKUNJA_TOKEN_FILE": "EMPTY"}, "GORDON_VIKUNJA_TOKEN_FILE: "},
		"whitespace-only token": {map[string]string{"GORDON_VIKUNJA_TOKEN_FILE": "BLANK"}, "GORDON_VIKUNJA_TOKEN_FILE: "},
		"bad duration":          {map[string]string{"GORDON_LEASE_TTL": "soon"}, `GORDON_LEASE_TTL="soon"`},
		"zero concurrency":      {map[string]string{"GORDON_WORKER_CONCURRENCY": "0"}, `GORDON_WORKER_CONCURRENCY="0"`},
		"unknown provider":      {map[string]string{"GORDON_UPSTREAM_LIMITS": "gitlab=2"}, `"gitlab=2"`},
		"zero limit":            {map[string]string{"GORDON_UPSTREAM_LIMITS": "vikunja=0"}, `"vikunja=0": want vikunja=<n>`},
		"limit without n":       {map[string]string{"GORDON_UPSTREAM_LIMITS": "github"}, `"github": want vikunja=<n>`},
		"provider twice":        {map[string]string{"GORDON_UPSTREAM_LIMITS": "github=2,github=9"}, "GORDON_UPSTREAM_LIMITS: github given twice"},
		"zero lease":            {map[string]string{"GORDON_LEASE_TTL": "0s"}, `GORDON_LEASE_TTL="0s"`},
		"trailing json":         {map[string]string{"GORDON_ROUTING_CONFIG_FILE": "TRAILING"}, "content after the JSON value"},
		"null json":             {map[string]string{"GORDON_ROUTING_CONFIG_FILE": "NULL"}, "null is not a configuration"},
		"root vikunja url":      {map[string]string{"GORDON_VIKUNJA_URL": "/"}, `GORDON_VIKUNJA_URL="": want an http or https URL`},
		"no scheme":             {map[string]string{"GORDON_GITHUB_API_URL": "api.github.com"}, `GORDON_GITHUB_API_URL="api.github.com"`},
		"ftp url":               {map[string]string{"GORDON_VIKUNJA_URL": "ftp://vikunja"}, `GORDON_VIKUNJA_URL="ftp://vikunja"`},
		"url with query":        {map[string]string{"GORDON_VIKUNJA_URL": "https://v/api/v1?x=1"}, "GORDON_VIKUNJA_URL: want a base URL without a query or fragment"},
		"url with bare ?":       {map[string]string{"GORDON_VIKUNJA_URL": "https://v/api/v1?"}, "without a query or fragment"},
		"url fragment":          {map[string]string{"GORDON_GITHUB_API_URL": "https://api.github.com#top"}, "GORDON_GITHUB_API_URL: want a base URL without a query or fragment"},
		"url bare #":            {map[string]string{"GORDON_GITHUB_API_URL": "https://api.github.com#"}, "without a query or fragment"},
		"backoff inverted":      {map[string]string{"GORDON_RETRY_BASE": "1m", "GORDON_RETRY_MAX": "1s"}, "below GORDON_RETRY_BASE"},
		"lease too short":       {map[string]string{"GORDON_LEASE_TTL": "5s"}, "must exceed GORDON_UPSTREAM_TIMEOUT"},
		"bad log level":         {map[string]string{"GORDON_LOG_LEVEL": "loud"}, "GORDON_LOG_LEVEL"},
		"no vikunja url":        {map[string]string{"GORDON_VIKUNJA_URL": ""}, "GORDON_VIKUNJA_URL is required"},
		"unknown field":         {map[string]string{"GORDON_ROUTING_CONFIG_FILE": "ROUTING"}, "unknown field"},
		"no dispatch":           {map[string]string{"GORDON_DISPATCH_CONFIG_FILE": ""}, "GORDON_DISPATCH_CONFIG_FILE is required"},
		"dispatch namespace": {map[string]string{"GORDON_DISPATCH_CONFIG_FILE": "DISPATCH_NS"},
			`namespace "default", want "kelos-agents"`},
		"dispatch unknown field": {map[string]string{"GORDON_DISPATCH_CONFIG_FILE": "DISPATCH_FIELD"}, `unknown field "namespaec"`},
		"bad kelos url": {map[string]string{"GORDON_DISPATCH_CONFIG_FILE": "DISPATCH", "GORDON_KELOS_API_URL": "k8s"},
			`GORDON_KELOS_API_URL="k8s"`},
		"kelos url userinfo": {map[string]string{"GORDON_DISPATCH_CONFIG_FILE": "DISPATCH",
			"GORDON_KELOS_API_URL": "https://admin:hunter2@k8s:6443"}, "GORDON_KELOS_API_URL: want a URL without user information"},
		"vikunja url userinfo": {map[string]string{"GORDON_VIKUNJA_URL": "https://bot@v/api/v1"},
			"GORDON_VIKUNJA_URL: want a URL without user information"},
		"userinfo, wrong scheme": {map[string]string{"GORDON_VIKUNJA_URL": "ftp://admin:hunter2@v"},
			"GORDON_VIKUNJA_URL: want a URL without user information"},
		"userinfo, no host": {map[string]string{"GORDON_VIKUNJA_URL": "https://admin:hunter2@"},
			"GORDON_VIKUNJA_URL: want a URL without user information"},
		"userinfo, no scheme": {map[string]string{"GORDON_VIKUNJA_URL": "admin:hunter2@v"},
			"GORDON_VIKUNJA_URL: want an http or https URL with a host"},
		"userinfo, no scheme or port": {map[string]string{"GORDON_GITHUB_API_URL": "admin:hunter2@api.github.com"},
			"GORDON_GITHUB_API_URL: want an http or https URL with a host"},
		"secret in query": {map[string]string{"GORDON_VIKUNJA_URL": "https://v/api/v1?sig=hunter2"},
			"GORDON_VIKUNJA_URL: want a base URL without a query or fragment"},
		"secret in fragment": {map[string]string{"GORDON_GITHUB_API_URL": "https://api.github.com#t=hunter2"},
			"GORDON_GITHUB_API_URL: want a base URL without a query or fragment"},
		"at sign in query": {map[string]string{"GORDON_VIKUNJA_URL": "https://v/api/v1?u=admin:hunter2@x"},
			"GORDON_VIKUNJA_URL: want a base URL without a query or fragment"},
		"userinfo, unparseable": {map[string]string{"GORDON_VIKUNJA_URL": "https://admin:hunter2@v:port"},
			"GORDON_VIKUNJA_URL: want an http or https URL with a host"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := workerEnv(t)
			for k, v := range tc.set {
				switch v {
				case "EMPTY":
					v = file(t, "empty", "\n")
				case "BLANK":
					v = file(t, "blank", "   \t  \n")
				case "ROUTING":
					v = file(t, "r.json", `{"versoin":"x"}`)
				case "NULL":
					v = file(t, "n.json", " null\n")
				case "DISPATCH":
					v = file(t, "d.json", `{"version":"d-1","namespace":"kelos-agents"}`)
				case "DISPATCH_NS":
					v = file(t, "d.json", `{"version":"d-1","namespace":"default"}`)
				case "DISPATCH_FIELD":
					v = file(t, "d.json", `{"namespaec":"kelos-agents"}`)
				case "TRAILING":
					v = file(t, "t.json", `{"version":"a"} {"version":"b"}`)
				}
				env[k] = v
			}
			_, err := LoadWorker(envOf(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadWorker = %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("LoadWorker = %v: the URL's password reached the error", err)
			}
		})
	}
}

func TestReceiver(t *testing.T) {
	key := file(t, "vk", "k")
	env := map[string]string{"GORDON_DATABASE_URL_FILE": file(t, "db", "postgres://r@db/g"),
		"GORDON_VIKUNJA_KEY_FILE": key, "GORDON_MAX_IN_FLIGHT": "8"}
	c, err := LoadReceiver(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.KeyFiles, map[event.Source]string{event.SourceVikunja: key}) || c.MaxInFlight != 8 ||
		c.RequestDeadline != DefaultRequestDeadline || c.ShutdownGrace != DefaultShutdownGrace ||
		c.ListenAddr != DefaultListenAddr || len(c.TrustedProxies) != 0 {
		t.Fatalf("receiver config %+v", c)
	}
	over := maps.Clone(env)
	over["GORDON_REQUEST_DEADLINE"], over["GORDON_SHUTDOWN_GRACE"] = "7s", "3s"
	over["GORDON_TRUSTED_PROXIES"] = "10.42.0.0/16,192.0.2.7"
	if c, err = LoadReceiver(envOf(over)); err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("192.0.2.7/32")}
	if c.RequestDeadline != 7*time.Second || c.ShutdownGrace != 3*time.Second ||
		!reflect.DeepEqual([]netip.Prefix(c.TrustedProxies), want) {
		t.Fatalf("receiver overrides %+v", c)
	}
	for name, tc := range map[string]struct{ k, v, want string }{
		"zero in flight": {"GORDON_MAX_IN_FLIGHT", "0", `GORDON_MAX_IN_FLIGHT="0"`},
		"bad proxy":      {"GORDON_TRUSTED_PROXIES", "10.0.0.0/33", "GORDON_TRUSTED_PROXIES"},
		"empty key file": {"GORDON_VIKUNJA_KEY_FILE", "EMPTY", " is empty"},
		"key directory":  {"GORDON_VIKUNJA_KEY_FILE", "DIR", "GORDON_VIKUNJA_KEY_FILE: read "},
	} {
		bad := maps.Clone(env)
		switch tc.v {
		case "EMPTY":
			bad[tc.k] = file(t, "empty-key", "\r\n")
		case "DIR":
			bad[tc.k] = t.TempDir()
		default:
			bad[tc.k] = tc.v
		}
		if _, err := LoadReceiver(envOf(bad)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: LoadReceiver = %v, want an error containing %q", name, err, tc.want)
		}
	}
	delete(env, "GORDON_VIKUNJA_KEY_FILE")
	if _, err := LoadReceiver(envOf(env)); err == nil || !strings.Contains(err.Error(), "no source key file") {
		t.Fatalf("LoadReceiver without keys = %v", err)
	}
	env["GORDON_GITHUB_KEY_FILE"] = "/nonexistent"
	if _, err := LoadReceiver(envOf(env)); err == nil || !strings.Contains(err.Error(), "GORDON_GITHUB_KEY_FILE") {
		t.Fatalf("LoadReceiver with a missing key file = %v", err)
	}
}

func TestMigrate(t *testing.T) {
	env := map[string]string{"GORDON_DATABASE_URL_FILE": file(t, "db", "postgres://owner@db/g"),
		"GORDON_RECEIVER_ROLE": "gordon_receiver", "GORDON_WORKER_ROLE": "gordon_worker"}
	c, err := LoadMigrate(envOf(env))
	if err != nil || c.ReceiverRole != "gordon_receiver" || c.WorkerRole != "gordon_worker" || c.DatabaseURL != "postgres://owner@db/g" {
		t.Fatalf("LoadMigrate = %+v, %v", c, err)
	}
}

func TestWorkerKelosDefaults(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kelos == nil || c.Kelos.APIURL != kelos.InClusterURL || c.Kelos.TokenFile != kelos.InClusterTokenFile ||
		c.Kelos.CAFile != kelos.InClusterCAFile || c.Kelos.BusyDelay != DefaultBusyDelay {
		t.Fatalf("kelos defaults %+v", c.Kelos)
	}
}

func TestLoadWorkerWithInvalidDispatchConfigNamesOnlyTheFile(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_DISPATCH_CONFIG_FILE"] = file(t, "d.json", `{"version":"d-1","namespace":"default"}`)
	_, err := LoadWorker(envOf(env))
	if err == nil || !strings.Contains(err.Error(), `namespace "default"`) ||
		strings.Contains(err.Error(), "GORDON_DISPATCH_CONFIG_FILE is required") {
		t.Fatalf("LoadWorker = %v, want the dispatch file's error alone", err)
	}
}
