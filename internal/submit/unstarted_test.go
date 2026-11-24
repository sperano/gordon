package submit

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/store"
)

// clock is the fixed time the unstarted Tasks are aged against.
var clock = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// phaseless is a Task the controller never gave a phase, created age ago.
func phaseless(name, variant string, age time.Duration) kelos.Task {
	t := running(name, variant, "vigie", "", "")
	t.Status = nil
	t.Metadata.CreationTimestamp = clock.Add(-age).Format(time.RFC3339)
	return t
}

func TestUnstarted(t *testing.T) {
	cases := map[string]struct {
		task kelos.Task
		want bool
	}{
		"past the grace":   {phaseless("a", "claude-opus", UnstartedGrace+time.Minute), true},
		"at the grace":     {phaseless("a", "claude-opus", UnstartedGrace), true},
		"within the grace": {phaseless("a", "claude-opus", UnstartedGrace-time.Second), false},
		"has a phase": {func() kelos.Task {
			t := phaseless("a", "claude-opus", time.Hour)
			t.Status = &kelos.Status{Phase: kelos.PhasePending}
			return t
		}(), false},
		"empty status phase": {func() kelos.Task {
			t := phaseless("a", "claude-opus", time.Hour)
			t.Status = &kelos.Status{}
			return t
		}(), true},
		"no creation time": {func() kelos.Task {
			t := phaseless("a", "claude-opus", time.Hour)
			t.Metadata.CreationTimestamp = ""
			return t
		}(), false},
	}
	for name, c := range cases {
		if got := unstarted(c.task, clock); got != c.want {
			t.Errorf("%s: unstarted = %v, want %v", name, got, c.want)
		}
	}
}

// TestVariantLimitSkipsUnstartedTasks: Tasks the controller refused (no
// phase past the grace) hold no slot and are reported; a Task still
// within the grace counts, as it may be starting.
func TestVariantLimitSkipsUnstartedTasks(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{Default: 1})
	e.exec.Now = func() time.Time { return clock }
	e.kelos.Put(phaseless("refused-1", "claude-opus", time.Hour))
	e.kelos.Put(phaseless("refused-2", "claude-sonnet", time.Hour))
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	if st, _, msg := e.action(t, name); st != store.ActionDone || len(e.kelos.Creates()) != 1 {
		t.Fatalf("with only refused Tasks: %s %q, %d creates; want submitted", st, msg, len(e.kelos.Creates()))
	}
	if got := testutil.ToFloat64(e.metrics.UnstartedTasks); got != 2 {
		t.Fatalf("unstarted gauge %v, want 2", got)
	}
}

func TestVariantLimitCountsStartingTasks(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{Default: 1})
	e.exec.Now = func() time.Time { return clock }
	e.kelos.Put(phaseless("starting-1", "claude-opus", time.Minute))
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	if st, _, msg := e.action(t, name); st != store.ActionPending || msg != "variant claude-opus runs 1 of 1" {
		t.Fatalf("behind a starting Task: %s %q; want queued", st, msg)
	}
	if got := testutil.ToFloat64(e.metrics.UnstartedTasks); got != 0 {
		t.Fatalf("unstarted gauge %v, want 0", got)
	}
}
