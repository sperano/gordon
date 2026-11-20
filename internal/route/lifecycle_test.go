package route_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

func synthetic(t *testing.T, id, project int64, s map[string]any) event.Delivery {
	return signed(t, event.SourceVikunja, nil, map[string]any{"event_name": "task.updated",
		"data": map[string]any{"task": map[string]any{"id": id, "project_id": project}, "gordon": s}})
}

func requeued(d route.Decision) []int64 {
	var out []int64
	for _, q := range d.Requeue {
		out = append(out, q.TaskID)
	}
	return out
}

// TestRequeueDepth pins the fan-out bound: a webhook requeues parents and,
// when done, the tasks it blocked; a requeued decision only its parents;
// one at MaxRequeueDepth (and every reconciliation) nothing, so a cycle
// of relations cannot loop.
func TestRequeueDepth(t *testing.T) {
	r := router()
	st := event.Task{ID: 5, ProjectID: 136, Done: true, Buckets: []string{"Done"},
		Relations: event.Relations{Parents: []event.Related{{ID: 7, ProjectID: 136}},
			Blocking: []event.Related{{ID: 8, ProjectID: 139}, {ID: 7, ProjectID: 136}}}}
	cases := []struct {
		name string
		d    event.Delivery
		want []int64
	}{
		{"webhook", updated(t, task(5, 136, nil)), []int64{7, 8}},
		{"depth 1", synthetic(t, 5, 136, map[string]any{"requeue": "k", "depth": 1}), []int64{7}},
		{"depth 2", synthetic(t, 5, 136, map[string]any{"requeue": "k", "depth": route.MaxRequeueDepth}), nil},
		{"reconcile", synthetic(t, 5, 136, map[string]any{"reconcile": "x", "depth": route.MaxRequeueDepth}), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.Decide(c.d, route.State{Task: &st}, replay.Ledger{})
			if got := requeued(d); !slices.Equal(got, c.want) {
				t.Errorf("requeued %v, want %v", got, c.want)
			}
			for _, q := range d.Requeue {
				if q.Depth != depthOf(t, c.d)+1 {
					t.Errorf("requeue of %d at depth %d", q.TaskID, q.Depth)
				}
			}
		})
	}
	open := st
	open.Done = false
	if got := requeued(r.Decide(updated(t, task(5, 136, nil)), route.State{Task: &open}, replay.Ledger{})); !slices.Equal(got, []int64{7}) {
		t.Errorf("an open task requeued %v, want only its parent", got)
	}
}

func depthOf(t *testing.T, d event.Delivery) int {
	t.Helper()
	ev, err := event.Parse(d)
	if err != nil {
		t.Fatalf("parsing the fixture delivery: %v", err)
	}
	if s := ev.Vikunja.Synthetic; s != nil {
		return s.Depth
	}
	return 0
}

// TestDragCancelNeedsTheBacklog pins that only a card in the backlog and
// in no Doing bucket is a drag: one in both (two views) stops nothing.
func TestDragCancelNeedsTheBacklog(t *testing.T) {
	r := router()
	led := replay.Ledger{Runs: []route.RunRecord{{Name: "vikunja-sillage-claude-opus-5", Active: true, Exists: true,
		Family: run.VikunjaImplement, TaskID: 5}}}
	both := &event.Task{ID: 5, ProjectID: 136, Labels: []string{"in progress"}, Assignees: []string{"claude-opus"},
		Buckets: []string{"To-Do", "Doing"}}
	for _, fd := range r.Decide(updated(t, task(5, 136, nil)), route.State{Task: both}, led).Families {
		if fd.Family == run.VikunjaDragCancel {
			t.Fatalf("a card still in Doing in another view was cancelled: %+v", fd)
		}
	}
	dragged := &event.Task{ID: 5, ProjectID: 136, Labels: []string{"in progress"}, Assignees: []string{"claude-opus"},
		Buckets: []string{"To-Do"}}
	fd, ok := familyOf(r.Decide(updated(t, task(5, 136, nil)), route.State{Task: dragged}, led), run.VikunjaDragCancel)
	if !ok || fd.Outcome != route.Act || len(fd.Actions) == 0 || fd.Actions[0].Kind != action.CancelRun ||
		fd.Actions[0].Detail != "vikunja-sillage-claude-opus-5" {
		t.Fatalf("a card dragged to the backlog: %+v (found %v), want its run cancelled", fd, ok)
	}
	done := &event.Task{ID: 5, ProjectID: 136, Done: true, Buckets: []string{"To-Do"}}
	for _, fd := range r.Decide(updated(t, task(5, 136, nil)), route.State{Task: done}, led).Families {
		if fd.Family == run.VikunjaDragCancel {
			t.Fatalf("a done task in To-Do was cancelled: %+v", fd)
		}
	}
}

// TestChainRedecisionConverges pins that deciding a chain again after its
// writes landed (the subtask assigned and in Doing) writes nothing more:
// Gordon's own task.updated events end there. The same chain before the
// subtask started writes its start.
func TestChainRedecisionConverges(t *testing.T) {
	r := router()
	parent := &event.Task{ID: 40, ProjectID: 136, Labels: []string{"chain", "in progress"}, Assignees: []string{"claude-opus"},
		Buckets: []string{"Doing"}, Relations: event.Relations{Subtasks: []event.Related{{ID: 41}}}}
	sub := event.Task{ID: 41, ProjectID: 136, Assignees: []string{"claude-opus"}, Buckets: []string{"Doing"}, Index: 1}
	d := r.Decide(updated(t, task(40, 136, []string{"chain"})), route.State{Task: parent, Subtasks: []event.Task{sub}}, replay.Ledger{})
	if keys := replay.WriteKeys(d); len(keys) != 0 {
		t.Errorf("a second decision wrote %v", keys)
	}
	unstarted := event.Task{ID: 41, ProjectID: 136, Buckets: []string{"To-Do"}, Index: 1}
	d = r.Decide(updated(t, task(40, 136, []string{"chain"})), route.State{Task: parent, Subtasks: []event.Task{unstarted}}, replay.Ledger{})
	want := []string{"assign:vikunja/task/41:claude-opus", "move-bucket:vikunja/task/41:Doing",
		"post-notice:vikunja/task/40:chain-started:41", "sync-label:vikunja/task/41:auto-merge"}
	if keys := replay.WriteKeys(d); !slices.Equal(keys, want) {
		t.Errorf("the unstarted chain wrote %v, want %v", keys, want)
	}
}

// chainStartedComment is Gordon's trusted chain-started notice for sub.
func chainStartedComment(sub int64) event.TaskComment {
	return event.TaskComment{ID: 1, Author: "gordon", Body: `<p>NEXT UP IN THE CHAIN: <a href="https://vikunja.spe.quebec/tasks/` +
		itoa(sub) + `">#` + itoa(sub) + `</a>.</p><!-- kelos:chain-started -->`}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestChainStoppedSubtaskHoldsWhileBlocked pins that a subtask the chain
// started and the owner stopped holds the chain even once it is blocked:
// the next subtask does not start past it.
func TestChainStoppedSubtaskHoldsWhileBlocked(t *testing.T) {
	r := router()
	parent := &event.Task{ID: 40, ProjectID: 136, Labels: []string{"chain", "in progress"}, Assignees: []string{"claude-opus"},
		Buckets: []string{"Doing"}, Relations: event.Relations{Subtasks: []event.Related{{ID: 41}, {ID: 42}}}}
	stopped := event.Task{ID: 41, ProjectID: 136, Buckets: []string{"To-Do"}, Index: 1,
		Relations: event.Relations{BlockedBy: []event.Related{{ID: 99, ProjectID: 136}}}}
	next := event.Task{ID: 42, ProjectID: 136, Buckets: []string{"To-Do"}, Index: 2}
	st := route.State{Task: parent, Subtasks: []event.Task{stopped, next}, Comments: []event.TaskComment{chainStartedComment(41)}}
	d := r.Decide(updated(t, task(40, 136, []string{"chain"})), st, replay.Ledger{})
	want := []string{"post-notice:vikunja/task/40:chain-stopped:41"}
	if keys := replay.WriteKeys(d); !slices.Equal(keys, want) {
		t.Errorf("wrote %v, want %v", keys, want)
	}
}

// TestChainTaskReadsOnlyAStepping pins which chain parents get their
// subtasks read: only one the chain steps through (Doing, open, unheld,
// unblocked), never a done, paused, held or blocked one.
func TestChainTaskReadsOnlyAStepping(t *testing.T) {
	r := router()
	base := event.Task{ID: 40, ProjectID: 136, Labels: []string{"chain"}, Buckets: []string{"Doing"}}
	cases := []struct {
		name string
		edit func(*event.Task)
		want bool
	}{
		{"stepping", func(*event.Task) {}, true},
		{"no chain label", func(t *event.Task) { t.Labels = nil }, false},
		{"done", func(t *event.Task) { t.Done = true }, false},
		{"paused", func(t *event.Task) { t.Buckets = []string{"To-Do"} }, false},
		{"discuss", func(t *event.Task) { t.Labels = append(t.Labels, "discuss") }, false},
		{"no-agent", func(t *event.Task) { t.Labels = append(t.Labels, "no-agent") }, false},
		{"blocked", func(t *event.Task) { t.Relations.BlockedBy = []event.Related{{ID: 9}} }, false},
	}
	for _, c := range cases {
		tk := base
		tk.Labels = slices.Clone(base.Labels)
		c.edit(&tk)
		if got := r.ChainTask(tk); got != c.want {
			t.Errorf("%s: ChainTask = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestUnconfiguredBlockerDoneRequeues pins that a blocker done in an
// unconfigured project is read and requeues the configured tasks it
// blocked, while an open one there is neither read nor requeued.
func TestUnconfiguredBlockerDoneRequeues(t *testing.T) {
	r := router()
	sent := task(5, 99, nil)
	sent["done"] = true
	ev, err := event.Parse(updated(t, sent))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Reads(ev); got != (route.Reads{Task: 5}) {
		t.Errorf("reads %+v, want the done task alone", got)
	}
	fresh := &event.Task{ID: 5, ProjectID: 99, Done: true,
		Relations: event.Relations{Blocking: []event.Related{{ID: 8, ProjectID: 136}, {ID: 9, ProjectID: 98}}}}
	d := r.Decide(updated(t, sent), route.State{Task: fresh}, replay.Ledger{})
	expect(t, d, route.Ignore, route.ReasonProjectNotConfigured)
	if got := requeued(d); !slices.Equal(got, []int64{8}) {
		t.Errorf("requeued %v, want the configured task it blocked", got)
	}
	open, err := event.Parse(updated(t, task(5, 99, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Reads(open); got != (route.Reads{}) {
		t.Errorf("an open unconfigured task reads %+v, want nothing", got)
	}
	if got := requeued(r.Decide(updated(t, task(5, 99, nil)), route.State{}, replay.Ledger{})); got != nil {
		t.Errorf("an unread unconfigured task requeued %v", got)
	}
}

// familyOf finds the decision's family f.
func familyOf(d route.Decision, f run.Family) (route.FamilyDecision, bool) {
	for _, fd := range d.Families {
		if fd.Family == f {
			return fd, true
		}
	}
	return route.FamilyDecision{}, false
}
