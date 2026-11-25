package config

import (
	"time"

	"github.com/sperano/gordon/internal/costs"
)

// Costs is the completion cost notices' configuration (Gordon 12).
type Costs struct {
	// APIURL is Sillage's read API base (its in-cluster Service);
	// LoadWorker requires it.
	APIURL string
	// Wait paces the checks of a notice waiting for a final estimate;
	// Deadline is how long after its run finished it waits before it is
	// posted marked not final; Refresh paces a live notice's checks;
	// LiveFor is how long after its last revision it is kept current.
	Wait, Deadline, Refresh, LiveFor time.Duration
}

func (r *reader) costs() Costs {
	c := Costs{APIURL: r.optionalURL("GORDON_SILLAGE_API_URL"),
		Wait:     r.duration("GORDON_COST_WAIT", costs.DefaultWait),
		Deadline: r.duration("GORDON_COST_DEADLINE", costs.DefaultDeadline),
		Refresh:  r.duration("GORDON_COST_REFRESH", costs.DefaultRefresh),
		LiveFor:  r.duration("GORDON_COST_LIVE_FOR", costs.DefaultLiveFor)}
	if c.Wait >= c.Deadline {
		r.fail("GORDON_COST_WAIT %s must be below GORDON_COST_DEADLINE %s", c.Wait, c.Deadline)
	}
	return c
}

// checkCosts refuses a worker without Sillage's API to read: its runs'
// cost notices would wait for ever.
func (r *reader) checkCosts(c Worker) {
	if c.Costs.APIURL == "" {
		r.fail("GORDON_SILLAGE_API_URL is required: the completion cost notices read it")
	}
}
