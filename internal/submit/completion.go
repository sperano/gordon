package submit

import (
	"context"
	"maps"
	"time"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/store"
)

// CompletionStore records finished runs.
type CompletionStore interface {
	RecordCompletion(ctx context.Context, c store.Completion) error
}

// Completions is the observer's run-finished event hook for the usage
// accounting (Gordon 10): it records each run generation it sees finish
// once, with its family, project, Task uid, phase, completion time and
// the Task's outputs (the usage the agent image reports), the agent's
// answer left out: it is the agent's report, not usage, and it may quote
// the task.
type Completions struct {
	Store CompletionStore
}

// Finished records the run's completion.
func (c *Completions) Finished(ctx context.Context, r store.ObservedRun, t kelos.Task) error {
	rec := store.Completion{Name: r.Name, InboxID: r.InboxID, Family: r.Family, Project: r.Project,
		TaskUID: t.Metadata.UID, Phase: t.Phase()}
	if t.Status != nil {
		rec.Results = maps.Clone(t.Status.Results)
		delete(rec.Results, kelos.ResultResponse)
		if at, err := time.Parse(time.RFC3339, t.Status.CompletionTime); err == nil {
			rec.Completed = &at
		}
	}
	return c.Store.RecordCompletion(ctx, rec)
}
