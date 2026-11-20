// Package action defines the external writes a routing decision intends.
// Each action carries a durable key: the same intent always yields the
// same key, so a worker that crashed after an ambiguous write can look the
// key up (and the external marker it leaves) before trying again.
package action

import "fmt"

// Kind is what an action does.
type Kind string

const (
	SubmitTask    Kind = "submit-task"
	React         Kind = "react"
	AddLabel      Kind = "add-label"
	RemoveLabel   Kind = "remove-label"
	MoveBucket    Kind = "move-bucket"
	MarkDone      Kind = "mark-done"
	PostNotice    Kind = "post-notice"
	ConvertDraft  Kind = "convert-to-draft"
	SkippedChecks Kind = "post-skipped-checks"
	// Assign assigns a Vikunja task to the user Value unless an agent
	// user already has it.
	Assign Kind = "assign"
	// SyncLabel makes the target carry the Value label of the task
	// Detail names, exactly as created by the configured creator.
	SyncLabel Kind = "sync-label"
	// CancelRun stops a Vikunja task's implementation and discuss runs:
	// queued submissions are cancelled, live Kelos Tasks deleted.
	CancelRun Kind = "cancel-run"
)

// Target is the object an action writes to.
type Target string

// VikunjaTask is a Vikunja task target.
func VikunjaTask(id int64) Target { return Target(fmt.Sprintf("vikunja/task/%d", id)) }

// VikunjaComment is a Vikunja comment target.
func VikunjaComment(id int64) Target { return Target(fmt.Sprintf("vikunja/comment/%d", id)) }

// PullRequest is a GitHub pull-request target.
func PullRequest(repo string, number int) Target {
	return Target(fmt.Sprintf("github/%s/pull/%d", repo, number))
}

// GitHubComment is a GitHub comment or review target; kind is the
// follow-up letter (c, l, r).
func GitHubComment(repo, kind string, id int64) Target {
	return Target(fmt.Sprintf("github/%s/%s/%d", repo, kind, id))
}

// KelosTask is a Kelos Task target.
func KelosTask(namespace, name string) Target {
	return Target(fmt.Sprintf("kelos/%s/%s", namespace, name))
}

// Action is one intended write.
type Action struct {
	Key    string `json:"key"`
	Kind   Kind   `json:"kind"`
	Target Target `json:"target"`
	// Value is the label, bucket, reaction or notice kind, the Task name,
	// or the username an assign gives the task; "" when the kind needs
	// none.
	Value string `json:"value,omitempty"`
	// Detail qualifies a notice (the unanswered reason code, the pull
	// request a pr-open or merged notice names, the run or subtask it is
	// about), names the runs a cancel-run stops (comma-separated) or the
	// task a sync-label copies from, or carries a GitHub reaction's
	// subject node id; it is not part of a reaction's key.
	Detail string `json:"detail,omitempty"`
	// After is the key of an action of the same decision that must be
	// done first ("" for none): a claim waits for its run's submission,
	// a chain's move for the subtask's assignment. When that action is
	// dead-lettered or cancelled, this one is cancelled.
	After string `json:"after,omitempty"`
}

// WaitFor returns a waiting for first: After set to first's key, so
// a.WaitFor(first) runs a once first is done.
func (a Action) WaitFor(first Action) Action {
	a.After = first.Key
	return a
}

// New builds an action and its durable key, kind:target[:value].
func New(kind Kind, target Target, value string) Action {
	key := string(kind) + ":" + string(target)
	if value != "" {
		key += ":" + value
	}
	return Action{Key: key, Kind: kind, Target: target, Value: value}
}

// Notice builds a post-notice action whose key includes the detail, so
// two different notices on one target never collide.
func Notice(target Target, kind, detail string) Action {
	a := New(PostNotice, target, kind)
	if detail != "" {
		a.Key += ":" + detail
		a.Detail = detail
	}
	return a
}
