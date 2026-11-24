package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
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
		Title     string `json:"title"`
		CreatedBy *struct {
			Username string `json:"username"`
		} `json:"created_by"`
	} `json:"labels"`
	Assignees []struct {
		Username string `json:"username"`
	} `json:"assignees"`
	Buckets []struct {
		Title string `json:"title"`
	} `json:"buckets"`
	Index     int64 `json:"index"`
	CreatedBy *struct {
		Username string `json:"username"`
	} `json:"created_by"`
	RelatedTasks map[string][]event.Related `json:"related_tasks"`
}

// Vikunja's related_tasks keys of the relations the lifecycle reads.
const (
	relationBlocked    = "blocked"
	relationBlocking   = "blocking"
	relationSubtask    = "subtask"
	relationParentTask = "parenttask"
)

// toTask converts an API task; Buckets is never nil.
func (raw vikunjaTask) toTask() event.Task {
	t := event.Task{ID: raw.ID, ProjectID: raw.ProjectID, Title: raw.Title, Description: raw.Description,
		Done: raw.Done, Buckets: []string{}, Index: raw.Index}
	if raw.CreatedBy != nil {
		t.CreatedBy = raw.CreatedBy.Username
	}
	for _, l := range raw.Labels {
		t.Labels = append(t.Labels, l.Title)
		if l.CreatedBy != nil && l.CreatedBy.Username != "" {
			if t.LabelCreators == nil {
				t.LabelCreators = map[string][]string{}
			}
			t.LabelCreators[l.Title] = append(t.LabelCreators[l.Title], l.CreatedBy.Username)
		}
	}
	for _, a := range raw.Assignees {
		t.Assignees = append(t.Assignees, a.Username)
	}
	for _, b := range raw.Buckets {
		t.Buckets = append(t.Buckets, b.Title)
	}
	t.Relations = event.Relations{
		BlockedBy: raw.RelatedTasks[relationBlocked], Blocking: raw.RelatedTasks[relationBlocking],
		Subtasks: raw.RelatedTasks[relationSubtask], Parents: raw.RelatedTasks[relationParentTask],
	}
	return t
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
	return raw.toTask(), nil
}

// maxCommentPages bounds the comment pages one read follows: 1000
// comments at Vikunja's 50 a page, far past what a prompt quotes
// (dispatch.DefaultCommentsMaxBytes). The empty page after the last
// one is still read, so exactly maxCommentPages full pages fit.
const maxCommentPages = 20

// emptyPageBytes is the room a page read keeps past the comment budget,
// so the empty page that ends the list ("[]", maybe with a newline) is
// read even once the comments before it used the whole budget.
const emptyPageBytes = 16

// Comments reads all of a task's comments, oldest first (GET
// /tasks/{id}/comments, page after page until an empty one), as one JSON
// array: the raw JSON a prompt quotes, as the legacy spawners'
// contextSources did, but with every page, not only the first. A task
// whose comments fit one page gets that page's array as the API returned it.
// maxBytes caps the pages' bytes: no page is read past what is left of it
// plus emptyPageBytes (so the empty page that ends the list still fits),
// and comments over it are Invalid.
func (v Vikunja) Comments(ctx context.Context, id int64, maxBytes int) (json.RawMessage, error) {
	if id <= 0 || maxBytes <= 0 {
		return nil, &Error{Provider: ProviderVikunja, Class: Invalid,
			Err: fmt.Errorf("task id %d, comment budget %d", id, maxBytes)}
	}
	var first json.RawMessage
	var all []json.RawMessage
	used := 0
	for page := 1; page <= maxCommentPages+1; page++ {
		raw, items, err := v.commentsPage(ctx, id, page, maxBytes-used+emptyPageBytes)
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
		if page > maxCommentPages {
			break
		}
		if used += len(raw); used > maxBytes {
			return nil, &Error{Provider: ProviderVikunja, Class: Invalid,
				Err: fmt.Errorf("comments of task %d: over %d bytes", id, maxBytes)}
		}
		all = append(all, items...)
	}
	return nil, &Error{Provider: ProviderVikunja, Class: Invalid,
		Err: fmt.Errorf("comments of task %d: more than %d pages", id, maxCommentPages)}
}

// commentsPage reads one page of a task's comments: the raw array and
// its items, the body capped at limit bytes.
func (v Vikunja) commentsPage(ctx context.Context, id int64, page, limit int) (json.RawMessage, []json.RawMessage, error) {
	var raw json.RawMessage
	var items []json.RawMessage
	query := url.Values{"page": {strconv.Itoa(page)}}
	err := v.Client.getCapped(ctx, fmt.Sprintf("/tasks/%d/comments", id), query, &raw, func() error {
		if b := bytes.TrimSpace(raw); len(b) == 0 || b[0] != '[' {
			return fmt.Errorf("comments of task %d: not a JSON array", id)
		}
		return json.Unmarshal(raw, &items)
	}, limit)
	return raw, items, err
}

// TaskReactions reads the reaction values on a task (GET
// /tasks/{id}/reactions: value -> users), sorted; the ntfy bookkeeping of
// the readiness notices lives there.
func (v Vikunja) TaskReactions(ctx context.Context, id int64) ([]string, error) {
	if id <= 0 {
		return nil, &Error{Provider: ProviderVikunja, Class: Invalid, Err: fmt.Errorf("task id %d", id)}
	}
	var raw map[string]json.RawMessage
	if err := v.Client.get(ctx, fmt.Sprintf("/tasks/%d/reactions", id), nil, &raw, nil); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for value := range raw {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}
