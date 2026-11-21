package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
)

// paged serves items listPerPage at a time and records the queries.
func paged(t *testing.T, path string, items []map[string]any, queries *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("path %s, want %s", r.URL.Path, path)
		}
		*queries = append(*queries, r.URL.RawQuery)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := min((page-1)*listPerPage, len(items))
		_ = json.NewEncoder(w).Encode(items[start:min(start+listPerPage, len(items))])
	}
}

func TestReviewsSkipPendingAndPage(t *testing.T) {
	var items []map[string]any
	for i := range listPerPage + 1 {
		items = append(items, map[string]any{"id": i + 1, "node_id": fmt.Sprint("N", i+1), "body": "b", "state": "COMMENTED",
			"commit_id": "abc", "user": map[string]any{"login": "cubic-dev-ai[bot]"}, "submitted_at": "2026-11-21T10:00:00Z"})
	}
	items[0]["state"] = pendingReview
	var queries []string
	c, _ := client(t, ProviderGitHub, paged(t, "/repos/sperano/vigie/pulls/4/reviews", items, &queries))
	got, err := GitHub{c}.Reviews(ctx, "sperano/vigie", 4)
	if err != nil || len(got) != listPerPage || len(queries) != 2 {
		t.Fatalf("%d reviews over %d pages, %v; want the %d submitted over 2", len(got), len(queries), err, listPerPage)
	}
	want := ListedReview{Review: event.Review{ID: 2, Author: "cubic-dev-ai[bot]", Body: "b", CommitID: "abc"},
		NodeID: "N2", Submitted: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("first review %+v, want %+v", got[0], want)
	}
	if _, err := (GitHub{c}).Reviews(ctx, "../x", 4); ClassOf(err) != Invalid {
		t.Errorf("unsafe repository: %v", err)
	}
}

func TestHistory(t *testing.T) {
	c, _ := client(t, ProviderGitHub, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/sperano/vigie/pulls/4/reviews":
			fmt.Fprint(w, `[{"id":1,"body":"r","state":"APPROVED","commit_id":"h","user":{"login":"bot"}}]`)
		case "/repos/sperano/vigie/issues/4/comments":
			fmt.Fprint(w, `[{"id":9,"body":"c","user":{"login":"gordon-cole-ai[bot]"}}]`)
		default:
			http.NotFound(w, r)
		}
	})
	reviews, comments, err := GitHub{c}.History(ctx, "sperano/vigie", 4)
	if err != nil || !reflect.DeepEqual(reviews, []event.Review{{ID: 1, Author: "bot", Body: "r", CommitID: "h"}}) ||
		!reflect.DeepEqual(comments, []event.IssueComment{{ID: 9, Author: "gordon-cole-ai[bot]", Body: "c"}}) {
		t.Fatalf("History = %+v %+v %v", reviews, comments, err)
	}
	if _, _, err := (GitHub{c}).History(ctx, "sperano/vigie", 5); ClassOf(err) != NotFound {
		t.Errorf("missing PR: %v", err)
	}
}

func pr(n int, updated, closed string, merged bool) map[string]any {
	p := map[string]any{"number": n, "state": "closed", "user": map[string]any{"login": "sperano"}, "updated_at": updated,
		"head": map[string]any{"ref": "kelos/vikunja-5", "sha": "abc", "repo": map[string]any{"full_name": "sperano/vigie"}},
		"base": map[string]any{"ref": "main"}, "labels": []map[string]any{{"name": "kelos:follow-up"}}}
	if closed != "" {
		p["closed_at"] = closed
	}
	if merged {
		p["merged_at"] = closed
	}
	return p
}

func TestClosedPullRequestsStopAtTheWindow(t *testing.T) {
	since := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var items []map[string]any
	for i := range listPerPage {
		items = append(items, pr(i+1, "2026-11-21T10:00:00Z", "2026-11-21T09:00:00Z", i == 0))
	}
	items[1]["closed_at"] = "2026-11-19T09:00:00Z"                                        // updated in the window, closed before it
	items = append(items, pr(500, "2026-11-19T10:00:00Z", "2026-11-19T10:00:00Z", false)) // the next page: older
	items[listPerPage-1]["updated_at"] = "2026-11-19T10:00:00Z"                           // the page ends before the window
	var queries []string
	c, _ := client(t, ProviderGitHub, paged(t, "/repos/sperano/vigie/pulls", items, &queries))
	got, err := GitHub{c}.ClosedPullRequests(ctx, "sperano/vigie", since)
	if err != nil || len(queries) != 1 {
		t.Fatalf("%v over %d pages; want one page, its last update before the window", err, len(queries))
	}
	if q := queries[0]; q != "direction=desc&page=1&per_page=100&sort=updated&state=closed" {
		t.Errorf("query %s", q)
	}
	if len(got) != listPerPage-1 || got[0].Number != 1 || !got[0].Merged || got[1].Number != 3 || got[1].Merged {
		t.Fatalf("closed %d, first %+v", len(got), got[0])
	}
	want := event.PullRequest{Repository: "sperano/vigie", Number: 1, State: "closed", Merged: true, Author: "sperano",
		HeadRef: "kelos/vikunja-5", HeadSHA: "abc", HeadRepo: "sperano/vigie", BaseRef: "main", Labels: []string{"kelos:follow-up"}}
	if !reflect.DeepEqual(got[0].PullRequest, want) || !got[0].Closed.Equal(time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("listed %+v", got[0])
	}
}

func TestOpenPullRequests(t *testing.T) {
	var queries []string
	c, _ := client(t, ProviderGitHub, paged(t, "/repos/sperano/vigie/pulls",
		[]map[string]any{pr(4, "2026-11-21T10:00:00Z", "", false)}, &queries))
	got, err := GitHub{c}.OpenPullRequests(ctx, "sperano/vigie")
	if err != nil || len(got) != 1 || got[0].Number != 4 || !got[0].Closed.IsZero() || queries[0] != "page=1&per_page=100&state=open" {
		t.Fatalf("open %+v %v %v", got, err, queries)
	}
}

func TestCommentsSince(t *testing.T) {
	since := time.Date(2026, 11, 20, 12, 0, 0, 0, time.FixedZone("x", 3600))
	for _, inline := range []bool{false, true} {
		path, url, kind := "/repos/sperano/vigie/issues/comments", "issue_url", "issues"
		if inline {
			path, url, kind = "/repos/sperano/vigie/pulls/comments", "pull_request_url", "pulls"
		}
		var queries []string
		c, _ := client(t, ProviderGitHub, paged(t, path, []map[string]any{
			{"id": 7, "node_id": "IC_7", "body": "/gordon go", "html_url": "h", "user": map[string]any{"login": "sperano"},
				"created_at": "2026-11-21T10:00:00Z", url: "https://api.github.com/repos/sperano/vigie/" + kind + "/12",
				"pull_request_review_id": 3},
			{"id": 8, "body": "x", "user": map[string]any{"login": "a"}, url: "https://api.github.com/repos/sperano/vigie/other/13"},
		}, &queries))
		got, err := GitHub{c}.CommentsSince(ctx, "sperano/vigie", since, inline)
		if err != nil || len(got) != 2 {
			t.Fatalf("inline %v: %+v %v", inline, got, err)
		}
		want := ListedComment{ID: 7, ReviewID: 3, NodeID: "IC_7", Body: "/gordon go", URL: "h", Author: "sperano", PR: 12,
			Created: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
		if !reflect.DeepEqual(got[0], want) || got[1].PR != 0 {
			t.Errorf("inline %v: %+v / %+v", inline, got[0], got[1])
		}
		if q := queries[0]; q != "direction=asc&page=1&per_page=100&since=2026-11-20T11%3A00%3A00Z&sort=created" {
			t.Errorf("inline %v: query %s", inline, q)
		}
	}
}

func TestPRNumber(t *testing.T) {
	for u, want := range map[string]int{
		"https://api.github.com/repos/o/r/issues/12": 12, "https://api.github.com/repos/o/r/pulls/12": 0,
		"https://api.github.com/repos/o/r/issues/0": 0, "https://api.github.com/repos/o/r/issues/x": 0, "%%": 0, "": 0,
	} {
		if got := prNumber(u, "issues"); got != want {
			t.Errorf("prNumber(%q) = %d, want %d", u, got, want)
		}
	}
}

func TestListStopsAtThePageBound(t *testing.T) {
	full := make([]map[string]any, listPerPage)
	for i := range full {
		full[i] = map[string]any{"id": i}
	}
	c, _ := client(t, ProviderGitHub, func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(full) })
	if _, err := (GitHub{c}).IssueComments(ctx, "sperano/vigie", 4); ClassOf(err) != Invalid {
		t.Fatalf("endless listing: %v, want invalid", err)
	}
}
