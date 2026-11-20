package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// maxSearchPages bounds one search listing (2500 matches).
const maxSearchPages = 50

// find pages through a search listing at path until an item has the id
// match picks, stopping at a short page or one with nothing new (a
// server that ignores page), as Writer.Comments does.
func find[T any](ctx context.Context, w *Writer, path, s string, match func(T) (int64, bool), key func(T) int64) (int64, bool, error) {
	seen := map[int64]bool{}
	for page := 1; page <= maxSearchPages; page++ {
		var batch []T
		q := url.Values{"s": {s}, "page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(commentsPageSize)}}
		if err := w.get(ctx, path, q, &batch); err != nil {
			return 0, false, err
		}
		fresh := 0
		for _, item := range batch {
			if id, ok := match(item); ok {
				return id, true, nil
			}
			if k := key(item); !seen[k] {
				seen[k], fresh = true, fresh+1
			}
		}
		if len(batch) < commentsPageSize || fresh == 0 {
			return 0, false, nil
		}
	}
	return 0, false, fmt.Errorf("search %s for %q: more than %d pages", path, s, maxSearchPages)
}

// LabelID is the id of the label titled exactly title the token sees.
func (w *Writer) LabelID(ctx context.Context, title string) (int64, bool, error) {
	return find(ctx, w, "/labels", title,
		func(l Label) (int64, bool) { return l.ID, l.Title == title },
		func(l Label) int64 { return l.ID })
}

// AddLabel puts a label on a task (Vikunja creates with PUT).
func (w *Writer) AddLabel(ctx context.Context, task, label int64) error {
	return w.do(ctx, http.MethodPut, fmt.Sprintf("/tasks/%d/labels", task), nil, map[string]int64{"label_id": label}, nil)
}

// RemoveLabel takes a label off a task; one already gone is removed.
func (w *Writer) RemoveLabel(ctx context.Context, task, label int64) error {
	err := w.do(ctx, http.MethodDelete, fmt.Sprintf("/tasks/%d/labels/%d", task, label), nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// View is a project view.
type View struct {
	ID         int64  `json:"id"`
	Kind       string `json:"view_kind"`
	BucketMode string `json:"bucket_configuration_mode"`
}

// Manual is a Kanban view whose buckets cards are moved between by hand.
func (v View) Manual() bool { return v.Kind == "kanban" && v.BucketMode == "manual" }

// Views lists a project's views.
func (w *Writer) Views(ctx context.Context, project int64) ([]View, error) {
	var out []View
	err := w.get(ctx, fmt.Sprintf("/projects/%d/views", project), nil, &out)
	return out, err
}

// Buckets lists a view's buckets.
func (w *Writer) Buckets(ctx context.Context, project, view int64) ([]Bucket, error) {
	var out []Bucket
	err := w.get(ctx, fmt.Sprintf("/projects/%d/views/%d/buckets", project, view), nil, &out)
	return out, err
}

// Move puts a task in a view's bucket.
func (w *Writer) Move(ctx context.Context, project, view, bucket, task int64) error {
	return w.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%d/views/%d/buckets/%d/tasks", project, view, bucket),
		nil, map[string]int64{"task_id": task}, nil)
}

// ProjectUser is the id of the user named exactly username among those
// with access to the project (the assignee picker's route, which API
// tokens may call), and false when there is none.
func (w *Writer) ProjectUser(ctx context.Context, project int64, username string) (int64, bool, error) {
	return find(ctx, w, fmt.Sprintf("/projects/%d/projectusers", project), username,
		func(u User) (int64, bool) { return u.ID, u.Username == username },
		func(u User) int64 { return u.ID })
}

// Assign adds an assignee.
func (w *Writer) Assign(ctx context.Context, task, user int64) error {
	return w.do(ctx, http.MethodPut, fmt.Sprintf("/tasks/%d/assignees", task), nil, map[string]int64{"user_id": user}, nil)
}

// Comment posts a comment and returns it.
func (w *Writer) Comment(ctx context.Context, task int64, text string) (Comment, error) {
	var c Comment
	err := w.do(ctx, http.MethodPut, fmt.Sprintf("/tasks/%d/comments", task), nil, map[string]string{"comment": text}, &c)
	return c, err
}

// UpdateComment replaces a comment's text.
func (w *Writer) UpdateComment(ctx context.Context, task, comment int64, text string) error {
	return w.do(ctx, http.MethodPost, fmt.Sprintf("/tasks/%d/comments/%d", task, comment), nil,
		map[string]string{"comment": text}, nil)
}

// Reactions reads a comment's reactions: value -> users.
func (w *Writer) Reactions(ctx context.Context, comment int64) (map[string][]User, error) {
	out := map[string][]User{}
	err := w.get(ctx, fmt.Sprintf("/comments/%d/reactions", comment), nil, &out)
	return out, err
}

// React adds a reaction (Vikunja creates with PUT; repeating is a no-op).
func (w *Writer) React(ctx context.Context, comment int64, value string) error {
	return w.do(ctx, http.MethodPut, fmt.Sprintf("/comments/%d/reactions", comment), nil, map[string]string{"value": value}, nil)
}

// MarkDone marks a task done: Vikunja's update replaces the task, so the
// task as read now is sent back with done set (merge_close.update_task).
func (w *Writer) MarkDone(ctx context.Context, task int64) error {
	var raw map[string]any
	if err := w.get(ctx, fmt.Sprintf("/tasks/%d", task), nil, &raw); err != nil {
		return err
	}
	raw["done"] = true
	return w.do(ctx, http.MethodPost, fmt.Sprintf("/tasks/%d", task), nil, raw, nil)
}
