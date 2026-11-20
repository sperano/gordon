package app

import (
	"testing"

	"github.com/sperano/gordon/internal/config"
)

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
