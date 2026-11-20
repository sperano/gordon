package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// LabelID is the id of the label titled exactly title the token sees.
func (w *Writer) LabelID(ctx context.Context, title string) (int64, bool, error) {
	var labels []Label
	if err := w.get(ctx, "/labels", url.Values{"s": {title}}, &labels); err != nil {
		return 0, false, err
	}
	for _, l := range labels {
		if l.Title == title {
			return l.ID, true, nil
		}
	}
	return 0, false, nil
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
	var users []User
	if err := w.get(ctx, fmt.Sprintf("/projects/%d/projectusers", project), url.Values{"s": {username}}, &users); err != nil {
		return 0, false, err
	}
	for _, u := range users {
		if u.Username == username {
			return u.ID, true, nil
		}
	}
	return 0, false, nil
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
