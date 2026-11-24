package submit

import (
	"time"

	"github.com/sperano/gordon/internal/kelos"
)

// UnstartedGrace is how long a Task may go without a status.phase before
// the variant limit stops counting it: the Kelos controller sets a phase
// within seconds of accepting a Task, so one still without a phase this
// long after its creation was refused (or the controller is down) and
// holds no agent. Counting it would hold its variant's slot until a human
// deletes it, starving every later run of the variant.
const UnstartedGrace = 15 * time.Minute

// unstarted reports a Task that has had no phase for UnstartedGrace or
// more. A Task whose creation time cannot be read is not: it counts, as
// before, rather than risk running past the limit.
func unstarted(t kelos.Task, now time.Time) bool {
	if t.Phase() != "" {
		return false
	}
	created, err := time.Parse(time.RFC3339, t.Metadata.CreationTimestamp)
	return err == nil && now.Sub(created) >= UnstartedGrace
}

// now is the executor's clock.
func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
