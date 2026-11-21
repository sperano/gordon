package submit

import (
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/worker"
)

const (
	ownReview   = "review-gordon-pr-challenger-7-bbbbbbbb"
	olderReview = "review-gordon-pr-challenger-7-aaaaaaaa"
)

func reviewTask(name, reviewer, pr, phase, created string) kelos.Task {
	return kelos.Task{Metadata: kelos.Metadata{Name: name, CreationTimestamp: created, Labels: map[string]string{
		RoleLabel: ReviewRole, WorkspaceLabel: "gordon", ReviewerLabel: reviewer, PRLabel: pr}},
		Spec: kelos.Spec{Prompt: "x"}, Status: &kelos.Status{Phase: phase}}
}

// seedReviews records review runs of Gordon, each created at its offset
// in seconds from now.
func seedReviews(t *testing.T, e *env, runs map[string]int) {
	t.Helper()
	var inbox int64
	storetest.QueryRow(t, e.dbURL, `INSERT INTO inbox (source, delivery_key, kind, scope, headers, body)
		VALUES ('github', 'seed/review', 'pull_request', 'github/x', '{}', '{}') RETURNING id`, nil, &inbox)
	for name, offset := range runs {
		var reviewer, pr string
		switch name {
		case "review-gordon-idiomatic-go-7-aaaaaaaa":
			reviewer, pr = "idiomatic-go", "7"
		case "review-gordon-pr-challenger-8-aaaaaaaa":
			reviewer, pr = "pr-challenger", "8"
		default:
			reviewer, pr = "pr-challenger", "7"
		}
		storetest.Exec(t, e.dbURL, `INSERT INTO runs (name, family, inbox_id, identity, profile, created_at)
			VALUES ($1, 'pr-review', $2, jsonb_build_object('family', 'pr-review', 'workspace', 'gordon',
				'reviewer', $3::text, 'pr', $4::int), '{}', now() + make_interval(secs => $5))`,
			name, inbox, reviewer, pr, offset)
	}
}

func supersede(name string) action.Action {
	return action.New(action.SupersedeReviews, action.KelosTask("kelos-agents", name), "")
}

// TestSupersedeDeletesOlderReviewsOfThePR: the review of a newer head
// deletes the unfinished review Tasks of the same PR by the same
// reviewer created before it, a legacy spawner's included, and finishes
// Gordon's older runs; nothing newer, finished, of another reviewer or
// of another PR.
func TestSupersedeDeletesOlderReviewsOfThePR(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	seedReviews(t, e, map[string]int{olderReview: -60, ownReview: 0,
		"review-gordon-idiomatic-go-7-aaaaaaaa": -60, "review-gordon-pr-challenger-8-aaaaaaaa": -60})
	for _, tk := range []kelos.Task{
		reviewTask(ownReview, "pr-challenger", "7", kelos.PhasePending, "2026-11-21T10:00:00Z"),
		reviewTask(olderReview, "pr-challenger", "7", kelos.PhaseRunning, "2026-11-21T09:00:00Z"),
		reviewTask("review-gordon-pr-challenger-7-legacy01", "pr-challenger", "7", "", "2026-11-21T09:30:00Z"),
		reviewTask("review-gordon-pr-challenger-7-done0001", "pr-challenger", "7", kelos.PhaseSucceeded, "2026-11-21T08:00:00Z"),
		reviewTask("review-gordon-pr-challenger-7-newer001", "pr-challenger", "7", kelos.PhaseRunning, "2026-11-21T11:00:00Z"),
		reviewTask("review-gordon-pr-challenger-8-aaaaaaaa", "pr-challenger", "8", kelos.PhaseRunning, "2026-11-21T09:00:00Z"),
		reviewTask("review-gordon-idiomatic-go-7-aaaaaaaa", "idiomatic-go", "7", kelos.PhaseRunning, "2026-11-21T09:00:00Z"),
	} {
		e.kelos.Put(tk)
	}
	s := &Superseder{Store: e.store, Kelos: e.api}
	if applied, err := s.Recover(ctx, supersede(ownReview)); err != nil || applied {
		t.Fatalf("Recover before = %v, %v", applied, err)
	}
	if err := s.Execute(ctx, supersede(ownReview)); err != nil {
		t.Fatal(err)
	}
	want := []string{olderReview, "review-gordon-pr-challenger-7-legacy01"}
	if got := e.kelos.Deletes(); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	for name, state := range map[string]string{olderReview: "finished", ownReview: "submitting",
		"review-gordon-idiomatic-go-7-aaaaaaaa": "submitting", "review-gordon-pr-challenger-8-aaaaaaaa": "submitting"} {
		if got, _ := e.run(t, name); got != state {
			t.Errorf("run %s %s, want %s", name, got, state)
		}
	}
	if applied, err := s.Recover(ctx, supersede(ownReview)); err != nil || !applied {
		t.Fatalf("Recover after = %v, %v", applied, err)
	}
}

// TestSupersedeByARestartOrAnUnlistedTaskDoesNothing: a limit restart
// reviews an older push, and a Task gone already supersedes nothing.
func TestSupersedeByARestartOrAnUnlistedTaskDoesNothing(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	seedReviews(t, e, map[string]int{ownReview: 0})
	s := &Superseder{Store: e.store, Kelos: e.api}
	e.kelos.Put(reviewTask(olderReview, "pr-challenger", "7", kelos.PhaseRunning, "2026-11-21T09:00:00Z"))
	if err := s.Execute(ctx, supersede(ownReview)); err != nil {
		t.Fatal(err)
	}
	restart := reviewTask(ownReview, "pr-challenger", "7", kelos.PhaseRunning, "2026-11-21T10:00:00Z")
	restart.Metadata.Annotations = map[string]string{RestartOf: "review-gordon-pr-challenger-7-cccccccc"}
	e.kelos.Put(restart)
	if err := s.Execute(ctx, supersede(ownReview)); err != nil {
		t.Fatal(err)
	}
	if got := e.kelos.Deletes(); len(got) != 0 {
		t.Fatalf("deleted %v", got)
	}
}

func TestSupersedeRefusesOtherTargets(t *testing.T) {
	s := &Superseder{}
	for _, a := range []action.Action{
		action.New(action.SupersedeReviews, action.VikunjaTask(7), ""),
		action.New(action.CancelRun, action.KelosTask("kelos-agents", ownReview), ""),
	} {
		if err := s.Execute(ctx, a); !worker.IsPermanent(err) {
			t.Errorf("%s %s: %v, want permanent", a.Kind, a.Target, err)
		}
	}
}
