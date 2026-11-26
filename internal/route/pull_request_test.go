package route_test

import (
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

type prOpts struct {
	action, author, head, headRepo string
	draft, merged                  bool
	labels                         []string
}

func prEvent(t *testing.T, o prOpts) event.Delivery {
	labels := []map[string]any{}
	for _, l := range o.labels {
		labels = append(labels, map[string]any{"name": l})
	}
	if o.headRepo == "" {
		o.headRepo = repo
	}
	state := "open"
	if o.action == "closed" {
		state = "closed"
	}
	return ghDelivery(t, "pull_request", map[string]any{"action": o.action, "sender": map[string]any{"login": o.author},
		"pull_request": map[string]any{"number": 4, "state": state, "draft": o.draft, "merged": o.merged,
			"user": map[string]any{"login": o.author}, "labels": labels, "base": map[string]any{"ref": "main"},
			"head": map[string]any{"ref": o.head, "sha": "abc12345", "repo": map[string]any{"full_name": o.headRepo}}}})
}

// reviewDispatch is one reviewer's submission and the supersede of the
// older heads' reviews that waits for it.
var reviewDispatch = []string{"submit-task:kelos/kelos-agents/vikunja-5-vigie-pr-challenger-4-abc12345",
	"supersede-reviews:kelos/kelos-agents/vikunja-5-vigie-pr-challenger-4-abc12345"}

func TestReviewGates(t *testing.T) {
	r := router()
	open := route.State{PullRequest: openPR()}
	noReview := route.State{PullRequest: openPR("kelos:no-review")}
	closed := route.State{PullRequest: &event.PullRequest{Repository: repo, Number: 4, State: "closed", Author: "sperano", HeadSHA: "abc12345"}}
	drafted := route.State{PullRequest: &event.PullRequest{Repository: repo, Number: 4, State: "open", Author: "sperano", Draft: true, HeadSHA: "abc12345"}}
	byStranger, byRenovate := openPR(), openPR()
	byStranger.Author, byRenovate.Author = "mallory", "renovate[bot]"
	skipped := []string{"post-skipped-checks:github/sperano/vigie/pull/4:review/pr-challenger@abc12345"}
	cases := []struct {
		name    string
		o       prOpts
		st      route.State
		outcome route.Outcome
		reason  string
		actions []string
	}{
		{"draft", prOpts{action: "opened", author: "sperano", head: "feature", draft: true}, open, route.Ignore, route.ReasonDraft, nil},
		{"stranger", prOpts{action: "opened", author: "mallory", head: "feature"}, route.State{PullRequest: byStranger},
			route.Reject, route.ReasonAuthorNotAllowed, nil},
		{"fresh author not allowed", prOpts{action: "opened", author: "sperano", head: "feature"}, route.State{PullRequest: byStranger},
			route.Reject, route.ReasonAuthorNotAllowed, nil},
		{"fresh author allowed", prOpts{action: "opened", author: "mallory", head: "feature"}, open,
			route.Dispatch, "", reviewDispatch},
		{"fresh skip author", prOpts{action: "opened", author: "sperano", head: "feature"}, route.State{PullRequest: byRenovate},
			route.Act, route.ReasonSkipAuthor, skipped},
		{"no-review other branch", prOpts{action: "opened", author: "sperano", head: "feature", labels: []string{"kelos:no-review"}},
			noReview, route.Ignore, route.ReasonNoReviewLabel, nil},
		{"no-review fork image-build", prOpts{action: "opened", author: "sperano", head: "image-build/x", headRepo: "fork/vigie",
			labels: []string{"kelos:no-review"}}, noReview, route.Ignore, route.ReasonNoReviewLabel, nil},
		{"no-review image-build", prOpts{action: "ready_for_review", author: "sperano", head: "image-build/x", labels: []string{"kelos:no-review"}},
			noReview, route.Act, route.ReasonNoReviewLabel, []string{"post-skipped-checks:github/sperano/vigie/pull/4:review/pr-challenger@abc12345"}},
		{"no-review added since", prOpts{action: "opened", author: "sperano", head: "image-build/x"},
			noReview, route.Act, route.ReasonNoReviewLabel, []string{"post-skipped-checks:github/sperano/vigie/pull/4:review/pr-challenger@abc12345"}},
		{"no-review removed since", prOpts{action: "opened", author: "sperano", head: "image-build/x", labels: []string{"kelos:no-review"}},
			open, route.Dispatch, "", reviewDispatch},
		{"closed since", prOpts{action: "synchronize", author: "sperano", head: "feature"}, closed, route.Stale, route.ReasonPullRequestClosed, nil},
		{"drafted since", prOpts{action: "synchronize", author: "sperano", head: "feature"}, drafted, route.Stale, route.ReasonDraft, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fd := family(t, r.Decide(prEvent(t, c.o), c.st, replay.Ledger{}))
			expectFamily(t, fd, run.PRReview, c.outcome, c.reason)
			if got := keys(fd.Actions); !slices.Equal(got, append([]string{}, c.actions...)) {
				t.Errorf("actions %v, want %v", got, c.actions)
			}
		})
	}
}

// TestReviewRunNameFromFreshBranch: only the fresh head branch decides
// whether a review is named after its Vikunja task.
func TestReviewRunNameFromFreshBranch(t *testing.T) {
	r := router()
	other := openPR()
	other.HeadRef = "feature"
	cases := []struct {
		name string
		o    prOpts
		pr   *event.PullRequest
		want string
	}{
		{"agent branch", prOpts{action: "opened", author: "sperano", head: "feature"}, openPR(),
			"vikunja-5-vigie-pr-challenger-4-abc12345"},
		{"other branch", prOpts{action: "synchronize", author: "sperano", head: "kelos/vikunja-5"}, other,
			"review-vigie-pr-challenger-4-abc12345"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fd := family(t, r.Decide(prEvent(t, c.o), route.State{PullRequest: c.pr}, replay.Ledger{}))
			if len(fd.Runs) != 1 || fd.Runs[0].Name != c.want || fd.Runs[0].Identity.HeadRef != c.pr.HeadRef {
				t.Errorf("runs %+v, want one named %s on %s", fd.Runs, c.want, c.pr.HeadRef)
			}
		})
	}
}

func TestReviewRunsDeduplicateAndRefuse(t *testing.T) {
	r := router()
	o := prOpts{action: "synchronize", author: "sperano", head: "feature"}
	pr := openPR()
	pr.Files = []string{"cmd/main.go"}
	led := replay.Ledger{Runs: []route.RunRecord{{Name: "vikunja-5-vigie-pr-challenger-4-abc12345", Active: true}}}
	fd := family(t, r.Decide(prEvent(t, o), route.State{PullRequest: pr}, led))
	if fd.Outcome != route.Dispatch || len(fd.Runs) != 1 || fd.Runs[0].Name != "vikunja-5-vigie-idiomatic-go-4-abc12345" {
		t.Errorf("partial duplicate: %+v", fd)
	}
	allActive := replay.Ledger{Runs: append(slices.Clone(led.Runs), route.RunRecord{Name: "vikunja-5-vigie-idiomatic-go-4-abc12345", Active: true})}
	expectFamily(t, family(t, r.Decide(prEvent(t, o), route.State{PullRequest: pr}, allActive)), run.PRReview, route.Duplicate, route.ReasonRunActive)
	led.Runs = append(led.Runs, route.RunRecord{Name: "vikunja-5-vigie-idiomatic-go-4-abc12345", Exists: true})
	expectFamily(t, family(t, r.Decide(prEvent(t, o), route.State{PullRequest: pr}, led)), run.PRReview, route.Duplicate, route.ReasonRunExists)
	none := router()
	none.Config.CommonReviewer = nil
	pr.Files = []string{"README.md"}
	expectFamily(t, family(t, none.Decide(prEvent(t, o), route.State{PullRequest: pr}, replay.Ledger{})), run.PRReview, route.Ignore, route.ReasonNoReviewers)
	pr.Files = []string{"cmd/main.go"}
	r.Config.CommonReviewer = []profile.Reviewer{{Name: "pr-challenger", Variant: "deepseek", Effort: "high"}}
	refused := family(t, r.Decide(prEvent(t, o), route.State{PullRequest: pr}, replay.Ledger{}))
	expectFamily(t, refused, run.PRReview, route.Reject, profile.CodeUnsupportedEffort)
	if len(refused.Runs) != 0 || len(refused.Actions) != 0 {
		t.Errorf("refused review keeps runs %v actions %v", refused.Runs, keys(refused.Actions))
	}
}

func TestTaskLink(t *testing.T) {
	r := router()
	agent := prOpts{action: "opened", author: "sperano", head: "kelos/vikunja-5"}
	done := &event.Task{ID: 5, Done: true, Labels: []string{"pr open"}, Buckets: []string{"Doing"}}
	d := r.Decide(prEvent(t, agent), route.State{PullRequest: openPR(), Task: done}, replay.Ledger{})
	if len(d.Families) != 2 {
		t.Fatalf("families %+v", d.Families)
	}
	if got := keys(d.Families[1].Actions); !slices.Equal(got, []string{"post-notice:vikunja/task/5:pr-open:sperano/vigie#4"}) {
		t.Errorf("pr-open on a done, labelled task: %v", got)
	}
	other := r.Decide(prEvent(t, agent), route.State{PullRequest: openPR(), Task: &event.Task{ID: 6}}, replay.Ledger{})
	expectFamily(t, other.Families[1], run.PROpen, route.Retry, route.ReasonStateUnavailable)
	closed := prOpts{action: "closed", author: "sperano", head: "kelos/vikunja-5"}
	shut := &event.PullRequest{Repository: repo, Number: 4, State: "closed"}
	labelled := &event.Task{ID: 5, Labels: []string{"in progress", "pr open", "action-needed"}}
	fd := family(t, r.Decide(prEvent(t, closed), route.State{PullRequest: shut, Task: labelled}, replay.Ledger{}))
	want := []string{"remove-label:vikunja/task/5:pr open", "remove-label:vikunja/task/5:action-needed"}
	if fd.Family != run.MergeClose || !slices.Equal(keys(fd.Actions), want) {
		t.Errorf("unmerged close %s %v, want %v", fd.Family, keys(fd.Actions), want)
	}
	mergedSince := &event.PullRequest{Repository: repo, Number: 4, State: "closed", Merged: true}
	fd = family(t, r.Decide(prEvent(t, closed), route.State{PullRequest: mergedSince, Task: labelled}, replay.Ledger{}))
	want = []string{"mark-done:vikunja/task/5", "remove-label:vikunja/task/5:in progress", "remove-label:vikunja/task/5:pr open",
		"remove-label:vikunja/task/5:action-needed", "post-notice:vikunja/task/5:merged:sperano/vigie#4"}
	if !slices.Equal(keys(fd.Actions), want) {
		t.Errorf("unmerged delivery of a merged PR %v, want %v", keys(fd.Actions), want)
	}
	draft := prOpts{action: "opened", author: "sperano", head: "kelos/vikunja-5", draft: true}
	both := r.Decide(prEvent(t, draft), route.State{PullRequest: openPR(), Task: labelled}, replay.Ledger{})
	expect(t, both, route.Act, "")
	edited := r.Decide(prEvent(t, prOpts{action: "edited", author: "sperano", head: "kelos/vikunja-5"}), route.State{PullRequest: openPR()}, replay.Ledger{})
	expect(t, edited, route.Ignore, route.ReasonActionNotRouted)
	for _, a := range []string{"edited", "labeled"} {
		unrouted := r.Decide(prEvent(t, prOpts{action: a, author: "sperano", head: "kelos/vikunja-5"}), route.State{}, replay.Ledger{})
		expect(t, unrouted, route.Ignore, route.ReasonActionNotRouted)
	}
	closedFeature := r.Decide(prEvent(t, prOpts{action: "closed", author: "sperano", head: "feature"}), route.State{}, replay.Ledger{})
	expect(t, closedFeature, route.Ignore, route.ReasonActionNotRouted)
	expect(t, r.Decide(prEvent(t, agent), route.State{}, replay.Ledger{}), route.Retry, route.ReasonStateUnavailable)
}
