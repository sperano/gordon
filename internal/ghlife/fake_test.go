package ghlife

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/store"
)

const (
	owner   = "sperano"
	repoNm  = "vigie"
	appBot  = "gordon-cole-ai[bot]"
	prNode  = "PR_4"
	labelID = "LA_follow"
)

// fakeGitHub is an in-memory GitHub API: the GraphQL operations and REST
// routes the writer calls, for pull request 4 of sperano/vigie. failNext
// makes the next request matching an operation apply its write, then
// answer 500: a write whose outcome the caller cannot know.
type fakeGitHub struct {
	*httptest.Server
	mu        sync.Mutex
	draft     bool
	labels    []string
	repoLabel bool
	reacted   map[string]bool // node -> EYES by the App
	// othersEyes: node -> EYES by someone else, a group the App's
	// viewerHasReacted is false in until it reacts too.
	othersEyes map[string]bool
	checks     map[string][]map[string]any
	comments   []comment
	private    bool
	writes     []string
	tokens     []string
	failNext   map[string]bool
	nextID     int64
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{repoLabel: true, reacted: map[string]bool{}, othersEyes: map[string]bool{}, checks: map[string][]map[string]any{},
		failNext: map[string]bool{}, nextID: 500}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Token is the fake App: it records each scope minted.
func (f *fakeGitHub) Token(_ context.Context, repo string, perms map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := slices.Sorted(maps.Keys(perms))
	f.tokens = append(f.tokens, repo+":"+strings.Join(keys, ","))
	return "tok", nil
}

func (f *fakeGitHub) writer() *Writer { return &Writer{App: f, BaseURL: f.URL, HTTP: f.Client()} }

func (f *fakeGitHub) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// fail answers 500 when the operation is marked, after its write.
func (f *fakeGitHub) fail(w http.ResponseWriter, op string) bool {
	if f.failNext[op] {
		delete(f.failNext, op)
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}
	return false
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/graphql" {
		f.graphql(w, r)
		return
	}
	f.rest(w, r)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	switch q := req.Query; {
	case q == reactedQuery:
		groups := []map[string]any{{"content": "HEART", "viewerHasReacted": true}}
		node := req.Variables["id"].(string)
		if f.reacted[node] || f.othersEyes[node] {
			groups = append(groups, map[string]any{"content": "EYES", "viewerHasReacted": f.reacted[node]})
		}
		reply(w, map[string]any{"data": map[string]any{"node": map[string]any{"reactionGroups": groups}}})
	case q == addReaction:
		f.reacted[req.Variables["subject"].(string)] = true
		f.writes = append(f.writes, "react "+req.Variables["subject"].(string))
		if !f.fail(w, "react") {
			reply(w, map[string]any{"data": map[string]any{}})
		}
	case q == prStateQuery:
		f.prState(w, req.Variables)
	case q == addLabels:
		f.labels = append(f.labels, "kelos:follow-up")
		f.writes = append(f.writes, "label "+req.Variables["label"].(string))
		reply(w, map[string]any{"data": map[string]any{}})
	case q == convertToDraft:
		f.draft = true
		f.writes = append(f.writes, "draft "+req.Variables["pull"].(string))
		reply(w, map[string]any{"data": map[string]any{}})
	default:
		reply(w, map[string]any{"errors": []map[string]string{{"message": "unknown query"}}})
	}
}

func (f *fakeGitHub) prState(w http.ResponseWriter, vars map[string]any) {
	if vars["number"].(float64) != 4 {
		reply(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": nil}}})
		return
	}
	nodes := []map[string]string{}
	for _, l := range f.labels {
		nodes = append(nodes, map[string]string{"name": l})
	}
	var label any
	if f.repoLabel && vars["label"] != "" {
		label = map[string]string{"id": labelID}
	}
	reply(w, map[string]any{"data": map[string]any{"repository": map[string]any{
		"pullRequest": map[string]any{"id": prNode, "isDraft": f.draft, "labels": map[string]any{"nodes": nodes}},
		"label":       label}}})
}

var (
	labelPath    = regexp.MustCompile(`^/repos/sperano/vigie/issues/4/labels/(.+)$`)
	checksPath   = regexp.MustCompile(`^/repos/sperano/vigie/commits/([0-9a-f]+)/check-runs$`)
	commentsPath = "/repos/sperano/vigie/issues/4/comments"
)

func (f *fakeGitHub) rest(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case r.Method == http.MethodDelete && labelPath.MatchString(p):
		name := labelPath.FindStringSubmatch(p)[1]
		i := slices.Index(f.labels, name)
		if i < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.labels = slices.Delete(f.labels, i, i+1)
		f.writes = append(f.writes, "unlabel "+name)
		reply(w, []any{})
	case r.Method == http.MethodGet && checksPath.MatchString(p):
		sha := checksPath.FindStringSubmatch(p)[1]
		var runs []map[string]any
		for _, c := range f.checks[sha] {
			if c["name"] == r.URL.Query().Get("check_name") {
				runs = append(runs, c)
			}
		}
		reply(w, map[string]any{"total_count": len(runs), "check_runs": runs})
	case r.Method == http.MethodPost && p == "/repos/sperano/vigie/check-runs":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sha := body["head_sha"].(string)
		f.checks[sha] = append(f.checks[sha], body)
		f.writes = append(f.writes, "check "+body["name"].(string)+"@"+sha)
		reply(w, body)
	case p == commentsPath:
		f.issueComments(w, r)
	case r.Method == http.MethodGet && p == "/repos/sperano/vigie":
		if f.fail(w, "repo") {
			return
		}
		reply(w, map[string]any{"private": f.private, "html_url": "https://github.com/sperano/vigie"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeGitHub) issueComments(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		c := comment{ID: f.nextID, Body: body["body"]}
		c.User.Login = appBot
		f.comments = append(f.comments, c)
		f.writes = append(f.writes, "comment")
		if !f.fail(w, "comment") {
			reply(w, c)
		}
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	start := min((page-1)*per, len(f.comments))
	reply(w, f.comments[start:min(start+per, len(f.comments))])
}

func (f *fakeGitHub) addComment(author, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	c := comment{ID: f.nextID, Body: body}
	c.User.Login = author
	f.comments = append(f.comments, c)
}

// fakeRuns serves runs' info.
type fakeRuns map[string]store.RunInfo

func (f fakeRuns) RunInfo(_ context.Context, name string) (store.RunInfo, error) {
	r, ok := f[name]
	if !ok {
		return store.RunInfo{}, fmt.Errorf("%w: %s", store.ErrUnknownRun, name)
	}
	return r, nil
}

// lockCount counts target locks taken.
type lockCount struct {
	mu      sync.Mutex
	targets []string
}

func (l *lockCount) WithTargetLock(ctx context.Context, target string, fn func(context.Context) error) error {
	l.mu.Lock()
	l.targets = append(l.targets, target)
	l.mu.Unlock()
	return fn(ctx)
}

func executor(f *fakeGitHub, runs fakeRuns) (*Executor, *lockCount) {
	l := &lockCount{}
	return &Executor{W: f.writer(), Policy: profile.Defaults().Policy, Runs: runs, Lock: l,
		Links: lifecycle.Links{Lucarne: "https://lucarne.example", Cluster: "hollingsworth", Namespace: "kelos-agents",
			Sillage: "https://sillage.example"}}, l
}
