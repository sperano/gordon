package submit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

// countingRuns is the store, counting the admissions it holds.
type countingRuns struct {
	Runs
	admissions int
}

func (c *countingRuns) WithAdmission(ctx context.Context, fn func(context.Context) error) error {
	c.admissions++
	return c.Runs.WithAdmission(ctx, fn)
}

func TestUncheckedSubmissionTakesNoAdmission(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	runs := &countingRuns{Runs: e.store}
	e.exec.Runs = runs
	name := e.implement(t, implementID, "claude-opus")
	// A follow-up is no agent run, so an unchecked one takes no admission.
	storetest.Exec(t, e.dbURL, `UPDATE runs SET task = task #- '{spec,branch}', family = 'vikunja-followup'`)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || runs.admissions != 0 || len(e.kelos.Creates()) != 1 {
		t.Fatalf("no limit, no branch: %s with %d admissions, %d creates; want created unlocked", st,
			runs.admissions, len(e.kelos.Creates()))
	}
}

// TestUncheckedAgentRunTakesTheAdmission: an implementation run with no
// limit and no branch is still created under the lock a drag-cancel holds.
func TestUncheckedAgentRunTakesTheAdmission(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	runs := &countingRuns{Runs: e.store}
	e.exec.Runs = runs
	name := e.implement(t, implementID, "claude-opus")
	storetest.Exec(t, e.dbURL, `UPDATE runs SET task = task #- '{spec,branch}'`)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || runs.admissions != 1 || len(e.kelos.Creates()) != 1 {
		t.Fatalf("an unchecked implementation: %s with %d admissions, %d creates; want one of each", st,
			runs.admissions, len(e.kelos.Creates()))
	}
}

func TestCheckedSubmissionTakesTheAdmission(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	runs := &countingRuns{Runs: e.store}
	e.exec.Runs = runs
	name := e.implement(t, implementID, "claude-opus") // on its task's branch
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || runs.admissions != 1 {
		t.Fatalf("a branch submission: %s with %d admissions, want one", st, runs.admissions)
	}
}

// refusingGets refuses every Get, as an API server without the RBAC.
type refusingGets struct{ TaskAPI }

func (refusingGets) Get(context.Context, string) (kelos.Task, json.RawMessage, error) {
	return kelos.Task{}, nil, &kelos.Error{Op: "get", Status: http.StatusForbidden, Sent: true}
}

func TestRefusedReadDeadLettersTheSubmission(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	e.exec.Kelos = refusingGets{e.api}
	name := e.implement(t, implementID, "claude-opus")
	e.perform(t)
	st, attempts, _ := e.action(t, name)
	if st != store.ActionFailed || attempts != 1 || e.submissions(ResultRefused) != 1 || len(e.kelos.Creates()) != 0 {
		t.Fatalf("a refused read: %s after %d attempts, %d creates; want dead-lettered at once", st, attempts,
			len(e.kelos.Creates()))
	}
}

// refusingLists refuses every List, as an API server without the RBAC.
type refusingLists struct{ TaskAPI }

func (refusingLists) List(context.Context, string) ([]kelos.Task, error) {
	return nil, &kelos.Error{Op: "list", Status: http.StatusForbidden, Sent: true}
}

func TestRefusedAdmissionListDeadLettersTheSubmission(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	e.exec.Kelos = refusingLists{e.api}
	name := e.implement(t, implementID, "claude-opus") // on its task's branch: admission lists
	e.perform(t)
	st, attempts, _ := e.action(t, name)
	if st != store.ActionFailed || attempts != 1 || e.submissions(ResultRefused) != 1 || len(e.kelos.Creates()) != 0 {
		t.Fatalf("a refused admission list: %s after %d attempts, %d creates; want dead-lettered at once", st,
			attempts, len(e.kelos.Creates()))
	}
}

// busyAdmission never gets the lock: its wait runs out, as one queued
// behind other submissions past the attempt deadline.
type busyAdmission struct{ Runs }

func (busyAdmission) WithAdmission(context.Context, func(context.Context) error) error {
	return fmt.Errorf("admission lock: %w", context.DeadlineExceeded)
}

func TestAdmissionWaitTimeoutDefersUncounted(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	e.exec.Runs = busyAdmission{e.store}
	name := e.implement(t, implementID, "claude-opus") // on its task's branch
	e.perform(t)
	st, attempts, msg := e.action(t, name)
	if st != store.ActionPending || attempts != 0 || msg != "waiting: admission lock: context deadline exceeded" ||
		len(e.kelos.Creates()) != 0 || e.submissions(ResultAdmission) != 1 ||
		testutil.ToFloat64(e.metrics.Deferred.WithLabelValues(store.QueueActions, LimitAdmission)) != 1 {
		t.Fatalf("lock wait ran out: %s, %d attempts, %q, %d creates; want deferred, uncounted", st, attempts, msg,
			len(e.kelos.Creates()))
	}
}

// timingOutLists has every List cut by its deadline, as an admission
// list that ran out after the lock was taken.
type timingOutLists struct{ TaskAPI }

func (timingOutLists) List(context.Context, string) ([]kelos.Task, error) {
	return nil, &kelos.Error{Op: "list", Sent: true, Err: context.DeadlineExceeded}
}

// TestDeadlineInsideTheLockIsNotDeferred: a deadline after the lock was
// taken is a counted attempt, not a deferral; only a lock wait defers.
func TestDeadlineInsideTheLockIsNotDeferred(t *testing.T) {
	e := newEnv(t, false, dispatch.Limits{})
	e.exec.Kelos = timingOutLists{e.api}
	name := e.implement(t, implementID, "claude-opus") // on its task's branch: admission lists
	e.perform(t)
	st, attempts, msg := e.action(t, name)
	if st != store.ActionPending || attempts != 1 || msg != "kelos list: context deadline exceeded" ||
		len(e.kelos.Creates()) != 0 || e.submissions(ResultAdmission) != 0 ||
		testutil.ToFloat64(e.metrics.Deferred.WithLabelValues(store.QueueActions, LimitAdmission)) != 0 {
		t.Fatalf("a list timing out under the lock: %s, %d attempts, %q, %d creates; want a counted retry",
			st, attempts, msg, len(e.kelos.Creates()))
	}
}
