package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/submit"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

const testKeyBits = 2048

func appDir(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, testKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, text := range map[string]string{githubapp.AppIDFile: "1", githubapp.InstallationIDFile: "2",
		githubapp.PrivateKeyFile: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNewGitHubApp(t *testing.T) {
	cfg := config.Worker{GitHubAPIURL: "https://api.example"}
	if a, err := newGitHubApp(cfg); a != nil || err != nil {
		t.Fatalf("no App dir: %v, %v, want none", a, err)
	}
	cfg.GitHub.AppDir = appDir(t)
	a, err := newGitHubApp(cfg)
	if err != nil || a.AppID != "1" || a.InstallationID != "2" || a.BaseURL != "https://api.example" {
		t.Fatalf("App %+v, %v", a, err)
	}
	cfg.GitHub.AppDir = filepath.Join(t.TempDir(), "missing")
	if _, err := newGitHubApp(cfg); err == nil || !strings.Contains(err.Error(), "github app appID") {
		t.Fatalf("missing dir: %v", err)
	}
}

// providers are the providers a kind's executor writes to.
func providers(t *testing.T, ex worker.Executor) []string {
	t.Helper()
	bp, ok := ex.(worker.ByProvider)
	if !ok {
		t.Fatalf("executor %T is not routed by provider", ex)
	}
	return slices.Sorted(maps.Keys(bp.Executors))
}

func TestExecutorsRouteGitHubKindsToTheApp(t *testing.T) {
	cfg := config.Worker{Routing: profile.Defaults(), GitHubAPIURL: "https://api.example"}
	cfg.Lifecycle.WriteToken = "w"
	app, err := githubapp.Load(appDir(t), cfg.GitHubAPIURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[action.Kind]worker.Executor{}
	lifecycleExecutors(out, cfg, nil, &kelos.Client{}, app)
	both := []string{upstream.ProviderGitHub, upstream.ProviderVikunja}
	for kind, want := range map[action.Kind][]string{
		action.React: both, action.AddLabel: both, action.RemoveLabel: both, action.PostNotice: both,
		action.ConvertDraft: {upstream.ProviderGitHub}, action.SkippedChecks: {upstream.ProviderGitHub},
		action.MoveBucket: {upstream.ProviderVikunja},
	} {
		if got := providers(t, out[kind]); !slices.Equal(got, want) {
			t.Errorf("%s writes to %v, want %v", kind, got, want)
		}
	}
	if _, ok := out[action.SupersedeReviews].(*submit.Superseder); !ok {
		t.Errorf("supersede executor %T", out[action.SupersedeReviews])
	}
	// Without the App the GitHub kinds stay queued; without Kelos there is
	// no supersede.
	out = map[action.Kind]worker.Executor{}
	lifecycleExecutors(out, cfg, nil, nil, nil)
	if _, ok := out[action.ConvertDraft]; ok {
		t.Error("a draft executor without the App")
	}
	if _, ok := out[action.SupersedeReviews]; ok {
		t.Error("a supersede executor without Kelos")
	}
	if got := providers(t, out[action.React]); !slices.Equal(got, []string{upstream.ProviderVikunja}) {
		t.Errorf("react without the App writes to %v", got)
	}
}

func TestGitHubReconcilerIsOptIn(t *testing.T) {
	cfg := config.Worker{Routing: profile.Defaults()}
	if r := githubReconciler(cfg, nil, upstream.GitHub{}, nil, discardLog()); r != nil {
		t.Fatalf("reconciler %+v with no interval", r)
	}
	cfg.GitHub = config.GitHub{ReconcileInterval: time.Minute, Lookback: time.Hour, Grace: time.Second}
	r := githubReconciler(cfg, nil, upstream.GitHub{}, nil, discardLog())
	if r == nil || r.Lookback != time.Hour || r.Grace != time.Second || len(r.Config.Projects) == 0 {
		t.Fatalf("reconciler %+v", r)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
