package metrics

import (
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

// Sources are the source label values.
var Sources = []string{string(event.SourceVikunja), string(event.SourceGitHub),
	string(event.SourceWikiComment), string(event.SourceImageBuild)}

// Outcomes are the outcome label values.
var Outcomes = []string{string(route.Dispatch), string(route.Act), string(route.Ignore), string(route.Reject),
	string(route.Duplicate), string(route.Stale), string(route.Conflict), string(route.Retry)}

// ActionKinds are the kind label values.
var ActionKinds = []string{string(action.SubmitTask), string(action.React), string(action.AddLabel),
	string(action.RemoveLabel), string(action.MoveBucket), string(action.MarkDone), string(action.PostNotice),
	string(action.ConvertDraft), string(action.SkippedChecks), string(action.Assign), string(action.SyncLabel),
	string(action.CancelRun)}

// QueueStates are the states each queue reports, zero when empty; any
// other state is summed under Other.
var QueueStates = map[string][]string{
	store.QueueInbox:   {"pending", "failed", Other},
	store.QueueActions: {store.ActionPending, store.ActionRunning, store.ActionAmbiguous, store.ActionFailed, Other},
}

// Families are the family label values, None included.
func Families() []string {
	out := []string{None}
	for _, f := range run.Families {
		out = append(out, string(f))
	}
	return out
}

// SetQueues sets the queue gauges from stats; a known state missing from
// stats is zero, and unknown states add up under Other.
func (m *Metrics) SetQueues(stats []store.QueueStat) {
	for q, states := range QueueStates {
		for _, s := range states {
			m.QueueDepth.WithLabelValues(q, s).Set(0)
			m.QueueOldest.WithLabelValues(q, s).Set(0)
		}
	}
	type gauge struct {
		count  int64
		oldest time.Duration
	}
	sums := map[[2]string]gauge{}
	for _, st := range stats {
		states, ok := QueueStates[st.Queue]
		if !ok {
			continue
		}
		k := [2]string{st.Queue, Bound(st.State, states)}
		g := sums[k]
		g.count += st.Count
		g.oldest = max(g.oldest, st.OldestAge)
		sums[k] = g
	}
	for k, g := range sums {
		m.QueueDepth.WithLabelValues(k[0], k[1]).Set(float64(g.count))
		m.QueueOldest.WithLabelValues(k[0], k[1]).Set(g.oldest.Seconds())
	}
}
