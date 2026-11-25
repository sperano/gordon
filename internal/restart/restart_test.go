package restart

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

// decision is one DecideFailed call.
type decision struct {
	name, mark string
	restart    *route.Run
	fd         route.FamilyDecision
}

type fakeStore struct {
	failed    []store.FailedRun
	projects  []string
	decisions []decision
	listErr   error
	// refuse makes DecideFailed fail (err) or find the run decided.
	refuse  map[string]error
	decided map[string]bool
}

func (f *fakeStore) FailedUndecided(_ context.Context, projects []string, limit int) ([]store.FailedRun, error) {
	f.projects = projects
	if limit != DefaultBatch {
		return nil, errors.New("unexpected batch")
	}
	return f.failed, f.listErr
}

func (f *fakeStore) DecideFailed(_ context.Context, r store.FailedRun, mark string, rn *route.Run, fd route.FamilyDecision) (bool, error) {
	if err := f.refuse[r.Name]; err != nil {
		return false, err
	}
	if f.decided[r.Name] {
		return false, nil
	}
	f.decisions = append(f.decisions, decision{r.Name, mark, rn, fd})
	return true, nil
}

type lister struct {
	tasks []kelos.Task
	err   error
}

func (l *lister) List(_ context.Context, selector string) ([]kelos.Task, error) {
	if selector != "" {
		return nil, errors.New("unexpected selector " + selector)
	}
	return l.tasks, l.err
}

var gates = []rollout.Gate{rollout.ReplayParity, rollout.ShadowParity, rollout.RollbackTested, rollout.LegacyDisabled}

func ownsRestarts(projects ...string) rollout.Table {
	var t rollout.Table
	for _, p := range projects {
		t.Rules = append(t.Rules, rollout.Rule{Project: p, Family: run.LimitRestart, Owner: rollout.Gordon, Evidence: gates})
	}
	return t
}

// now is the tests' clock; most stops' limits reset an hour before it.
var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func formatInt(n int64) string { return strconv.FormatInt(n, 10) }

// stopTask is a Task of the run the limit stopped, its limit reset at reset.
func stopTask(name string, inbox int64, labels map[string]string, created string, reset time.Time) kelos.Task {
	return kelos.Task{
		Metadata: kelos.Metadata{Name: name, UID: "uid-" + name, CreationTimestamp: created, Labels: labels,
			Annotations: map[string]string{dispatch.InboxAnnotation: formatInt(inbox), "vikunja.spe.quebec/source": "vikunja",
				"kelos.spe.quebec/limit-restarted-as": "stale", "other.example/x": "y"}},
		Spec: kelos.Spec{Prompt: "do it", Branch: "kelos/vikunja-5"},
		Status: &kelos.Status{Phase: kelos.PhaseFailed, CompletionTime: "2026-11-20T09:00:00Z",
			Results: map[string]string{kelos.ResultLimitResetsAt: formatInt(reset.Unix()), kelos.ResultLimitType: "weekly"}},
	}
}

func implLabels(task string) map[string]string {
	return map[string]string{"vikunja.spe.quebec/project": "gordon", "vikunja.spe.quebec/task": task}
}

func implRun(name string, inbox int64, task int64, retry int) store.FailedRun {
	id := run.Identity{Family: run.VikunjaImplement, Project: "gordon", Variant: "claude-opus", TaskID: task, Retry: retry}
	return store.FailedRun{Name: name, InboxID: inbox, RequestKey: "req", Project: "gordon", Identity: id,
		Profile: profile.Resolved{Variant: "claude-opus"}}
}

func reaper(st *fakeStore, tasks []kelos.Task, owner rollout.Table) (*Reaper, *bytes.Buffer) {
	var logs bytes.Buffer
	return &Reaper{Store: st, Kelos: &lister{tasks: tasks}, Config: profile.Defaults(), Ownership: owner,
		Now: func() time.Time { return now }, Metrics: metrics.New(), Log: logging.New(&logs, slog.LevelDebug)}, &logs
}

func keys(actions []action.Action) []string {
	out := make([]string, len(actions))
	for i, a := range actions {
		out[i] = a.Key + "<" + a.After
	}
	return out
}

func TestRestartAfterTheReset(t *testing.T) {
	name := "vikunja-5-gordon-opus"
	st := &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0)}}
	task := stopTask(name, 9, implLabels("5"), "2026-11-20T08:00:00Z", now.Add(-time.Hour))
	r, _ := reaper(st, []kelos.Task{task}, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.projects, []string{"gordon"}) {
		t.Errorf("read the failed runs of %v, want [gordon]", st.projects)
	}
	if len(st.decisions) != 1 {
		t.Fatalf("decisions %+v", st.decisions)
	}
	d := st.decisions[0]
	if d.name != name || d.mark != store.LimitRestarted || d.restart == nil {
		t.Fatalf("decision %+v", d)
	}
	rn := *d.restart
	if rn.Name != name+"-retry1" || rn.Identity.Retry != 1 || rn.Branch != "kelos/vikunja-5" || rn.Profile.Variant != "claude-opus" {
		t.Errorf("restart run %+v", rn)
	}
	got := rn.Task
	wantAnn := map[string]string{dispatch.InboxAnnotation: "9", "vikunja.spe.quebec/source": "vikunja",
		RestartsAnnotation: "1", RestartOfAnnotation: name}
	if got.Metadata.Name != rn.Name || !maps.Equal(got.Metadata.Labels, task.Metadata.Labels) ||
		!maps.Equal(got.Metadata.Annotations, wantAnn) || got.Spec.Prompt != "do it" || got.Status != nil || got.Metadata.UID != "" {
		t.Errorf("restart Task %+v", got)
	}
	submit := "submit-task:kelos/kelos-agents/" + rn.Name
	want := []string{submit + "<",
		"post-notice:vikunja/task/5:restart:" + rn.Name + "<" + submit,
		"post-notice:vikunja/task/5:start:" + rn.Name + "<" + submit}
	if !slices.Equal(keys(d.fd.Actions), want) {
		t.Errorf("actions %v, want %v", keys(d.fd.Actions), want)
	}
	if d.fd.Family != run.LimitRestart || d.fd.Project != "gordon" || d.fd.Outcome != route.Dispatch || len(d.fd.Runs) != 1 {
		t.Errorf("family decision %+v", d.fd)
	}
	if n := testutil.ToFloat64(r.Metrics.LimitRestarts.WithLabelValues(store.LimitRestarted)); n != 1 {
		t.Errorf("restarted counted %v", n)
	}
}

func TestRestartWaits(t *testing.T) {
	name := "vikunja-5-gordon-opus"
	cases := map[string]time.Time{
		"before the reset":      now.Add(time.Hour),
		"inside the grace time": now.Add(-DefaultGrace + time.Second),
	}
	for label, reset := range cases {
		st := &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0)}}
		r, _ := reaper(st, []kelos.Task{stopTask(name, 9, implLabels("5"), "", reset)}, ownsRestarts("gordon"))
		if err := r.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(st.decisions) != 0 {
			t.Errorf("%s: decided %+v", label, st.decisions)
		}
	}
}

// TestRestartWithoutAKnownReset: no epoch and no completion time tells
// when the limit resets, so the stop restarts at once.
func TestRestartWithoutAKnownReset(t *testing.T) {
	name := "vikunja-5-gordon-opus"
	task := stopTask(name, 9, implLabels("5"), "", now)
	task.Status.CompletionTime = ""
	delete(task.Status.Results, kelos.ResultLimitResetsAt)
	st := &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0)}}
	r, _ := reaper(st, []kelos.Task{task}, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.decisions) != 1 || st.decisions[0].mark != store.LimitRestarted {
		t.Errorf("decisions %+v", st.decisions)
	}
}

// TestRestartAfterTheFallback: a stop that tells no reset but its
// completion time restarts the fallback time after it.
func TestRestartAfterTheFallback(t *testing.T) {
	name := "vikunja-5-gordon-opus"
	completed := now.Add(-DefaultFallback / 2)
	cases := []struct {
		name    string
		at      time.Time
		decided bool
	}{
		{"before the fallback", now, false},
		{"after the fallback and the grace", completed.Add(DefaultFallback + DefaultGrace), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := stopTask(name, 9, implLabels("5"), "", now)
			task.Status.CompletionTime = completed.Format(time.RFC3339)
			delete(task.Status.Results, kelos.ResultLimitResetsAt)
			st := &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0)}}
			r, _ := reaper(st, []kelos.Task{task}, ownsRestarts("gordon"))
			r.Now = func() time.Time { return c.at }
			if err := r.Once(context.Background()); err != nil {
				t.Fatal(err)
			}
			restarted := len(st.decisions) == 1 && st.decisions[0].mark == store.LimitRestarted
			if c.decided && !restarted || !c.decided && len(st.decisions) != 0 {
				t.Errorf("decisions %+v, want restarted %v", st.decisions, c.decided)
			}
		})
	}
}

func TestNoRestartWhereLegacyOwnsThem(t *testing.T) {
	st := &fakeStore{failed: []store.FailedRun{implRun("vikunja-5-gordon-opus", 9, 5, 0)}}
	r, _ := reaper(st, nil, ownsRestarts("vigie"))
	r.Config.Projects = slices.DeleteFunc(slices.Clone(r.Config.Projects), func(p profile.Project) bool { return p.Name == "vigie" })
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.projects != nil || len(st.decisions) != 0 {
		t.Errorf("read %v, decided %+v: no project is Gordon's", st.projects, st.decisions)
	}
}

func TestBusyGroupWaits(t *testing.T) {
	name := "vikunja-5-gordon-opus"
	stop := stopTask(name, 9, implLabels("5"), "", now.Add(-time.Hour))
	running := kelos.Task{Metadata: kelos.Metadata{Name: "vikunja-fu-gordon-5-c3", Labels: implLabels("5")},
		Status: &kelos.Status{Phase: kelos.PhaseRunning}}
	// A model-free Task of the same task holds nothing.
	prOpen := kelos.Task{Metadata: kelos.Metadata{Name: "pr-open-5", Labels: map[string]string{
		"vikunja.spe.quebec/project": "gordon", "vikunja.spe.quebec/task": "6", "vikunja.spe.quebec/role": "pr-open"}}}
	otherStop := stopTask("vikunja-6-gordon-opus", 10, implLabels("6"), "", now.Add(-time.Hour))
	st := &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0), implRun("vikunja-6-gordon-opus", 10, 6, 0)}}
	r, _ := reaper(st, []kelos.Task{stop, running, prOpen, otherStop}, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.decisions) != 1 || st.decisions[0].name != "vikunja-6-gordon-opus" {
		t.Errorf("decisions %+v: only task 6's group is free", st.decisions)
	}
}

func TestNewestRestartsOlderSuperseded(t *testing.T) {
	older, newer := "vikunja-fu-gordon-5-c3", "vikunja-fu-gordon-5-c4"
	fu := func(name string, comment int64) store.FailedRun {
		f := implRun(name, 9, 5, 0)
		f.Identity = run.Identity{Family: run.VikunjaFollowUp, Project: "gordon", TaskID: 5, CommentID: comment}
		return f
	}
	st := &fakeStore{failed: []store.FailedRun{fu(newer, 4), fu(older, 3)}}
	tasks := []kelos.Task{
		stopTask(newer, 9, implLabels("5"), "2026-11-20T08:00:00Z", now.Add(-time.Hour)),
		stopTask(older, 9, implLabels("5"), "2026-11-20T07:00:00Z", now.Add(-time.Hour)),
	}
	r, _ := reaper(st, tasks, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.decisions) != 2 || st.decisions[0].name != older || st.decisions[0].mark != store.LimitSuperseded ||
		st.decisions[0].restart != nil || len(st.decisions[0].fd.Actions) != 0 ||
		st.decisions[1].name != newer || st.decisions[1].mark != store.LimitRestarted ||
		st.decisions[1].restart.Name != newer+"-retry1" {
		t.Errorf("decisions %+v", st.decisions)
	}
}

func TestGiveUpAfterTheLastRestart(t *testing.T) {
	name := "vikunja-5-gordon-opus-retry3"
	st := &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 3)}}
	r, _ := reaper(st, []kelos.Task{stopTask(name, 9, implLabels("5"), "", now.Add(time.Hour))}, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.decisions) != 1 || st.decisions[0].mark != store.LimitGaveUp || st.decisions[0].restart != nil {
		t.Fatalf("decisions %+v", st.decisions)
	}
	if want := []string{"post-notice:vikunja/task/5:restart-gave-up:" + name + "<"}; !slices.Equal(keys(st.decisions[0].fd.Actions), want) {
		t.Errorf("actions %v, want %v", keys(st.decisions[0].fd.Actions), want)
	}
	// A configured maximum counts instead of the default.
	st = &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 3)}}
	r, _ = reaper(st, []kelos.Task{stopTask(name, 9, implLabels("5"), "", now.Add(-time.Hour))}, ownsRestarts("gordon"))
	r.MaxRestarts = 4
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.decisions) != 1 || st.decisions[0].mark != store.LimitRestarted || st.decisions[0].restart.Name != "vikunja-5-gordon-opus-retry4" {
		t.Errorf("with 4 restarts allowed: %+v", st.decisions)
	}
}

// TestPullRequestGiveUp: a PR follow-up or conflict run gives up on its
// pull request; a review, announced nowhere, gives up silently.
func TestPullRequestGiveUp(t *testing.T) {
	cases := []struct {
		name string
		id   run.Identity
		want []string
	}{
		{"follow-up", run.Identity{Family: run.PRFollowUp, Workspace: "gordon", PR: 7, CommentKind: "r", CommentID: 9, Retry: 3},
			[]string{"post-notice:github/sperano/gordon/pull/7:restart-gave-up:stopped<"}},
		{"conflict", run.Identity{Family: run.PRConflict, Workspace: "gordon", PR: 7, Retry: 3},
			[]string{"post-notice:github/sperano/gordon/pull/7:restart-gave-up:stopped<"}},
		{"review", run.Identity{Family: run.PRReview, Workspace: "gordon", Reviewer: "pr-challenger", PR: 7, Retry: 3}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &fakeStore{failed: []store.FailedRun{{Name: "stopped", InboxID: 3, Project: "gordon", Identity: c.id}}}
			task := stopTask("stopped", 3, map[string]string{"vikunja.spe.quebec/role": "pr-followup"}, "", now.Add(-time.Hour))
			r, _ := reaper(st, []kelos.Task{task}, ownsRestarts("gordon"))
			if err := r.Once(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(st.decisions) != 1 || st.decisions[0].mark != store.LimitGaveUp {
				t.Fatalf("decisions %+v", st.decisions)
			}
			if got := keys(st.decisions[0].fd.Actions); !slices.Equal(got, c.want) {
				t.Errorf("actions %v, want %v", got, c.want)
			}
		})
	}
}

func TestRestartNameTooLong(t *testing.T) {
	f := implRun("", 9, 5, 0)
	f.Identity.Project = strings.Repeat("p", run.MaxNameLength-len("vikunja-5--opus")-len("-retry1")+1)
	f.Name = f.Identity.Name()
	st := &fakeStore{failed: []store.FailedRun{f}}
	r, _ := reaper(st, []kelos.Task{stopTask(f.Name, 9, implLabels("5"), "", now.Add(-time.Hour))}, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.decisions) != 1 || st.decisions[0].mark != store.LimitNameTooLong || st.decisions[0].restart != nil {
		t.Errorf("decisions %+v", st.decisions)
	}
}

func TestNotALimitStop(t *testing.T) {
	plain := stopTask("vikunja-5-gordon-opus", 9, implLabels("5"), "", now)
	plain.Status.Results = map[string]string{kelos.ResultResponse: "tests failed"}
	foreign := stopTask("vikunja-6-gordon-opus", 99, implLabels("6"), "", now.Add(-time.Hour))
	st := &fakeStore{failed: []store.FailedRun{implRun(plain.Metadata.Name, 9, 5, 0), implRun(foreign.Metadata.Name, 10, 6, 0),
		implRun("vikunja-7-gordon-opus", 11, 7, 0)}}
	r, _ := reaper(st, []kelos.Task{plain, foreign}, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The Task not listed (gone) is left for the observer.
	var got []string
	for _, d := range st.decisions {
		got = append(got, d.name+"="+d.mark)
	}
	want := []string{plain.Metadata.Name + "=" + store.LimitNone, foreign.Metadata.Name + "=" + store.LimitNone}
	if !slices.Equal(got, want) {
		t.Errorf("decisions %v, want %v (another generation's Task is not this run's)", got, want)
	}
}

func TestPullRequestRestarts(t *testing.T) {
	fuName, reviewName, conflictName := "pr-followup-gordon-7-r9", "review-gordon-pr-challenger-7-01234567", "pr-conflict-gordon-7"
	fu := store.FailedRun{Name: fuName, InboxID: 3, Project: "gordon", Identity: run.Identity{Family: run.PRFollowUp,
		Workspace: "gordon", PR: 7, CommentKind: "r", CommentID: 9, HeadRef: "kelos/vikunja-5"}}
	rv := store.FailedRun{Name: reviewName, InboxID: 4, Project: "gordon", Identity: run.Identity{Family: run.PRReview,
		Workspace: "gordon", Reviewer: "pr-challenger", PR: 7, HeadSHA: "0123456789"}}
	reviewLabels := map[string]string{"vikunja.spe.quebec/role": "pr-review", "vikunja.spe.quebec/workspace": "gordon",
		"vikunja.spe.quebec/reviewer": "pr-challenger", "vikunja.spe.quebec/pr": "7"}
	cf := store.FailedRun{Name: conflictName, InboxID: 5, Project: "gordon", Identity: run.Identity{Family: run.PRConflict,
		Workspace: "gordon", PR: 8}}
	st := &fakeStore{failed: []store.FailedRun{fu, rv, cf}}
	tasks := []kelos.Task{stopTask(fuName, 3, map[string]string{"vikunja.spe.quebec/role": "pr-followup"}, "", now.Add(-time.Hour)),
		stopTask(reviewName, 4, reviewLabels, "", now.Add(-time.Hour)),
		stopTask(conflictName, 5, map[string]string{"vikunja.spe.quebec/role": "pr-conflict", "vikunja.spe.quebec/pr": "8"}, "", now.Add(-time.Hour))}
	r, _ := reaper(st, tasks, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, d := range st.decisions {
		got[d.name] = keys(d.fd.Actions)
	}
	fuSubmit := "submit-task:kelos/kelos-agents/" + fuName + "-retry1"
	cfRestart := cf.Identity
	cfRestart.Retry = 1
	cfSubmit := "submit-task:kelos/kelos-agents/" + cfRestart.Name()
	want := map[string][]string{
		conflictName: {cfSubmit + "<", "post-notice:github/sperano/gordon/pull/8:start:" + cfRestart.Name() + "<" + cfSubmit},
		fuName:       {fuSubmit + "<", "post-notice:github/sperano/gordon/pull/7:start:" + fuName + "-retry1<" + fuSubmit},
		// A review restart announces nothing and supersedes nothing.
		reviewName: {"submit-task:kelos/kelos-agents/" + reviewName + "-retry1<"},
	}
	if !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("actions %v, want %v", got, want)
	}
}

func TestPassErrors(t *testing.T) {
	name := "vikunja-5-gordon-opus"
	st := &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0)}, listErr: errors.New("store down")}
	r, _ := reaper(st, nil, ownsRestarts("gordon"))
	if err := r.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Errorf("store failure: %v", err)
	}
	st = &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0)}}
	r, _ = reaper(st, nil, ownsRestarts("gordon"))
	r.Kelos = &lister{err: errors.New("kelos down")}
	if err := r.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "kelos down") || len(st.decisions) != 0 {
		t.Errorf("list failure: %v, decisions %+v", err, st.decisions)
	}
	// A failed decision does not stop the next group's; one another
	// replica took is not counted.
	other := "vikunja-6-gordon-opus"
	taken := "vikunja-7-gordon-opus"
	st = &fakeStore{failed: []store.FailedRun{implRun(name, 9, 5, 0), implRun(other, 10, 6, 0), implRun(taken, 11, 7, 0)},
		refuse: map[string]error{name: errors.New("conflict")}, decided: map[string]bool{taken: true}}
	tasks := []kelos.Task{stopTask(name, 9, implLabels("5"), "", now.Add(-time.Hour)),
		stopTask(other, 10, implLabels("6"), "", now.Add(-time.Hour)), stopTask(taken, 11, implLabels("7"), "", now.Add(-time.Hour))}
	r, _ = reaper(st, tasks, ownsRestarts("gordon"))
	err := r.Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "limit stop "+name+": conflict") {
		t.Errorf("decision failure: %v", err)
	}
	if len(st.decisions) != 1 || st.decisions[0].name != other {
		t.Errorf("decisions %+v, want task 6's", st.decisions)
	}
	if n := testutil.ToFloat64(r.Metrics.LimitRestarts.WithLabelValues(store.LimitRestarted)); n != 1 {
		t.Errorf("restarted counted %v, want 1 (the taken one is not)", n)
	}
}

func TestNoFailedRunsListsNoTasks(t *testing.T) {
	st := &fakeStore{}
	r, _ := reaper(st, nil, ownsRestarts("gordon"))
	r.Kelos = &lister{err: errors.New("must not be called")}
	if err := r.Once(context.Background()); err != nil {
		t.Errorf("Once: %v", err)
	}
}
