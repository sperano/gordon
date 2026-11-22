package store_test

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

// failedRun records the run of withTask as submitted, then observed
// failed with its Task still present; it returns its inbox id.
func failedRun(t *testing.T, s *store.Store) int64 {
	t.Helper()
	id := recorded(t, s, "k1", "vikunja/comment/1", withTask("p"))
	if err := s.RunSubmitted(ctx, runName, id, "uid-1", kelos.PhaseRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveRun(ctx, runName, id, true, kelos.PhaseFailed); err != nil {
		t.Fatal(err)
	}
	return id
}

func undecided(t *testing.T, s *store.Store, projects ...string) []store.FailedRun {
	t.Helper()
	runs, err := s.FailedUndecided(ctx, projects, 10)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestFailedUndecided(t *testing.T) {
	s, dbURL := storetest.Open(t)
	if got := undecided(t, s, "gordon"); len(got) != 0 {
		t.Fatalf("no runs: %+v", got)
	}
	id := failedRun(t, s)
	got := undecided(t, s, "gordon")
	if len(got) != 1 {
		t.Fatalf("FailedUndecided = %+v", got)
	}
	f := got[0]
	if f.Name != runName || f.InboxID != id || f.RequestKey != "vikunja/comment/1" || f.Project != "gordon" ||
		f.Identity.Family != run.VikunjaFollowUp || f.Identity.TaskID != 5 || f.Profile.ConfigVersion != "v1" {
		t.Errorf("failed run %+v", f)
	}
	if got := undecided(t, s, "vigie"); len(got) != 0 {
		t.Errorf("another project's runs: %+v", got)
	}
	// A failed run whose Task is gone, or one that succeeded, is none.
	storetest.Exec(t, dbURL, `UPDATE runs SET task_exists = false`)
	if got := undecided(t, s, "gordon"); len(got) != 0 {
		t.Errorf("gone Task: %+v", got)
	}
	storetest.Exec(t, dbURL, `UPDATE runs SET task_exists = true, phase = 'Succeeded'`)
	if got := undecided(t, s, "gordon"); len(got) != 0 {
		t.Errorf("succeeded run: %+v", got)
	}
}

// TestDecideFailedRecordsTheRestart: the decision, the restart run of the
// stopped run's generation and its actions are recorded together, once.
func TestDecideFailedRecordsTheRestart(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := failedRun(t, s)
	f := undecided(t, s, "gordon")[0]
	rid := f.Identity
	rid.Retry = 1
	restart := route.Run{Name: rid.Name(), Identity: rid, Profile: f.Profile,
		Task: &kelos.Task{Metadata: kelos.Metadata{Name: rid.Name()}, Spec: kelos.Spec{Prompt: "p"}}}
	submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, restart.Name), "")
	notice := action.Notice(action.VikunjaTask(5), "restart", restart.Name).WaitFor(submit)
	fd := route.FamilyDecision{Family: run.LimitRestart, Project: "gordon", Actions: []action.Action{submit, notice}}
	ok, err := s.DecideFailed(ctx, f, store.LimitRestarted, &restart, fd)
	if err != nil || !ok {
		t.Fatalf("DecideFailed = %v, %v", ok, err)
	}
	var mark string
	storetest.QueryRow(t, dbURL, `SELECT limit_restart FROM runs WHERE name = $1`, []any{runName}, &mark)
	if mark != store.LimitRestarted {
		t.Errorf("mark %q", mark)
	}
	r, err := s.RunToSubmit(ctx, restart.Name)
	if err != nil || r.InboxID != id || r.State != "submitting" || r.Family != run.VikunjaFollowUp || r.Task == nil {
		t.Fatalf("restart run %+v, %v", r, err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM actions WHERE inbox_id = $1 AND family = 'limit-restart'
		AND key IN ($2, $3)`, id, submit.Key, notice.Key); n != 2 {
		t.Errorf("%d restart actions queued, want 2", n)
	}
	if got := undecided(t, s, "gordon"); len(got) != 0 {
		t.Errorf("decided run still undecided: %+v", got)
	}
	// Decided already (another replica): nothing is written again.
	ok, err = s.DecideFailed(ctx, f, store.LimitSuperseded, nil, route.FamilyDecision{Family: run.LimitRestart,
		Actions: []action.Action{action.Notice(action.VikunjaTask(5), "x", "")}})
	if err != nil || ok {
		t.Fatalf("second DecideFailed = %v, %v", ok, err)
	}
	storetest.QueryRow(t, dbURL, `SELECT limit_restart FROM runs WHERE name = $1`, []any{runName}, &mark)
	if mark != store.LimitRestarted || count(t, dbURL, `SELECT count(*) FROM actions WHERE key = 'post-notice:vikunja/task/5:x'`) != 0 {
		t.Errorf("a decided run was decided again: mark %q", mark)
	}
}

// TestDecideFailedRestartNameTaken: a restart whose name an active run
// holds drops its submission and what waits for it, keeping the rest.
func TestDecideFailedRestartNameTaken(t *testing.T) {
	s, dbURL := storetest.Open(t)
	failedRun(t, s)
	f := undecided(t, s, "gordon")[0]
	// The name is taken: the failed run's own Task is still present, so
	// a restart under its name stands for one whose name a run holds.
	restart := route.Run{Name: runName, Identity: f.Identity, Profile: f.Profile}
	other := f
	other.Name = "vikunja-fu-gordon-5-c2"
	copyRun(t, dbURL, other.Name, "finished")
	submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, restart.Name), "")
	notice := action.Notice(action.VikunjaTask(5), "restart", restart.Name).WaitFor(submit)
	kept := action.Notice(action.VikunjaTask(5), "kept", "")
	fd := route.FamilyDecision{Family: run.LimitRestart, Project: "gordon", Actions: []action.Action{submit, notice, kept}}
	ok, err := s.DecideFailed(ctx, other, store.LimitRestarted, &restart, fd)
	if err != nil || !ok {
		t.Fatalf("DecideFailed = %v, %v", ok, err)
	}
	var keys []string
	storetest.QueryRow(t, dbURL, `SELECT coalesce(array_agg(key ORDER BY key), '{}') FROM actions WHERE family = 'limit-restart'`,
		nil, &keys)
	if want := []string{kept.Key}; !slices.Equal(keys, want) {
		t.Errorf("queued %v, want %v: the taken run's submission and its notice dropped", keys, want)
	}
}

// TestNewGenerationIsUndecided: a run recorded again after cleanup starts
// with no limit decision of its earlier generation.
func TestNewGenerationIsUndecided(t *testing.T) {
	s, dbURL := storetest.Open(t)
	failedRun(t, s)
	f := undecided(t, s, "gordon")[0]
	if ok, err := s.DecideFailed(ctx, f, store.LimitNone, nil, route.FamilyDecision{Family: run.LimitRestart}); err != nil || !ok {
		t.Fatalf("DecideFailed = %v, %v", ok, err)
	}
	storetest.Exec(t, dbURL, `UPDATE runs SET task_exists = false`)
	recorded(t, s, "k2", "vikunja/comment/1b", withTask("p"))
	var mark string
	storetest.QueryRow(t, dbURL, `SELECT limit_restart FROM runs WHERE name = $1`, []any{runName}, &mark)
	if mark != "" {
		t.Errorf("new generation's mark %q, want none", mark)
	}
}

func TestRecordCompletionOnce(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := failedRun(t, s)
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	c := store.Completion{Name: runName, InboxID: id, Family: run.VikunjaFollowUp, Project: "gordon", TaskUID: "uid-1",
		Phase: kelos.PhaseFailed, Completed: &at, Results: map[string]string{"cost-usd": "1.5"}}
	if err := s.RecordCompletion(ctx, c); err != nil {
		t.Fatal(err)
	}
	second := c
	second.Phase, second.Results = kelos.PhaseSucceeded, nil
	if err := s.RecordCompletion(ctx, second); err != nil {
		t.Fatal(err)
	}
	var phase, family, project, uid string
	var completed time.Time
	var results map[string]string
	storetest.QueryRow(t, dbURL, `SELECT phase, family, project, task_uid, completed_at, results FROM run_completions
		WHERE name = $1 AND inbox_id = $2`, []any{runName, id}, &phase, &family, &project, &uid, &completed, &results)
	if phase != kelos.PhaseFailed || family != string(run.VikunjaFollowUp) || project != "gordon" || uid != "uid-1" ||
		!completed.Equal(at) || !maps.Equal(results, c.Results) {
		t.Errorf("completion %s %s %s %s %v %v: the first record stands", phase, family, project, uid, completed, results)
	}
	// No completion time and no results record a null time and {}.
	bare := store.Completion{Name: "other", InboxID: id, Family: run.PRReview, Phase: kelos.PhaseSucceeded}
	if err := s.RecordCompletion(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if n := count(t, dbURL, `SELECT count(*) FROM run_completions WHERE name = 'other' AND completed_at IS NULL AND results = '{}'`); n != 1 {
		t.Errorf("bare completion not recorded as such")
	}
}

// TestGrantWorkerRestartRights: the worker role decides failed runs (the
// mark, the restart run, its actions), records completions and reads the
// taken runs; it cannot rewrite or delete a completion.
func TestGrantWorkerRestartRights(t *testing.T) {
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
	f := undecided(t, ws, "gordon")[0]
	rid := f.Identity
	rid.Retry = 1
	restart := route.Run{Name: rid.Name(), Identity: rid, Profile: f.Profile}
	fd := route.FamilyDecision{Family: run.LimitRestart, Project: "gordon",
		Actions: []action.Action{action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, restart.Name), "")}}
	if ok, err := ws.DecideFailed(ctx, f, store.LimitRestarted, &restart, fd); err != nil || !ok {
		t.Fatalf("worker DecideFailed = %v, %v", ok, err)
	}
	if err := ws.RecordCompletion(ctx, store.Completion{Name: runName, InboxID: id, Family: run.VikunjaFollowUp,
		Phase: kelos.PhaseFailed}); err != nil {
		t.Fatalf("worker RecordCompletion: %v", err)
	}
	taken, err := ws.RunsTaken(ctx, []string{runName, restart.Name, "nothing"})
	if err != nil || !taken[runName] || !taken[restart.Name] || taken["nothing"] {
		t.Fatalf("worker RunsTaken = %v, %v; want the failed run (its Task is there) and the submitting restart", taken, err)
	}
	conn, err := pgx.Connect(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{`UPDATE run_completions SET phase = 'x'`, `DELETE FROM run_completions`} {
		if _, err := conn.Exec(ctx, q); !denied(err) {
			t.Errorf("worker %s = %v, want permission denied", q, err)
		}
	}
}
