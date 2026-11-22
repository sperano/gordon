package ghlife

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

const mergeHead = "0123456789abcdef0123456789abcdef01234567"

// mergeGitHub is a fake of the pull request a merge reads and merges.
type mergeGitHub struct {
	*httptest.Server
	mu         sync.Mutex
	state      string
	merged     bool
	head       string
	answer     int    // the merge's status; 0 merges
	answerBody string // its body when it is refused
	limited    bool   // a refusal is a rate limit
	merges     []map[string]string
	tokens     []string
}

func newMergeGitHub(t *testing.T) *mergeGitHub {
	g := &mergeGitHub{state: "open", head: mergeHead}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

func (g *mergeGitHub) Token(_ context.Context, repo string, perms map[string]string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var parts []string
	for k, v := range perms {
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	g.tokens = append(g.tokens, repo+":"+strings.Join(parts, ","))
	return "tok", nil
}

func (g *mergeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/sperano/vigie/pulls/4":
		reply(w, map[string]any{"state": g.state, "merged": g.merged, "head": map[string]string{"sha": g.head}})
	case r.Method == http.MethodPut && r.URL.Path == "/repos/sperano/vigie/pulls/4/merge":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.merges = append(g.merges, body)
		if g.answer == 0 {
			g.merged, g.state = true, "closed"
			reply(w, map[string]any{"merged": true})
			return
		}
		if g.limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
		}
		w.WriteHeader(g.answer)
		_, _ = io.WriteString(w, g.answerBody)
	default:
		http.Error(w, "no route", http.StatusNotFound)
	}
}

// vikunjaNotices records the notices the merge asks the Vikunja executor for.
type vikunjaNotices struct{ posted []action.Action }

func (v *vikunjaNotices) Execute(_ context.Context, a action.Action) error {
	v.posted = append(v.posted, a)
	return nil
}

func (v *vikunjaNotices) Recover(context.Context, action.Action) (bool, error) { return false, nil }

func mergeExecutor(g *mergeGitHub, v *vikunjaNotices) *Executor {
	ex := &Executor{W: &Writer{App: g, BaseURL: g.URL, HTTP: g.Client()}, Policy: profile.Defaults().Policy}
	if v != nil {
		ex.Vikunja = v
	}
	return ex
}

func mergeAction() action.Action {
	a := action.New(action.MergePullRequest, action.PullRequest("sperano/vigie", 4), mergeHead)
	a.Detail = "5"
	return a
}

func TestMergePinsTheHead(t *testing.T) {
	g := newMergeGitHub(t)
	ex := mergeExecutor(g, &vikunjaNotices{})
	ex.Policy.Readiness.MergeMethod = "rebase"
	for range 2 {
		if err := ex.Execute(ctx, mergeAction()); err != nil {
			t.Fatal(err)
		}
	}
	want := []map[string]string{{"sha": mergeHead, "merge_method": "rebase"}}
	if len(g.merges) != 1 || !mapsEqual(g.merges[0], want[0]) {
		t.Fatalf("merges %v, want %v once", g.merges, want)
	}
	if !slices.Contains(g.tokens, "vigie:contents=write,pull_requests=write") {
		t.Errorf("tokens %v: the merge needs its own write token", g.tokens)
	}
	if done, err := ex.Recover(ctx, mergeAction()); !done || err != nil {
		t.Errorf("Recover after the merge = %v, %v", done, err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestMergeLeavesWhatIsNotItsHead(t *testing.T) {
	for name, change := range map[string]func(*mergeGitHub){
		"merged by hand": func(g *mergeGitHub) { g.merged, g.state = true, "closed" },
		"closed":         func(g *mergeGitHub) { g.state = "closed" },
		"pushed since":   func(g *mergeGitHub) { g.head = "fedcba9876543210" },
	} {
		g := newMergeGitHub(t)
		change(g)
		if err := mergeExecutor(g, nil).Execute(ctx, mergeAction()); err != nil || len(g.merges) != 0 {
			t.Errorf("%s: %v, merges %v", name, err, g.merges)
		}
	}
	bad := mergeAction()
	bad.Value = "not-a-sha"
	if err := mergeExecutor(newMergeGitHub(t), nil).Execute(ctx, bad); !worker.IsPermanent(err) {
		t.Errorf("bad head: %v", err)
	}
}

func TestMergeRefusalIsReported(t *testing.T) {
	g := newMergeGitHub(t)
	g.answer = http.StatusMethodNotAllowed
	g.answerBody = "{\"message\":\"Pull Request is not mergeable\",\n  \"status\":\"405\"}"
	v := &vikunjaNotices{}
	if err := mergeExecutor(g, v).Execute(ctx, mergeAction()); err != nil {
		t.Fatal(err)
	}
	if len(v.posted) != 1 {
		t.Fatalf("notices %+v", v.posted)
	}
	n := v.posted[0]
	msg := "PUT " + g.URL + "/repos/sperano/vigie/pulls/4/merge: HTTP 405 Method Not Allowed: " +
		`{"message":"Pull Request is not mergeable", "status":"405"}`
	want := action.Notice(action.VikunjaTask(5), notice.MergeRefused,
		notice.Head{Repo: "sperano/vigie", Number: 4, SHA: mergeHead, Extra: msg}.Detail())
	if n.Key != want.Key || n.Value != want.Value || n.Detail != want.Detail || n.Target != want.Target {
		t.Errorf("notice %+v, want %+v", n, want)
	}
	if err := mergeExecutor(g, nil).Execute(ctx, mergeAction()); !worker.IsPermanent(err) {
		t.Errorf("refusal without a Vikunja writer: %v", err)
	}
	noTask := mergeAction()
	noTask.Detail = ""
	if err := mergeExecutor(g, v).Execute(ctx, noTask); !worker.IsPermanent(err) {
		t.Errorf("refusal without a task: %v", err)
	}
}

func TestMergeRetriesWhatIsNoRefusal(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		limited bool
	}{{"server error", http.StatusBadGateway, false}, {"rate limit", http.StatusForbidden, true},
		{"not found", http.StatusNotFound, false}}
	for _, c := range cases {
		g := newMergeGitHub(t)
		g.answer, g.limited = c.status, c.limited
		v := &vikunjaNotices{}
		if err := mergeExecutor(g, v).Execute(ctx, mergeAction()); err == nil || len(v.posted) != 0 {
			t.Errorf("%s: %v with notices %v, want the error and no refusal notice", c.name, err, v.posted)
		}
	}
	g := newMergeGitHub(t)
	g.answer = http.StatusConflict
	g.mu.Lock()
	g.head = mergeHead
	g.mu.Unlock()
	// A refusal for a PR merged meanwhile is no refusal.
	g.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			g.mu.Lock()
			g.merged = true
			g.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		g.serve(w, r)
	})
	v := &vikunjaNotices{}
	if err := mergeExecutor(g, v).Execute(ctx, mergeAction()); err != nil || len(v.posted) != 0 {
		t.Errorf("merged meanwhile: %v, notices %v", err, v.posted)
	}
}

// readsGitHub serves the readiness reads.
func readsGitHub(t *testing.T, pages map[string][]any) (*Reads, *[]string) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.Path
		if r.URL.Path == "/graphql" {
			var req struct {
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			key = fmt.Sprintf("graphql after=%v", req.Variables["after"])
		} else if p := r.URL.Query().Get("page"); p != "" {
			key += "?page=" + p
		}
		seen = append(seen, key)
		answers, ok := pages[key]
		if !ok || len(answers) == 0 {
			http.Error(w, "no "+key, http.StatusNotFound)
			return
		}
		reply(w, answers[0])
	}))
	t.Cleanup(srv.Close)
	g := &mergeGitHub{}
	return &Reads{W: &Writer{App: g, BaseURL: srv.URL, HTTP: srv.Client()}}, &seen
}

func TestReadsCheckRunsEveryPage(t *testing.T) {
	path := "/repos/sperano/vigie/commits/" + mergeHead + "/check-runs"
	first := make([]map[string]any, readPageSize)
	for i := range first {
		first[i] = map[string]any{"id": i + 1, "name": fmt.Sprintf("c%d", i), "status": "queued", "conclusion": nil}
	}
	r, seen := readsGitHub(t, map[string][]any{
		path + "?page=1": {map[string]any{"total_count": readPageSize + 1, "check_runs": first}},
		path + "?page=2": {map[string]any{"total_count": readPageSize + 1, "check_runs": []map[string]any{
			{"id": 900, "name": "check", "status": "completed", "conclusion": "success", "completed_at": "2026-11-22T11:00:00Z"}}}},
	})
	runs, err := r.CheckRuns(ctx, "sperano/vigie", mergeHead)
	if err != nil || len(runs) != readPageSize+1 || len(*seen) != 2 {
		t.Fatalf("%d runs over %v, %v", len(runs), *seen, err)
	}
	last := runs[len(runs)-1]
	want := event.CheckRun{ID: 900, Name: "check", Status: "completed", Conclusion: "success",
		CompletedAt: time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)}
	if last != want || runs[0].Conclusion != "" {
		t.Errorf("runs %+v ... %+v", runs[0], last)
	}
	if _, err := r.CheckRuns(ctx, "sperano/vigie", "../x"); !worker.IsPermanent(err) {
		t.Errorf("bad SHA: %v", err)
	}
	if _, err := r.CheckRuns(ctx, "../vigie", mergeHead); !worker.IsPermanent(err) {
		t.Errorf("bad repository: %v", err)
	}
}

func TestReadsCommittedAndThreads(t *testing.T) {
	r, seen := readsGitHub(t, map[string][]any{
		"/repos/sperano/vigie/commits/" + mergeHead: {map[string]any{"commit": map[string]any{
			"committer": map[string]any{"date": "2026-11-22T10:30:00Z"}}}},
		"graphql after=<nil>": {threadsPage(true, "c1", map[string]any{"isResolved": false,
			"comments": map[string]any{"nodes": []any{map[string]any{"body": "bug", "url": "u1", "author": map[string]any{"login": "gordon-cole-ai"}}}}},
			map[string]any{"isResolved": true, "comments": map[string]any{"nodes": []any{}}})},
		"graphql after=c1": {threadsPage(false, "", map[string]any{"isResolved": true,
			"comments": map[string]any{"nodes": []any{map[string]any{"body": "ok", "url": "u2", "author": nil}}}})},
	})
	at, err := r.Committed(ctx, "sperano/vigie", mergeHead)
	if err != nil || !at.Equal(time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("Committed = %v, %v", at, err)
	}
	threads, err := r.ReviewThreads(ctx, "sperano/vigie", 4)
	want := []event.ReviewThread{{Author: "gordon-cole-ai", Body: "bug", URL: "u1"}, {Resolved: true, Body: "ok", URL: "u2"}}
	if err != nil || !slices.Equal(threads, want) {
		t.Errorf("ReviewThreads = %+v, %v; want %+v", threads, err, want)
	}
	if !slices.Contains(*seen, "graphql after=c1") {
		t.Errorf("second page not read: %v", *seen)
	}
	noCursor, _ := readsGitHub(t, map[string][]any{"graphql after=<nil>": {threadsPage(true, "")}})
	if _, err := noCursor.ReviewThreads(ctx, "sperano/vigie", 4); err == nil {
		t.Error("a next page without a cursor was accepted")
	}
}

func threadsPage(next bool, cursor string, nodes ...any) map[string]any {
	if nodes == nil {
		nodes = []any{}
	}
	return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"reviewThreads": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}}}}}
}

func readinessOwners(projects ...string) rollout.Table {
	var t rollout.Table
	for _, p := range projects {
		t.Rules = append(t.Rules, rollout.Rule{Project: p, Family: run.PRReadiness, Owner: rollout.Shadow,
			Evidence: []rollout.Gate{rollout.ReplayParity}})
	}
	return t
}

func TestReadinessReconcilerQueuesCandidates(t *testing.T) {
	cfg := profile.Defaults()
	draft := agentPR(5, "sperano", "kelos/vikunja-6")
	draft.Draft = true
	lister := &fakeLister{open: map[string][]upstream.ListedPR{
		"sperano/vigie": {agentPR(4, "sperano", "kelos/vikunja-5"), draft, agentPR(6, "renovate[bot]", "kelos/vikunja-7"),
			agentPR(7, "sperano", "feature/x")},
		"sperano/hollingsworth": {agentPR(8, "sperano", "kelos/vikunja-9")},
	}}
	in := &fakeInbox{}
	m := metrics.New()
	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	rr := &ReadinessReconciler{GitHub: lister, Inbox: in, Config: cfg, Ownership: readinessOwners("vigie", "portal"),
		Interval: time.Minute, Now: func() time.Time { return at }, Metrics: m, Log: slog.New(slog.DiscardHandler)}
	if err := rr.Once(ctx); err != nil {
		t.Fatal(err)
	}
	// portal shares hollingsworth's repository but is not its PR project.
	window := at.Unix() / 60
	want := []string{fmt.Sprintf("github/delivery/gordon-readiness/sperano/vigie/4/abc12345/%d", window)}
	var got []string
	for _, d := range in.stored {
		got = append(got, d.DeliveryKey)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
	ev, err := event.Parse(event.Delivery{Source: in.stored[0].Source, Headers: in.stored[0].Headers, Body: in.stored[0].Body})
	if err != nil || ev.Action != event.ActionReadiness || !ev.GitHub.Synthetic || ev.GitHub.PullRequest.Number != 4 ||
		ev.GitHub.PullRequest.HeadSHA != "abc12345" {
		t.Errorf("delivery %+v, %v", ev, err)
	}
	if n := testutil.ToFloat64(m.ReadinessReconcileQueued); n != 1 {
		t.Errorf("queued metric %v", n)
	}
	// The same window queues nothing new; the next one does.
	if err := rr.Once(ctx); err != nil || len(in.stored) != 1 {
		t.Errorf("same window: %v, %d stored", err, len(in.stored))
	}
	at = at.Add(time.Minute)
	if err := rr.Once(ctx); err != nil || len(in.stored) != 2 {
		t.Errorf("next window: %v, %d stored", err, len(in.stored))
	}
}

func TestReadinessReconcilerSkipsLegacyAndKeepsGoing(t *testing.T) {
	cfg := profile.Defaults()
	lister := &fakeLister{fail: "sperano/vigie", open: map[string][]upstream.ListedPR{
		"sperano/lucarne": {agentPR(4, "sperano", "kelos/vikunja-5")},
		"sperano/sillage": {agentPR(5, "sperano", "kelos/vikunja-6")},
	}}
	lister.open["sperano/lucarne"][0].Repository, lister.open["sperano/lucarne"][0].HeadRepo = "sperano/lucarne", "sperano/lucarne"
	in := &fakeInbox{}
	rr := &ReadinessReconciler{GitHub: lister, Inbox: in, Config: cfg, Ownership: readinessOwners("vigie", "lucarne"),
		Interval: time.Minute, Metrics: metrics.New(), Log: slog.New(slog.DiscardHandler)}
	err := rr.Once(ctx)
	if err == nil || !strings.Contains(err.Error(), "sperano/vigie") || len(in.stored) != 1 ||
		!strings.Contains(in.stored[0].DeliveryKey, "sperano/lucarne/4/") {
		t.Errorf("err %v, stored %+v: want vigie's failure reported, lucarne queued, sillage (legacy) skipped", err, in.stored)
	}
	if errors.Is(err, context.Canceled) {
		t.Error("unexpected cancellation")
	}
}
