package worker

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/upstream"
)

var ctx = context.Background()

const (
	testTTL         = time.Minute
	testMaxAttempts = 3
	testConcurrency = 4
)

var testBackoff = Backoff{Base: time.Second, Max: time.Minute}

// fakeTasks serves tasks by id; a missing id answers like Vikunja's 404,
// and down answers like an outage.
type fakeTasks struct {
	mu    sync.Mutex
	tasks map[int64]event.Task
	down  bool
	reads int
}

func (f *fakeTasks) Task(_ context.Context, id int64) (event.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.down {
		return event.Task{}, &upstream.Error{Provider: upstream.ProviderVikunja, Class: upstream.Unavailable, Status: 503}
	}
	t, ok := f.tasks[id]
	if !ok {
		return event.Task{}, &upstream.Error{Provider: upstream.ProviderVikunja, Class: upstream.NotFound, Status: 404}
	}
	return t, nil
}

// fakePRs serves pull requests by repo#number.
type fakePRs struct {
	mu    sync.Mutex
	prs   map[string]event.PullRequest
	reads int
}

func (f *fakePRs) PullRequest(_ context.Context, repo string, n int, _ bool) (event.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	pr, ok := f.prs[fmt.Sprintf("%s#%d", repo, n)]
	if !ok {
		return event.PullRequest{}, &upstream.Error{Provider: upstream.ProviderGitHub, Class: upstream.NotFound, Status: 404}
	}
	return pr, nil
}

type env struct {
	store  *store.Store
	dbURL  string
	tasks  *fakeTasks
	prs    *fakePRs
	proc   *Processor
	logs   *bytes.Buffer
	metric *metrics.Metrics
}

// gordonOwns is an ownership table under which Gordon executes family.
func gordonOwns(f run.Family) rollout.Table {
	return rollout.Table{Rules: []rollout.Rule{{Project: rollout.AllProjects, Family: f, Owner: rollout.Gordon,
		Evidence: []rollout.Gate{rollout.ReplayParity, rollout.ShadowParity, rollout.RollbackTested, rollout.LegacyDisabled}}}}
}

func newEnv(t *testing.T, ownership rollout.Table) *env {
	t.Helper()
	s, dbURL := storetest.Open(t)
	logs := &bytes.Buffer{}
	m := metrics.New()
	tasks, prs := &fakeTasks{tasks: map[int64]event.Task{}}, &fakePRs{prs: map[string]event.PullRequest{}}
	cfg := profile.Defaults()
	proc := &Processor{
		Store: s, Router: route.Router{Config: cfg, Ownership: ownership},
		State: StateReader{Config: cfg, Tasks: tasks, PRs: prs},
		Owner: "test-worker", Concurrency: testConcurrency, LeaseTTL: testTTL, MaxAttempts: testMaxAttempts,
		Backoff: testBackoff, Projects: &Limiter{}, DeferDelay: time.Second,
		Metrics: m, Log: logging.New(logs, slog.LevelDebug),
	}
	return &env{store: s, dbURL: dbURL, tasks: tasks, prs: prs, proc: proc, logs: logs, metric: m}
}

// storedDelivery is a stored delivery of body as the receiver would keep it.
func storedDelivery(t *testing.T, source event.Source, headers map[string]string, body string) store.Delivery {
	t.Helper()
	d := event.Delivery{Source: source, Headers: headers, Body: []byte(body)}
	ev, err := event.Parse(d)
	if err != nil {
		t.Fatalf("test delivery does not parse: %v", err)
	}
	return store.Delivery{Source: source, DeliveryKey: ev.DeliveryKey, RequestKey: ev.RequestKey,
		Kind: ev.Kind, Scope: fmt.Sprintf("%s/scope", source), Headers: headers, Body: []byte(body)}
}

func (e *env) accept(t *testing.T, d store.Delivery) int64 {
	t.Helper()
	id, dup, err := e.store.Accept(ctx, d)
	if err != nil || dup {
		t.Fatalf("Accept = %v, %v", dup, err)
	}
	return id
}

func (e *env) tick(t *testing.T) int {
	t.Helper()
	n, err := e.proc.Tick(ctx, ctx)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// due makes every queued row due now, standing in for the backoff passing.
func (e *env) due(t *testing.T) {
	t.Helper()
	storetest.Exec(t, e.dbURL, `UPDATE inbox SET next_attempt_at = now()`)
	storetest.Exec(t, e.dbURL, `UPDATE actions SET next_attempt_at = now()`)
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	storetest.QueryRow(t, e.dbURL, sql, args, &n)
	return n
}

func (e *env) inboxRow(t *testing.T, id int64) (state string, attempts int, lastErr string) {
	t.Helper()
	storetest.QueryRow(t, e.dbURL, `SELECT state, attempts, last_error FROM inbox WHERE id = $1`, []any{id},
		&state, &attempts, &lastErr)
	return state, attempts, lastErr
}
