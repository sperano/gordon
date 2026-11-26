package submit

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/store"
)

// Observed run states, the run_observations_total state label.
const (
	ObservedActive   = "active"
	ObservedFinished = "finished"
	ObservedGone     = "gone"
)

// ObservedStates lists every observation label.
var ObservedStates = []string{ObservedActive, ObservedFinished, ObservedGone}

// DefaultObserveBatch is how many runs one observation pass reads.
const DefaultObserveBatch = 500

// ObserverStore is the part of the store the observer uses.
type ObserverStore interface {
	ObservedRuns(ctx context.Context, limit int) ([]store.ObservedRun, error)
	ObserveRun(ctx context.Context, name string, inboxID int64, exists bool, phase string) error
}

// Observer keeps runs current from Kelos: a run is active while its Task
// has not finished, finished once it has, and its Task gone once Kelos
// cleaned it up (ttlSecondsAfterFinished) or someone deleted it, which is
// what lets the owner's next request run under the same name again. A run
// whose submission was dead-lettered is finished too, so it does not
// block that name for ever.
type Observer struct {
	Store ObserverStore
	Kelos TaskAPI
	// Finished are told of each run this observer sees finish (its Task
	// Succeeded or Failed), in order: the run's completion record, the
	// lifecycle's failure notices.
	Finished []FinishHook
	// Unsubmitted is told of each run whose submission ended without a
	// Task (dead-lettered), before the run is recorded finished (nil:
	// none): the lifecycle takes a held run's queued label off.
	Unsubmitted UnsubmittedHook
	Batch       int
	Metrics     *metrics.Metrics
	Log         *slog.Logger
}

// FinishHook handles a run whose Task t finished.
type FinishHook interface {
	Finished(ctx context.Context, r store.ObservedRun, t kelos.Task) error
}

// UnsubmittedHook handles a run finished before its Task was created.
type UnsubmittedHook interface {
	Unsubmitted(ctx context.Context, r store.ObservedRun) error
}

// Once observes one batch of runs against one Task list.
func (o *Observer) Once(ctx context.Context) error {
	batch := o.Batch
	if batch <= 0 {
		batch = DefaultObserveBatch
	}
	runs, err := o.Store.ObservedRuns(ctx, batch)
	if err != nil || len(runs) == 0 {
		return err
	}
	// Runs are read before the list: a Task created after the list
	// belongs to a run read as submitting, which a missing Task leaves
	// alone while its submission is still queued.
	tasks, err := o.Kelos.List(ctx, "")
	if err != nil {
		return err
	}
	byName := make(map[string]kelos.Task, len(tasks))
	for _, t := range tasks {
		byName[t.Metadata.Name] = t
	}
	for _, r := range runs {
		if err := o.observe(ctx, r, byName); err != nil {
			return err
		}
	}
	return nil
}

func (o *Observer) observe(ctx context.Context, r store.ObservedRun, tasks map[string]kelos.Task) error {
	t, present := tasks[r.Name]
	switch {
	case present:
		state := ObservedActive
		if t.Finished() {
			state = ObservedFinished
		}
		if r.Exists && r.Phase == t.Phase() && r.State != store.RunSubmitting {
			return nil
		}
		return o.record(ctx, r, &t, state)
	case r.State == store.RunSubmitting && pendingSubmit(r.SubmitState):
		return nil // not created yet
	default:
		if r.State == store.RunSubmitting && o.Unsubmitted != nil {
			// Written first, as the finish hooks: an observation not
			// recorded after it is observed, and told, again.
			if err := o.Unsubmitted.Unsubmitted(ctx, r); err != nil {
				return fmt.Errorf("lifecycle of unsubmitted run %s: %w", r.Name, err)
			}
		}
		return o.record(ctx, r, nil, ObservedGone)
	}
}

// pendingSubmit reports a submission that may still create the Task.
func pendingSubmit(state string) bool {
	switch state {
	case store.ActionPending, store.ActionRunning, store.ActionAmbiguous:
		return true
	}
	return false
}

// record records the run's Task t as observed (nil: gone). The hooks
// write idempotently, so they go first: an observation not recorded after
// them is observed again, and their writes are not made twice; one
// recorded before a failed hook would be lost.
func (o *Observer) record(ctx context.Context, r store.ObservedRun, t *kelos.Task, state string) error {
	if t != nil && state == ObservedFinished && r.State != store.RunFinished {
		for _, h := range o.Finished {
			if err := h.Finished(ctx, r, *t); err != nil {
				return fmt.Errorf("lifecycle of finished run %s: %w", r.Name, err)
			}
		}
	}
	exists, phase := t != nil, ""
	if exists {
		phase = t.Phase()
	}
	if err := o.Store.ObserveRun(ctx, r.Name, r.InboxID, exists, phase); err != nil {
		return err
	}
	o.Metrics.RunObservations.WithLabelValues(state).Inc()
	o.Log.Debug("run observed", "task", r.Name, "state", state, "phase", phase)
	return nil
}
