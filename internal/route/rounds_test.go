package route_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

const (
	cubic      = "cubic-dev-ai[bot]"
	kelosBot   = "gordon-cole-ai[bot]"
	blocking   = "**Review verdict: blocking** (review/pr-challenger)"
	suggestion = "**Review verdict: suggestions** (review/pr-challenger)"
	capNotice  = "post-notice:github/sperano/vigie/pull/4:review-cap"
)

// started is a start notice naming a review follow-up of PR 4.
func started(author string, review int64) event.IssueComment {
	return event.IssueComment{ID: review, Author: author,
		Body: fmt.Sprintf("A KELOS AGENT HAS STARTED THIS FOLLOW-UP ROUND.\n\n| KELOS TASK | `pr-followup-vigie-4-r%d` |", review)}
}

// rounds is a history of n earlier bot rounds of PR 4, each announced by
// gordon-cole-ai[bot], each on its own head.
func rounds(n int) *route.PRHistory {
	h := &route.PRHistory{}
	for i := range n {
		id := int64(1000 + i)
		h.Reviews = append(h.Reviews, event.Review{ID: id, Author: cubic, Body: "**2 issues found**", CommitID: fmt.Sprint("head", i)})
		h.Comments = append(h.Comments, started(kelosBot, id))
	}
	return h
}

func botReview(t *testing.T, sender, body, commit string) event.Delivery {
	b := review(sender, body)
	b["review"].(map[string]any)["commit_id"] = commit
	return ghDelivery(t, "pull_request_review", b)
}

func TestRoundCapCountsAnnouncedBotRounds(t *testing.T) {
	r := router()
	r.Config.Policy.MaxRoundsPerPR = 3
	d := botReview(t, cubic, "**1 issue found**", "now")
	cases := []struct {
		name    string
		h       *route.PRHistory
		led     replay.Ledger
		outcome route.Outcome
		reason  string
	}{
		{"below the cap", rounds(2), replay.Ledger{}, route.Dispatch, ""},
		{"at the cap", rounds(3), replay.Ledger{}, route.Act, route.ReasonRoundCap},
		{"a submitted run not announced yet counts", rounds(2), replay.Ledger{Runs: []route.RunRecord{
			{Name: "pr-followup-vigie-4-r999", Family: run.PRFollowUp, Workspace: "vigie", PR: 4}}}, route.Act, route.ReasonRoundCap},
		{"another PR's run does not count", rounds(2), replay.Ledger{Runs: []route.RunRecord{
			{Name: "pr-followup-vigie-5-r999", Family: run.PRFollowUp, Workspace: "vigie", PR: 5}}}, route.Dispatch, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "a submitted run not announced yet counts" {
				c.h.Reviews = append(c.h.Reviews, event.Review{ID: 999, Author: kelosBot, Body: blocking})
			}
			fd := family(t, r.Decide(d, route.State{PullRequest: openPR(), History: c.h}, c.led))
			expectFamily(t, fd, run.PRFollowUp, c.outcome, c.reason)
			if c.outcome == route.Act {
				if got := keys(fd.Actions); !slices.Equal(got, []string{capNotice}) || len(fd.Runs) != 0 {
					t.Errorf("capped actions %v runs %v, want only the cap notice", got, fd.Runs)
				}
			}
		})
	}
}

func TestRoundCapIgnoresWhatIsNoRound(t *testing.T) {
	r := router()
	r.Config.Policy.MaxRoundsPerPR = 2
	h := rounds(1)
	// A stranger's comment naming a run, the owner's own review named by
	// a start notice, and the review under decision are no rounds.
	h.Comments = append(h.Comments, started("mallory", 1001))
	h.Reviews = append(h.Reviews, event.Review{ID: 1001, Author: cubic, Body: "**1 issue found**"},
		event.Review{ID: 1002, Author: "sperano", Body: "/gordon fix"}, event.Review{ID: 88, Author: cubic, Body: "**1 issue found**"})
	h.Comments = append(h.Comments, started("sperano", 1002), started(kelosBot, 88))
	fd := family(t, r.Decide(botReview(t, cubic, "**1 issue found**", "now"), route.State{PullRequest: openPR(), History: h}, replay.Ledger{}))
	expectFamily(t, fd, run.PRFollowUp, route.Dispatch, "")
}

func TestOwnerRequestIsNeverCapped(t *testing.T) {
	r := router()
	r.Config.Policy.MaxRoundsPerPR = 1
	d := ghDelivery(t, "issue_comment", issueComment("created", "sperano", "/gordon go on", true))
	fd := family(t, r.Decide(d, route.State{PullRequest: openPR()}, replay.Ledger{}))
	expectFamily(t, fd, run.PRFollowUp, route.Dispatch, "")
}

func TestRoundCapNeedsTheHistory(t *testing.T) {
	r := router()
	d := botReview(t, cubic, "**1 issue found**", "now")
	expectFamily(t, family(t, r.Decide(d, route.State{PullRequest: openPR()}, replay.Ledger{})),
		run.PRFollowUp, route.Retry, route.ReasonStateUnavailable)
	r.Config.Policy.MaxRoundsPerPR, r.Config.Policy.SuggestionsMaxRounds = 0, 0
	expectFamily(t, family(t, r.Decide(d, route.State{PullRequest: openPR()}, replay.Ledger{})),
		run.PRFollowUp, route.Dispatch, "")
	// Without the fix-suggestions label there is no suggestions cap.
	r.Config.Policy.SuggestionsMaxRounds, r.Config.Policy.FixSuggestionsLabel = 4, ""
	expectFamily(t, family(t, r.Decide(d, route.State{PullRequest: openPR()}, replay.Ledger{})),
		run.PRFollowUp, route.Dispatch, "")
}

// suggestionRounds are announced suggestions-only Kelos reviews, one per
// head in heads.
func suggestionRounds(heads ...string) *route.PRHistory {
	h := &route.PRHistory{}
	for i, head := range heads {
		id := int64(2000 + i)
		h.Reviews = append(h.Reviews, event.Review{ID: id, Author: kelosBot, Body: suggestion, CommitID: head})
		h.Comments = append(h.Comments, started(kelosBot, id))
	}
	return h
}

func TestSuggestionsCapCountsReviewedHeads(t *testing.T) {
	r := router()
	r.Config.Policy.SuggestionsMaxRounds = 2
	pr := openPR("kelos:fix-suggestions")
	d := botReview(t, kelosBot, suggestion, "now")
	removed := "remove-label:github/sperano/vigie/pull/4:kelos:fix-suggestions"
	cases := []struct {
		name    string
		h       *route.PRHistory
		outcome route.Outcome
		reason  string
	}{
		{"one head", suggestionRounds("a"), route.Dispatch, ""},
		{"two reviewers on one head count once", suggestionRounds("a", "a"), route.Dispatch, ""},
		{"the head under review is not over", suggestionRounds("a", "now"), route.Dispatch, ""},
		{"two heads", suggestionRounds("a", "b"), route.Act, route.ReasonSuggestionsCap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fd := family(t, r.Decide(d, route.State{PullRequest: pr, History: c.h}, replay.Ledger{}))
			expectFamily(t, fd, run.PRFollowUp, c.outcome, c.reason)
			if c.outcome == route.Act {
				want := []string{removed, "post-notice:github/sperano/vigie/pull/4:suggestions-cap"}
				if got := keys(fd.Actions); !slices.Equal(got, want) {
					t.Errorf("actions %v, want %v", got, want)
				}
			}
		})
	}
	// Blocking findings are never held by the suggestions cap.
	fd := family(t, r.Decide(botReview(t, kelosBot, blocking, "now"), route.State{PullRequest: pr,
		History: suggestionRounds("a", "b")}, replay.Ledger{}))
	expectFamily(t, fd, run.PRFollowUp, route.Dispatch, "")
}

func TestCappedRequestIsNotDraftedOrAnnounced(t *testing.T) {
	r := router()
	r.Config.Policy.MaxRoundsPerPR = 1
	fd := family(t, r.Decide(botReview(t, cubic, "**1 issue found**", "now"),
		route.State{PullRequest: openPR(), History: rounds(1)}, replay.Ledger{}))
	for _, a := range fd.Actions {
		if a.Key != capNotice {
			t.Errorf("capped request writes %s", a.Key)
		}
	}
	// The same request dispatched below the cap is drafted and announced.
	fd = family(t, r.Decide(botReview(t, cubic, "**1 issue found**", "now"),
		route.State{PullRequest: openPR(), History: rounds(0)}, replay.Ledger{}))
	want := []string{"submit-task:kelos/kelos-agents/pr-followup-vigie-4-r88",
		"add-label:github/sperano/vigie/pull/4:kelos:follow-up", "convert-to-draft:github/sperano/vigie/pull/4",
		"post-notice:github/sperano/vigie/pull/4:start:pr-followup-vigie-4-r88"}
	if got := keys(fd.Actions); !slices.Equal(got, want) {
		t.Errorf("actions %v, want %v", got, want)
	}
	if fd.Actions[2].After != fd.Actions[1].Key || fd.Actions[3].After != fd.Actions[0].Key {
		t.Errorf("draft waits for %q, notice for %q", fd.Actions[2].After, fd.Actions[3].After)
	}
}
