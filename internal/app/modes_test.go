package app

import (
	"testing"

	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/metrics"
)

func TestVikunjaReaderUsesConfiguredTokenFile(t *testing.T) {
	cfg := config.Worker{VikunjaTokenFile: "/vikunja/token"}
	r := vikunjaReader(cfg, metrics.New())
	if got := r.Client.TokenFile; got != cfg.VikunjaTokenFile {
		t.Fatalf("TokenFile = %q, want %q", got, cfg.VikunjaTokenFile)
	}
}

func TestGitHubReaderUsesConfiguredTokenFile(t *testing.T) {
	cfg := config.Worker{GitHubTokenFile: "/github/token"}
	r := githubReader(cfg, metrics.New())
	if got := r.Client.TokenFile; got != cfg.GitHubTokenFile {
		t.Fatalf("TokenFile = %q, want %q", got, cfg.GitHubTokenFile)
	}
}

func TestActionProvidersBoundEveryProvider(t *testing.T) {
	const github = 2
	l := actionProviders(config.Worker{UpstreamLimit: map[string]int{"github": github}})
	for provider, limit := range map[string]int{"github": github, "kelos": config.DefaultUpstreamLimit} {
		for i := range limit {
			if !l.TryAcquire(provider) {
				t.Fatalf("%s refused slot %d of %d", provider, i+1, limit)
			}
		}
		if l.TryAcquire(provider) {
			t.Fatalf("%s took a slot over its limit %d", provider, limit)
		}
	}
}
