package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/intake"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store/storetest"
)

const replaySlots = 8

// TestReplayThroughTheService replays every legacy fixture through the
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
	e := newEnv(t, r.Ownership)
	e.proc.Router = r
	e.proc.State.Config = r.Config
	if f.State.Task != nil {
		e.tasks.tasks[f.State.Task.ID] = *f.State.Task
	}
	if pr := f.State.PullRequest; pr != nil {
		e.prs.prs[fmt.Sprintf("%s#%d", pr.Repository, pr.Number)] = *pr
	}
	seedLedger(t, e, f.Ledger)
	d, err := f.Build()
	if err != nil {
		t.Fatal(err)
	}
	if answeredByReceiver(t, e, f.Expect, post(t, e, r.Keys, d)) {
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
		state := "finished"
		if r.Active {
			state = "active"
		}
		storetest.Exec(t, e.dbURL, `INSERT INTO runs (name, family, inbox_id, identity, profile, state, task_exists)
			VALUES ($1, 'seed', `+seed+`, '{}', '{}', $2, $3)`, r.Name, state, r.Exists)
	}
}
