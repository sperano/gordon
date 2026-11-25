// Package costs publishes each finished run's completion cost notice on
// its Vikunja task (Gordon 12): the run's estimated API-equivalent cost,
// the task's total to date, the run's categories, coverage, the price
// table and its assumptions, read from Sillage's cost API.
//
// The observer's Hook records every finished run generation of a
// Vikunja task once, as waiting. The Publisher pass then reads Sillage:
// a run whose estimate is not final yet (its usage still arriving, or
// Sillage not having read the run) waits, up to a deadline after which
// its notice is posted marked not final; Sillage unavailable is a
// retry with backoff, never an estimate of zero. A notice's text is
// stored with a revision in the transaction that queues its
// publish-cost action, so a retried or recovered write publishes the same
// text, found again on Vikunja by its revision marker. The newest
// notice of a task stays live: when its run's estimate or the task's
// total changes (a review, a fix, late usage), its comment is edited in
// place to a new revision, never posted again; an older one is frozen.
package costs

import (
	"context"
	"slices"
	"time"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

// Families are the runs that get a completion cost notice: those that
// work on a Vikunja task. A PR run's cost (reviews, PR fixes, conflict
// fixes) shows in the task total of the task's live notice.
var Families = []run.Family{run.VikunjaImplement, run.VikunjaDiscuss, run.VikunjaFollowUp,
	run.VikunjaFollowUpAnswer, run.VikunjaFollowUpDiscuss}

// HookStore records finished runs' cost notices.
type HookStore interface {
	RecordCostRun(ctx context.Context, c store.CostRun) error
}

// Hook is the observer's run-finished hook: it records the cost notice
// of each finished run of Families on a Vikunja task: failed runs and
// runs a limit stopped included
// (they used tokens too), each generation once.
type Hook struct {
	Store HookStore
	Now   func() time.Time
}

// Finished records the run's cost notice as waiting.
func (h *Hook) Finished(ctx context.Context, r store.ObservedRun, t kelos.Task) error {
	if r.TaskID <= 0 || !slices.Contains(Families, r.Family) {
		return nil
	}
	finished := h.now()
	if t.Status != nil {
		if at, err := time.Parse(time.RFC3339, t.Status.CompletionTime); err == nil {
			finished = at
		}
	}
	return h.Store.RecordCostRun(ctx, store.CostRun{Name: r.Name, InboxID: r.InboxID, Family: r.Family,
		Project: r.Project, TaskID: r.TaskID, TaskUID: t.Metadata.UID, Phase: t.Phase(), Finished: finished})
}

func (h *Hook) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
