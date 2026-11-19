package route_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

const repo = "sperano/vigie"

func ghDelivery(t *testing.T, kind string, body map[string]any) event.Delivery {
	body["repository"] = map[string]any{"full_name": body["_repo"]}
	if body["_repo"] == nil {
		body["repository"] = map[string]any{"full_name": repo}
	}
	delete(body, "_repo")
	return signed(t, event.SourceGitHub, map[string]string{event.GitHubEventHeader: kind, event.GitHubDeliveryHeader: "guid"}, body)
}

func openPR(labels ...string) *event.PullRequest {
	return &event.PullRequest{Repository: repo, Number: 4, State: "open", Author: "sperano",
		HeadRef: "kelos/vikunja-5", HeadSHA: "abc12345", HeadRepo: repo, Labels: labels}
}

func issueComment(action, sender, text string, onPR bool) map[string]any {
	issue := map[string]any{"number": 4}
	if onPR {
		issue["pull_request"] = map[string]any{}
	}
	return map[string]any{"action": action, "sender": map[string]any{"login": sender}, "issue": issue,
		"comment": map[string]any{"id": 77, "node_id": "IC_77", "body": text}}
}

func review(sender, text string) map[string]any {
	return map[string]any{"action": "submitted", "sender": map[string]any{"login": sender},
		"review":       map[string]any{"id": 88, "node_id": "PRR_88", "body": text},
		"pull_request": map[string]any{"number": 4, "state": "open", "user": map[string]any{"login": "sperano"}}}
}

func TestPRCommentNonRequests(t *testing.T) {
	r := router()
	st := route.State{PullRequest: openPR()}
	cases := []struct {
		name    string
		kind    string
		body    map[string]any
		outcome route.Outcome
		reason  string
	}{
		{"plain issue", "issue_comment", issueComment("created", "sperano", "/gordon x", false), route.Ignore, route.ReasonNotAPullRequest},
		{"edited", "issue_comment", issueComment("edited", "sperano", "/gordon x", true), route.Ignore, route.ReasonActionNotRouted},
		{"no trigger", "issue_comment", issueComment("created", "sperano", "thanks", true), route.Ignore, route.ReasonNotARequest},
		{"trigger prefix only", "issue_comment", issueComment("created", "sperano", "/gordonx", true), route.Ignore, route.ReasonNotARequest},
		{"bot suggestions without opt-in", "pull_request_review",
			review("gordon-cole-ai[bot]", "**Review verdict: suggestions** (review/pr-challenger)"), route.Ignore, route.ReasonNotARequest},
		{"bot approve", "pull_request_review", review("gordon-cole-ai[bot]", "**Review verdict: approve** (review/pr-challenger)"),
			route.Ignore, route.ReasonNotARequest},
		{"unknown bot", "pull_request_review", review("other[bot]", "**Review verdict: blocking**"), route.Ignore, route.ReasonNotARequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expect(t, r.Decide(ghDelivery(t, c.kind, c.body), st, replay.Ledger{}), c.outcome, c.reason)
		})
	}
	unknown := issueComment("created", "sperano", "/gordon x", true)
	unknown["_repo"] = "someone/else"
	expect(t, r.Decide(ghDelivery(t, "issue_comment", unknown), st, replay.Ledger{}), route.Ignore, route.ReasonRepositoryNotConfigured)
}

func TestPRCommentGates(t *testing.T) {
	r := router()
	body := func() event.Delivery {
		return ghDelivery(t, "issue_comment", issueComment("created", "sperano", "/gordon\nplease", true))
	}
	expect(t, r.Decide(body(), route.State{}, replay.Ledger{}), route.Retry, route.ReasonStateUnavailable)
	expect(t, r.Decide(body(), route.State{PullRequest: openPR()}, replay.Ledger{Requests: []string{"github/sperano/vigie/c/77"}}),
		route.Duplicate, route.ReasonRequestHandled)
	stranger := openPR()
	stranger.Author = "mallory"
	expectFamily(t, family(t, r.Decide(body(), route.State{PullRequest: stranger}, replay.Ledger{})), run.PRFollowUp,
		route.Reject, route.ReasonAuthorNotAllowed)
	draft := openPR()
	draft.Draft = true
	fd := family(t, r.Decide(body(), route.State{PullRequest: draft}, replay.Ledger{}))
	want := []string{"submit-task:kelos/kelos-agents/pr-followup-vigie-4-c77", "react:github/sperano/vigie/c/77:eyes"}
	if got := keys(fd.Actions); !slices.Equal(got, want) {
		t.Errorf("draft PR actions %v, want %v", got, want)
	}
}

func TestBotSuggestionsNeedOptIn(t *testing.T) {
	r := router()
	d := r.Decide(ghDelivery(t, "pull_request_review", review("gordon-cole-ai[bot]", "**Review verdict: suggestions** (review/x)")),
		route.State{PullRequest: openPR("kelos:fix-suggestions")}, replay.Ledger{})
	fd := family(t, d)
	expectFamily(t, fd, run.PRFollowUp, route.Dispatch, "")
	if fd.Runs[0].Name != "pr-followup-vigie-4-r88" || fd.Runs[0].Profile.Variant != "claude-opus" {
		t.Errorf("run %s variant %s", fd.Runs[0].Name, fd.Runs[0].Profile.Variant)
	}
	cubic := r.Decide(ghDelivery(t, "pull_request_review", review("cubic-dev-ai[bot]", "**3 issues found** across 2 files")),
		route.State{PullRequest: openPR()}, replay.Ledger{})
	expectFamily(t, family(t, cubic), run.PRFollowUp, route.Dispatch, "")
	suggestions := review("gordon-cole-ai[bot]", "**Review verdict: suggestions** (review/x)")
	unknown := r.Decide(ghDelivery(t, "pull_request_review", suggestions), route.State{}, replay.Ledger{})
	expect(t, unknown, route.Retry, route.ReasonStateUnavailable)
	labelled := review("gordon-cole-ai[bot]", "**Review verdict: suggestions** (review/x)")
	labelled["pull_request"].(map[string]any)["labels"] = []map[string]any{{"name": "kelos:fix-suggestions"}}
	handled := replay.Ledger{Requests: []string{"github/sperano/vigie/r/88"}}
	// The delivery's label opts in while the fresh PR is unavailable, so
	// the review is a request: already handled, not waiting for state.
	expect(t, r.Decide(ghDelivery(t, "pull_request_review", labelled), route.State{}, handled), route.Duplicate, route.ReasonRequestHandled)
	blocking := review("gordon-cole-ai[bot]", "**Review verdict: blocking** (review/x)")
	expect(t, r.Decide(ghDelivery(t, "pull_request_review", blocking), route.State{}, handled), route.Duplicate, route.ReasonRequestHandled)
}

func TestBadBotPatternRejectsInsteadOfPanicking(t *testing.T) {
	r := router()
	r.Config.Policy.ReviewBots = []profile.Bot{{Login: "gordon-cole-ai[bot]", FindingsPattern: "("}}
	d := r.Decide(ghDelivery(t, "pull_request_review", review("gordon-cole-ai[bot]", "**Review verdict: blocking**")),
		route.State{PullRequest: openPR()}, replay.Ledger{})
	expect(t, d, route.Reject, route.ReasonBadBotPattern)
	r.Config.Policy.ReviewBots = []profile.Bot{{Login: "gordon-cole-ai[bot]", FindingsPattern: "^never$", SuggestionsPattern: "("}}
	d = r.Decide(ghDelivery(t, "pull_request_review", review("gordon-cole-ai[bot]", "**Review verdict: suggestions**")),
		route.State{PullRequest: openPR("kelos:fix-suggestions")}, replay.Ledger{})
	expect(t, d, route.Reject, route.ReasonBadBotPattern)
}

func TestTaskNameTooLongIsRejected(t *testing.T) {
	r := router()
	for i := range r.Config.Projects {
		if r.Config.Projects[i].Name == "vigie" {
			r.Config.Projects[i].Workspace = strings.Repeat("w", run.MaxNameLength)
		}
	}
	d := r.Decide(ghDelivery(t, "issue_comment", issueComment("created", "sperano", "/gordon go", true)),
		route.State{PullRequest: openPR()}, replay.Ledger{})
	fd := family(t, d)
	expectFamily(t, fd, run.PRFollowUp, route.Reject, route.ReasonNameTooLong)
	if len(fd.Runs) != 0 || len(fd.Actions) != 0 {
		t.Errorf("rejected run keeps runs %v actions %v", fd.Runs, keys(fd.Actions))
	}
}

func TestPRFollowUpExistingRunReplaysWrites(t *testing.T) {
	r := router()
	const name = "pr-followup-vigie-4-c77"
	owner := ghDelivery(t, "issue_comment", issueComment("created", "sperano", "/gordon\nplease", true))
	bot := ghDelivery(t, "pull_request_review", review("cubic-dev-ai[bot]", "**3 issues found**"))
	react := "react:github/sperano/vigie/c/77:eyes"
	drafts := []string{"add-label:github/sperano/vigie/pull/4:kelos:follow-up", "convert-to-draft:github/sperano/vigie/pull/4"}
	active := replay.Ledger{Runs: []route.RunRecord{{Name: name, Active: true}}}
	finished := replay.Ledger{Runs: []route.RunRecord{{Name: name, Exists: true}}}
	botActive := replay.Ledger{Runs: []route.RunRecord{{Name: "pr-followup-vigie-4-r88", Active: true}}}
	botFinished := replay.Ledger{Runs: []route.RunRecord{{Name: "pr-followup-vigie-4-r88", Exists: true}}}
	cases := []struct {
		name    string
		d       event.Delivery
		led     replay.Ledger
		outcome route.Outcome
		reason  string
		actions []string
	}{
		{"owner active", owner, active, route.Act, route.ReasonRunActive, append([]string{react}, drafts...)},
		{"owner finished", owner, finished, route.Act, route.ReasonRunExists, []string{react}},
		{"bot active", bot, botActive, route.Act, route.ReasonRunActive, drafts},
		{"bot finished", bot, botFinished, route.Duplicate, route.ReasonRunExists, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fd := family(t, r.Decide(c.d, route.State{PullRequest: openPR()}, c.led))
			expectFamily(t, fd, run.PRFollowUp, c.outcome, c.reason)
			if len(fd.Runs) != 0 {
				t.Errorf("runs %+v, want none", fd.Runs)
			}
			if got := keys(fd.Actions); !slices.Equal(got, c.actions) {
				t.Errorf("actions %v, want %v", got, c.actions)
			}
		})
	}
}

func TestOwnerReactionTargetsNodeID(t *testing.T) {
	r := router()
	st := route.State{PullRequest: openPR()}
	fd := family(t, r.Decide(ghDelivery(t, "pull_request_review", review("sperano", "/gordon fix it")), st, replay.Ledger{}))
	if i := slices.IndexFunc(fd.Actions, func(a action.Action) bool { return a.Kind == action.React }); i < 0 ||
		fd.Actions[i].Key != "react:github/sperano/vigie/r/88:eyes" || fd.Actions[i].Detail != "PRR_88" {
		t.Errorf("review reaction %+v, want r/88 with node id PRR_88", fd.Actions)
	}
	noNode := issueComment("created", "sperano", "/gordon go", true)
	delete(noNode["comment"].(map[string]any), "node_id")
	fd = family(t, r.Decide(ghDelivery(t, "issue_comment", noNode), st, replay.Ledger{}))
	want := []string{"submit-task:kelos/kelos-agents/pr-followup-vigie-4-c77",
		"add-label:github/sperano/vigie/pull/4:kelos:follow-up", "convert-to-draft:github/sperano/vigie/pull/4"}
	if got := keys(fd.Actions); !slices.Equal(got, want) {
		t.Errorf("no node id actions %v, want %v", got, want)
	}
}
