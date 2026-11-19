package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Vikunja event names, as in the delivery's event_name.
const (
	vikunjaTaskUpdated    = "task.updated"
	vikunjaCommentCreated = "task.comment.created"
)

// dataDigestBytes is how much of the data digest a task.updated delivery
// key keeps: 16 bytes, 128 bits, ample against accidental collisions.
const dataDigestBytes = 16

// ErrMalformed marks a delivery whose body is not a payload Gordon can
// route (the legacy enricher's 400).
var ErrMalformed = errors.New("malformed delivery")

// ErrUnsupported marks a well-formed delivery of an event type Gordon does
// not subscribe to.
var ErrUnsupported = errors.New("unsupported event")

type vikunjaBody struct {
	EventName string          `json:"event_name"`
	Data      json.RawMessage `json:"data"`
}

type vikunjaData struct {
	Task *struct {
		ID        *int64 `json:"id"`
		ProjectID int64  `json:"project_id"`
		Done      bool   `json:"done"`
		Labels    []struct {
			Title string `json:"title"`
		} `json:"labels"`
		Assignees []struct {
			Username string `json:"username"`
		} `json:"assignees"`
		Buckets *[]struct {
			Title string `json:"title"`
		} `json:"buckets"`
	} `json:"task"`
	Comment *struct {
		ID      int64  `json:"id"`
		Comment string `json:"comment"`
	} `json:"comment"`
	Doer *struct {
		Username string `json:"username"`
	} `json:"doer"`
	Redispatch string `json:"redispatch"`
}

// ParseVikunja parses an authenticated Vikunja delivery body.
//
// A comment's request key is its comment id, so a legacy reconciler
// resend (data.redispatch set) is the same request. A task.updated has no
// delivery id; its delivery key is the digest of its compacted data, the
// legacy generic server's `$.data` deduplication.
func ParseVikunja(body []byte) (Event, error) {
	var raw vikunjaBody
	if err := json.Unmarshal(body, &raw); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var data vikunjaData
	if len(raw.Data) == 0 || json.Unmarshal(raw.Data, &data) != nil {
		return Event{}, fmt.Errorf("%w: data is not an object", ErrMalformed)
	}
	if data.Task == nil || data.Task.ID == nil {
		return Event{}, fmt.Errorf("%w: data.task.id missing", ErrMalformed)
	}
	ev := Event{Source: SourceVikunja, Vikunja: &Vikunja{
		Task: vikunjaTask(data), Redispatch: data.Redispatch,
	}}
	if data.Doer != nil {
		ev.Actor = data.Doer.Username
	}
	switch raw.EventName {
	case vikunjaTaskUpdated:
		return taskUpdated(ev, raw.Data)
	case vikunjaCommentCreated:
		return commentCreated(ev, data)
	default:
		return Event{}, fmt.Errorf("%w: vikunja %q", ErrUnsupported, raw.EventName)
	}
}

func vikunjaTask(data vikunjaData) Task {
	t := data.Task
	task := Task{ID: *t.ID, ProjectID: t.ProjectID, Done: t.Done}
	for _, l := range t.Labels {
		task.Labels = append(task.Labels, l.Title)
	}
	for _, a := range t.Assignees {
		task.Assignees = append(task.Assignees, a.Username)
	}
	if t.Buckets != nil {
		task.Buckets = []string{}
		for _, b := range *t.Buckets {
			task.Buckets = append(task.Buckets, b.Title)
		}
	}
	return task
}

func taskUpdated(ev Event, data json.RawMessage) (Event, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	sum := sha256.Sum256(compact.Bytes())
	ev.Kind = VikunjaTaskUpdated
	ev.DeliveryKey = fmt.Sprintf("vikunja/task.updated/%d/%s",
		ev.Vikunja.Task.ID, hex.EncodeToString(sum[:dataDigestBytes]))
	ev.RequestKey = fmt.Sprintf("vikunja/task/%d", ev.Vikunja.Task.ID)
	return ev, nil
}

func commentCreated(ev Event, data vikunjaData) (Event, error) {
	if data.Comment == nil || data.Comment.ID == 0 {
		return Event{}, fmt.Errorf("%w: data.comment.id missing", ErrMalformed)
	}
	ev.Kind = VikunjaTaskCommentCreated
	ev.Vikunja.Comment = &Comment{ID: data.Comment.ID, Body: data.Comment.Comment}
	ev.RequestKey = fmt.Sprintf("vikunja/comment/%d", data.Comment.ID)
	ev.DeliveryKey = ev.RequestKey
	if data.Redispatch != "" {
		ev.DeliveryKey += "/redispatch/" + data.Redispatch
	}
	return ev, nil
}
