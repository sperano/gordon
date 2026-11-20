package submit

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/worker"
)

// Legacy Task labels a drag-cancel selects on (drag_cancel.py).
const (
	TaskLabel = "vikunja.spe.quebec/task"
	RoleLabel = "vikunja.spe.quebec/role"
)

// vikunjaTaskPrefix starts an action.VikunjaTask target.
const vikunjaTaskPrefix = "vikunja/task/"

// CancelStore is the part of the store a cancellation uses.
type CancelStore interface {
	CancelRuns(ctx context.Context, names []string) ([]string, error)
	WithAdmission(ctx context.Context, fn func(context.Context) error) error
}

// Deleter deletes Kelos Tasks.
type Deleter interface {
	TaskAPI
	Delete(ctx context.Context, name string) error
}

// Canceller stops a Vikunja task's implementation and discuss runs
// (action.CancelRun, drag_cancel.py). Holding the admission lock, so no
// submission is between its check and its create, it finishes Gordon's
// runs (a queued submission is never created) and deletes every live
// agent Task of the task, the limit reaper's restarts and a legacy
// spawner's included: Tasks labelled with the task and no role, named
// vikunja-<...>-<task>[-retry<n>] but not vikunja-fu-*.
type Canceller struct {
	Store CancelStore
	Kelos Deleter
}

func agentTaskName(task string) *regexp.Regexp {
	return regexp.MustCompile(`^vikunja-(?:[^f]|f[^u]|fu[^-]).*-` + regexp.QuoteMeta(task) + `(?:-retry[0-9]+)?$`)
}

func (c *Canceller) task(a action.Action) (string, error) {
	id, ok := strings.CutPrefix(string(a.Target), vikunjaTaskPrefix)
	if a.Kind != action.CancelRun || !ok || id == "" || strings.Contains(id, "/") {
		return "", worker.Permanent(fmt.Errorf("not a run cancellation: %s %s", a.Kind, a.Target))
	}
	return id, nil
}

// live lists the task's agent Tasks that have not finished.
func (c *Canceller) live(ctx context.Context, task string) ([]string, error) {
	tasks, err := c.Kelos.List(ctx, fmt.Sprintf("%s=%s,!%s", TaskLabel, task, RoleLabel))
	if err != nil {
		return nil, err
	}
	re := agentTaskName(task)
	var out []string
	for _, t := range tasks {
		if re.MatchString(t.Metadata.Name) && !t.Finished() {
			out = append(out, t.Metadata.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Execute cancels the runs the action names and deletes the live Tasks.
func (c *Canceller) Execute(ctx context.Context, a action.Action) error {
	task, err := c.task(a)
	if err != nil {
		return err
	}
	return c.Store.WithAdmission(ctx, func(ctx context.Context) error {
		if _, err := c.Store.CancelRuns(ctx, strings.Split(a.Detail, ",")); err != nil {
			return err
		}
		names, err := c.live(ctx, task)
		if err != nil {
			return refusedPermanent(err)
		}
		for _, n := range names {
			if err := c.Kelos.Delete(ctx, n); err != nil {
				return refusedPermanent(err)
			}
		}
		return nil
	})
}

// Recover finishes the runs the action names, as Execute does, then
// reports the cancel applied once no live agent Task is left. Both hold
// the admission lock: a first attempt that failed before its store
// cancellation committed would otherwise be recovered with its queued
// submission still to create a Task.
func (c *Canceller) Recover(ctx context.Context, a action.Action) (bool, error) {
	task, err := c.task(a)
	if err != nil {
		return false, err
	}
	applied := false
	err = c.Store.WithAdmission(ctx, func(ctx context.Context) error {
		if _, err := c.Store.CancelRuns(ctx, strings.Split(a.Detail, ",")); err != nil {
			return err
		}
		names, err := c.live(ctx, task)
		applied = err == nil && len(names) == 0
		return err
	})
	return applied, err
}

// refusedPermanent dead-letters what the Kelos API refuses (RBAC): a
// retry cannot help.
func refusedPermanent(err error) error {
	if kelos.IsRefused(err) {
		return worker.Permanent(err)
	}
	return err
}
