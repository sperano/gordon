package submit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

var ctx = context.Background()

const (
	testTTL     = time.Minute
	gordonID    = 147
	implementID = 1624
	busyDelay   = 5 * time.Minute
)

// tasks serves fresh Vikunja tasks by id.
type tasks struct {
	mu sync.Mutex
	m  map[int64]event.Task
}

func (f *tasks) Task(_ context.Context, id int64) (event.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.m[id]
	if !ok {
		return event.Task{}, &upstream.Error{Provider: upstream.ProviderVikunja, Class: upstream.NotFound, Status: http.StatusNotFound}
	}
	return t, nil
}

func (f *tasks) Comments(context.Context, int64, int) (json.RawMessage, error) {
	return json.RawMessage("[]"), nil
}

type noPRs struct{}

func (noPRs) PullRequest(context.Context, string, int, bool) (event.PullRequest, error) {
	return event.PullRequest{}, &upstream.Error{Provider: upstream.ProviderGitHub, Class: upstream.NotFound}
}

// env is a worker that executes every family, its outbox submitting to a
// fake Kelos API, with the dispatch fixture rendered from Hollingsworth.
type env struct {
	store   *store.Store
	dbURL   string
	kelos   *kelostest.Server
	api     *kelos.Client
	tasks   *tasks
	proc    *worker.Processor
	out     *worker.Outbox
	exec    *Executor
	metrics *metrics.Metrics
	logs    *bytes.Buffer
}

func fixture(t *testing.T) dispatch.Config {
	t.Helper()
	b, err := os.ReadFile("../dispatch/testdata/hollingsworth.json")
	if err != nil {
		t.Fatal(err)
	}
	var c dispatch.Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func gordonOwnsAll() rollout.Table {
	var tbl rollout.Table
	for _, f := range run.Families {
		tbl.Rules = append(tbl.Rules, rollout.Rule{Project: rollout.AllProjects, Family: f, Owner: rollout.Gordon,
			Evidence: []rollout.Gate{rollout.ReplayParity, rollout.ShadowParity, rollout.RollbackTested, rollout.LegacyDisabled}})
	}
	return tbl
}

func newEnv(t *testing.T, shadow bool, limits dispatch.Limits) *env {
	t.Helper()
	s, dbURL := storetest.Open(t)
	srv := kelostest.New(route.KelosNamespace)
	t.Cleanup(srv.Close)
	e := &env{store: s, dbURL: dbURL, kelos: srv, tasks: &tasks{m: map[int64]event.Task{}}, metrics: metrics.New(),
		logs: &bytes.Buffer{}}
	e.api = &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	log := logging.New(e.logs, slog.LevelDebug)
	routing, cfg := profile.Defaults(), fixture(t)
	cfg.Limits = limits
	backoff := worker.Backoff{Base: time.Second, Max: time.Minute}
	e.proc = &worker.Processor{Store: s, Router: route.Router{Config: routing, Ownership: gordonOwnsAll()},
		State:  worker.StateReader{Config: routing, Tasks: e.tasks, PRs: noPRs{}},
		Tasks:  dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}, Comments: e.tasks},
		Shadow: shadow, Owner: "w", Concurrency: 4, LeaseTTL: testTTL, MaxAttempts: 3, Backoff: backoff,
		Projects: &worker.Limiter{}, DeferDelay: time.Second, Metrics: e.metrics, Log: log}
	e.exec = &Executor{Runs: s, Kelos: e.api, Limits: limits, BusyDelay: busyDelay, Metrics: e.metrics, Log: log}
	e.out = &worker.Outbox{Store: s, Executors: map[action.Kind]worker.Executor{action.SubmitTask: e.exec},
		Owner: "w", Concurrency: 4, LeaseTTL: testTTL, MaxAttempts: 3, Backoff: backoff, Providers: &worker.Limiter{},
		Projects: &worker.Limiter{}, DeferDelay: time.Second, NoExecutorDelay: time.Hour, Metrics: e.metrics, Log: log}
	return e
}

func doing(id int64, variant string, labels ...string) event.Task {
	return event.Task{ID: id, ProjectID: gordonID, Title: "Ship it", Description: "<p>now</p>", Labels: labels,
		Assignees: []string{variant}, Buckets: []string{"Doing"}}
}

// implement stores a task.updated for task id (eligible in the fresh
// state) and decides it; it returns the run name.
func (e *env) implement(t *testing.T, id int64, variant string) string {
	t.Helper()
	task := doing(id, variant)
	e.tasks.mu.Lock()
	e.tasks.m[id] = task
	e.tasks.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"event_name": "task.updated", "data": map[string]any{
		"task": map[string]any{"id": id, "project_id": gordonID, "assignees": []map[string]string{{"username": variant}},
			"buckets": []map[string]string{{"title": "Doing"}}},
		"doer": map[string]string{"username": "eric"}}})
	ev, err := event.Parse(event.Delivery{Source: event.SourceVikunja, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.Accept(ctx, store.Delivery{Source: event.SourceVikunja, DeliveryKey: ev.DeliveryKey,
		RequestKey: ev.RequestKey, Kind: ev.Kind, Scope: ev.Scope(), Body: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.proc.Tick(ctx, ctx); err != nil {
		t.Fatal(err)
	}
	return run.Identity{Family: run.VikunjaImplement, Project: "gordon", Variant: variant, TaskID: id}.Name()
}

func (e *env) perform(t *testing.T) {
	t.Helper()
	if _, err := e.out.Tick(ctx, ctx); err != nil {
		t.Fatal(err)
	}
}

func (e *env) due(t *testing.T) {
	t.Helper()
	storetest.Exec(t, e.dbURL, `UPDATE actions SET next_attempt_at = now()`)
}

func (e *env) action(t *testing.T, name string) (state string, attempts int, lastErr string) {
	t.Helper()
	storetest.QueryRow(t, e.dbURL, `SELECT state, attempts, last_error FROM actions WHERE target = $1`,
		[]any{string(action.KelosTask(route.KelosNamespace, name))}, &state, &attempts, &lastErr)
	return state, attempts, lastErr
}

func (e *env) run(t *testing.T, name string) (state string, exists bool) {
	t.Helper()
	storetest.QueryRow(t, e.dbURL, `SELECT state, task_exists FROM runs WHERE name = $1`, []any{name}, &state, &exists)
	return state, exists
}

func (e *env) submissions(result string) float64 {
	return testutil.ToFloat64(e.metrics.Submissions.WithLabelValues(result))
}

// running is another Task holding a slot or a branch.
func running(name, variant, workspace, branch, phase string) kelos.Task {
	return kelos.Task{Metadata: kelos.Metadata{Name: name, Labels: map[string]string{dispatch.VariantLabel: variant}},
		Spec:   kelos.Spec{Prompt: "x", Branch: branch, Worker: &kelos.Worker{Type: "claude-code", WorkspaceRef: &kelos.Ref{Name: workspace}}},
		Status: &kelos.Status{Phase: phase}}
}

func TestSubmitsTheBuiltTaskExactly(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	if st, _, msg := e.action(t, name); st != store.ActionDone {
		t.Fatalf("submission %s (%s), want done", st, msg)
	}
	creates := e.kelos.Creates()
	if len(creates) != 1 {
		t.Fatalf("%d creates, want 1", len(creates))
	}
	var built kelos.Task
	storetest.QueryRow(t, e.dbURL, `SELECT task FROM runs WHERE name = $1`, []any{name}, &built)
	built.APIVersion, built.Kind = kelos.APIVersion, kelos.KindTask
	sent, _ := json.Marshal(creates[0])
	if diff, err := kelos.Diff(built, sent); err != nil || len(diff) > 0 || creates[0].Metadata.Name != name {
		t.Fatalf("submitted %s differs from the recorded Task at %v (%v)", creates[0].Metadata.Name, diff, err)
	}
	var inbox int64
	storetest.QueryRow(t, e.dbURL, `SELECT inbox_id FROM runs WHERE name = $1`, []any{name}, &inbox)
	if creates[0].Metadata.Annotations[dispatch.InboxAnnotation] != strconv.FormatInt(inbox, 10) ||
		creates[0].Metadata.Labels[dispatch.ProfileLabel] != "default" || creates[0].Spec.Branch != "kelos/vikunja-1624" {
		t.Fatalf("submitted metadata %+v branch %q", creates[0].Metadata, creates[0].Spec.Branch)
	}
	if st, exists := e.run(t, name); st != "active" || !exists || e.submissions(ResultCreated) != 1 {
		t.Fatalf("run %s exists %v, created %v", st, exists, e.submissions(ResultCreated))
	}
}

func TestLostCreateResponseIsRecoveredNotRepeated(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.kelos.FailCreates(kelostest.DropResponse)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionAmbiguous {
		t.Fatalf("after a lost response: %s, want ambiguous", st)
	}
	e.due(t)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || len(e.kelos.Creates()) != 1 {
		t.Fatalf("state %s after %d creates; want done by recovery with one create", st, len(e.kelos.Creates()))
	}
	if st, exists := e.run(t, name); st != "active" || !exists || e.submissions(ResultRecovered) != 1 {
		t.Fatalf("run %s exists %v recovered %v", st, exists, e.submissions(ResultRecovered))
	}
}

func TestWorkerDiedAfterCreatingIsNotRepeated(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	claimed, err := e.store.ClaimActions(ctx, "dead", 1, testTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if err := e.exec.Execute(ctx, claimed[0].Action); err != nil { // created, then the worker died
		t.Fatal(err)
	}
	storetest.Exec(t, e.dbURL, `UPDATE actions SET lease_until = now() - interval '1 second'`)
	if _, _, err := e.store.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	e.due(t)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || len(e.kelos.Creates()) != 1 {
		t.Fatalf("state %s after %d creates; want done with the one create", st, len(e.kelos.Creates()))
	}
}

func TestLegacyTaskOfTheSameNameIsNotDuplicated(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := run.Identity{Family: run.VikunjaImplement, Project: "gordon", Variant: "claude-opus", TaskID: implementID}.Name()
	e.kelos.Put(running(name, "claude-opus", "gordon", "kelos/vikunja-1624", kelos.PhaseRunning))
	e.implement(t, implementID, "claude-opus")
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || len(e.kelos.Creates()) != 0 {
		t.Fatalf("state %s after %d creates; want done without creating", st, len(e.kelos.Creates()))
	}
	if st, exists := e.run(t, name); st != "active" || !exists || e.submissions(ResultCollision) != 1 {
		t.Fatalf("run %s exists %v collisions %v; want the legacy Task followed", st, exists, e.submissions(ResultCollision))
	}
}

func TestCreateConflictWithAnotherSubmitter(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	// Another submitter creates the Task between our check and our create.
	api := &raceAPI{TaskAPI: e.api, before: func() {
		e.kelos.Put(running(name, "claude-opus", "gordon", "kelos/vikunja-1624", kelos.PhasePending))
	}}
	e.exec.Kelos = api
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || e.submissions(ResultCollision) != 1 {
		t.Fatalf("state %s collisions %v; want done as taken", st, e.submissions(ResultCollision))
	}
}

// raceAPI runs before ahead of the first create.
type raceAPI struct {
	TaskAPI
	before func()
	once   sync.Once
}

func (r *raceAPI) Create(ctx context.Context, t kelos.Task) (kelos.Task, error) {
	r.once.Do(r.before)
	return r.TaskAPI.Create(ctx, t)
}

func TestAdmissionRefusalDeadLettersAndFreesTheName(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	e.kelos.Admit = func(kelos.Task) error { return errors.New("profile default does not grant Secrets x") }
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	if st, _, msg := e.action(t, name); st != store.ActionFailed || e.submissions(ResultRefused) != 1 {
		t.Fatalf("refused submission %s (%s), want failed", st, msg)
	}
	obs := &Observer{Store: e.store, Kelos: e.api, Metrics: e.metrics, Log: logging.New(e.logs, slog.LevelDebug)}
	if err := obs.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if st, exists := e.run(t, name); st != "finished" || exists {
		t.Fatalf("run of a refused submission %s exists %v; want finished, so the owner can retry", st, exists)
	}
}

func TestTransientCreateFailureRetries(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.kelos.FailCreates(http.StatusTooManyRequests)
	e.perform(t)
	if st, attempts, _ := e.action(t, name); st != store.ActionPending || attempts != 1 || e.submissions(ResultError) != 1 {
		t.Fatalf("after a 429: %s, %d attempts", st, attempts)
	}
	e.due(t)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || len(e.kelos.Creates()) != 2 {
		t.Fatalf("state %s after %d creates; want done on the second", st, len(e.kelos.Creates()))
	}
}

func TestVariantLimitKeepsSubmissionsQueued(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{Default: 1})
	e.kelos.Put(running("other-1", "claude-opus", "vigie", "", kelos.PhaseRunning))
	e.kelos.Put(running("waiting-1", "claude-opus", "vigie", "b", kelos.PhaseWaiting))
	e.kelos.Put(running("done-1", "claude-opus", "vigie", "", kelos.PhaseSucceeded))
	e.kelos.Put(running("sonnet-1", "claude-sonnet", "vigie", "", kelos.PhaseRunning))
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	if st, attempts, msg := e.action(t, name); st != store.ActionPending || attempts != 0 ||
		msg != "variant claude-opus runs 1 of 1" || len(e.kelos.Creates()) != 0 {
		t.Fatalf("over the limit: %s, %d attempts, %q, %d creates; want queued, uncounted", st, attempts, msg, len(e.kelos.Creates()))
	}
	if e.submissions(ResultLimit) != 1 ||
		testutil.ToFloat64(e.metrics.Deferred.WithLabelValues(store.QueueActions, LimitVariant)) != 1 {
		t.Fatal("limit deferral not counted")
	}
	e.kelos.SetPhase("other-1", kelos.PhaseSucceeded)
	e.due(t)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || len(e.kelos.Creates()) != 1 {
		t.Fatalf("after a slot freed: %s, %d creates", st, len(e.kelos.Creates()))
	}
}

func TestConcurrentSubmissionsRespectTheLimit(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{Default: 2})
	for id := int64(1); id <= 4; id++ {
		e.implement(t, id, "claude-opus")
	}
	e.perform(t) // four actions performed at once
	if n := len(e.kelos.Creates()); n != 2 {
		t.Fatalf("%d Tasks created at once, want the limit 2", n)
	}
	if n := e.submissions(ResultLimit); n != 2 {
		t.Fatalf("%v deferred, want 2", n)
	}
}

func TestHeldBranchKeepsSubmissionQueued(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	// A follow-up on the task's branch in its Workspace holds Kelos's lock;
	// the read-only copy and other branches do not. They are listed first,
	// so a check that ignored the Workspace or the branch would name them.
	e.kelos.Put(running("vikunja-discuss-x", "claude-opus", "gordon-readonly", "kelos/vikunja-1624", kelos.PhaseRunning))
	e.kelos.Put(running("other-branch", "claude-opus", "gordon", "kelos/vikunja-9", kelos.PhaseRunning))
	e.kelos.Put(running("vikunja-fu-gordon-1624-c1", "claude-opus", "gordon", "kelos/vikunja-1624", kelos.PhaseRunning))
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	st, attempts, msg := e.action(t, name)
	if st != store.ActionPending || attempts != 0 || msg != "branch kelos/vikunja-1624 held by vikunja-fu-gordon-1624-c1" ||
		e.submissions(ResultBranch) != 1 ||
		testutil.ToFloat64(e.metrics.Deferred.WithLabelValues(store.QueueActions, LimitBranch)) != 1 {
		t.Fatalf("on a held branch: %s, %d attempts, %q", st, attempts, msg)
	}
	var wait float64
	storetest.QueryRow(t, e.dbURL, `SELECT extract(epoch FROM next_attempt_at - now())::float8 FROM actions`, nil, &wait)
	if wait < (busyDelay - time.Minute).Seconds() {
		t.Fatalf("deferred for %.0fs, want the busy delay", wait)
	}
	e.kelos.SetPhase("vikunja-fu-gordon-1624-c1", kelos.PhaseFailed)
	e.due(t)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone {
		t.Fatalf("after the branch freed: %s", st)
	}
}

func TestRecoveringARunWithoutABuiltTaskFailsAtOnce(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.kelos.FailCreates(kelostest.DropResponse)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionAmbiguous {
		t.Fatalf("after a lost response: %s, want ambiguous", st)
	}
	storetest.Exec(t, e.dbURL, `UPDATE runs SET task = NULL`)
	e.due(t)
	e.perform(t)
	if st, attempts, msg := e.action(t, name); st != store.ActionFailed || attempts != 2 ||
		msg != "permanent: "+name+": run has no built Task" {
		t.Fatalf("state %s after %d attempts (%s), want failed at once for the missing Task", st, attempts, msg)
	}
}

func TestSubmitWithoutABuiltTaskIsPermanent(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	storetest.Exec(t, e.dbURL, `UPDATE runs SET task = NULL`)
	e.perform(t)
	if st, _, msg := e.action(t, name); st != store.ActionFailed || msg != "permanent: "+name+": run has no built Task" {
		t.Fatalf("state %s (%s), want failed for the missing Task", st, msg)
	}
	other := action.New(action.SubmitTask, action.KelosTask("default", "x"), "")
	if err := e.exec.Execute(ctx, other); err == nil ||
		err.Error() != "permanent: not a submission in kelos-agents: submit-task kelos/default/x" {
		t.Fatalf("submission outside %s = %v, want a permanent refusal", route.KelosNamespace, err)
	}
	unknown := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, "nobody"), "")
	if err := e.exec.Execute(ctx, unknown); err == nil || err.Error() != "permanent: nobody: no such run" {
		t.Fatalf("submission of an unknown run = %v, want a permanent refusal", err)
	}
}
