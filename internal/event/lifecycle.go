package event

import (
	"slices"
	"time"
)

// Related is one related task as Vikunja embeds it in related_tasks: its
// own labels and relations are not included.
type Related struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Index     int64  `json:"index,omitempty"`
	Done      bool   `json:"done"`
}

// Relations are the relation kinds the lifecycle reads.
type Relations struct {
	// BlockedBy are the tasks this one waits for (Vikunja's "blocked").
	BlockedBy []Related `json:"blocked_by,omitempty"`
	// Blocking are the tasks waiting for this one (Vikunja's "blocking").
	Blocking []Related `json:"blocking,omitempty"`
	Subtasks []Related `json:"subtasks,omitempty"`
	Parents  []Related `json:"parents,omitempty"`
}

// OpenBlockers are the tasks blocking t that are not done.
func (t Task) OpenBlockers() []Related {
	var out []Related
	for _, b := range t.Relations.BlockedBy {
		if !b.Done {
			out = append(out, b)
		}
	}
	return out
}

// TaskComment is a Vikunja task comment as the comments API lists it.
type TaskComment struct {
	ID      int64     `json:"id"`
	Author  string    `json:"author"`
	Body    string    `json:"body"`
	Created time.Time `json:"created,omitzero"`
	// Reactions maps a reaction value to the usernames who left it.
	Reactions map[string][]string `json:"reactions,omitempty"`
}

// ReactedBy reports whether user left the reaction value.
func (c TaskComment) ReactedBy(value, user string) bool {
	return value != "" && slices.Contains(c.Reactions[value], user)
}

// Synthetic marks a Vikunja delivery Gordon queued itself: Requeue names
// the delivery whose decision asked for it (a related task changed),
// Reconcile the reconciliation pass that found the task or comment. Depth
// bounds the requeue fan-out (route.MaxRequeueDepth).
type Synthetic struct {
	Requeue   string `json:"requeue,omitempty"`
	Reconcile string `json:"reconcile,omitempty"`
	Depth     int    `json:"depth"`
}
