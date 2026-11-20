package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

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

// maxCommentPages bounds the comment pages one read follows: 1000
// comments at Vikunja's 50 a page, far past what a prompt quotes
// (dispatch.DefaultCommentsMaxBytes).
const maxCommentPages = 20

// Comments reads all of a task's comments, oldest first (GET
// /tasks/{id}/comments, page after page until an empty one), as one JSON
// array: the raw JSON a prompt quotes, as the legacy spawners'
// contextSources did, but with every page, not only the first. A task
// whose comments fit one page gets that page's array as the API returned it.
func (v Vikunja) Comments(ctx context.Context, id int64) (json.RawMessage, error) {
	if id <= 0 {
		return nil, &Error{Provider: ProviderVikunja, Class: Invalid, Err: fmt.Errorf("task id %d", id)}
	}
	var first json.RawMessage
	var all []json.RawMessage
	for page := 1; page <= maxCommentPages; page++ {
		raw, items, err := v.commentsPage(ctx, id, page)
		if err != nil {
			return nil, err
		}
		if page == 1 {
			first = raw
		}
		if len(items) == 0 {
			if page <= 2 {
				return first, nil
			}
			return json.Marshal(all)
		}
		all = append(all, items...)
	}
	return nil, &Error{Provider: ProviderVikunja, Class: Invalid,
		Err: fmt.Errorf("comments of task %d: more than %d pages", id, maxCommentPages)}
}

// commentsPage reads one page of a task's comments: the raw array and
// its items.
func (v Vikunja) commentsPage(ctx context.Context, id int64, page int) (json.RawMessage, []json.RawMessage, error) {
	var raw json.RawMessage
	var items []json.RawMessage
	query := url.Values{"page": {strconv.Itoa(page)}}
	err := v.Client.get(ctx, fmt.Sprintf("/tasks/%d/comments", id), query, &raw, func() error {
		if b := bytes.TrimSpace(raw); len(b) == 0 || b[0] != '[' {
			return fmt.Errorf("comments of task %d: not a JSON array", id)
		}
		return json.Unmarshal(raw, &items)
	})
	return raw, items, err
}
