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
	// Value is the label, bucket, reaction or notice kind, or the Task
	// name; "" when the kind needs none.
	Value string `json:"value,omitempty"`
	// Detail qualifies a notice: the unanswered reason code, or the pull
	// request a pr-open or merged notice names.
	Detail string `json:"detail,omitempty"`
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
