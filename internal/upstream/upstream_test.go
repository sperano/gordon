package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
)

var ctx = context.Background()

// blockedFor is how long a test lets a read that must wait show it waits.
const blockedFor = 50 * time.Millisecond

type observed struct {
	mu      sync.Mutex
	results []string
}

func (o *observed) observe(provider, result string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.results = append(o.results, provider+"/"+result)
}

func client(t *testing.T, provider string, h http.HandlerFunc) (*Client, *observed) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	o := &observed{}
	return &Client{Provider: provider, BaseURL: srv.URL, Authorization: "Bearer test-token",
		HTTP: srv.Client(), Observe: o.observe}, o
}

func TestVikunjaTask(t *testing.T) {
	c, o := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/tasks/42" || r.URL.Query().Get("expand") != "buckets" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request %s %s auth %q", r.Method, r.URL, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"id":42,"project_id":7,"title":"T","description":"<p>D</p>","done":false,"labels":[{"title":"in progress"}],
			"assignees":[{"username":"claude-opus"},{"username":"eric"}],"buckets":[{"title":"Doing"}]}`)
	})
	got, err := Vikunja{c}.Task(ctx, 42)
	want := event.Task{ID: 42, ProjectID: 7, Title: "T", Description: "<p>D</p>", Labels: []string{"in progress"},
		Assignees: []string{"claude-opus", "eric"}, Buckets: []string{"Doing"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Task = %+v, %v; want %+v", got, err, want)
	}
	if !reflect.DeepEqual(o.results, []string{"vikunja/ok"}) {
		t.Fatalf("observed %v", o.results)
	}
}

func TestVikunjaTaskInNoBucketHasEmptyBuckets(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":3,"project_id":1,"buckets":null}`)
	})
	got, err := Vikunja{c}.Task(ctx, 3)
	if err != nil || got.Buckets == nil || len(got.Buckets) != 0 {
		t.Fatalf("Task = %+v, %v; want non-nil empty buckets", got, err)
	}
}

func TestVikunjaTaskRefusesOtherTask(t *testing.T) {
	c, o := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":4}`)
	})
	if _, err := (Vikunja{c}).Task(ctx, 3); ClassOf(err) != Invalid {
		t.Fatalf("Task answering another id = %v, want invalid", err)
	}
	if !reflect.DeepEqual(o.results, []string{"vikunja/invalid"}) {
		t.Fatalf("observed %v, want the read counted invalid, not ok", o.results)
	}
	if _, err := (Vikunja{c}).Task(ctx, 0); ClassOf(err) != Invalid {
		t.Fatalf("Task(0) = %v, want invalid", err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status    int
		remaining string
		want      Class
	}{
		{http.StatusNotFound, "", NotFound}, {http.StatusGone, "", NotFound},
		{http.StatusUnauthorized, "", Unauthorized}, {http.StatusForbidden, "", Unauthorized},
		{http.StatusForbidden, "0", RateLimited}, {http.StatusTooManyRequests, "", RateLimited},
		{http.StatusBadGateway, "", Unavailable}, {http.StatusInternalServerError, "", Unavailable},
		{http.StatusBadRequest, "", Invalid},
	}
	for _, tc := range cases {
		c, o := client(t, ProviderGitHub, func(w http.ResponseWriter, _ *http.Request) {
			if tc.remaining != "" {
				w.Header().Set("X-RateLimit-Remaining", tc.remaining)
			}
			w.WriteHeader(tc.status)
		})
		_, err := Vikunja{c}.Task(ctx, 1)
		var e *Error
		if !errors.As(err, &e) || e.Class != tc.want || e.Status != tc.status {
			t.Errorf("HTTP %d (remaining %q) = %v, want class %s", tc.status, tc.remaining, err, tc.want)
		}
		if len(o.results) != 1 || o.results[0] != "github/"+string(tc.want) {
			t.Errorf("HTTP %d observed %v", tc.status, o.results)
		}
	}
}

func TestUnreachableIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c := &Client{Provider: ProviderVikunja, BaseURL: srv.URL, HTTP: http.DefaultClient}
	if _, err := (Vikunja{c}).Task(ctx, 1); ClassOf(err) != Unavailable {
		t.Fatalf("closed server = %v, want unavailable", err)
	}
}

func TestBadJSONAndOversizedAreInvalid(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id":`) })
	if _, err := (Vikunja{c}).Task(ctx, 1); ClassOf(err) != Invalid {
		t.Fatalf("truncated JSON = %v, want invalid", err)
	}
	big, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":1,"pad":"`+strings.Repeat("x", MaxResponseBytes)+`"}`)
	})
	_, err := Vikunja{big}.Task(ctx, 1)
	if ClassOf(err) != Invalid || !strings.Contains(err.Error(), "response over") {
		t.Fatalf("oversized response = %v, want invalid over the cap", err)
	}
}

func TestLimitBoundsInFlight(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		fmt.Fprint(w, `{"id":1}`)
	})
	c.Limit = NewLimit(1)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := (Vikunja{c}).Task(ctx, 1); first <- err }()
	<-entered // the first read holds the only slot
	go func() { _, err := (Vikunja{c}).Task(ctx, 1); second <- err }()
	select {
	case <-entered:
		t.Fatal("a second read reached the provider while the limit was full")
	case err := <-second:
		t.Fatalf("second read returned %v before the slot was free", err)
	case <-time.After(blockedFor):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first read = %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second read after the slot freed = %v", err)
	}
	if len(c.Limit) != 0 {
		t.Fatalf("%d slots still held after both reads", len(c.Limit))
	}
}

func TestLimitWaitEndsWithTheContext(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(http.ResponseWriter, *http.Request) {
		t.Error("read sent while the limit was full")
	})
	c.Limit = NewLimit(1)
	c.Limit <- struct{}{} // the only slot is taken
	short, cancel := context.WithTimeout(ctx, blockedFor)
	defer cancel()
	if _, err := (Vikunja{c}).Task(short, 1); ClassOf(err) != Unavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Task with a full limit = %v, want unavailable once its deadline passed", err)
	}
	if NewLimit(0) != nil {
		t.Fatal("NewLimit(0) is not unbounded")
	}
}

func TestGitHubPullRequestWithFiles(t *testing.T) {
	var paths []string
	c, _ := client(t, ProviderGitHub, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("X-GitHub-Api-Version") != GitHubAPIVersion {
			t.Errorf("missing API version header")
		}
		switch {
		case r.URL.Path == "/repos/sperano/gordon/pulls/7":
			fmt.Fprint(w, `{"number":7,"title":"Fix","html_url":"https://github.com/sperano/gordon/pull/7","state":"open","draft":true,"merged":false,"user":{"login":"bot"},
				"head":{"ref":"kelos/vikunja-5","sha":"abc","repo":{"full_name":"sperano/gordon"}},
				"base":{"ref":"main"},"labels":[{"name":"kelos:claude-opus"}]}`)
		case r.URL.Query().Get("page") == "1":
			names := make([]string, filesPerPage)
			for i := range names {
				names[i] = fmt.Sprintf(`{"filename":"f%d.go"}`, i)
			}
			fmt.Fprint(w, "["+strings.Join(names, ",")+"]")
		default:
			fmt.Fprint(w, `[{"filename":"last.md"}]`)
		}
	})
	c.Headers = GitHubHeaders
	pr, err := GitHub{c}.PullRequest(ctx, "sperano/gordon", 7, true)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Repository != "sperano/gordon" || pr.Number != 7 || pr.Title != "Fix" || pr.URL != "https://github.com/sperano/gordon/pull/7" ||
		pr.State != "open" || !pr.Draft || pr.Author != "bot" ||
		pr.HeadRef != "kelos/vikunja-5" || pr.HeadSHA != "abc" || pr.HeadRepo != "sperano/gordon" ||
		pr.BaseRef != "main" || !reflect.DeepEqual(pr.Labels, []string{"kelos:claude-opus"}) {
		t.Fatalf("pull request = %+v", pr)
	}
	if len(pr.Files) != filesPerPage+1 || pr.Files[0] != "f0.go" || pr.Files[filesPerPage] != "last.md" {
		t.Fatalf("files = %d (%v), want both pages", len(pr.Files), pr.Files)
	}
	want := []string{"/repos/sperano/gordon/pulls/7?", "/repos/sperano/gordon/pulls/7/files?page=1&per_page=100",
		"/repos/sperano/gordon/pulls/7/files?page=2&per_page=100"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("requests %v, want %v", paths, want)
	}
}

func TestGitHubPullRequestWithoutFilesReadsOnce(t *testing.T) {
	calls := 0
	c, _ := client(t, ProviderGitHub, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, `{"number":7,"state":"closed","merged":true}`)
	})
	pr, err := GitHub{c}.PullRequest(ctx, "sperano/gordon", 7, false)
	if err != nil || !pr.Merged || pr.Files != nil || calls != 1 {
		t.Fatalf("PullRequest = %+v, %v after %d calls; want merged, no files, one call", pr, err, calls)
	}
}

func TestGitHubPullRequestRefusesOtherNumber(t *testing.T) {
	c, o := client(t, ProviderGitHub, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":8,"state":"open"}`)
	})
	_, err := GitHub{c}.PullRequest(ctx, "sperano/gordon", 7, false)
	if ClassOf(err) != Invalid || !strings.Contains(err.Error(), "got 8") {
		t.Fatalf("PullRequest answering #8 = %v, want invalid", err)
	}
	if !reflect.DeepEqual(o.results, []string{"github/invalid"}) {
		t.Fatalf("observed %v, want the read counted invalid", o.results)
	}
}

func TestGitHubPullRequestRefusesIncompleteFiles(t *testing.T) {
	c, _ := client(t, ProviderGitHub, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			fmt.Fprint(w, `[{"filename":"a.go"}]`)
			return
		}
		fmt.Fprint(w, `{"number":7,"changed_files":3001}`)
	})
	_, err := GitHub{c}.PullRequest(ctx, "sperano/gordon", 7, true)
	if ClassOf(err) != Invalid || !strings.Contains(err.Error(), "lists 1 of its 3001 files") {
		t.Fatalf("PullRequest with a truncated file list = %v, want invalid", err)
	}
}

func TestGitHubPullRequestFailsOnAFailedFilesPage(t *testing.T) {
	c, _ := client(t, ProviderGitHub, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `{"number":7}`)
		case r.URL.Query().Get("page") == "1":
			names := make([]string, filesPerPage)
			for i := range names {
				names[i] = fmt.Sprintf(`{"filename":"f%d.go"}`, i)
			}
			fmt.Fprint(w, "["+strings.Join(names, ",")+"]")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	pr, err := GitHub{c}.PullRequest(ctx, "sperano/gordon", 7, true)
	if ClassOf(err) != Unavailable || pr.Files != nil {
		t.Fatalf("PullRequest with page 2 failing = %d files, %v; want no files and unavailable", len(pr.Files), err)
	}
}

func TestGitHubRefusesUnsafeRepository(t *testing.T) {
	c, _ := client(t, ProviderGitHub, func(http.ResponseWriter, *http.Request) {
		t.Error("request sent for an unsafe repository")
	})
	for _, repo := range []string{"../../user", "a/b/c", "a/b?x=1", "", "a", "sperano/..", "../gordon", "sperano/.", "./gordon"} {
		if _, err := (GitHub{c}).PullRequest(ctx, repo, 1, false); ClassOf(err) != Invalid {
			t.Errorf("PullRequest(%q) = %v, want invalid", repo, err)
		}
	}
	if _, err := (GitHub{c}).PullRequest(ctx, "a/b", 0, false); ClassOf(err) != Invalid {
		t.Errorf("PullRequest(#0) = %v, want invalid", err)
	}
}

func TestVikunjaComments(t *testing.T) {
	c, o := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks/42/comments" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request %s auth %q", r.URL, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, ` [{"id":1,"comment":"<p>hi</p>"}]`)
	})
	got, err := Vikunja{c}.Comments(ctx, 42)
	if err != nil || string(got) != `[{"id":1,"comment":"<p>hi</p>"}]` {
		t.Fatalf("Comments = %s, %v; want the raw array", got, err)
	}
	if !reflect.DeepEqual(o.results, []string{"vikunja/ok"}) {
		t.Fatalf("observed %v", o.results)
	}
}

func TestVikunjaCommentsRefusals(t *testing.T) {
	c, o := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"message":"not a list"}`)
	})
	if _, err := (Vikunja{c}).Comments(ctx, 42); ClassOf(err) != Invalid {
		t.Fatalf("object answer = %v, want invalid", err)
	}
	if _, err := (Vikunja{c}).Comments(ctx, 0); ClassOf(err) != Invalid || len(o.results) != 1 {
		t.Fatalf("Comments(0) = %v after %d requests, want invalid without a request", err, len(o.results))
	}
}
