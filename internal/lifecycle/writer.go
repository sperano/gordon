// Package lifecycle performs the Vikunja lifecycle's writes (Gordon 06):
// the outbox executors of labels, buckets, assignments, reactions and
// notices on Vikunja tasks. Every write is preceded by a read of the
// state it changes, so a repeated or recovered action that already
// applied writes nothing twice; a notice is found again by its marker on
// a comment by Gordon's own Vikunja user.
package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sperano/gordon/internal/worker"
)

// maxResponseBytes caps one Vikunja response.
const maxResponseBytes = 8 << 20

// commentsPageSize is Vikunja's page size; a shorter page is the last.
const commentsPageSize = 50

// maxCommentPages bounds one comment listing (2500 comments).
const maxCommentPages = 50

// Writer is an authenticated Vikunja API client for the lifecycle's
// reads and writes.
type Writer struct {
	BaseURL string
	// Authorization is the full header value ("Bearer <token>").
	Authorization string
	HTTP          *http.Client
}

// ErrStatus is a Vikunja answer outside 2xx.
type ErrStatus struct {
	Method, Path string
	Status       int
}

func (e *ErrStatus) Error() string {
	return fmt.Sprintf("vikunja %s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// do sends one request and decodes the answer into out (nil: discarded).
// A read failure is a plain error (retried). A write that failed at the
// transport after it was sent, or that Vikunja answered with a 5xx, may
// have applied: Ambiguous. A 4xx other than 408 and 429 is Permanent.
func (w *Writer) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := w.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return worker.Permanent(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return worker.Permanent(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", w.Authorization)
	write := method != http.MethodGet
	resp, err := w.HTTP.Do(req)
	if err != nil {
		if write {
			return worker.Ambiguous(err)
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil && write {
		return worker.Ambiguous(err)
	}
	if err != nil {
		return err
	}
	return classify(method, path, resp.StatusCode, write, data, out)
}

func classify(method, path string, status int, write bool, data []byte, out any) error {
	st := &ErrStatus{Method: method, Path: path, Status: status}
	switch {
	case status >= http.StatusInternalServerError && write:
		return worker.Ambiguous(st)
	case status >= http.StatusInternalServerError, status == http.StatusTooManyRequests, status == http.StatusRequestTimeout:
		return st
	case status >= http.StatusBadRequest:
		return worker.Permanent(st)
	case out == nil:
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("vikunja %s %s: %w", method, path, err)
	}
	return nil
}

// IsNotFound reports a Vikunja 404.
func IsNotFound(err error) bool {
	var st *ErrStatus
	return errors.As(err, &st) && st.Status == http.StatusNotFound
}

func (w *Writer) get(ctx context.Context, path string, query url.Values, out any) error {
	return w.do(ctx, http.MethodGet, path, query, nil, out)
}

// User is a Vikunja user as the API embeds it.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Label is a label on a task.
type Label struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	CreatedBy User   `json:"created_by"`
}

// Bucket is a task's bucket in one view.
type Bucket struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	ViewID int64  `json:"project_view_id"`
}

// Task is a task as the lifecycle writes it: with ids, not only titles.
type Task struct {
	ID        int64    `json:"id"`
	ProjectID int64    `json:"project_id"`
	Title     string   `json:"title"`
	Index     int64    `json:"index"`
	Done      bool     `json:"done"`
	Labels    []Label  `json:"labels"`
	Assignees []User   `json:"assignees"`
	Buckets   []Bucket `json:"buckets"`
	// RelatedTasks holds the related tasks by Vikunja relation kind.
	RelatedTasks map[string][]struct {
		ID int64 `json:"id"`
	} `json:"related_tasks"`
}

// Task reads a task with its buckets in every view.
func (w *Writer) Task(ctx context.Context, id int64) (Task, error) {
	var t Task
	err := w.get(ctx, fmt.Sprintf("/tasks/%d", id), url.Values{"expand": {"buckets"}}, &t)
	return t, err
}

// Comment is a task comment.
type Comment struct {
	ID      int64  `json:"id"`
	Comment string `json:"comment"`
	Author  User   `json:"author"`
}

// Comments reads every comment of a task, oldest first.
func (w *Writer) Comments(ctx context.Context, task int64) ([]Comment, error) {
	var out []Comment
	seen := map[int64]bool{}
	for page := 1; page <= maxCommentPages; page++ {
		var batch []Comment
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(commentsPageSize)}}
		if err := w.get(ctx, fmt.Sprintf("/tasks/%d/comments", task), q, &batch); err != nil {
			return nil, err
		}
		fresh := 0
		for _, c := range batch {
			if !seen[c.ID] {
				seen[c.ID], fresh = true, fresh+1
				out = append(out, c)
			}
		}
		if len(batch) < commentsPageSize || fresh == 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("comments of task %d: more than %d pages", task, maxCommentPages)
}
