package route

import (
	"slices"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// Reads is the fresh upstream state the router reads to decide an event.
type Reads struct {
	// Task is the Vikunja task to read; 0 for none.
	Task int64
	// PullRequest reads the event's pull request, Files its changed files.
	PullRequest, Files bool
	// OwnerRequest marks an owner `/gordon` request, whose state the
	// router reads only once it knows the request is not handled yet.
	OwnerRequest bool
}

// Reads is what the router would read to decide ev: nothing for an event
// it refuses before looking at the state (an unconfigured project or
// repository, a comment that is no request from the owner or a review
// bot), and a pull request's files only for an action that may review.
func (r Router) Reads(ev event.Event) Reads {
	p := r.Config.Policy
	switch ev.Kind {
	case event.VikunjaTaskUpdated:
		if _, ok := r.Config.Project(ev.Vikunja.Task.ProjectID); ok {
			return Reads{Task: ev.Vikunja.Task.ID}
		}
	case event.VikunjaTaskCommentCreated:
		c := ev.Vikunja.Comment
		if c != nil && ev.Actor == p.VikunjaOwner && isRequest(c.Body, p.Trigger, true) {
			return Reads{Task: ev.Vikunja.Task.ID, OwnerRequest: true}
		}
	case event.GitHubIssueComment, event.GitHubReview, event.GitHubReviewComment:
		return r.commentReads(ev)
	case event.GitHubPullRequest:
		if _, ok := r.Config.PRProject(ev.GitHub.Repository); !ok {
			return Reads{}
		}
		reviews, links := slices.Contains(reviewActions, ev.Action), r.linksTask(ev)
		out := Reads{PullRequest: reviews || links, Files: reviews}
		if id, ok := run.TaskIDFromBranch(ev.GitHub.PullRequest.HeadRef); ok && links {
			out.Task = id
		}
		return out
	}
	return Reads{}
}

func (r Router) commentReads(ev event.Event) Reads {
	p, gh := r.Config.Policy, ev.GitHub
	if _, ok := r.Config.PRProject(gh.Repository); !ok || requestActions[ev.Kind] != ev.Action || !gh.OnPullRequest {
		return Reads{}
	}
	if isRequest(gh.Body, p.Trigger, false) {
		if ev.Actor != p.GitHubOwner {
			return Reads{}
		}
		return Reads{PullRequest: true, OwnerRequest: true}
	}
	if ev.Kind == event.GitHubReview && slices.ContainsFunc(p.ReviewBots, func(b profile.Bot) bool { return b.Login == ev.Actor }) {
		return Reads{PullRequest: true}
	}
	return Reads{}
}
