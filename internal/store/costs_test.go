package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

var costNow = time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)

func costRun(id int64, name string) store.CostRun {
	return store.CostRun{Name: name, InboxID: id, Family: run.VikunjaFollowUp, Project: "gordon", TaskID: 5,
		TaskUID: "uid-1", Phase: kelos.PhaseFailed, Finished: costNow.Add(-time.Hour)}
}

func costDue(t *testing.T, s *store.Store, at time.Time) []store.CostNotice {
	t.Helper()
	due, err := s.CostDue(ctx, at, 5*time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	return due
}

// TestRecordCostRunOnce: a duplicate completion event of a generation
// records nothing more; the first record stands.
func TestRecordCostRunOnce(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := failedRun(t, s)
	c := costRun(id, runName)
	if err := s.RecordCostRun(ctx, c); err != nil {
		t.Fatal(err)
	}
	dup := c
	dup.Phase, dup.TaskUID = kelos.PhaseSucceeded, "uid-2"
	if err := s.RecordCostRun(ctx, dup); err != nil {
		t.Fatal(err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM cost_notices WHERE name = $1 AND inbox_id = $2 AND phase = 'Failed'
		AND task_uid = 'uid-1' AND state = 'waiting' AND rev = 0`, runName, id); n != 1 {
		t.Errorf("cost notices %d, want the first record", n)
	}
	due := costDue(t, s, costNow)
	want := store.CostNotice{CostRun: c, State: store.CostWaiting, LiveUntil: c.Finished}
	if len(due) != 1 || !due[0].Finished.Equal(c.Finished) || !due[0].LiveUntil.Equal(c.Finished) {
		t.Fatalf("due %+v, want %+v", due, want)
	}
	due[0].Finished, due[0].LiveUntil = c.Finished, c.Finished
	if due[0] != want {
		t.Errorf("due %+v, want %+v", due[0], want)
	}
	// Held: not due again until the hold passed.
	if got := costDue(t, s, costNow.Add(4*time.Minute)); len(got) != 0 {
		t.Errorf("held row due again: %+v", got)
	}
	if got := costDue(t, s, costNow.Add(5*time.Minute)); len(got) != 1 {
		t.Errorf("row after its hold: %+v", got)
	}
}

func TestCostCheckCountsFailures(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := failedRun(t, s)
	if err := s.RecordCostRun(ctx, costRun(id, runName)); err != nil {
		t.Fatal(err)
	}
	n := costDue(t, s, costNow)[0]
	next := costNow.Add(time.Minute)
	if ok, err := s.CostCheck(ctx, n, next, true, "sillage cost: HTTP 503"); err != nil || !ok {
		t.Fatalf("CostCheck = %v, %v", ok, err)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM cost_notices WHERE failures = 1 AND last_error = 'sillage cost: HTTP 503'
		AND next_check = $1`, next); got != 1 {
		t.Error("failure not recorded")
	}
	n = costDue(t, s, next)[0]
	if n.Failures != 1 {
		t.Errorf("failures %d", n.Failures)
	}
	if ok, err := s.CostCheck(ctx, n, next.Add(time.Minute), false, ""); err != nil || !ok {
		t.Fatalf("CostCheck = %v, %v", ok, err)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM cost_notices WHERE failures = 0 AND last_error = ''`); got != 1 {
		t.Error("a check that found nothing did not reset the failures")
	}
	stale := n
	stale.Rev = 3
	if ok, err := s.CostCheck(ctx, stale, next, true, "x"); err != nil || ok {
		t.Errorf("stale CostCheck = %v, %v, want fenced out", ok, err)
	}
}

// TestCostPublishQueuesAndFreezes: a publication stores the text at the
// next revision and queues its action in one transaction; the task's
// first notice of another run freezes the live one; a stale writer is
// fenced out.
func TestCostPublishQueuesAndFreezes(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := failedRun(t, s)
	older, newer := costRun(id, runName), costRun(id, "vikunja-fu-gordon-5-c2")
	newer.Finished = costNow.Add(-30 * time.Minute)
	for _, c := range []store.CostRun{older, newer} {
		if err := s.RecordCostRun(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	due := costDue(t, s, costNow)
	if len(due) != 2 || due[0].Name != runName {
		t.Fatalf("due %+v, want the older first", due)
	}
	live, next := costNow.Add(14*24*time.Hour), costNow.Add(15*time.Minute)
	if ok, err := s.CostPublish(ctx, due[0], "<p>one</p>", "d1", costNow, live, next); err != nil || !ok {
		t.Fatalf("CostPublish = %v, %v", ok, err)
	}
	if ok, err := s.CostPublish(ctx, due[0], "<p>again</p>", "d2", costNow, live, next); err != nil || ok {
		t.Fatalf("stale CostPublish = %v, %v, want fenced out", ok, err)
	}
	body, rev, err := s.CostBody(ctx, runName, id)
	if err != nil || body != "<p>one</p>" || rev != 1 {
		t.Fatalf("CostBody = %q, %d, %v", body, rev, err)
	}
	a := action.CostNotice(5, runName, id, 1)
	if n := count(t, dbURL, `SELECT count(*) FROM actions WHERE inbox_id = $1 AND key = $2 AND kind = 'publish-cost'
		AND value = $3 AND detail = $4 AND family = 'vikunja-cost' AND state = 'pending'`,
		id, a.Key, runName, a.Detail); n != 1 {
		t.Errorf("publish action not queued")
	}
	if n := count(t, dbURL, `SELECT count(*) FROM cost_notices WHERE name = $1 AND state = 'live' AND digest = 'd1'
		AND published_at = $2 AND live_until = $3 AND next_check = $4`, runName, costNow, live, next); n != 1 {
		t.Errorf("published row not live")
	}
	if ok, err := s.CostPublish(ctx, due[1], "<p>two</p>", "d3", costNow, live, next); err != nil || !ok {
		t.Fatalf("second notice CostPublish = %v, %v", ok, err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM cost_notices WHERE name = $1 AND state = 'frozen'`, runName); n != 1 {
		t.Errorf("older notice not frozen")
	}
	if got := costDue(t, s, next); len(got) != 1 || got[0].Name != newer.Name || got[0].State != store.CostLive ||
		got[0].Rev != 1 || got[0].Digest != "d3" || !got[0].LiveUntil.Equal(live) {
		t.Fatalf("due %+v, want only the newer, live", got)
	}
}

// TestCostRevisionIsEditedInPlace: a live notice's next revision queues
// its own action and freezes nothing.
func TestCostRevisionIsEditedInPlace(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := failedRun(t, s)
	if err := s.RecordCostRun(ctx, costRun(id, runName)); err != nil {
		t.Fatal(err)
	}
	n := costDue(t, s, costNow)[0]
	if _, err := s.CostPublish(ctx, n, "<p>one</p>", "d1", costNow, costNow.Add(time.Hour), costNow); err != nil {
		t.Fatal(err)
	}
	n = costDue(t, s, costNow)[0]
	if ok, err := s.CostPublish(ctx, n, "<p>two</p>", "d2", costNow, costNow.Add(time.Hour), costNow); err != nil || !ok {
		t.Fatalf("CostPublish rev 2 = %v, %v", ok, err)
	}
	if body, rev, _ := s.CostBody(ctx, runName, id); body != "<p>two</p>" || rev != 2 {
		t.Errorf("CostBody = %q, %d", body, rev)
	}
	if got := count(t, dbURL, `SELECT count(*) FROM actions WHERE kind = 'publish-cost'`); got != 2 {
		t.Errorf("publish actions %d, want one per revision", got)
	}
	n = costDue(t, s, costNow)[0]
	if ok, err := s.CostSettle(ctx, n); err != nil || !ok {
		t.Fatalf("CostSettle = %v, %v", ok, err)
	}
	if got := costDue(t, s, costNow.Add(time.Hour)); len(got) != 0 {
		t.Errorf("settled notice due: %+v", got)
	}
	if ok, err := s.CostSettle(ctx, n); err != nil || ok {
		t.Errorf("second CostSettle = %v, %v, want fenced out", ok, err)
	}
}

func TestCostBodyOfNone(t *testing.T) {
	s, _ := storetest.Open(t)
	if _, _, err := s.CostBody(ctx, "nothing", 1); !errors.Is(err, store.ErrNoCostNotice) {
		t.Errorf("CostBody = %v, want ErrNoCostNotice", err)
	}
}

// TestGrantWorkerCostRights: the worker records, takes, checks,
// publishes and settles cost notices; it cannot rewrite a notice's run
// or delete one.
func TestGrantWorkerCostRights(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	worker := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Worker: worker}); err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	id := failedRun(t, owner)
	if err := ws.RecordCostRun(ctx, costRun(id, runName)); err != nil {
		t.Fatalf("worker RecordCostRun: %v", err)
	}
	n := costDue(t, ws, costNow)[0]
	if ok, err := ws.CostCheck(ctx, n, costNow, true, "x"); err != nil || !ok {
		t.Fatalf("worker CostCheck = %v, %v", ok, err)
	}
	n = costDue(t, ws, costNow)[0]
	if ok, err := ws.CostPublish(ctx, n, "b", "d", costNow, costNow, costNow); err != nil || !ok {
		t.Fatalf("worker CostPublish = %v, %v", ok, err)
	}
	if _, _, err := ws.CostBody(ctx, runName, id); err != nil {
		t.Fatalf("worker CostBody: %v", err)
	}
	n = costDue(t, ws, costNow)[0]
	if ok, err := ws.CostSettle(ctx, n); err != nil || !ok {
		t.Fatalf("worker CostSettle = %v, %v", ok, err)
	}
	conn, err := pgx.Connect(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{`UPDATE cost_notices SET task_id = 6`, `UPDATE cost_notices SET name = 'x'`,
		`DELETE FROM cost_notices`} {
		if _, err := conn.Exec(ctx, q); !denied(err) {
			t.Errorf("worker %s = %v, want permission denied", q, err)
		}
	}
}
