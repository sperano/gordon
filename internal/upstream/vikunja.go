package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/sperano/gordon/internal/event"
)

// Vikunja reads Vikunja tasks.
type Vikunja struct{ Client *Client }

type vikunjaTask struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Done        bool   `json:"done"`
	Labels      []struct {
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
	err := v.Client.get(ctx, fmt.Sprintf("/tasks/%d", id), url.Values{"expand": {"buckets"}}, &raw, func() error {
		if raw.ID != id {
			return fmt.Errorf("asked for task %d, got %d", id, raw.ID)
		}
		return nil
	})
	if err != nil {
		return event.Task{}, err
	}
	t := event.Task{ID: raw.ID, ProjectID: raw.ProjectID, Title: raw.Title, Description: raw.Description,
		Done: raw.Done, Buckets: []string{}}
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

// Comments reads a task's comments as the API returns them (GET
// /tasks/{id}/comments, a JSON array): the raw JSON a prompt quotes, as
// the legacy spawners' contextSources did.
func (v Vikunja) Comments(ctx context.Context, id int64) (json.RawMessage, error) {
	if id <= 0 {
		return nil, &Error{Provider: ProviderVikunja, Class: Invalid, Err: fmt.Errorf("task id %d", id)}
	}
	var raw json.RawMessage
	err := v.Client.get(ctx, fmt.Sprintf("/tasks/%d/comments", id), nil, &raw, func() error {
		if b := bytes.TrimSpace(raw); len(b) == 0 || b[0] != '[' {
			return fmt.Errorf("comments of task %d: not a JSON array", id)
		}
		return nil
	})
	return raw, err
}
