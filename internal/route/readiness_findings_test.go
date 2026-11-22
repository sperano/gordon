package route_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
)

const (
	verdictBlocking = "**Review verdict: blocking**\n\nfix it"
	suggestions     = "**Review verdict: suggestions**\n\nmaybe"
	approve         = "**Review verdict: approve**"
)

func headReview(id int64, body, commit string, at time.Duration) event.Review {
	return event.Review{ID: id, Author: gordonBot, Body: body, CommitID: commit, SubmittedAt: readAt.Add(-at)}
}

// startNotice is a PR follow-up's start notice naming its run.
func startNotice(name string) event.IssueComment {
	return event.IssueComment{Author: gordonBot, Body: "BACK ON THE CASE!\n\n| KELOS TASK | `" + name + "` |"}
}

func TestReadinessWaitsForAPendingFollowUp(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*route.Readiness)
		led     replay.Ledger
		pending bool
	}{
		{"blocking review, nothing started", func(*route.Readiness) {}, replay.Ledger{}, true},
		{"its follow-up Task exists", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "pr-followup-vigie-4-r99", Phase: "Succeeded"}}
		}, replay.Ledger{}, false},
		{"the poller's follow-up restarted", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "pr-followup-vigie-4-p99-retry1", Phase: "Failed"}}
		}, replay.Ledger{}, false},
		{"Gordon's finished follow-up run", func(*route.Readiness) {}, replay.Ledger{Runs: []route.RunRecord{{
			Name: "pr-followup-vigie-4-r99", Family: "pr-followup", Workspace: "vigie", PR: 4}}}, false},
		{"announced on the PR", func(rd *route.Readiness) {
			rd.Comments = []event.IssueComment{startNotice("pr-followup-vigie-4-r99")}
		}, replay.Ledger{}, false},
		{"announced by someone else", func(rd *route.Readiness) {
			c := startNotice("pr-followup-vigie-4-r99")
			c.Author = "mallory"
			rd.Comments = []event.IssueComment{c}
		}, replay.Ledger{}, true},
		{"another review's follow-up", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "pr-followup-vigie-4-r98", Phase: "Succeeded"}}
		}, replay.Ledger{}, true},
		{"an approving review", func(rd *route.Readiness) { rd.Reviews[0].Body = approve }, replay.Ledger{}, false},
		{"suggestions without the label", func(rd *route.Readiness) { rd.Reviews[0].Body = suggestions }, replay.Ledger{}, false},
		{"suggestions with the label", func(rd *route.Readiness) {
			rd.Reviews[0].Body = suggestions
			rd.PullRequest.Labels = []string{"kelos:fix-suggestions"}
		}, replay.Ledger{}, true},
		{"approving review, unresolved bot thread", func(rd *route.Readiness) {
			rd.Reviews[0].Body = approve
			rd.Threads = []event.ReviewThread{{Author: "gordon-cole-ai", Body: "a bug", URL: "u"}}
		}, replay.Ledger{}, true},
		{"approving review, resolved or foreign threads", func(rd *route.Readiness) {
			rd.Reviews[0].Body = approve
			rd.Threads = []event.ReviewThread{{Resolved: true, Author: "gordon-cole-ai", Body: "a bug"},
				{Author: "mallory", Body: "a bug"}}
		}, replay.Ledger{}, false},
		{"approving review, suggestion thread without the label", func(rd *route.Readiness) {
			rd.Reviews[0].Body = approve
			rd.Threads = []event.ReviewThread{{Author: "gordon-cole-ai", Body: "**Suggestion** rename"}}
		}, replay.Ledger{}, false},
		{"only the latest review counts", func(rd *route.Readiness) {
			rd.Reviews = append(rd.Reviews, headReview(100, approve, readyHead, time.Minute))
		}, replay.Ledger{}, false},
	}
	for _, c := range cases {
		rd := readyState()
		rd.Reviews = []event.Review{headReview(99, verdictBlocking, readyHead, time.Hour)}
		c.change(&rd)
		fd := decideReady(t, rd, c.led)
		if got := fd.Reason == route.ReasonFollowUpPending; got != c.pending {
			t.Errorf("%s: %s/%q, pending %v", c.name, fd.Outcome, fd.Reason, c.pending)
		}
	}
}

// capped is readyState at a one-round cap: review 50's follow-up ran
// (its start notice), review 99 on the head has findings nobody starts.
func capped(body string) (route.Readiness, *route.Router) {
	r := router()
	r.Config.Policy.MaxRoundsPerPR = 1
	rd := readyState()
	rd.Reviews = []event.Review{headReview(50, verdictBlocking, "aaaaaaaa", 2*time.Hour), headReview(99, body, readyHead, time.Hour)}
	rd.Comments = []event.IssueComment{startNotice("pr-followup-vigie-4-r50")}
	return rd, &r
}

func decideWith(t *testing.T, r *route.Router, rd route.Readiness) route.FamilyDecision {
	t.Helper()
	dec := r.DecideAuthenticated(readinessDelivery(t), route.State{Readiness: []route.Readiness{rd}, ReadinessRead: true}, replay.Ledger{})
	return family(t, dec)
}

func TestReadinessAnnouncesTheRoundLimit(t *testing.T) {
	rd, r := capped(verdictBlocking)
	rd.Task.Labels = append(rd.Task.Labels, "ready to merge")
	fd := decideWith(t, r, rd)
	limit := "post-notice:" + readyTask + ":round-limit:" + headDetail(notice.LimitedExtra([]notice.Limited{{Author: gordonBot, Review: 99}}))
	roundLabel := "add-label:" + readyTask + ":round-limit-reached"
	expectFamily(t, fd, "pr-readiness", route.Act, route.ReasonRoundLimit)
	expectActions(t, fd, []string{limit, "remove-label:" + readyTask + ":ready to merge", actionNeeded, roundLabel},
		map[string]string{actionNeeded: limit, roundLabel: actionNeeded})
	// Announced: the labels only, and nothing once they are on.
	rd.TaskComments = append(rd.TaskComments, event.TaskComment{Author: "gordon",
		Body: "<p>ROUND LIMIT REACHED: <a>sperano/vigie#4</a>. HEAD <code>01234567</code>. ...</p>"})
	rd.Task.Labels = []string{"pr open", "round-limit-reached", "action-needed"}
	fd = decideWith(t, r, rd)
	expectFamily(t, fd, "pr-readiness", route.Duplicate, route.ReasonRoundLimitAnnounced)
	if len(fd.Actions) != 0 {
		t.Errorf("announced round limit wrote %v", keys(fd.Actions))
	}
}

func TestReadinessRoundLimitCases(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*route.Readiness, *route.Router)
		reason  string
		limited []notice.Limited
	}{
		{"blocking verdict check at the cap", func(rd *route.Readiness, _ *route.Router) {
			rd.CheckRuns[1].Conclusion = "failure"
		}, route.ReasonRoundLimit, []notice.Limited{{Author: gordonBot, Review: 99}}},
		{"CI failure with a blocking verdict at the cap", func(rd *route.Readiness, _ *route.Router) {
			rd.CheckRuns[0].Conclusion, rd.CheckRuns[1].Conclusion = "failure", "failure"
		}, route.ReasonChecksUnacceptable, nil},
		{"no cap configured", func(_ *route.Readiness, r *route.Router) { r.Config.Policy.MaxRoundsPerPR = 0 },
			route.ReasonFollowUpPending, nil},
		{"below the cap", func(rd *route.Readiness, _ *route.Router) { rd.Comments = nil }, route.ReasonFollowUpPending, nil},
		{"announced review is no limit", func(rd *route.Readiness, _ *route.Router) {
			rd.Comments = append(rd.Comments, startNotice("pr-followup-vigie-4-r99"))
		}, route.ReasonReady, nil},
		{"older head's review when none on the head", func(rd *route.Readiness, _ *route.Router) {
			rd.Reviews[1].CommitID = "bbbbbbbb"
		}, route.ReasonRoundLimit, []notice.Limited{{Author: gordonBot, Review: 99}}},
		{"approving review with unresolved threads", func(rd *route.Readiness, _ *route.Router) {
			rd.Reviews[1].Body = approve
			rd.Threads = []event.ReviewThread{{Author: gordonBot, Body: "a bug"}}
		}, route.ReasonRoundLimit, []notice.Limited{{Author: gordonBot, Review: 99}}},
		{"approving review, threads resolved", func(rd *route.Readiness, _ *route.Router) {
			rd.Reviews[1].Body = approve
			rd.Threads = []event.ReviewThread{{Resolved: true, Author: gordonBot, Body: "a bug"}}
		}, route.ReasonReady, nil},
	}
	for _, c := range cases {
		rd, r := capped(verdictBlocking)
		c.change(&rd, r)
		fd := decideWith(t, r, rd)
		if fd.Reason != c.reason {
			t.Errorf("%s: %s/%q, want %q", c.name, fd.Outcome, fd.Reason, c.reason)
			continue
		}
		if c.limited == nil {
			for _, k := range keys(fd.Actions) {
				if strings.Contains(k, "round-limit") {
					t.Errorf("%s: %s without a round limit", c.name, k)
				}
			}
			continue
		}
		want := "post-notice:" + readyTask + ":round-limit:" + headDetail(notice.LimitedExtra(c.limited))
		if len(fd.Actions) == 0 || fd.Actions[0].Key != want {
			t.Errorf("%s: actions %v, want %s first", c.name, keys(fd.Actions), want)
		}
	}
}

func TestReadinessSuggestionsCapLimitsSuggestions(t *testing.T) {
	r := router()
	rd := readyState()
	rd.Reviews = []event.Review{headReview(99, suggestions, readyHead, time.Hour)}
	rd.Comments = []event.IssueComment{{Author: gordonBot, Body: notice.SuggestionsCapText(4, "kelos:fix-suggestions", "/gordon")}}
	r.Config.Policy.MaxRoundsPerPR = 0
	fd := decideWith(t, &r, rd)
	want := "post-notice:" + readyTask + ":round-limit:" + headDetail(notice.LimitedExtra([]notice.Limited{{Author: gordonBot, Review: 99}}))
	if fd.Reason != route.ReasonRoundLimit || !slices.Contains(keys(fd.Actions), want) {
		t.Errorf("suggestions cap: %q %v", fd.Reason, keys(fd.Actions))
	}
	rd.Comments[0].Author = "mallory"
	if fd := decideWith(t, &r, rd); fd.Reason != route.ReasonReady {
		t.Errorf("a cap notice by someone else counted: %q", fd.Reason)
	}
}
