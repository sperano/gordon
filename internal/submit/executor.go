// Package submit performs Gordon's Kelos writes and follows their
// results: the submit-task executor of the outbox, the observer that
// keeps each run's state current from Kelos, and the shadow comparison of
// the Tasks shadow decisions would have submitted with the legacy ones.
package submit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

// TaskAPI is the part of the Kelos Task API Gordon uses (kelos.Client).
type TaskAPI interface {
	Create(ctx context.Context, t kelos.Task) (kelos.Task, error)
	Get(ctx context.Context, name string) (kelos.Task, json.RawMessage, error)
	List(ctx context.Context, selector string) ([]kelos.Task, error)
}

// Runs is the part of the store the executor uses.
type Runs interface {
	RunToSubmit(ctx context.Context, name string) (store.SubmitRun, error)
	RunSubmitted(ctx context.Context, name string, inboxID int64, uid, phase string) error
	ObserveRun(ctx context.Context, name string, inboxID int64, exists bool, phase string) error
	WithAdmission(ctx context.Context, fn func(context.Context) error) error
}

// Submission results, the kelos_submissions_total result label.
const (
	ResultCreated   = "created"
	ResultRecovered = "recovered"
	ResultCollision = "collision"
	ResultLimit     = "deferred-limit"
	ResultBranch    = "deferred-branch"
	ResultRefused   = "refused"
	ResultAmbiguous = "ambiguous"
	ResultError     = "error"
)

// Results lists every submission result.
var Results = []string{ResultCreated, ResultRecovered, ResultCollision, ResultLimit, ResultBranch,
	ResultRefused, ResultAmbiguous, ResultError}

// Deferral limit labels (deferred_total{queue="actions"}).
const (
	LimitVariant = "variant"
	LimitBranch  = "branch"
)

// ErrNoTask is a submit action whose run has no built Task.
var ErrNoTask = errors.New("run has no built Task")

// Executor submits runs' Tasks (action.SubmitTask). The Task is the one
// built when the decision was recorded (runs.task), so a retry or a
// recovery submits exactly what was decided. Before creating it, a Task
// of that name is looked for: this generation's (its inbox annotation)
// means the create already landed (a lost response, a crash), another
// submitter's (a legacy spawner during the migration) means the run is
// taken; neither is created twice. A run past its variant's concurrency
// limit, or whose branch another unfinished Task holds in the same
// Workspace (Kelos's branch lock), stays queued instead of waiting in
// Kelos, where a waiting Task would hold a limit slot.
type Executor struct {
	Runs   Runs
	Kelos  TaskAPI
	Limits dispatch.Limits
	// BusyDelay is how long a deferred submission waits.
	BusyDelay time.Duration
	Metrics   *metrics.Metrics
	Log       *slog.Logger
}

// taskName is the Task name of a submit action's target,
// kelos/<namespace>/<name>.
func taskName(a action.Action) (string, error) {
	prefix := string(action.KelosTask(route.KelosNamespace, ""))
	name, ok := strings.CutPrefix(string(a.Target), prefix)
	if a.Kind != action.SubmitTask || !ok || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("not a submission in %s: %s %s", route.KelosNamespace, a.Kind, a.Target)
	}
	return name, nil
}

// Execute submits the run's Task.
func (e *Executor) Execute(ctx context.Context, a action.Action) error {
	r, err := e.run(ctx, a)
	if err != nil || r.Submitted() {
		return err
	}
	if found, err := e.existing(ctx, r); found || err != nil {
		return err
	}
	if !e.checked(r) {
		created, err := e.Kelos.Create(ctx, *r.Task)
		return e.created(ctx, r, created, err)
	}
	// The limit and branch check and the create that follows hold the
	// database-wide admission lock together, so no two submissions, in
	// any replica, both pass a check one of them fills.
	var created kelos.Task
	var createErr error
	if err := e.Runs.WithAdmission(ctx, func(ctx context.Context) error {
		if err := e.admit(ctx, r); err != nil {
			return err
		}
		created, createErr = e.Kelos.Create(ctx, *r.Task)
		return nil
	}); err != nil {
		return err
	}
	return e.created(ctx, r, created, createErr)
}

// checked reports a submission admission checks: its variant has a limit
// or its Task a branch. Any other is created without the lock.
func (e *Executor) checked(r store.SubmitRun) bool {
	return e.Limits.For(r.Variant) > 0 || r.Task.Spec.Branch != ""
}

// created records the outcome of the run's create.
func (e *Executor) created(ctx context.Context, r store.SubmitRun, created kelos.Task, err error) error {
	switch {
	case err == nil:
		e.count(ResultCreated)
		e.Log.Info("task created", "task", r.Name, "uid", created.Metadata.UID)
		return e.Runs.RunSubmitted(ctx, r.Name, r.InboxID, created.Metadata.UID, created.Phase())
	case kelos.IsAlreadyExists(err):
		// Created meanwhile, by this run's earlier attempt or by another
		// submitter: classify the Task now there.
		if found, gerr := e.existing(ctx, r); found || gerr != nil {
			return gerr
		}
		return err
	case kelos.IsAmbiguous(err):
		e.count(ResultAmbiguous)
		return worker.Ambiguous(err)
	case kelos.IsRefused(err):
		e.count(ResultRefused)
		return worker.Permanent(err)
	default:
		e.count(ResultError)
		return err
	}
}

// Recover reports whether the run's Task already exists: created by an
// earlier attempt of this generation, or taken by another submitter.
func (e *Executor) Recover(ctx context.Context, a action.Action) (bool, error) {
	r, err := e.run(ctx, a)
	if err != nil || r.Submitted() {
		return err == nil, err
	}
	return e.existing(ctx, r)
}

// run reads the action's run; a run without a built Task cannot be
// submitted, ever.
func (e *Executor) run(ctx context.Context, a action.Action) (store.SubmitRun, error) {
	name, err := taskName(a)
	if err != nil {
		return store.SubmitRun{}, worker.Permanent(err)
	}
	r, err := e.Runs.RunToSubmit(ctx, name)
	switch {
	case errors.Is(err, store.ErrNoRun):
		return r, worker.Permanent(fmt.Errorf("%s: %w", name, err))
	case err != nil:
		return r, err
	case r.Task == nil:
		return r, worker.Permanent(fmt.Errorf("%s: %w", name, ErrNoTask))
	}
	return r, nil
}

// existing looks for a Task of the run's name and records it: found is
// true when there is one, whoever created it.
func (e *Executor) existing(ctx context.Context, r store.SubmitRun) (bool, error) {
	t, _, err := e.Kelos.Get(ctx, r.Name)
	switch {
	case kelos.IsNotFound(err):
		return false, nil
	case kelos.IsRefused(err):
		// The read itself is forbidden (RBAC, a bad token): retrying the
		// submission cannot help.
		e.count(ResultRefused)
		return false, worker.Permanent(err)
	case err != nil:
		return false, err
	}
	if t.Metadata.Annotations[dispatch.InboxAnnotation] == strconv.FormatInt(r.InboxID, 10) {
		e.count(ResultRecovered)
		e.Log.Info("task already created by this run", "task", r.Name, "uid", t.Metadata.UID)
		return true, e.Runs.RunSubmitted(ctx, r.Name, r.InboxID, t.Metadata.UID, t.Phase())
	}
	// Another submitter's Task holds the name: the run is taken, as the
	// legacy name deduplication made it; the observer follows that Task.
	e.count(ResultCollision)
	e.Log.Warn("task name taken by another submitter; not submitting", "task", r.Name,
		"uid", t.Metadata.UID, "phase", t.Phase())
	return true, e.Runs.ObserveRun(ctx, r.Name, r.InboxID, true, t.Phase())
}

// admit defers a submission Kelos cannot run yet: its variant's limit is
// full, or its branch is held in its Workspace.
func (e *Executor) admit(ctx context.Context, r store.SubmitRun) error {
	limit := e.Limits.For(r.Variant)
	branch, ws := r.Task.Spec.Branch, r.Task.WorkspaceName()
	tasks, err := e.Kelos.List(ctx, "")
	if err != nil {
		return err
	}
	running := 0
	for _, t := range tasks {
		if t.Metadata.Name == r.Name || t.Finished() {
			continue
		}
		if branch != "" && t.Spec.Branch == branch && t.WorkspaceName() == ws {
			e.count(ResultBranch)
			return worker.Deferred(fmt.Errorf("branch %s held by %s", branch, t.Metadata.Name), LimitBranch, e.BusyDelay)
		}
		if t.Metadata.Labels[dispatch.VariantLabel] == r.Variant && t.Phase() != kelos.PhaseWaiting {
			running++
		}
	}
	if limit > 0 && running >= limit {
		e.count(ResultLimit)
		return worker.Deferred(fmt.Errorf("variant %s runs %d of %d", r.Variant, running, limit), LimitVariant, e.BusyDelay)
	}
	return nil
}

func (e *Executor) count(result string) {
	e.Metrics.Submissions.WithLabelValues(result).Inc()
}
