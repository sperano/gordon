package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/sperano/gordon/internal/event"
)

// TaskCommentsMaxBytes caps the comments one TaskComments read takes in.
const TaskCommentsMaxBytes = 4 << 20

// TaskPageSize is Vikunja's default and maximum page size.
const TaskPageSize = 50

// maxTaskPages bounds one task listing: 2500 tasks.
const maxTaskPages = 50

type vikunjaUser struct {
	Username string `json:"username"`
}

type vikunjaComment struct {
	ID        int64                    `json:"id"`
	Comment   string                   `json:"comment"`
	Author    vikunjaUser              `json:"author"`
	Created   time.Time                `json:"created"`
	Reactions map[string][]vikunjaUser `json:"reactions"`
}

func (c vikunjaComment) toComment() event.TaskComment {
	out := event.TaskComment{ID: c.ID, Author: c.Author.Username, Body: c.Comment, Created: c.Created}
	for value, users := range c.Reactions {
		if out.Reactions == nil {
			out.Reactions = map[string][]string{}
		}
		for _, u := range users {
			out.Reactions[value] = append(out.Reactions[value], u.Username)
		}
	}
	return out
}

// TaskComments reads every comment of a task, oldest first, with their
// authors: the comments the lifecycle trusts by author (its own notices,
// the owner's requests).
func (v Vikunja) TaskComments(ctx context.Context, id int64) ([]event.TaskComment, error) {
	raw, err := v.Comments(ctx, id, TaskCommentsMaxBytes)
	if err != nil {
		return nil, err
	}
	var items []vikunjaComment
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, &Error{Provider: ProviderVikunja, Class: Invalid, Err: err}
	}
	out := make([]event.TaskComment, len(items))
	for i, c := range items {
		out[i] = c.toComment()
	}
	return out, nil
}

// ListedTask is a task of a listing, with the comments Vikunja embeds
// (expand=comments: at most a page, the oldest) and its comment count.
type ListedTask struct {
	Task         event.Task
	Comments     []event.TaskComment
	CommentCount int
	Updated      time.Time
}

type listedTask struct {
	vikunjaTask
	Comments     []vikunjaComment `json:"comments"`
	CommentCount int              `json:"comment_count"`
	Updated      time.Time        `json:"updated"`
}

// Tasks lists every task matching a Vikunja filter (GET /tasks, page by
// page until an empty or all-seen page, so a server capping pages below
// TaskPageSize truncates nothing), with their buckets and, with comments,
// their first comments and comment count.
func (v Vikunja) Tasks(ctx context.Context, filter string, comments bool) ([]ListedTask, error) {
	expand := []string{"buckets"}
	if comments {
		expand = append(expand, "comments", "comment_count")
	}
	var out []ListedTask
	seen := map[int64]bool{}
	for page := 1; page <= maxTaskPages+1; page++ {
		q := url.Values{"filter": {filter}, "expand": expand,
			"per_page": {strconv.Itoa(TaskPageSize)}, "page": {strconv.Itoa(page)}}
		var items []listedTask
		if err := v.Client.get(ctx, "/tasks", q, &items, nil); err != nil {
			return nil, err
		}
		fresh := 0
		for _, it := range items {
			if !seen[it.ID] {
				seen[it.ID], fresh = true, fresh+1
				out = append(out, it.toListed())
			}
		}
		if fresh == 0 {
			return out, nil
		}
	}
	return nil, &Error{Provider: ProviderVikunja, Class: Invalid, Err: fmt.Errorf("more than %d task pages", maxTaskPages)}
}

func (it listedTask) toListed() ListedTask {
	l := ListedTask{Task: it.toTask(), CommentCount: it.CommentCount, Updated: it.Updated}
	for _, c := range it.Comments {
		l.Comments = append(l.Comments, c.toComment())
	}
	return l
}

// NewestComments reads a task's newest page of comments, newest first:
// for a task whose embedded comments were cut at a page.
func (v Vikunja) NewestComments(ctx context.Context, id int64) ([]event.TaskComment, error) {
	var items []vikunjaComment
	q := url.Values{"order_by": {"desc"}}
	if err := v.Client.get(ctx, fmt.Sprintf("/tasks/%d/comments", id), q, &items, nil); err != nil {
		return nil, err
	}
	out := make([]event.TaskComment, len(items))
	for i, c := range items {
		out[i] = c.toComment()
	}
	return out, nil
}

// LabelID is the id of the label titled exactly title that the token
// sees (GET /labels?s=), and false when it sees none.
func (v Vikunja) LabelID(ctx context.Context, title string) (int64, bool, error) {
	var labels []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	if err := v.Client.get(ctx, "/labels", url.Values{"s": {title}}, &labels, nil); err != nil {
		return 0, false, err
	}
	for _, l := range labels {
		if l.Title == title {
			return l.ID, true, nil
		}
	}
	return 0, false, nil
}
