package worker

import (
	"context"
	"log/slog"
	"time"
)

// TickFunc claims and processes one batch: ctx bounds claiming, work the
// processing of what it claimed. It returns how many rows it claimed.
type TickFunc func(ctx, work context.Context) (int, error)

// Loop runs tick until ctx ends, waiting idle after a batch that claimed
// nothing or failed. Once ctx ends it claims nothing more; the batch in
// flight keeps running for up to grace, then its work context is
// cancelled (its rows' leases expire and another worker recovers them).
// Loop returns when that batch has returned.
func Loop(ctx context.Context, name string, grace, idle time.Duration, log *slog.Logger, tick TickFunc) {
	work, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(grace, cancel) })
	defer stop()
	for ctx.Err() == nil {
		n, err := tick(ctx, work)
		if err != nil && ctx.Err() == nil {
			log.Warn("claim failed", "loop", name, "error", err)
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(idle):
		}
	}
	log.Info("stopped", "loop", name)
}

// Every runs fn now and then every interval until ctx ends.
func Every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
