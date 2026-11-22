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
	// Chain reads, once the task is read and ChainTask holds for it, its
	// subtasks and its comments.
	Chain bool
	// History reads a pull request's reviews and conversation comments,
	// which the review-round caps count (a review bot's request only).
	History bool
	// Readiness reads the readiness state of the event's pull request (a
	// synthetic readiness delivery) or of every open candidate on the
	// completed check run's head.
	Readiness bool
}

// ChainTask reports a fresh chain parent whose chain steps through its
// subtasks (the default case of Router.chain): it carries the chain
// label, is open, in Doing, not held by the discuss or no-agent label and
// not blocked. Only then are its subtasks and comments read, so a done,
// paused or dragged parent's cleanup never waits on reads it does not use.
func (r Router) ChainTask(t event.Task) bool {
	p := r.Config.Policy
	return p.ChainLabel != "" && t.HasLabel(p.ChainLabel) && !t.Done && t.InBucket(p.DoingBucket) &&
		!t.HasLabel(p.DiscussLabel) && !t.HasLabel(p.NoAgentLabel) && len(t.OpenBlockers()) == 0
}

// Reads is what the router would read to decide ev: nothing for an event
// it refuses before looking at the state (an unconfigured project's open
// task or repository, a comment that is no request from the owner or a review
// bot), and a pull request's files only for an action that may review.
func (r Router) Reads(ev event.Event) Reads {
	p := r.Config.Policy
	switch ev.Kind {
	case event.VikunjaTaskUpdated:
		if _, ok := r.Config.Project(ev.Vikunja.Task.ProjectID); ok {
			return Reads{Task: ev.Vikunja.Task.ID, Chain: true}
		}
		if ev.Vikunja.Task.Done { // a blocker anywhere: its relations requeue
			return Reads{Task: ev.Vikunja.Task.ID}
		}
	case event.VikunjaTaskCommentCreated:
		c := ev.Vikunja.Comment
		if c != nil && ev.Actor == p.VikunjaOwner && isRequest(c.Body, p.Trigger, true) {
			return Reads{Task: ev.Vikunja.Task.ID, OwnerRequest: true}
		}
	case event.GitHubIssueComment, event.GitHubReview, event.GitHubReviewComment:
		return r.commentReads(ev)
	case event.GitHubCheckRun:
		_, ok := r.Config.PRProject(ev.GitHub.Repository)
		return Reads{Readiness: ok && ev.Action == event.ActionCompleted}
	case event.GitHubPullRequest:
		if _, ok := r.Config.PRProject(ev.GitHub.Repository); !ok {
			return Reads{}
		}
		switch ev.Action {
		case event.ActionConflict:
			return Reads{PullRequest: ev.GitHub.Synthetic}
		case event.ActionReadiness:
			return Reads{Readiness: ev.GitHub.Synthetic}
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
	if ev.Kind != event.GitHubReview {
		return Reads{}
	}
	i := slices.IndexFunc(p.ReviewBots, func(b profile.Bot) bool { return b.Login == ev.Actor })
	if i < 0 {
		return Reads{}
	}
	// Only a review that can be a request is capped: an ignored one
	// reads no history, whose failure would retry it.
	found, err := botFindings(p.ReviewBots[i], gh.Body, true)
	return Reads{PullRequest: true, History: r.capsOn() && found && err == nil}
}
