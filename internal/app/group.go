package app

import (
	"context"
	"sync"
)

// group runs functions until all return; the first error cancels the
// others' context and is returned.
type group struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	err    error
}

func newGroup(ctx context.Context) *group {
	ctx, cancel := context.WithCancel(ctx)
	return &group{ctx: ctx, cancel: cancel}
}

func (g *group) Go(fn func(ctx context.Context) error) {
	g.wg.Go(func() {
		if err := fn(g.ctx); err != nil {
			g.once.Do(func() { g.err = err })
			g.cancel()
		}
	})
}

func (g *group) Wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
}
