package store_test

import (
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store/storetest"
)

// prRun is a decision submitting one run of identity id.
func prRun(id run.Identity) route.Decision {
	dec := submitOnly()
	fd := &dec.Families[0]
	fd.Family, fd.Runs[0].Name, fd.Runs[0].Identity = id.Family, id.Name(), id
	fd.Actions = []action.Action{action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, id.Name()), "")}
	return dec
}

func TestLedgerPRRuns(t *testing.T) {
	s, dbURL := storetest.Open(t)
	follow := func(pr int, kind string, comment int64) run.Identity {
		return run.Identity{Family: run.PRFollowUp, Workspace: "vigie", PR: pr, CommentKind: kind, CommentID: comment}
	}
	review := run.Identity{Family: run.PRReview, Workspace: "vigie", Reviewer: "pr-challenger", PR: 4, HeadSHA: "abc12345"}
	conflict := run.Identity{Family: run.PRConflict, Workspace: "vigie", PR: 4, Episode: "0123abcd-4-11111111-22222222",
		HeadRef: "kelos/vikunja-9"}
	implement := run.Identity{Family: run.VikunjaImplement, Project: "vigie", Workspace: "vigie", PR: 4, Variant: "v", TaskID: 9}
	for _, id := range []run.Identity{follow(4, "r", 88), follow(4, "c", 77), follow(44, "r", 9), review, conflict, implement} {
		dec := prRun(id)
		dec.RequestKey = id.Name()
		recorded(t, s, "k"+id.Name(), dec.RequestKey, dec)
	}
	const finished = "pr-followup-vigie-4-c77"
	storetest.Exec(t, dbURL, `UPDATE runs SET state = 'finished' WHERE name = $1`, finished)
	led := s.Ledger(ctx)
	got := led.PRRuns("vigie", 4)
	families := map[string]run.Family{}
	var names []string
	for _, r := range got {
		names, families[r.Name] = append(names, r.Name), r.Family
		if r.Workspace != "vigie" || r.PR != 4 || r.Active != (r.Name != finished) {
			t.Errorf("record %+v; want active unless finished", r)
		}
	}
	want := []string{conflict.Name(), "pr-followup-vigie-4-c77", "pr-followup-vigie-4-r88", review.Name()}
	if led.Err != nil || !slices.Equal(names, want) {
		t.Fatalf("PRRuns = %v (%v); want PR 4's follow-ups, review and conflict run, not PR 44's nor the task's run",
			names, led.Err)
	}
	if families[review.Name()] != run.PRReview || families[conflict.Name()] != run.PRConflict ||
		families["pr-followup-vigie-4-r88"] != run.PRFollowUp {
		t.Errorf("families %v", families)
	}
	if none := led.PRRuns("gordon", 4); len(none) != 0 {
		t.Fatalf("another Workspace's PR 4 has runs %+v", none)
	}
}

func TestOlderReviews(t *testing.T) {
	s, dbURL := storetest.Open(t)
	review := func(reviewer string, pr int, sha string) run.Identity {
		return run.Identity{Family: run.PRReview, Workspace: "vigie", Reviewer: reviewer, PR: pr, HeadSHA: sha}
	}
	ids := []run.Identity{review("pr-challenger", 4, "aaaaaaaa"), review("pr-challenger", 4, "bbbbbbbb"),
		review("pr-challenger", 4, "cccccccc"), review("idiomatic-go", 4, "aaaaaaaa"), review("pr-challenger", 5, "aaaaaaaa"),
		review("pr-challenger", 4, "dddddddd")}
	for i, id := range ids {
		recorded(t, s, "k"+id.Name(), "", prRun(id))
		storetest.Exec(t, dbURL, `UPDATE runs SET created_at = created_at + make_interval(secs => $2) WHERE name = $1`, id.Name(), i)
	}
	storetest.Exec(t, dbURL, `UPDATE runs SET state = 'finished' WHERE name = $1`, ids[0].Name())
	got, err := s.OlderReviews(ctx, ids[2].Name())
	if err != nil || !slices.Equal(got, []string{ids[1].Name()}) {
		t.Fatalf("OlderReviews = %v, %v; want the unfinished earlier review of the same PR and reviewer", got, err)
	}
	if got, err := s.OlderReviews(ctx, "review-unknown"); err != nil || len(got) != 0 {
		t.Fatalf("OlderReviews of an unknown run = %v, %v", got, err)
	}
}
