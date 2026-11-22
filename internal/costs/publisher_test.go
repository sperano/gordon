package costs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/sillage"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

const runName = "vikunja-gordon-claude-opus-5"

var now = time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)

// row is a fake cost_notices row.
type row struct {
	store.CostNotice
	body      string
	next      time.Time
	lastError string
}

// fakeStore keeps cost notice rows with the store's fencing: a write
// whose state and revision no longer match changes nothing.
type fakeStore struct {
	rows    []*row
	actions []action.Action
	// race makes the next fenced write find the row changed.
	race bool
}

func (f *fakeStore) add(n store.CostNotice) *row {
	r := &row{CostNotice: n, next: n.Finished}
	f.rows = append(f.rows, r)
	return r
}

func (f *fakeStore) CostDue(_ context.Context, at time.Time, hold time.Duration, limit int) ([]store.CostNotice, error) {
	var due []*row
	for _, r := range f.rows {
		if (r.State == store.CostWaiting || r.State == store.CostLive) && !r.next.After(at) {
			due = append(due, r)
		}
	}
	slices.SortFunc(due, func(a, b *row) int { return cmp.Or(a.next.Compare(b.next), cmp.Compare(a.Name, b.Name)) })
	var out []store.CostNotice
	for _, r := range due[:min(limit, len(due))] {
		r.next = at.Add(hold)
		out = append(out, r.CostNotice)
	}
	return out, nil
}

func (f *fakeStore) fenced(n store.CostNotice) *row {
	if f.race {
		f.race = false
		return nil
	}
	for _, r := range f.rows {
		if r.Name == n.Name && r.InboxID == n.InboxID && r.State == n.State && r.Rev == n.Rev {
			return r
		}
	}
	return nil
}

func (f *fakeStore) CostCheck(_ context.Context, n store.CostNotice, next time.Time, failed bool, reason string) (bool, error) {
	r := f.fenced(n)
	if r == nil {
		return false, nil
	}
	r.next, r.lastError = next, reason
	if failed {
		r.Failures++
	} else {
		r.Failures = 0
	}
	return true, nil
}

func (f *fakeStore) CostPublish(_ context.Context, n store.CostNotice, body, digest string, _, liveUntil, next time.Time) (bool, error) {
	r := f.fenced(n)
	if r == nil {
		return false, nil
	}
	r.Rev++
	r.body, r.Digest, r.State, r.LiveUntil, r.next, r.Failures, r.lastError = body, digest, store.CostLive, liveUntil, next, 0, ""
	if n.State == store.CostWaiting {
		for _, o := range f.rows {
			if o == r || o.TaskID != n.TaskID {
				continue
			}
			if o.State == store.CostLive && o.Finished.Before(r.Finished) {
				o.State = store.CostFrozen
			}
			if o.State != store.CostWaiting && o.Finished.After(r.Finished) {
				r.State = store.CostFrozen
			}
		}
	}
	f.actions = append(f.actions, action.CostNotice(n.TaskID, n.Name, n.InboxID, r.Rev))
	return true, nil
}

func (f *fakeStore) CostSettle(_ context.Context, n store.CostNotice) (bool, error) {
	r := f.fenced(n)
	if r == nil {
		return false, nil
	}
	r.State = store.CostSettled
	return true, nil
}

// fakeSillage answers each task's cost, counting reads.
type fakeSillage struct {
	costs map[int64]sillage.TaskCost
	err   error
	reads map[int64]int
	hosts []string
}

func (s *fakeSillage) TaskCost(_ context.Context, host string, id int64) (sillage.TaskCost, error) {
	s.reads[id]++
	s.hosts = append(s.hosts, host)
	if s.err != nil {
		return sillage.TaskCost{}, s.err
	}
	c, ok := s.costs[id]
	if !ok {
		return sillage.TaskCost{}, sillage.ErrNotFound
	}
	return c, nil
}

type links struct{}

func (links) Run(uid string) string      { return "https://lucarne.example/runs/c/kelos-agents/" + uid }
func (links) Activity(task int64) string { return "https://sillage.example/task/vikunja.example/5" }

func fixture(t *testing.T) sillage.TaskCost {
	t.Helper()
	data, err := os.ReadFile("testdata/cost.json")
	if err != nil {
		t.Fatal(err)
	}
	var c sillage.TaskCost
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func publisher(s *fakeStore, sl *fakeSillage) (*Publisher, *bytes.Buffer) {
	var logs bytes.Buffer
	return &Publisher{Store: s, Sillage: sl, Ownership: owns(run.VikunjaCost), Links: links{}, VikunjaWeb: "https://vikunja.example",
		Now: func() time.Time { return now }, Metrics: metrics.New(), Log: logging.New(&logs, slog.LevelDebug)}, &logs
}

func waitingRow(finished time.Time) store.CostNotice {
	return store.CostNotice{CostRun: store.CostRun{Name: runName, InboxID: 9, Family: "vikunja-implement",
		Project: "gordon", TaskID: 5, TaskUID: "uid-1", Phase: kelos.PhaseSucceeded, Finished: finished},
		State: store.CostWaiting}
}

func checks(p *Publisher, result string) float64 {
	return testutil.ToFloat64(p.Metrics.CostChecks.WithLabelValues(result))
}

func once(t *testing.T, p *Publisher) {
	t.Helper()
	if err := p.Once(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestTerminalBeforeUsage: a run Gordon saw finish before Sillage read
// its usage waits (Sillage has no story, then no such run, then the run
// not final) and is posted once its estimate is final, with the run's
// own figure: never summed from its attempts, models or subagent threads.
func TestTerminalBeforeUsage(t *testing.T) {
	s := &fakeStore{}
	r := s.add(waitingRow(now.Add(-10 * time.Minute)))
	sl := &fakeSillage{costs: map[int64]sillage.TaskCost{}, reads: map[int64]int{}}
	p, _ := publisher(s, sl)

	final := fixture(t)
	noRun := final
	noRun.Runs = final.Runs[1:]
	notFinal := fixture(t)
	notFinal.Runs[0].Final = false
	for i, c := range []*sillage.TaskCost{nil, &noRun, &notFinal} {
		if c != nil {
			sl.costs[5] = *c
		}
		r.next = now
		once(t, p)
		if r.State != store.CostWaiting || !r.next.Equal(now.Add(DefaultWait)) || len(s.actions) != 0 || r.Failures != 0 {
			t.Fatalf("check %d: row %+v next %s, actions %v; want waiting", i, r.CostNotice, r.next, s.actions)
		}
	}
	if got := checks(p, ResultWaiting); got != 3 {
		t.Errorf("waiting checks %v", got)
	}
	sl.costs[5] = final
	r.next = now
	once(t, p)
	want := action.CostNotice(5, runName, 9, 1)
	if r.State != store.CostLive || r.Rev != 1 || len(s.actions) != 1 || s.actions[0] != want {
		t.Fatalf("row %+v, actions %v; want live at revision 1 with %v", r.CostNotice, s.actions, want)
	}
	if !r.next.Equal(now.Add(DefaultRefresh)) || !r.LiveUntil.Equal(now.Add(DefaultLiveFor)) || r.Digest != Digest(r.body) {
		t.Errorf("next %s, live until %s, digest %s", r.next, r.LiveUntil, r.Digest)
	}
	for _, part := range []string{"THIS RUN</td><td>$1.50</td>", "TASK TO DATE</td><td>AT LEAST $2.25 OVER 2 RUNS</td>",
		"UNCACHED INPUT $0.50 · OUTPUT $1.00", "<code>2026-11-22.1</code>, VERIFIED 2026-11-22",
		"<code>api-equivalent</code>, <code>standard-tier-by-default</code>",
		`<a href="https://lucarne.example/runs/c/kelos-agents/uid-1">lucarne</a>`,
		`<a href="https://sillage.example/task/vikunja.example/5#cost">sillage</a>`,
		"<!-- kelos:cost task=" + runName + " -->"} {
		if !strings.Contains(r.body, part) {
			t.Errorf("notice lacks %q:\n%s", part, r.body)
		}
	}
	// The run's attempts, models and threads (two subagents) each add up
	// to its $1.50: shown once, never their sum.
	if strings.Contains(r.body, "$3.00") || strings.Contains(r.body, "$4.50") {
		t.Errorf("run counted more than once: %s", r.body)
	}
	if strings.Contains(r.body, "NOT FINAL") || strings.Contains(r.body, "FAILED RUN") || strings.Contains(r.body, "CACHE READS") {
		t.Errorf("final succeeded notice: %s", r.body)
	}
	if !slices.Equal(sl.hosts, []string{"vikunja.example", "vikunja.example", "vikunja.example", "vikunja.example"}) {
		t.Errorf("hosts %v", sl.hosts)
	}
	if got := checks(p, ResultPublished); got != 1 {
		t.Errorf("published checks %v", got)
	}
}

// TestDeadlinePostsNotFinal: past the deadline a notice is posted with
// what Sillage has, marked not final; a run Sillage never saw is UNKNOWN,
// never zero.
func TestDeadlinePostsNotFinal(t *testing.T) {
	s := &fakeStore{}
	failed := waitingRow(now.Add(-DefaultDeadline))
	failed.Phase = kelos.PhaseFailed
	r := s.add(failed)
	sl := &fakeSillage{costs: map[int64]sillage.TaskCost{}, reads: map[int64]int{}}
	p, _ := publisher(s, sl)
	once(t, p)
	if r.State != store.CostLive || len(s.actions) != 1 {
		t.Fatalf("row %+v, actions %v; want posted at the deadline", r.CostNotice, s.actions)
	}
	for _, part := range []string{"THE TAB FOR FAILED RUN", "THIS RUN</td><td>UNKNOWN</td>", "TASK TO DATE</td><td>UNKNOWN</td>",
		"PRICES</td><td>UNKNOWN</td>", "SILLAGE HAS NO RECORD OF THIS RUN'S USAGE YET.", "NOT FINAL"} {
		if !strings.Contains(r.body, part) {
			t.Errorf("notice lacks %q:\n%s", part, r.body)
		}
	}
	if strings.Contains(r.body, "$0") {
		t.Errorf("unknown shown as zero: %s", r.body)
	}
	// Sillage then reads the run, final: the live notice is edited to it.
	sl.costs[5] = fixture(t)
	r.next = now
	once(t, p)
	if r.Rev != 2 || len(s.actions) != 2 || s.actions[1] != action.CostNotice(5, runName, 9, 2) ||
		strings.Contains(r.body, "NOT FINAL") || !strings.Contains(r.body, "THIS RUN</td><td>$1.50</td>") {
		t.Fatalf("row %+v, actions %v, body %s; want revision 2, final", r.CostNotice, s.actions, r.body)
	}
	if got := checks(p, ResultUpdated); got != 1 {
		t.Errorf("updated checks %v", got)
	}
}

// TestSillageOutage: Sillage failing to answer is a retry with backoff,
// past the deadline too; nothing is posted, so no zero estimate.
func TestSillageOutage(t *testing.T) {
	s := &fakeStore{}
	r := s.add(waitingRow(now.Add(-2 * DefaultDeadline)))
	sl := &fakeSillage{err: &sillage.StatusError{Status: 503}, reads: map[int64]int{}}
	p, logs := publisher(s, sl)
	p.Backoff = worker.Backoff{Base: time.Minute, Max: 3 * time.Minute}
	for i, wait := range []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute} {
		r.next = now
		once(t, p)
		if r.Failures != i+1 || !r.next.Equal(now.Add(wait)) || r.lastError != "sillage cost: HTTP 503" {
			t.Fatalf("failure %d: failures %d, next %s, error %q", i+1, r.Failures, r.next, r.lastError)
		}
	}
	if r.State != store.CostWaiting || len(s.actions) != 0 {
		t.Fatalf("row %+v, actions %v; want still waiting", r.CostNotice, s.actions)
	}
	if got := checks(p, ResultUnavailable); got != 3 {
		t.Errorf("unavailable checks %v", got)
	}
	if !strings.Contains(logs.String(), `"msg":"sillage cost read failed"`) {
		t.Errorf("logs %s", logs.String())
	}
	sl.err = errors.Join(sillage.ErrIncompatible, errors.New("no price table version"))
	r.next = now
	once(t, p)
	if got := checks(p, ResultIncompatible); got != 1 || len(s.actions) != 0 {
		t.Errorf("incompatible checks %v, actions %v", got, s.actions)
	}
	// Back up and final: posted, and the failure count reset.
	sl.err, sl.costs = nil, map[int64]sillage.TaskCost{5: fixture(t)}
	r.next = now
	once(t, p)
	if r.State != store.CostLive || r.Failures != 0 || r.lastError != "" {
		t.Fatalf("row %+v after recovery", r.CostNotice)
	}
}

// TestOutagePastTheLiveWindow: a notice Sillage still fails to answer
// once its live window passed settles as it is: a waiting one unposted,
// a live one with its last revision; one inside the window retries.
func TestOutagePastTheLiveWindow(t *testing.T) {
	s := &fakeStore{}
	expired := s.add(waitingRow(now.Add(-DefaultLiveFor)))
	inside := waitingRow(now.Add(-DefaultLiveFor + time.Second))
	inside.Name, inside.TaskID = "vikunja-gordon-claude-opus-6", 6
	retried := s.add(inside)
	liveRow := waitingRow(now.Add(-time.Hour))
	liveRow.Name, liveRow.TaskID, liveRow.State, liveRow.Rev, liveRow.LiveUntil = "vikunja-gordon-claude-opus-7", 7,
		store.CostLive, 1, now
	live := s.add(liveRow)
	for _, r := range s.rows {
		r.next = now
	}
	p, logs := publisher(s, &fakeSillage{err: &sillage.StatusError{Status: 503}, reads: map[int64]int{}})
	once(t, p)
	if expired.State != store.CostSettled || live.State != store.CostSettled || live.Rev != 1 || len(s.actions) != 0 {
		t.Fatalf("expired %+v, live %+v, actions %v; want both settled, nothing posted",
			expired.CostNotice, live.CostNotice, s.actions)
	}
	if retried.State != store.CostWaiting || retried.Failures != 1 || !retried.next.Equal(now.Add(DefaultRetryBase)) {
		t.Errorf("inside the window: %+v, next %s; want a retry", retried.CostNotice, retried.next)
	}
	if got := checks(p, ResultExpired); got != 2 {
		t.Errorf("expired checks %v, want 2", got)
	}
	if got := checks(p, ResultUnavailable); got != 1 {
		t.Errorf("unavailable checks %v, want 1", got)
	}
	if !strings.Contains(logs.String(), `"msg":"sillage cost read failed past the live window"`) {
		t.Errorf("logs %s", logs.String())
	}
}

// TestDefaultBackoff: without a configured backoff, the pass's own.
func TestDefaultBackoff(t *testing.T) {
	s := &fakeStore{}
	r := s.add(waitingRow(now))
	p, _ := publisher(s, &fakeSillage{err: errors.New("dial"), reads: map[int64]int{}})
	once(t, p)
	if !r.next.Equal(now.Add(DefaultRetryBase)) {
		t.Errorf("next %s, want %s", r.next, now.Add(DefaultRetryBase))
	}
}

// TestLateCorrections: a live notice whose estimate is unchanged is
// left alone, one whose task total changed (a late review, late usage)
// is edited to a new revision, and one unchanged past its live window
// settles.
func TestLateCorrections(t *testing.T) {
	s := &fakeStore{}
	r := s.add(waitingRow(now.Add(-time.Hour)))
	c := fixture(t)
	sl := &fakeSillage{costs: map[int64]sillage.TaskCost{5: c}, reads: map[int64]int{}}
	p, _ := publisher(s, sl)
	once(t, p)
	r.next = now
	once(t, p)
	if r.Rev != 1 || len(s.actions) != 1 || !r.next.Equal(now.Add(DefaultRefresh)) {
		t.Fatalf("unchanged: row %+v, actions %v", r.CostNotice, s.actions)
	}
	if got := checks(p, ResultUnchanged); got != 1 {
		t.Errorf("unchanged checks %v", got)
	}
	c.Total.EstimatedUSD, c.Total.Status, c.Total.Notes = "3.100000", sillage.StatusComplete, nil
	sl.costs[5] = c
	r.next = now
	once(t, p)
	if r.Rev != 2 || len(s.actions) != 2 || !strings.Contains(r.body, "TASK TO DATE</td><td>$3.10 OVER 2 RUNS</td>") {
		t.Fatalf("corrected: row %+v, body %s", r.CostNotice, r.body)
	}
	r.next, r.LiveUntil = now, now
	once(t, p)
	if r.State != store.CostSettled || len(s.actions) != 2 {
		t.Fatalf("settle: row %+v, actions %v", r.CostNotice, s.actions)
	}
	if got := checks(p, ResultSettled); got != 1 {
		t.Errorf("settled checks %v", got)
	}
}

// TestNewNoticeFreezesTheOlder: the task's next run posts its own notice
// and the older one stops updating; both read the task's cost once.
func TestNewNoticeFreezesTheOlder(t *testing.T) {
	s := &fakeStore{}
	older := s.add(waitingRow(now.Add(-time.Hour)))
	sl := &fakeSillage{costs: map[int64]sillage.TaskCost{5: fixture(t)}, reads: map[int64]int{}}
	p, _ := publisher(s, sl)
	once(t, p)
	newer := waitingRow(now.Add(-30 * time.Minute))
	newer.Name, newer.InboxID, newer.TaskUID = "vikunja-fu-gordon-5-c77", 12, "uid-2"
	nr := s.add(newer)
	older.next = now
	sl.reads = map[int64]int{}
	once(t, p)
	if sl.reads[5] != 1 {
		t.Errorf("reads %v, want one per task", sl.reads)
	}
	if nr.State != store.CostLive || older.State != store.CostFrozen {
		t.Fatalf("newer %s, older %s; want live and frozen", nr.State, older.State)
	}
	if !strings.Contains(nr.body, "THIS RUN</td><td>AT LEAST $0.75</td>") ||
		!strings.Contains(nr.body, "(<code>price-unknown</code>)") {
		t.Errorf("newer notice: %s", nr.body)
	}
	if len(s.actions) != 2 || s.actions[1] != action.CostNotice(5, nr.Name, 12, 1) {
		t.Errorf("actions %v", s.actions)
	}
}

// TestRaceIsCounted: a row another pass changed first writes nothing.
func TestRaceIsCounted(t *testing.T) {
	s := &fakeStore{race: true}
	r := s.add(waitingRow(now.Add(-time.Hour)))
	p, _ := publisher(s, &fakeSillage{costs: map[int64]sillage.TaskCost{5: fixture(t)}, reads: map[int64]int{}})
	once(t, p)
	if r.State != store.CostWaiting || len(s.actions) != 0 || checks(p, ResultRaced) != 1 {
		t.Errorf("row %+v, actions %v, raced %v", r.CostNotice, s.actions, checks(p, ResultRaced))
	}
}

func TestNoVikunjaHost(t *testing.T) {
	s := &fakeStore{}
	s.add(waitingRow(now))
	p, _ := publisher(s, &fakeSillage{reads: map[int64]int{}})
	p.VikunjaWeb = ""
	if err := p.Once(ctx); err == nil || err.Error() != `cost notices: no Vikunja host in ""` {
		t.Errorf("Once = %v", err)
	}
}

// TestRollbackStopsTheNotices: once the project's rule is gone, a due
// notice reads nothing and writes nothing; it is checked again later.
func TestRollbackStopsTheNotices(t *testing.T) {
	s := &fakeStore{}
	r := s.add(waitingRow(now.Add(-time.Hour)))
	sl := &fakeSillage{costs: map[int64]sillage.TaskCost{5: fixture(t)}, reads: map[int64]int{}}
	p, _ := publisher(s, sl)
	p.Ownership = owns(run.VikunjaFailure)
	once(t, p)
	if r.State != store.CostWaiting || len(s.actions) != 0 || len(sl.reads) != 0 || !r.next.Equal(now.Add(DefaultRefresh)) {
		t.Errorf("row %+v next %s, actions %v, reads %v", r.CostNotice, r.next, s.actions, sl.reads)
	}
	if got := checks(p, ResultNotOwned); got != 1 {
		t.Errorf("not-owned checks %v", got)
	}
}
