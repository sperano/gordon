package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

// reactionEmoji maps the router's reaction names to the emoji Vikunja
// stores (webhook_enricher.FOLLOWUP_REACTION, the reconciler's ⏳).
var reactionEmoji = map[string]string{"eyes": "👀", "hourglass": "⏳"}

// Runs reads the runs a notice names, and whether a Vikunja task has
// another run still held back from Kelos.
type Runs interface {
	RunInfo(ctx context.Context, name string) (store.RunInfo, error)
	TaskHeldBesides(ctx context.Context, task int64, name string, families []run.Family) (bool, error)
}

// Locker serialises the check and the post of notices on one target, so
// two decisions of one task never post the same notice twice.
type Locker interface {
	WithTargetLock(ctx context.Context, target string, fn func(context.Context) error) error
}

// TaskMessages reads a Kelos Task's status message ("" when it is gone);
// an error is a read that failed and is retried.
type TaskMessages func(ctx context.Context, name string) (string, error)

// TaskLimits reads the limit that stopped a Kelos Task ("" when it is
// gone or tells none); an error is a read that failed and is retried.
type TaskLimits func(ctx context.Context, name string) (string, error)

// Executor performs the Vikunja actions (the outbox's Executor).
type Executor struct {
	W      *Writer
	Policy profile.Policy
	// Agents are the agent variants' Vikunja users.
	Agents []string
	// AutoAssignUsers caches each opted-in project's configured user's id.
	AutoAssignUsers map[int64]int64
	Runs            Runs
	Lock            Locker
	Links           Links
	Messages        TaskMessages
	// Limits reads a stopped Task's limit for its restart notices;
	// MaxRestarts is the restarts a chain may take, which they name.
	Limits      TaskLimits
	MaxRestarts int
	// Namespace is the Kelos namespace a failure notice's retry names.
	Namespace string
	// Ntfy sends ready heads (nil: a notify action fails).
	Ntfy *Ntfy
	// Costs holds the cost notices' texts (nil: a publish-cost action
	// fails).
	Costs CostBodies
}

// ErrNotVikunja is an action this executor does not write.
var ErrNotVikunja = errors.New("not a Vikunja action")

// target parses vikunja/task/<id> or vikunja/comment/<id>.
func target(a action.Action) (kind string, id int64, err error) {
	parts := strings.Split(string(a.Target), "/")
	if len(parts) != 3 || parts[0] != "vikunja" {
		return "", 0, worker.Permanent(fmt.Errorf("%w: %s", ErrNotVikunja, a.Target))
	}
	id, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, worker.Permanent(fmt.Errorf("%w: %s", ErrNotVikunja, a.Target))
	}
	return parts[1], id, nil
}

func taskTarget(a action.Action) (int64, error) {
	kind, id, err := target(a)
	if err == nil && kind != "task" {
		err = worker.Permanent(fmt.Errorf("%s needs a task target: %s", a.Kind, a.Target))
	}
	return id, err
}

// handler is one kind: applied checks whether the write is already
// there (nothing left to do), write makes it.
type handler struct {
	applied func(ctx context.Context, a action.Action) (bool, error)
	write   func(ctx context.Context, a action.Action) error
}

func (e *Executor) handler(k action.Kind) (handler, bool) {
	switch k {
	case action.AddLabel:
		return handler{e.labelApplied, e.addLabel}, true
	case action.RemoveLabel:
		return handler{e.unlabelApplied, e.unlabel}, true
	case action.MoveBucket:
		return handler{e.moveApplied, e.move}, true
	case action.Assign:
		return handler{e.assignApplied, e.assign}, true
	case action.AssignIfUnassigned:
		return handler{e.assignIfUnassignedApplied, e.assignIfUnassigned}, true
	case action.SyncLabel:
		return handler{e.syncApplied, e.sync}, true
	case action.React:
		return handler{e.reactApplied, e.react}, true
	case action.MarkDone:
		return handler{e.doneApplied, e.markDone}, true
	case action.PostNotice:
		return handler{e.noticeApplied, e.postNotice}, true // checks again under the target lock
	case action.Notify:
		return handler{e.notifyApplied, e.notify}, true // checks again under the target lock
	case action.PublishCost:
		return handler{e.costApplied, e.publishCost}, true // checks again under the target lock
	}
	return handler{}, false
}

// Execute makes the write unless it is already there.
func (e *Executor) Execute(ctx context.Context, a action.Action) error {
	h, ok := e.handler(a.Kind)
	if !ok {
		return worker.Permanent(fmt.Errorf("no Vikunja writer for %s", a.Kind))
	}
	return e.once(ctx, h, a)
}

func (e *Executor) once(ctx context.Context, h handler, a action.Action) error {
	done, err := h.applied(ctx, a)
	if err != nil || done {
		return err
	}
	return h.write(ctx, a)
}

// Recover reports whether the write is there.
func (e *Executor) Recover(ctx context.Context, a action.Action) (bool, error) {
	h, ok := e.handler(a.Kind)
	if !ok {
		return false, nil
	}
	return h.applied(ctx, a)
}

func hasLabel(t Task, title string) bool {
	return slices.ContainsFunc(t.Labels, func(l Label) bool { return l.Title == title })
}

func inBucket(t Task, title string) bool {
	return slices.ContainsFunc(t.Buckets, func(b Bucket) bool { return b.Title == title })
}

// skip reports a conditional label or move write the task no longer
// wants: a claim on a card dragged back to the backlog or done since, a
// label for an undone task on one done since, a removal from a done task
// on one reopened since.
func (e *Executor) skip(t Task, a action.Action) bool {
	switch a.Detail {
	case route.DetailClaim:
		p := e.Policy
		dragged := p.BacklogBucket != "" && inBucket(t, p.BacklogBucket) && !inBucket(t, p.DoingBucket)
		return t.Done || dragged
	case route.DetailUnlessDone:
		return t.Done
	case route.DetailIfDone:
		return !t.Done
	}
	return false
}

func (e *Executor) labelApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := taskTarget(a)
	if err != nil {
		return false, err
	}
	t, err := e.W.Task(ctx, id)
	if err != nil {
		return false, err
	}
	if hasLabel(t, a.Value) || e.skip(t, a) {
		return true, nil
	}
	if name := route.HeldRun(a.Detail); name != "" {
		held, err := e.held(ctx, name)
		return !held || t.Done, err
	}
	return false, nil
}

// held reports a run still waiting for its Task: a held run's queued
// label and notice are not written once it started or stopped.
func (e *Executor) held(ctx context.Context, name string) (bool, error) {
	r, err := e.Runs.RunInfo(ctx, name)
	if errors.Is(err, store.ErrUnknownRun) {
		return false, nil
	}
	return err == nil && r.State == store.RunSubmitting, err
}

func (e *Executor) addLabel(ctx context.Context, a action.Action) error {
	id, _ := taskTarget(a)
	label, ok, err := e.W.LabelID(ctx, a.Value)
	switch {
	case err != nil:
		return err
	case !ok:
		return worker.Permanent(fmt.Errorf("no label titled %q visible to Gordon", a.Value))
	}
	if err := e.W.AddLabel(ctx, id, label); err != nil {
		return err
	}
	return e.unlessStillHeld(ctx, a)
}

func (e *Executor) unlabelApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := taskTarget(a)
	if err != nil {
		return false, err
	}
	t, err := e.W.Task(ctx, id)
	if err != nil {
		return false, err
	}
	if !hasLabel(t, a.Value) || e.skip(t, a) {
		return true, nil
	}
	if name := route.UnheldRun(a.Detail); name != "" {
		// Another run of the task still held keeps the label: its own
		// removal takes it off.
		return e.Runs.TaskHeldBesides(ctx, id, name, run.TaskFamilies)
	}
	return false, nil
}

// removeLabel removes every label of that title, by the ids on the task
// (Gordon may not see a label by title it did not create).
func (e *Executor) removeLabel(ctx context.Context, a action.Action) error {
	_, err := e.removeLabels(ctx, a)
	return err
}

// removeLabels removes the labels and returns the ids it removed.
func (e *Executor) removeLabels(ctx context.Context, a action.Action) ([]int64, error) {
	id, _ := taskTarget(a)
	t, err := e.W.Task(ctx, id)
	if err != nil {
		return nil, err
	}
	var removed []int64
	for _, l := range t.Labels {
		if l.Title == a.Value {
			if err := e.W.RemoveLabel(ctx, id, l.ID); err != nil {
				return removed, err
			}
			removed = append(removed, l.ID)
		}
	}
	return removed, nil
}

func (e *Executor) doneApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := taskTarget(a)
	if err != nil {
		return false, err
	}
	t, err := e.W.Task(ctx, id)
	return err == nil && t.Done, err
}

func (e *Executor) markDone(ctx context.Context, a action.Action) error {
	id, _ := taskTarget(a)
	return e.W.MarkDone(ctx, id)
}
