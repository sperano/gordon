// Package event defines the typed events Gordon routes and how a raw,
// already authenticated webhook delivery becomes one.
//
// Two keys identify an event. Its delivery key names one transport
// delivery: a redelivery of the same bytes or of the same GitHub delivery
// GUID carries the same key and is a duplicate. Its request key names the
// owner request or state change behind it: a reconciler resend of a
// `/gordon` comment has a new delivery key but the comment's request key,
// while a new `/gordon` comment is a new request even on the same task.
package event

import (
	"fmt"
	"strings"
	"time"
)

// Source is the webhook source a delivery arrived on.
type Source string

const (
	SourceVikunja Source = "vikunja"
	SourceGitHub  Source = "github"
	// SourceWikiComment is the legacy Wiki.js comment poller's dispatch
	// source. Its family is retired, never routed by Gordon.
	SourceWikiComment Source = "wiki-comment"
	// SourceImageBuild is the model-free image-build request source. Image
	// builds stay with Kelos, never routed by Gordon.
	SourceImageBuild Source = "image-build"
)

// Kind is the source-qualified event type.
type Kind string

const (
	VikunjaTaskUpdated        Kind = "vikunja/task.updated"
	VikunjaTaskCommentCreated Kind = "vikunja/task.comment.created"
	GitHubIssueComment        Kind = "github/issue_comment"
	GitHubReview              Kind = "github/pull_request_review"
	GitHubReviewComment       Kind = "github/pull_request_review_comment"
	GitHubPullRequest         Kind = "github/pull_request"
	GitHubCheckRun            Kind = "github/check_run"
	GitHubPush                Kind = "github/push"
)

// Delivery is one webhook request as received, before authentication.
type Delivery struct {
	Source     Source
	Headers    map[string]string
	Body       []byte
	ReceivedAt time.Time
}

// Header returns the delivery's header value, matching the name
// case-insensitively as HTTP does.
func (d Delivery) Header(name string) string {
	if v, ok := d.Headers[name]; ok {
		return v
	}
	for k, v := range d.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// Event is a parsed, authenticated delivery.
type Event struct {
	Source      Source
	Kind        Kind
	Action      string
	DeliveryKey string
	// RequestKey is empty for an event that is no request (a push, a
	// check run).
	RequestKey string
	// Actor is the Vikunja username or GitHub login that caused it.
	Actor   string
	Vikunja *Vikunja
	GitHub  *GitHub
}

// Scope is the project hint the worker's per-project limit keys on: the
// Vikunja project or the GitHub repository; "" for neither.
func (e Event) Scope() string {
	switch {
	case e.Vikunja != nil:
		return fmt.Sprintf("vikunja/project/%d", e.Vikunja.Task.ProjectID)
	case e.GitHub != nil:
		return "github/" + e.GitHub.Repository
	}
	return ""
}

// Vikunja is the part of a Vikunja delivery Gordon routes on.
type Vikunja struct {
	Task    Task
	Comment *Comment
	// Redispatch is set on a legacy reconciler resend.
	Redispatch string
	// Synthetic is set on a delivery Gordon itself queued (a requeue
	// after a related change, or a reconciliation): nil on a webhook.
	Synthetic *Synthetic
}

// Task is a Vikunja task snapshot, from a delivery or a fresh read.
type Task struct {
	ID        int64 `json:"id"`
	ProjectID int64 `json:"project_id"`
	// Title and Description (HTML) are what a run's prompt quotes.
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Done        bool     `json:"done"`
	Labels      []string `json:"labels"`
	Assignees   []string `json:"assignees"`
	// Buckets holds bucket titles; nil when the snapshot did not carry
	// them (a raw task.updated or any comment delivery).
	Buckets []string `json:"buckets"`
	// Index is the task's number in its project, the chain order.
	Index int64 `json:"index,omitempty"`
	// Relations are the task's related tasks, fresh reads only.
	Relations Relations `json:"relations,omitzero"`
}

// Comment is a Vikunja task comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// HasLabel reports whether the task carries the label title.
func (t Task) HasLabel(title string) bool { return contains(t.Labels, title) }

// InBucket reports whether the task is in a bucket of that title.
func (t Task) InBucket(title string) bool { return contains(t.Buckets, title) }

// GitHub is the part of a GitHub delivery Gordon routes on.
type GitHub struct {
	Repository string
	// CommentKind is the legacy follow-up Task-name letter: 'c' issue
	// comment, 'l' inline review comment, 'r' review; 0 otherwise.
	CommentKind byte
	CommentID   int64
	// CommentNodeID is the comment's or review's GraphQL node id, which
	// addReaction needs: REST has no reactions on a submitted review.
	CommentNodeID string
	Body          string
	// CommentURL is the comment's or review's html_url; ReviewID is the
	// review an inline comment belongs to (0 otherwise).
	CommentURL string
	ReviewID   int64
	// ReviewCommitID is a submitted review's commit_id: the head it
	// reviewed, one suggestions round per head.
	ReviewCommitID string
	// OnPullRequest is false for an issue_comment on a plain issue.
	OnPullRequest bool
	PullRequest   PullRequest
	// Synthetic is a delivery the worker queued (SyntheticHeader); only
	// one carries ConflictWorkspace, the Workspace whose conflict
	// reconciliation found the conflict.
	Synthetic         bool
	ConflictWorkspace string
}

// PullRequest is a GitHub pull-request snapshot, from a delivery or a
// fresh read. An issue_comment delivery carries only its number.
type PullRequest struct {
	Repository string   `json:"repository"`
	Number     int      `json:"number"`
	Title      string   `json:"title,omitempty"`
	URL        string   `json:"url,omitempty"`
	State      string   `json:"state"`
	Draft      bool     `json:"draft"`
	Merged     bool     `json:"merged"`
	Author     string   `json:"author"`
	HeadRef    string   `json:"head_ref"`
	HeadSHA    string   `json:"head_sha"`
	HeadRepo   string   `json:"head_repo"`
	BaseRef    string   `json:"base_ref"`
	Labels     []string `json:"labels"`
	Files      []string `json:"files"`
	// BaseSHA is the PR's base.sha (not the base branch's tip);
	// Mergeable (nil while GitHub computes it) and MergeableState come
	// from a fresh read of the one pull request.
	BaseSHA        string `json:"base_sha,omitempty"`
	Mergeable      *bool  `json:"mergeable,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
}

// Conflicting reports a pull request GitHub found in conflict with its
// base (reconcile_conflicts.py: mergeable false, mergeable_state dirty).
func (p PullRequest) Conflicting() bool {
	return p.Mergeable != nil && !*p.Mergeable && p.MergeableState == MergeableDirty
}

// MergeableDirty is GitHub's mergeable_state of a merge conflict.
const MergeableDirty = "dirty"

// PullRequestOpen is GitHub's state of an open pull request.
const PullRequestOpen = "open"

// HasLabel reports whether the pull request carries the label.
func (p PullRequest) HasLabel(name string) bool { return contains(p.Labels, name) }

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// Parse parses an authenticated delivery of a routed source (Vikunja or
// GitHub); any other source is ErrUnsupported.
func Parse(d Delivery) (Event, error) {
	switch d.Source {
	case SourceGitHub:
		return ParseGitHub(d)
	case SourceVikunja:
		return ParseVikunja(d.Body)
	default:
		return Event{}, fmt.Errorf("%w: source %q", ErrUnsupported, d.Source)
	}
}
