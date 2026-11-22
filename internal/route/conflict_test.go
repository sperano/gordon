package route_test

import (
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

const (
	conflictBase = "89abcdef0123456789"
	conflictHead = "abc12345"
)

// conflictEvent is the conflict reconciliation's delivery of a conflict
// on vigie's PR 4, synthetic unless webhook.
func conflictEvent(t *testing.T, workspace string, webhook bool) event.Delivery {
	t.Helper()
	d := ghDelivery(t, "pull_request", map[string]any{"action": event.ActionConflict, "sender": map[string]any{"login": "sperano"},
		"gordon": map[string]any{"workspace": workspace},
		"pull_request": map[string]any{"number": 4, "state": "open", "user": map[string]any{"login": "sperano"},
			"base": map[string]any{"ref": "main", "sha": conflictBase},
			"head": map[string]any{"ref": "kelos/vikunja-5", "sha": conflictHead, "repo": map[string]any{"full_name": repo}}}})
	if !webhook {
		d.Headers[event.SyntheticHeader] = "reconcile"
	}
	return d
}

// conflictedPR is the fresh PR in conflict, with labels.
func conflictedPR(labels ...string) *event.PullRequest {
	pr := openPR(labels...)
	no := false
	pr.BaseRef, pr.BaseSHA, pr.Mergeable, pr.MergeableState = "main", conflictBase, &no, event.MergeableDirty
	return pr
}

func TestEpisodeIsTheLegacyID(t *testing.T) {
	// reconcile_conflicts.conflict_payload's id for these inputs.
	got := route.Episode("sperano/gordon", "gordon", "claude-opus", 7, "89abcdef0123456789abcdef", "0123456789abcdef")
	if got != "1309fa22-7-89abcdef-01234567" {
		t.Errorf("Episode = %q", got)
	}
	if got := route.Episode("r", "w", "v", 1, "ab", "cd"); got != "1b81654c-1-ab-cd" {
		t.Errorf("short SHAs: %q", got)
	}
}

func TestConflictDispatch(t *testing.T) {
	r := router()
	dec := r.DecideAuthenticated(conflictEvent(t, "vigie", false), route.State{PullRequest: conflictedPR("kelos:gpt-sol")}, replay.Ledger{})
	fd := family(t, dec)
	expectFamily(t, fd, run.PRConflict, route.Dispatch, "")
	episode := route.Episode(repo, "vigie", "gpt-sol", 4, conflictBase, conflictHead)
	name := "pr-conflict-" + episode
	if len(fd.Runs) != 1 {
		t.Fatalf("runs %+v", fd.Runs)
	}
	rn := fd.Runs[0]
	if rn.Name != name || rn.Branch != "kelos/vikunja-5" || rn.Identity.Variant != "gpt-sol" || rn.Identity.Workspace != "vigie" ||
		rn.Identity.Project != "vigie" || rn.Profile.Mode != "followup" {
		t.Errorf("run %+v", rn)
	}
	want := []string{"submit-task:kelos/kelos-agents/" + name, "post-notice:github/sperano/vigie/pull/4:start:" + name}
	if !slices.Equal(keys(fd.Actions), want) || fd.Actions[1].After != fd.Actions[0].Key {
		t.Errorf("actions %v, want %v, the notice after the submission", keys(fd.Actions), want)
	}
	if dec.RequestKey != "github/sperano/vigie/pull/4/gordon-conflict/"+conflictHead {
		t.Errorf("request key %q", dec.RequestKey)
	}
}

func TestConflictGates(t *testing.T) {
	r := router()
	byStranger, fork, closed, drafted, moved, rebased := conflictedPR("kelos:gpt-sol"), conflictedPR("kelos:gpt-sol"),
		conflictedPR("kelos:gpt-sol"), conflictedPR("kelos:gpt-sol"), conflictedPR("kelos:gpt-sol"), conflictedPR("kelos:gpt-sol")
	byStranger.Author, fork.HeadRepo, closed.State, drafted.Draft = "mallory", "fork/vigie", "closed", true
	moved.HeadSHA, rebased.BaseSHA = "fedcba98", "0000000000"
	unknown, clean, behind := conflictedPR("kelos:gpt-sol"), conflictedPR("kelos:gpt-sol"), conflictedPR("kelos:gpt-sol")
	yes := true
	unknown.Mergeable, clean.Mergeable, clean.MergeableState, behind.MergeableState = nil, &yes, "clean", "behind"
	retargeted := conflictedPR("kelos:gpt-sol")
	retargeted.BaseRef = "feature"
	cases := []struct {
		name    string
		d       event.Delivery
		st      route.State
		outcome route.Outcome
		reason  string
	}{
		{"stranger", conflictEvent(t, "vigie", false), route.State{PullRequest: byStranger}, route.Reject, route.ReasonAuthorNotAllowed},
		{"fork", conflictEvent(t, "vigie", false), route.State{PullRequest: fork}, route.Ignore, route.ReasonForeignHead},
		{"closed since", conflictEvent(t, "vigie", false), route.State{PullRequest: closed}, route.Stale, route.ReasonPullRequestClosed},
		{"drafted since", conflictEvent(t, "vigie", false), route.State{PullRequest: drafted}, route.Stale, route.ReasonDraft},
		{"head moved", conflictEvent(t, "vigie", false), route.State{PullRequest: moved}, route.Stale, route.ReasonEpisodeMoved},
		{"base moved", conflictEvent(t, "vigie", false), route.State{PullRequest: rebased}, route.Stale, route.ReasonEpisodeMoved},
		{"base ref changed", conflictEvent(t, "vigie", false), route.State{PullRequest: retargeted}, route.Stale, route.ReasonEpisodeMoved},
		{"mergeability unknown", conflictEvent(t, "vigie", false), route.State{PullRequest: unknown}, route.Retry, route.ReasonMergeabilityUnknown},
		{"mergeable", conflictEvent(t, "vigie", false), route.State{PullRequest: clean}, route.Stale, route.ReasonNoConflict},
		{"not dirty", conflictEvent(t, "vigie", false), route.State{PullRequest: behind}, route.Stale, route.ReasonNoConflict},
		{"no kelos label", conflictEvent(t, "vigie", false), route.State{PullRequest: conflictedPR("bug")}, route.Ignore, route.ReasonNoVariantLabel},
		{"two variants", conflictEvent(t, "vigie", false), route.State{PullRequest: conflictedPR("kelos:gpt-sol", "kelos:deepseek")},
			route.Conflict, route.ReasonMultipleVariantLabels},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fd := family(t, r.DecideAuthenticated(c.d, c.st, replay.Ledger{}))
			expectFamily(t, fd, run.PRConflict, c.outcome, c.reason)
			if len(fd.Runs) != 0 || len(fd.Actions) != 0 {
				t.Errorf("runs %v actions %v", fd.Runs, keys(fd.Actions))
			}
		})
	}
}

func TestConflictRefusedBeforeTheState(t *testing.T) {
	r := router()
	// A webhook never carries the action: GitHub sends none such.
	expect(t, r.DecideAuthenticated(conflictEvent(t, "vigie", true), route.State{PullRequest: conflictedPR("kelos:gpt-sol")},
		replay.Ledger{}), route.Ignore, route.ReasonActionNotRouted)
	expect(t, r.DecideAuthenticated(conflictEvent(t, "elsewhere", false), route.State{PullRequest: conflictedPR("kelos:gpt-sol")},
		replay.Ledger{}), route.Ignore, route.ReasonWorkspaceNotConfigured)
	expect(t, r.DecideAuthenticated(conflictEvent(t, "vigie", false), route.State{}, replay.Ledger{}),
		route.Retry, route.ReasonStateUnavailable)
	if got := r.Reads(mustParse(t, conflictEvent(t, "vigie", false))); !got.PullRequest || got.Files || got.Task != 0 {
		t.Errorf("synthetic conflict reads %+v, want the PR alone", got)
	}
	if got := r.Reads(mustParse(t, conflictEvent(t, "vigie", true))); got.PullRequest {
		t.Errorf("webhook conflict reads %+v, want nothing", got)
	}
}

func TestConflictRunDeduplicates(t *testing.T) {
	r := router()
	name := "pr-conflict-" + route.Episode(repo, "vigie", "claude-opus", 4, conflictBase, conflictHead)
	st := route.State{PullRequest: conflictedPR("kelos:follow-up")}
	led := replay.Ledger{Runs: []route.RunRecord{{Name: name, Exists: true}}}
	expectFamily(t, family(t, r.DecideAuthenticated(conflictEvent(t, "vigie", false), st, led)), run.PRConflict,
		route.Duplicate, route.ReasonRunExists)
	// Gone and finished: the conflict that lasts runs again, as legacy's
	// name was free again after its TTL.
	led.Runs[0].Exists = false
	expectFamily(t, family(t, r.DecideAuthenticated(conflictEvent(t, "vigie", false), st, led)), run.PRConflict, route.Dispatch, "")
}

func TestConflictVariant(t *testing.T) {
	r := router()
	cases := []struct {
		labels []string
		want   string
		ok     bool
	}{
		{[]string{"kelos:follow-up"}, "claude-opus", true},
		{[]string{"kelos:claude-opus", "kelos:gpt-sol"}, "gpt-sol", true},
		{[]string{"kelos:gpt-sol", "kelos:deepseek"}, "", false},
		{[]string{"bug"}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		got, ok := r.ConflictVariant(event.PullRequest{Labels: c.labels})
		if got != c.want || ok != c.ok {
			t.Errorf("%v: %q, %v; want %q, %v", c.labels, got, ok, c.want, c.ok)
		}
	}
}

func mustParse(t *testing.T, d event.Delivery) event.Event {
	t.Helper()
	ev, err := event.Parse(d)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}
