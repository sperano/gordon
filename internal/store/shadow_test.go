package store_test

import (
	"testing"

	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

func TestShadowTasksRecordedAndCompared(t *testing.T) {
	for _, tc := range []struct {
		name            string
		execute, shadow bool
	}{{"global shadow", true, true}, {"family not executed", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, dbURL := storetest.Open(t)
			id := accept(t, s, "k")
			dec := withTask("would run")
			dec.Families[0].Execute = tc.execute
			if _, err := s.Complete(ctx, claimOne(t, s, "w").Lease, dec, tc.shadow); err != nil {
				t.Fatal(err)
			}
			if n := count(t, dbURL, `SELECT count(*) FROM runs`); n != 0 {
				t.Fatalf("%d runs recorded in shadow", n)
			}
			pending, err := s.PendingShadow(ctx, 10)
			if err != nil || len(pending) != 1 {
				t.Fatalf("PendingShadow = %+v, %v", pending, err)
			}
			p := pending[0]
			if p.Name != runName || p.InboxID != id || p.Family != "vikunja-followup" || p.Task.Spec.Prompt != "would run" {
				t.Fatalf("shadow task %+v", p)
			}
			if err := s.ShadowCompared(ctx, runName, id, store.ShadowDiffer, []string{"spec.prompt"}); err != nil {
				t.Fatal(err)
			}
			// A second comparison of the same row changes nothing.
			if err := s.ShadowCompared(ctx, runName, id, store.ShadowMatch, nil); err != nil {
				t.Fatal(err)
			}
			var result, diff string
			storetest.QueryRow(t, dbURL, `SELECT result, diff::text FROM shadow_tasks`, nil, &result, &diff)
			if result != store.ShadowDiffer || diff != `["spec.prompt"]` {
				t.Fatalf("shadow result %s diff %s", result, diff)
			}
			if pending, _ = s.PendingShadow(ctx, 10); len(pending) != 0 {
				t.Fatal("a compared shadow task is still pending")
			}
		})
	}
}

func TestExecutedRunsAndRunsWithoutTaskLeaveNoShadow(t *testing.T) {
	s, dbURL := storetest.Open(t)
	recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	accept(t, s, "k2")
	if _, err := s.Complete(ctx, claimOne(t, s, "w").Lease, dispatch(true), true); err != nil {
		t.Fatal(err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM shadow_tasks`); n != 0 {
		t.Fatalf("%d shadow tasks, want none: one run executed, the other without a Task", n)
	}
}

func TestShadowMissingAndPrune(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := accept(t, s, "k")
	if _, err := s.Complete(ctx, claimOne(t, s, "w").Lease, withTask("p"), true); err != nil {
		t.Fatal(err)
	}
	if err := s.ShadowCompared(ctx, runName, id, store.ShadowMissing, nil); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	storetest.QueryRow(t, dbURL, `SELECT diff IS NULL FROM shadow_tasks`, nil, &isNull)
	if !isNull {
		t.Fatal("a missing comparison recorded a diff")
	}
	storetest.Exec(t, dbURL, `UPDATE shadow_tasks SET compared_at = now() - interval '2 hours'`)
	if n, err := s.PruneTasks(ctx, longDelay); err != nil || n != 1 {
		t.Fatalf("PruneTasks = %d, %v; want the compared shadow Task", n, err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM shadow_tasks WHERE task IS NULL`); n != 1 {
		t.Fatal("the shadow row was dropped or kept its Task")
	}
}
