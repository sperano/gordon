package route_test

import (
	"reflect"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

const heldRun = "vikunja-5-sillage-opus"

func queuedRouter() route.Router {
	r := router()
	r.Config.Policy.QueuedLabel = "queued"
	return r
}

func queuedPolicy() profile.Policy {
	p := profile.Defaults().Policy
	p.QueuedLabel = "queued"
	return p
}

func TestHoldActions(t *testing.T) {
	h := notice.Hold{Run: heldRun, Reason: notice.HoldLimit, Variant: "claude-opus", Running: 2, Limit: 2}
	target := action.VikunjaTask(5)
	want := []action.Action{
		{Key: "add-label:vikunja/task/5:queued:" + heldRun, Kind: action.AddLabel, Target: target, Value: "queued",
			Detail: route.DetailHeld + heldRun},
		{Key: "post-notice:vikunja/task/5:queued:" + heldRun + ":limit:claude-opus:2", Kind: action.PostNotice,
			Target: target, Value: notice.Queued, Detail: heldRun + ":limit:claude-opus:2:2"},
		{Key: "remove-label:vikunja/task/5:queued:created", Kind: action.RemoveLabel, Target: target, Value: "queued",
			After: "submit-task:kelos/kelos-agents/" + heldRun},
	}
	if got := route.HoldActions(queuedPolicy(), 5, h); !reflect.DeepEqual(got, want) {
		t.Fatalf("hold actions\n%+v\nwant\n%+v", got, want)
	}
	if got := route.HoldActions(profile.Defaults().Policy, 5, h); got != nil {
		t.Fatalf("hold actions without a queued label: %+v", got)
	}
}

func TestReleaseAction(t *testing.T) {
	a, ok := route.ReleaseAction(queuedPolicy(), 5)
	want := action.Action{Key: "remove-label:vikunja/task/5:queued:released", Kind: action.RemoveLabel,
		Target: action.VikunjaTask(5), Value: "queued"}
	if !ok || a != want {
		t.Fatalf("release %+v (%v), want %+v", a, ok, want)
	}
	if _, ok := route.ReleaseAction(profile.Defaults().Policy, 5); ok {
		t.Fatal("a release without a queued label")
	}
}

func TestHeldRun(t *testing.T) {
	for detail, want := range map[string]string{route.DetailHeld + heldRun: heldRun, route.DetailClaim: "", "": ""} {
		if got := route.HeldRun(detail); got != want {
			t.Fatalf("HeldRun(%q) = %q, want %q", detail, got, want)
		}
	}
}

// queuedTask is task 5 of sillage, open, not in Doing (so no run starts).
func queuedTask(labels ...string) *event.Task {
	return &event.Task{ID: 5, ProjectID: 136, Labels: labels, Buckets: []string{"Review"}}
}

func TestQueuedCleanup(t *testing.T) {
	r := queuedRouter()
	d := updated(t, task(5, 136, nil))
	cases := []struct {
		name string
		runs []route.RunRecord
		want route.FamilyDecision
	}{
		{"no run held", []route.RunRecord{{Name: heldRun, Active: true, Exists: true, Family: run.VikunjaImplement, TaskID: 5}},
			route.FamilyDecision{Family: run.VikunjaQueued, Project: "sillage", Outcome: route.Act,
				Actions: []action.Action{{Key: "remove-label:vikunja/task/5:queued:cleared", Kind: action.RemoveLabel,
					Target: action.VikunjaTask(5), Value: "queued"}}}},
		{"a follow-up held", []route.RunRecord{{Name: "vikunja-fu-sillage-5-c9", Active: true, Family: run.VikunjaFollowUp, TaskID: 5}},
			route.FamilyDecision{Family: run.VikunjaQueued, Project: "sillage", Outcome: route.Ignore, Reason: route.ReasonRunHeld}},
		{"a review held", []route.RunRecord{{Name: "review", Active: true, Family: run.PRReview, TaskID: 5}},
			route.FamilyDecision{Family: run.VikunjaQueued, Project: "sillage", Outcome: route.Act,
				Actions: []action.Action{{Key: "remove-label:vikunja/task/5:queued:cleared", Kind: action.RemoveLabel,
					Target: action.VikunjaTask(5), Value: "queued"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fd, ok := familyOf(r.Decide(d, route.State{Task: queuedTask("queued")}, replay.Ledger{Runs: c.runs}), run.VikunjaQueued)
			if !ok || !reflect.DeepEqual(fd, c.want) {
				t.Fatalf("cleanup %+v (found %v), want %+v", fd, ok, c.want)
			}
		})
	}
	if fd, ok := familyOf(r.Decide(d, route.State{Task: queuedTask()}, replay.Ledger{}), run.VikunjaQueued); ok {
		t.Fatalf("a task without the label got a cleanup: %+v", fd)
	}
	if fd, ok := familyOf(router().Decide(d, route.State{Task: queuedTask("queued")}, replay.Ledger{}), run.VikunjaQueued); ok {
		t.Fatalf("a cleanup with no queued label configured: %+v", fd)
	}
}

// TestDragCancelTakesQueuedOff: a held run dragged back is cancelled,
// then the queued label goes; the stop notice still follows the
// cancellation alone (the task has no in-progress label).
func TestDragCancelTakesQueuedOff(t *testing.T) {
	r := queuedRouter()
	led := replay.Ledger{Runs: []route.RunRecord{{Name: heldRun, Active: true, Family: run.VikunjaImplement, TaskID: 5}}}
	dragged := &event.Task{ID: 5, ProjectID: 136, Labels: []string{"queued"}, Assignees: []string{"claude-opus"},
		Buckets: []string{"To-Do"}}
	fd, ok := familyOf(r.Decide(updated(t, task(5, 136, nil)), route.State{Task: dragged}, led), run.VikunjaDragCancel)
	cancel := "cancel-run:vikunja/task/5"
	want := []action.Action{
		{Key: cancel, Kind: action.CancelRun, Target: action.VikunjaTask(5), Detail: heldRun},
		{Key: "remove-label:vikunja/task/5:queued:stopped", Kind: action.RemoveLabel, Target: action.VikunjaTask(5),
			Value: "queued", After: cancel},
		{Key: "post-notice:vikunja/task/5:stop:" + heldRun, Kind: action.PostNotice, Target: action.VikunjaTask(5),
			Value: notice.Stop, Detail: heldRun, After: cancel},
	}
	if !ok || !reflect.DeepEqual(fd.Actions, want) {
		t.Fatalf("drag-cancel actions\n%+v\nwant\n%+v", fd.Actions, want)
	}
	dragged.Labels = nil
	fd, _ = familyOf(r.Decide(updated(t, task(5, 136, nil)), route.State{Task: dragged}, led), run.VikunjaDragCancel)
	if len(fd.Actions) != 2 || fd.Actions[1].Kind != action.PostNotice {
		t.Fatalf("drag-cancel of a task without queued: %+v, want the cancel and the notice", fd.Actions)
	}
}
