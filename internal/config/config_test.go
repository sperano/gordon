package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
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

func workerEnv(t *testing.T) map[string]string {
	return map[string]string{
		"GORDON_DATABASE_URL_FILE":  file(t, "db", "postgres://worker:pw@db/gordon\n"),
		"GORDON_VIKUNJA_URL":        "https://vikunja.example/api/v1/",
		"GORDON_VIKUNJA_TOKEN_FILE": file(t, "vt", "vtoken\n"),
		"GORDON_GITHUB_TOKEN_FILE":  file(t, "gt", "gtoken"),
		"GORDON_WORKER_ID":          "worker-0",
	}
}

func TestWorkerDefaults(t *testing.T) {
	c, err := LoadWorker(envOf(workerEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Shadow {
		t.Fatal("worker executes by default; shadow must be the default")
	}
	if c.DatabaseURL != "postgres://worker:pw@db/gordon" || c.VikunjaToken != "vtoken" || c.GitHubToken != "gtoken" {
		t.Fatalf("secrets read as %q %q %q, want trimmed file contents", c.DatabaseURL, c.VikunjaToken, c.GitHubToken)
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
	if c.Routing.Version != profile.Defaults().Version || len(c.Ownership.Rules) != 0 {
		t.Fatal("routing defaults or empty ownership not applied")
	}
	if strings.Contains(c.String(), "pw") || strings.Contains(c.String(), "token") {
		t.Fatalf("String() leaks a secret: %s", c)
	}
}

func TestWorkerOverrides(t *testing.T) {
	env := workerEnv(t)
	env["GORDON_SHADOW"] = "false"
	env["GORDON_UPSTREAM_LIMITS"] = "github=9, vikunja=1"
	env["GORDON_RETRY_BASE"] = "1s"
	env["GORDON_RETRY_MAX"] = "3s"
	env["GORDON_LOG_LEVEL"] = "debug"
	env["GORDON_ROUTING_CONFIG_FILE"] = file(t, "routing.json", `{"version":"custom-1"}`)
	env["GORDON_OWNERSHIP_FILE"] = file(t, "own.json",
		`{"rules":[{"project":"*","family":"pr-review","owner":"shadow","evidence":["replay-parity"]}]}`)
	c, err := LoadWorker(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.Shadow || c.UpstreamLimit["github"] != 9 || c.UpstreamLimit["vikunja"] != 1 ||
		c.Backoff.Base != time.Second || c.Backoff.Max != 3*time.Second || c.LogLevel != slog.LevelDebug ||
		c.Routing.Version != "custom-1" || len(c.Ownership.Rules) != 1 {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestWorkerRefusals(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"missing database": {map[string]string{"GORDON_DATABASE_URL_FILE": ""}, "GORDON_DATABASE_URL_FILE is required"},
		"unreadable token": {map[string]string{"GORDON_GITHUB_TOKEN_FILE": "/nonexistent"}, "GORDON_GITHUB_TOKEN_FILE: open /nonexistent"},
		"empty token":      {map[string]string{"GORDON_VIKUNJA_TOKEN_FILE": "EMPTY"}, "GORDON_VIKUNJA_TOKEN_FILE: "},
		"bad duration":     {map[string]string{"GORDON_LEASE_TTL": "soon"}, `GORDON_LEASE_TTL="soon"`},
		"zero concurrency": {map[string]string{"GORDON_WORKER_CONCURRENCY": "0"}, `GORDON_WORKER_CONCURRENCY="0"`},
		"bad bool":         {map[string]string{"GORDON_SHADOW": "maybe"}, `GORDON_SHADOW="maybe"`},
		"unknown provider": {map[string]string{"GORDON_UPSTREAM_LIMITS": "gitlab=2"}, `"gitlab=2"`},
		"backoff inverted": {map[string]string{"GORDON_RETRY_BASE": "1m", "GORDON_RETRY_MAX": "1s"}, "below GORDON_RETRY_BASE"},
		"lease too short":  {map[string]string{"GORDON_LEASE_TTL": "5s"}, "must exceed GORDON_UPSTREAM_TIMEOUT"},
		"bad log level":    {map[string]string{"GORDON_LOG_LEVEL": "loud"}, "GORDON_LOG_LEVEL"},
		"no vikunja url":   {map[string]string{"GORDON_VIKUNJA_URL": ""}, "GORDON_VIKUNJA_URL is required"},
		"unknown field":    {map[string]string{"GORDON_ROUTING_CONFIG_FILE": "ROUTING"}, "unknown field"},
		"ungated owner":    {map[string]string{"GORDON_OWNERSHIP_FILE": "OWNERSHIP"}, "lacks gate"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := workerEnv(t)
			for k, v := range tc.set {
				switch v {
				case "EMPTY":
					v = file(t, "empty", "\n")
				case "ROUTING":
					v = file(t, "r.json", `{"versoin":"x"}`)
				case "OWNERSHIP":
					v = file(t, "o.json", `{"rules":[{"project":"*","family":"pr-review","owner":"gordon"}]}`)
				}
				env[k] = v
			}
			_, err := LoadWorker(envOf(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadWorker = %v, want an error containing %q", err, tc.want)
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
		c.RequestDeadline != DefaultRequestDeadline || c.ListenAddr != DefaultListenAddr {
		t.Fatalf("receiver config %+v", c)
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
