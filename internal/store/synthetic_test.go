package store_test

import (
	"errors"
	"testing"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

// TestSyntheticRefusesIDsTheRouterRefuses: Vikunja's data can carry a
// task or comment id that is not positive; building its delivery fails
// instead of panicking the worker.
func TestSyntheticRefusesIDsTheRouterRefuses(t *testing.T) {
	syn := event.Synthetic{Reconcile: "request"}
	for _, id := range []int64{0, -3} {
		if _, err := store.TaskUpdate(id, 3, syn); !errors.Is(err, event.ErrMalformed) {
			t.Errorf("TaskUpdate(%d) = %v, want ErrMalformed", id, err)
		}
		c := event.TaskComment{ID: id, Body: "/gordon go", Author: "owner"}
		if _, err := store.CommentCreated(7, 3, c, syn); !errors.Is(err, event.ErrMalformed) {
			t.Errorf("CommentCreated(comment %d) = %v, want ErrMalformed", id, err)
		}
	}
	if _, err := store.CommentCreated(0, 3, event.TaskComment{ID: 9, Author: "owner"}, syn); !errors.Is(err, event.ErrMalformed) {
		t.Errorf("CommentCreated(task 0) = %v, want ErrMalformed", err)
	}
	d, err := store.TaskUpdate(7, 3, syn)
	if err != nil || d.RequestKey != "vikunja/task/7" || d.Headers[store.SyntheticHeader] != store.SyntheticReconcile {
		t.Fatalf("TaskUpdate(7) = %+v, %v", d, err)
	}
}

// TestCompleteSkipsARequeueOfAnInvalidTask: one bad relation id does not
// fail the decision or drop the other requeues.
func TestCompleteSkipsARequeueOfAnInvalidTask(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k1")
	dec := route.Decision{Outcome: route.Ignore, DeliveryKey: "k1",
		Requeue: []route.Requeue{{TaskID: 0, ProjectID: 3, Depth: 1}, {TaskID: 8, ProjectID: 3, Depth: 1}}}
	if _, err := s.Complete(ctx, claimOne(t, s, "w").Lease, dec); err != nil {
		t.Fatal(err)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM inbox WHERE request_key = 'vikunja/task/8'`); got != 1 {
		t.Fatalf("%d requeues of task 8, want 1", got)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM inbox WHERE headers ? 'X-Gordon-Synthetic'`); got != 1 {
		t.Fatalf("%d requeues queued, want 1", got)
	}
}

// TestEnqueueCountsNothingOnFailure: a failed insert rolls the whole
// batch back, so the count of stored deliveries is zero.
func TestEnqueueCountsNothingOnFailure(t *testing.T) {
	s, dbURL := storetest.Open(t)
	good, err := store.TaskUpdate(7, 3, event.Synthetic{Reconcile: "x"})
	if err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.DeliveryKey = "bad\x00key" // Postgres text refuses NUL
	n, err := s.Enqueue(ctx, []store.Delivery{good, bad})
	if err == nil || n != 0 {
		t.Fatalf("Enqueue = %d, %v; want 0 and an error", n, err)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM inbox`); got != 0 {
		t.Fatalf("%d rows stored after a failed Enqueue", got)
	}
}
