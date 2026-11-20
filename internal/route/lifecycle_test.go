package route_test

import (
	"slices"
	"testing"

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
				if q.Depth != depthOf(c.d)+1 {
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

func depthOf(d event.Delivery) int {
	ev, _ := event.Parse(d)
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
	done := &event.Task{ID: 5, ProjectID: 136, Done: true, Buckets: []string{"To-Do"}}
	for _, fd := range r.Decide(updated(t, task(5, 136, nil)), route.State{Task: done}, led).Families {
		if fd.Family == run.VikunjaDragCancel {
			t.Fatalf("a done task in To-Do was cancelled: %+v", fd)
		}
	}
}

// TestChainRedecisionConverges pins that deciding a chain again after its
// writes landed (the subtask assigned and in Doing, the notice posted)
// writes nothing more: Gordon's own task.updated events end there.
func TestChainRedecisionConverges(t *testing.T) {
	r := router()
	parent := &event.Task{ID: 40, ProjectID: 136, Labels: []string{"chain", "in progress"}, Assignees: []string{"claude-opus"},
		Buckets: []string{"Doing"}, Relations: event.Relations{Subtasks: []event.Related{{ID: 41}}}}
	sub := event.Task{ID: 41, ProjectID: 136, Assignees: []string{"claude-opus"}, Buckets: []string{"Doing"}, Index: 1}
	d := r.Decide(updated(t, task(40, 136, []string{"chain"})), route.State{Task: parent, Subtasks: []event.Task{sub}}, replay.Ledger{})
	if keys := replay.WriteKeys(d); len(keys) != 0 {
		t.Errorf("a second decision wrote %v", keys)
	}
}
