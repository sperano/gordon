package sillage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// costBody is a cost response in Sillage's shape (trimmed from its
// mixed-models golden): fields Gordon does not read are ignored.
const costBody = `{
  "root": {"task": {"instance": "vikunja.example.com", "id": 301}},
  "prices": {"version": "2026-11-22.1", "verifiedAt": "2026-11-22", "sources": []},
  "assumptions": [{"code": "api-equivalent", "text": "An estimate."}],
  "feed": {"on": true, "readThrough": "2026-11-23T13:00:00Z"},
  "final": true,
  "total": {"status": "partial", "estimatedUSD": "18.886400", "reportedUSD": "15.660000", "requests": 9,
    "failedRequests": 1, "categories": [{"category": "output", "tokens": 231000, "estimatedUSD": "4.295000"}],
    "notes": ["cache-write-ttl-unknown"]},
  "byRole": [],
  "runs": [
    {"key": {"cluster": "c", "namespace": "kelos-agents", "uid": "uid-a"}, "taskName": "vikunja-x-1", "role": "implementation",
     "outcome": "failed", "final": true, "status": "complete", "estimatedUSD": "1.500000", "requests": 2, "failedRequests": 0,
     "categories": [], "attempts": [{"pod": "p1"}, {"pod": "p2"}], "models": [], "threads": [{"agent": ""}, {"agent": "sub-1"}]},
    {"key": {"cluster": "c", "namespace": "kelos-agents", "uid": "uid-b"}, "role": "pr-follow-up", "outcome": "running",
     "final": false, "status": "unknown", "requests": 0, "failedRequests": 0, "categories": [], "attempts": [], "models": [], "threads": []}
  ]
}`

func serve(t *testing.T, status int, body string) (*Client, *string) {
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck // test server
	}))
	t.Cleanup(s.Close)
	return &Client{BaseURL: s.URL + "/", HTTP: s.Client()}, &path
}

func TestTaskCost(t *testing.T) {
	c, path := serve(t, http.StatusOK, costBody)
	got, err := c.TaskCost(context.Background(), "vikunja.example.com", 301)
	if err != nil {
		t.Fatal(err)
	}
	if *path != "/api/v1/cost/task/vikunja.example.com/301" {
		t.Errorf("path %q", *path)
	}
	if got.Prices.Version != "2026-11-22.1" || got.Prices.VerifiedAt != "2026-11-22" || !got.Final || !got.Feed.On ||
		got.Total.Status != StatusPartial || got.Total.EstimatedUSD != "18.886400" || len(got.Runs) != 2 ||
		len(got.Assumptions) != 1 || got.Assumptions[0].Code != "api-equivalent" {
		t.Errorf("cost %+v", got)
	}
	r, ok := got.RunByUID("uid-a")
	if !ok || r.TaskName != "vikunja-x-1" || !r.Final || r.EstimatedUSD != "1.500000" || len(r.Attempts) != 2 ||
		len(r.Threads) != 2 || r.Threads[1].Agent != "sub-1" {
		t.Errorf("run uid-a %+v, %v", r, ok)
	}
	if r, ok := got.RunByUID("uid-b"); !ok || r.Final || r.Status != StatusUnknown || r.EstimatedUSD != "" {
		t.Errorf("run uid-b %+v, %v", r, ok)
	}
	if _, ok := got.RunByUID(""); ok {
		t.Error("the empty uid matched a run")
	}
	if _, ok := got.RunByUID("uid-z"); ok {
		t.Error("an unknown uid matched a run")
	}
}

func TestTaskCostErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"no story", http.StatusNotFound, `{"title":"not found"}`, ErrNotFound},
		{"not JSON", http.StatusOK, "<html>", ErrIncompatible},
		{"no price version", http.StatusOK, `{"prices":{"verifiedAt":"d"},"total":{"status":"complete"}}`, ErrIncompatible},
		{"no price date", http.StatusOK, `{"prices":{"version":"v"},"total":{"status":"complete"}}`, ErrIncompatible},
		{"unknown total status", http.StatusOK, `{"prices":{"version":"v","verifiedAt":"d"},"total":{"status":"zero"}}`, ErrIncompatible},
		{"unknown run status", http.StatusOK, `{"prices":{"version":"v","verifiedAt":"d"},"total":{"status":"none"},` +
			`"runs":[{"key":{"uid":"u"},"status":""}]}`, ErrIncompatible},
		{"too large", http.StatusOK, `{"prices":{"version":"v","verifiedAt":"d"},"total":{"status":"none"},"pad":"` +
			strings.Repeat("x", maxResponseBytes) + `"}`, ErrIncompatible},
	} {
		cl, _ := serve(t, c.status, c.body)
		if _, err := cl.TaskCost(context.Background(), "v", 1); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// TestTaskCostAtTheSizeCap: a valid answer of exactly maxResponseBytes
// is read; the "too large" case above differs from it only by its size.
func TestTaskCostAtTheSizeCap(t *testing.T) {
	head, tail := `{"prices":{"version":"v","verifiedAt":"d"},"total":{"status":"none"},"pad":"`, `"}`
	body := head + strings.Repeat("x", maxResponseBytes-len(head)-len(tail)) + tail
	cl, _ := serve(t, http.StatusOK, body)
	if got, err := cl.TaskCost(context.Background(), "v", 1); err != nil || got.Prices.Version != "v" {
		t.Errorf("answer at the cap: %+v, %v", got.Prices, err)
	}
}

func TestTaskCostUnavailable(t *testing.T) {
	cl, _ := serve(t, http.StatusServiceUnavailable, "down")
	_, err := cl.TaskCost(context.Background(), "v", 1)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusServiceUnavailable || errors.Is(err, ErrNotFound) {
		t.Errorf("503: %v, want StatusError 503", err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	down := &Client{BaseURL: closed.URL, HTTP: &http.Client{}}
	if _, err := down.TaskCost(context.Background(), "v", 1); err == nil || errors.Is(err, ErrIncompatible) ||
		errors.Is(err, ErrNotFound) || !strings.HasPrefix(err.Error(), "sillage cost: ") {
		t.Errorf("refused connection: %v, want a transport error", err)
	}
}
