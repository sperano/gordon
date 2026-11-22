package config

import (
	"time"

	"github.com/sperano/gordon/internal/restart"
)

// Restart is the limit restart pass's configuration (limit_reaper.py's
// MAX_RESTARTS, RESET_GRACE_SECONDS, FALLBACK_DELAY_SECONDS).
type Restart struct {
	// Max bounds the restarts of one chain.
	Max int
	// Grace is waited after the limit's reset; Fallback after the
	// Task's completion when no reset time is known.
	Grace, Fallback time.Duration
}

func (r *reader) restart() Restart {
	return Restart{Max: r.integer("GORDON_LIMIT_RESTART_MAX", restart.DefaultMaxRestarts, 1),
		Grace:    r.duration("GORDON_LIMIT_RESTART_GRACE", restart.DefaultGrace),
		Fallback: r.duration("GORDON_LIMIT_RESTART_FALLBACK", restart.DefaultFallback)}
}
