package submit

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

func agentTask(name, task, role, phase string) kelos.Task {
	labels := map[string]string{TaskLabel: task}
	if role != "" {
		labels[RoleLabel] = role
	}
	return kelos.Task{Metadata: kelos.Metadata{Name: name, Labels: labels}, Spec: kelos.Spec{Prompt: "x"},
		Status: &kelos.Status{Phase: phase}}
}

func cancelOf(task string, runs ...string) action.Action {
	a := action.New(action.CancelRun, action.Target("vikunja/task/"+task), "")
	for i, r := range runs {
		if i > 0 {
			a.Detail += ","
		}
		a.Detail += r
	}
	return a
}

// TestCancelDeletesTheLiveAgentTasks: a drag deletes the task's live
// implementation and discuss Tasks, a limit restart and a legacy
// spawner's included, never a follow-up, a finished Task or another task's.
func TestCancelDeletesTheLiveAgentTasks(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	for _, tk := range []kelos.Task{
		agentTask("vikunja-gordon-claude-opus-7", "7", "", kelos.PhaseRunning),
		agentTask("vikunja-gordon-claude-opus-7-retry1", "7", "", kelos.PhasePending),
		agentTask("vikunja-discuss-gordon-gpt-sol-7", "7", "", kelos.PhaseSucceeded),
		agentTask("vikunja-fu-gordon-7-c1", "7", "task-followup", kelos.PhaseRunning),
		agentTask("vikunja-fu-gordon-7-c2", "7", "", kelos.PhaseRunning),
		agentTask("vikunja-gordon-claude-opus-17", "17", "", kelos.PhaseRunning),
	} {
		e.kelos.Put(tk)
	}
	c := &Canceller{Store: e.store, Kelos: e.api}
	a := cancelOf("7", "vikunja-gordon-claude-opus-7")
	if applied, err := c.Recover(ctx, a); err != nil || applied {
		t.Fatalf("Recover before = %v, %v", applied, err)
	}
	if err := c.Execute(ctx, a); err != nil {
		t.Fatal(err)
	}
	want := []string{"vikunja-gordon-claude-opus-7", "vikunja-gordon-claude-opus-7-retry1"}
	if got := e.kelos.Deletes(); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	if applied, err := c.Recover(ctx, a); err != nil || !applied {
		t.Fatalf("Recover after = %v, %v", applied, err)
	}
	if err := c.Execute(ctx, cancelOf("7")); err != nil || len(e.kelos.Deletes()) != 2 {
		t.Fatalf("a second cancellation: %v, deletes %v", err, e.kelos.Deletes())
	}
}

// TestCancelledRunIsNeverCreated: a drag before the queued submission
// ran finishes the run; the submission then creates nothing.
func TestCancelledRunIsNeverCreated(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	c := &Canceller{Store: e.store, Kelos: e.api}
	if err := c.Execute(ctx, cancelOf("1624", name)); err != nil {
		t.Fatal(err)
	}
	e.perform(t)
	if creates := e.kelos.Creates(); len(creates) != 0 {
		t.Fatalf("created %d Tasks for a cancelled run", len(creates))
	}
	if st, _, msg := e.action(t, name); st != store.ActionDone || msg != "cancelled: run stopped" {
		t.Fatalf("submission %s %q, want done, cancelled", st, msg)
	}
	if st, _ := e.run(t, name); st != "finished" {
		t.Fatalf("run %s, want finished", st)
	}
}

// cancelOnAdmission is the store with a drag-cancel landing between the
// submission's first read of its run and its admission lock.
type cancelOnAdmission struct {
	*store.Store
	name string
}

func (c cancelOnAdmission) WithAdmission(ctx context.Context, fn func(context.Context) error) error {
	if _, err := c.CancelRuns(ctx, []string{c.name}); err != nil {
		return err
	}
	return c.Store.WithAdmission(ctx, fn)
}

// TestRunCancelledDuringAdmissionIsNotCreated: the submission reads its
// run again under the admission lock, so a cancellation after its first
// read creates nothing.
func TestRunCancelledDuringAdmissionIsNotCreated(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	e.exec.Runs = cancelOnAdmission{Store: e.store, name: name}
	if err := e.exec.Execute(ctx, action.New(action.SubmitTask, action.KelosTask("kelos-agents", name), "")); err != nil {
		t.Fatal(err)
	}
	if creates := e.kelos.Creates(); len(creates) != 0 {
		t.Fatalf("created %d Tasks after the cancellation", len(creates))
	}
	if !strings.Contains(e.logs.String(), "run stopped before its Task was created") {
		t.Fatalf("the in-lock re-read did not stop it: %s", e.logs.String())
	}
}

// TestUncheckedRunCancelledDuringAdmissionIsNotCreated: an implementation
// run with no limit and no branch also reads its run again under the lock.
func TestUncheckedRunCancelledDuringAdmissionIsNotCreated(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	storetest.Exec(t, e.dbURL, `UPDATE runs SET task = task #- '{spec,branch}'`)
	e.exec.Runs = cancelOnAdmission{Store: e.store, name: name}
	if err := e.exec.Execute(ctx, action.New(action.SubmitTask, action.KelosTask("kelos-agents", name), "")); err != nil {
		t.Fatal(err)
	}
	if creates := e.kelos.Creates(); len(creates) != 0 {
		t.Fatalf("created %d Tasks after the cancellation", len(creates))
	}
}

// submitOnAdmission is the store with another submitter creating the
// run's Task between the submission's first read and its admission lock.
type submitOnAdmission struct {
	*store.Store
	name    string
	inboxID int64
}

func (s submitOnAdmission) WithAdmission(ctx context.Context, fn func(context.Context) error) error {
	if err := s.RunSubmitted(ctx, s.name, s.inboxID, "uid-other", kelos.PhaseRunning); err != nil {
		return err
	}
	return s.Store.WithAdmission(ctx, fn)
}

// TestRunSubmittedDuringAdmissionIsNotLoggedStopped: a run another
// submitter created is skipped and logged as already submitted.
func TestRunSubmittedDuringAdmissionIsNotLoggedStopped(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	r, err := e.store.RunToSubmit(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	e.exec.Runs = submitOnAdmission{Store: e.store, name: name, inboxID: r.InboxID}
	if err := e.exec.Execute(ctx, action.New(action.SubmitTask, action.KelosTask("kelos-agents", name), "")); err != nil {
		t.Fatal(err)
	}
	logs := e.logs.String()
	if len(e.kelos.Creates()) != 0 || !strings.Contains(logs, "run already submitted by another submitter") ||
		strings.Contains(logs, "run stopped before its Task was created") {
		t.Fatalf("creates %d, logs: %s", len(e.kelos.Creates()), logs)
	}
}

// TestRecoverFinishesTheRuns: a retry recovered with no live agent Task
// still finishes the queued run its first attempt did not, so its
// submission creates nothing.
func TestRecoverFinishesTheRuns(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	c := &Canceller{Store: e.store, Kelos: e.api}
	if applied, err := c.Recover(ctx, cancelOf("1624", name)); err != nil || !applied {
		t.Fatalf("Recover = %v, %v; want applied", applied, err)
	}
	if st, _ := e.run(t, name); st != "finished" {
		t.Fatalf("run %s after Recover, want finished", st)
	}
	e.perform(t)
	if creates := e.kelos.Creates(); len(creates) != 0 {
		t.Fatalf("created %d Tasks for a recovered cancellation", len(creates))
	}
}

func TestCancelRefusesOtherTargets(t *testing.T) {
	c := &Canceller{}
	if err := c.Execute(ctx, action.New(action.CancelRun, action.Target("vikunja/comment/7"), "")); err == nil {
		t.Fatal("a comment target was accepted")
	}
}
