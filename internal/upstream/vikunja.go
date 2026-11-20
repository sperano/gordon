package upstream

import (
	"context"
	"fmt"
	"net/url"

	"github.com/sperano/gordon/internal/event"
)

// Vikunja reads Vikunja tasks.
type Vikunja struct{ Client *Client }

type vikunjaTask struct {
	ID        int64 `json:"id"`
	ProjectID int64 `json:"project_id"`
	Done      bool  `json:"done"`
	Labels    []struct {
		Title string `json:"title"`
	} `json:"labels"`
	Assignees []struct {
		Username string `json:"username"`
	} `json:"assignees"`
	Buckets []struct {
		Title string `json:"title"`
	} `json:"buckets"`
}

// Task reads a task with its Kanban buckets (GET /tasks/{id}?expand=buckets,
// as the legacy enricher does). A task in no bucket has empty, not nil,
// Buckets: the fresh state always carries them.
func (v Vikunja) Task(ctx context.Context, id int64) (event.Task, error) {
	if id <= 0 {
		return event.Task{}, &Error{Provider: ProviderVikunja, Class: Invalid, Err: fmt.Errorf("task id %d", id)}
	}
	var raw vikunjaTask
	err := v.Client.get(ctx, fmt.Sprintf("/tasks/%d", id), url.Values{"expand": {"buckets"}}, &raw)
	if err != nil {
		return event.Task{}, err
	}
	if raw.ID != id {
		return event.Task{}, &Error{Provider: ProviderVikunja, Class: Invalid,
			Err: fmt.Errorf("asked for task %d, got %d", id, raw.ID)}
	}
	t := event.Task{ID: raw.ID, ProjectID: raw.ProjectID, Done: raw.Done, Buckets: []string{}}
	for _, l := range raw.Labels {
		t.Labels = append(t.Labels, l.Title)
	}
	for _, a := range raw.Assignees {
		t.Assignees = append(t.Assignees, a.Username)
	}
	for _, b := range raw.Buckets {
		t.Buckets = append(t.Buckets, b.Title)
	}
	return t, nil
}
