package worker

import (
	"context"
	"errors"
	"time"

	"github.com/sperano/gordon/internal/action"
)

// errNoProviderExecutor defers an action whose provider has no writer in
// this release (a GitHub write before Gordon 08).
var errNoProviderExecutor = errors.New(ReasonNoExecutor)

// ByProvider routes one action kind to the executor of its target's
// provider (vikunja, github, kelos); a provider without one defers the
// action for Delay, so it stays queued for the release that brings it.
type ByProvider struct {
	Executors map[string]Executor
	Delay     time.Duration
}

func (b ByProvider) executor(a action.Action) (Executor, error) {
	if ex, ok := b.Executors[provider(a.Target)]; ok {
		return ex, nil
	}
	return nil, Deferred(errNoProviderExecutor, "", b.Delay)
}

// Execute performs the action with its provider's executor.
func (b ByProvider) Execute(ctx context.Context, a action.Action) error {
	ex, err := b.executor(a)
	if err != nil {
		return err
	}
	return ex.Execute(ctx, a)
}

// Recover recovers with the provider's executor; without one nothing was
// ever written, so nothing is applied.
func (b ByProvider) Recover(ctx context.Context, a action.Action) (bool, error) {
	ex, err := b.executor(a)
	if err != nil {
		return false, nil
	}
	return ex.Recover(ctx, a)
}
