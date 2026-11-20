package submit

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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
	storetest.Exec(t, e.dbURL, `UPDATE runs SET task = task #- '{spec,branch}'`)
	e.perform(t)
	if st, _, _ := e.action(t, name); st != store.ActionDone || runs.admissions != 0 || len(e.kelos.Creates()) != 1 {
		t.Fatalf("no limit, no branch: %s with %d admissions, %d creates; want created unlocked", st,
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
