package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/intake"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/upstream"
)

const replaySlots = 8

// TestReplayThroughTheService replays every fixture through the
// receiver, the store and the worker, with the fixture's fresh state
// behind fake readers and its ledger seeded in the database: the recorded
// decision must be the fixture's, exactly, as the in-memory router test
// requires. Refusals the receiver answers itself (signatures, a stored
// redelivery) are checked at the receiver, with nothing stored.
func TestReplayThroughTheService(t *testing.T) {
	fixtures, err := replay.Load("../replay/testdata")
	if err != nil {
		t.Fatal(err)
	}
	storetest.AdminURL(t)
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) { replayFixture(t, f) })
	}
}

func replayFixture(t *testing.T, f replay.Fixture) {
	r := f.Router()
	e := newEnv(t)
	e.proc.Router = r
	e.proc.State.Config = r.Config
	if f.State.Task != nil {
		e.tasks.tasks[f.State.Task.ID] = *f.State.Task
		e.tasks.comments[f.State.Task.ID] = f.State.Comments
	}
	for _, sub := range f.State.Subtasks {
		e.tasks.tasks[sub.ID] = sub
	}
	if pr := f.State.PullRequest; pr != nil {
		e.prs.prs[fmt.Sprintf("%s#%d", pr.Repository, pr.Number)] = *pr
		if h := f.State.History; h != nil {
			e.prs.history[fmt.Sprintf("%s#%d", pr.Repository, pr.Number)] = *h
		}
	}
	serveReadiness(e, f.State.Readiness)
	seedLedger(t, e, f.Ledger)
	if !deliver(t, e, f, r.Keys) {
		return
	}
	e.tick(t)
	exp := f.Expect
	if exp.Outcome == route.Retry {
		if n := e.count(t, `SELECT count(*) FROM inbox WHERE state = 'pending' AND last_error LIKE 'state-unavailable%'`); n != 1 {
			t.Fatal("retry fixture not queued for a retry")
		}
		return
	}
	var raw []byte
	storetest.QueryRow(t, e.dbURL, `SELECT decision FROM decisions d JOIN inbox i ON i.id = d.inbox_id
		WHERE i.delivery_key NOT LIKE 'seed/%'`, nil, &raw)
	var got route.Decision
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(exp)
	have, _ := json.Marshal(got)
	if !bytes.Equal(want, have) {
		t.Fatalf("recorded decision\n%s\nwant\n%s", have, want)
	}
}

// serveReadiness puts the fixture's readiness state behind the readers.
func serveReadiness(e *env, rds []route.Readiness) {
	if len(rds) == 0 {
		return
	}
	fr := &fixtureReadiness{byHead: map[string]route.Readiness{}}
	for _, rd := range rds {
		pr := rd.PullRequest
		key := fmt.Sprintf("%s#%d", pr.Repository, pr.Number)
		e.prs.prs[key] = pr
		e.prs.history[key] = route.PRHistory{Reviews: rd.Reviews, Comments: rd.Comments}
		e.tasks.tasks[rd.Task.ID] = rd.Task
		e.tasks.comments[rd.Task.ID] = rd.TaskComments
		fr.byHead[pr.Repository+"@"+pr.HeadSHA] = rd
		fr.open = append(fr.open, upstream.ListedPR{PullRequest: pr})
		fr.now = rd.ReadAt
	}
	e.proc.State.Readiness = Readiness{GitHub: fr, Kelos: fr, Reactions: fr, Open: fr, Now: func() time.Time { return fr.now }}
}

// fixtureReadiness serves a fixture's readiness state.
type fixtureReadiness struct {
	byHead map[string]route.Readiness
	open   []upstream.ListedPR
	now    time.Time
}

func (f *fixtureReadiness) CheckRuns(_ context.Context, repo, sha string) ([]event.CheckRun, error) {
	return f.byHead[repo+"@"+sha].CheckRuns, nil
}

func (f *fixtureReadiness) Committed(_ context.Context, repo, sha string) (time.Time, error) {
	return f.byHead[repo+"@"+sha].HeadCommitted, nil
}

func (f *fixtureReadiness) ReviewThreads(_ context.Context, repo string, n int) ([]event.ReviewThread, error) {
	for _, rd := range f.byHead {
		if rd.PullRequest.Repository == repo && rd.PullRequest.Number == n {
			return rd.Threads, nil
		}
	}
	return nil, nil
}

// Tasks serves every fixture Task for the Workspace selector, none for
// the task's: the union is the fixture's list.
func (f *fixtureReadiness) Tasks(_ context.Context, selector string) ([]route.KelosTask, error) {
	if !strings.HasPrefix(selector, route.WorkspaceLabel+" in ") {
		return nil, nil
	}
	var out []route.KelosTask
	for _, rd := range f.byHead {
		out = append(out, rd.Tasks...)
	}
	return out, nil
}

func (f *fixtureReadiness) TaskReactions(_ context.Context, id int64) ([]string, error) {
	for _, rd := range f.byHead {
		if rd.Task.ID == id {
			return rd.Reactions, nil
		}
	}
	return nil, nil
}

func (f *fixtureReadiness) OpenPullRequests(context.Context, string) ([]upstream.ListedPR, error) {
	return f.open, nil
}

// deliver stores the fixture's delivery: through the receiver, or, for a
// synthetic one (a reconciliation's), as the worker queues its own: no
// receiver ever stores one, nor keeps its header. False when the
// receiver's answer ends the fixture.
func deliver(t *testing.T, e *env, f replay.Fixture, keys auth.Keys) bool {
	t.Helper()
	d, err := f.Build()
	if err != nil {
		t.Fatal(err)
	}
	if d.Header(event.SyntheticHeader) != "" {
		enqueueSynthetic(t, e, d)
		return true
	}
	return !answeredByReceiver(t, e, f.Expect, post(t, e, keys, d))
}

// enqueueSynthetic stores a synthetic GitHub delivery as the worker's
// reconciliations do (store.GitHubSynthetic's keys), signature dropped.
func enqueueSynthetic(t *testing.T, e *env, d event.Delivery) {
	t.Helper()
	sig, _ := auth.Header(d.Source)
	headers := map[string]string{}
	for k, v := range d.Headers {
		if k != sig {
			headers[k] = v
		}
	}
	ev, err := event.ParseGitHub(event.Delivery{Source: d.Source, Headers: headers, Body: d.Body})
	if err != nil {
		t.Fatal(err)
	}
	sd := store.Delivery{Source: d.Source, DeliveryKey: ev.DeliveryKey, RequestKey: ev.RequestKey, Kind: ev.Kind,
		Scope: ev.Scope(), Headers: headers, Body: d.Body}
	if n, err := e.store.Enqueue(context.Background(), []store.Delivery{sd}); err != nil || n != 1 {
		t.Fatalf("Enqueue = %d, %v", n, err)
	}
}

// answeredByReceiver checks the receiver's answer; true when the fixture
// ends there (a refusal, a stored redelivery, an unrouted event).
func answeredByReceiver(t *testing.T, e *env, exp route.Decision, status int) bool {
	t.Helper()
	switch {
	case exp.Outcome == route.Reject && exp.Status != 0:
		if status != exp.Status {
			t.Fatalf("receiver answered %d, want %d", status, exp.Status)
		}
		e.assertStored(t, 0)
		return true
	case exp.Outcome == route.Duplicate && exp.Reason == route.ReasonRedelivery:
		if status != http.StatusOK {
			t.Fatalf("receiver answered %d to a stored redelivery, want 200", status)
		}
		return true
	case exp.Outcome == route.Ignore && exp.Reason == route.ReasonUnsupported:
		if status != http.StatusAccepted {
			t.Fatalf("receiver answered %d, want 202", status)
		}
		e.assertStored(t, 0)
		return true
	}
	if status != http.StatusAccepted {
		t.Fatalf("receiver answered %d, want 202", status)
	}
	return false
}

// post sends the delivery through a receiver on the env's store.
func post(t *testing.T, e *env, keys auth.Keys, d event.Delivery) int {
	t.Helper()
	rc := &intake.Receiver{Inbox: e.store, Keys: func() auth.Keys { return keys }, Slots: intake.NewSlots(replaySlots),
		Metrics: metrics.New(), Log: logging.New(io.Discard, slog.LevelInfo), Deadline: time.Minute}
	mux := http.NewServeMux()
	mux.Handle(intake.Path, rc)
	req := httptest.NewRequest(http.MethodPost, "/webhook/"+string(d.Source), bytes.NewReader(d.Body))
	for k, v := range d.Headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func (e *env) assertStored(t *testing.T, want int) {
	t.Helper()
	if n := e.count(t, `SELECT count(*) FROM inbox WHERE delivery_key NOT LIKE 'seed/%'`); n != want {
		t.Fatalf("%d deliveries stored, want %d", n, want)
	}
}

// seedLedger writes the fixture's ledger: decided deliveries, handled
// requests and runs (attached to one seed delivery).
func seedLedger(t *testing.T, e *env, l replay.Ledger) {
	t.Helper()
	storetest.Exec(t, e.dbURL, `INSERT INTO inbox (source, delivery_key, state) VALUES ('vikunja', 'seed/ledger', 'done')`)
	for _, k := range l.Deliveries {
		storetest.Exec(t, e.dbURL, `INSERT INTO inbox (source, delivery_key, state) VALUES ('vikunja', $1, 'done')`, k)
	}
	seed := `(SELECT id FROM inbox WHERE delivery_key = 'seed/ledger')`
	for _, k := range l.Requests {
		storetest.Exec(t, e.dbURL, `INSERT INTO requests (request_key, inbox_id, outcome, executed)
			VALUES ($1, `+seed+`, 'dispatch', false)`, k)
	}
	for _, r := range l.Runs {
		state, family := "finished", string(r.Family)
		if r.Active {
			state = "active"
		}
		if family == "" {
			family = "seed"
		}
		identity, _ := json.Marshal(map[string]any{"family": family, "task_id": r.TaskID}) //nolint:errcheck // plain map
		storetest.Exec(t, e.dbURL, `INSERT INTO runs (name, family, inbox_id, identity, profile, state, task_exists)
			VALUES ($1, $2, `+seed+`, $3, '{}', $4, $5)`, r.Name, family, identity, state, r.Exists)
	}
}
