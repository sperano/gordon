package lifecycle

import (
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/route"
)

// TestStaleDoingMoveIsSkipped: a delayed move into Doing never puts a
// done task back there, and a claim's move never pulls a card dragged
// back to the backlog; a chain's move of a subtask waiting in To-Do
// still runs.
func TestStaleDoingMoveIsSkipped(t *testing.T) {
	f := newFake(t)
	f.board()
	ex := executor(f, nil)
	todo, review := f.buckets[1][0], f.buckets[1][2]
	chain := action.New(action.MoveBucket, action.VikunjaTask(5), "Doing")
	claim := chain
	claim.Detail = route.DetailClaim
	cases := []struct {
		name   string
		task   Task
		a      action.Action
		bucket string
	}{
		{"chain move of a done subtask", Task{ID: 5, ProjectID: 3, Done: true, Buckets: []Bucket{review}}, chain, "Review"},
		{"claim move of a done task", Task{ID: 5, ProjectID: 3, Done: true, Buckets: []Bucket{review}}, claim, "Review"},
		{"claim move of a dragged card", Task{ID: 5, ProjectID: 3, Buckets: []Bucket{todo}}, claim, "To-Do"},
		{"chain move of a waiting subtask", Task{ID: 5, ProjectID: 3, Buckets: []Bucket{todo}}, chain, "Doing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.put(c.task)
			applied, err := ex.Recover(ctx, c.a)
			if err != nil || applied != (c.bucket != "Doing") {
				t.Fatalf("recover: %v, applied %v", err, applied)
			}
			if err := ex.Execute(ctx, c.a); err != nil || f.inBucket(5, 1) != c.bucket {
				t.Fatalf("move: %v, bucket %q, want %q", err, f.inBucket(5, 1), c.bucket)
			}
		})
	}
}
