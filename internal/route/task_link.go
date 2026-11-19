package route

import (
	"fmt"
	"slices"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

const closedAction = "closed"

// taskLink decides the model-free writes that tie an agent PR to its
// Vikunja task: pr-open on opened/reopened (pr_open.py) and merge-close
// on closed (merge_close.py). It applies only to an allowed author's PR
// on a kelos/vikunja-<id> branch.
func (r Router) taskLink(ev event.Event, proj profile.Project, st State) (FamilyDecision, bool) {
	if !r.linksTask(ev) {
		return FamilyDecision{}, false
	}
	sent := ev.GitHub.PullRequest
	taskID, _ := run.TaskIDFromBranch(sent.HeadRef)
	fd := r.family(run.MergeClose, proj.Name)
	if slices.Contains(openActions, ev.Action) {
		fd = r.family(run.PROpen, proj.Name)
	}
	if st.Task == nil || st.Task.ID != taskID {
		fd.Outcome, fd.Reason = Retry, ReasonStateUnavailable
		return fd, true
	}
	if fd.Family == run.PROpen {
		r.prOpen(&fd, sent, *st.PullRequest, *st.Task)
	} else {
		r.mergeClose(&fd, sent, *st.PullRequest, *st.Task)
	}
	return fd, true
}

// linksTask reports whether the delivery is a pr-open or merge-close
// action on an allowed author's kelos/vikunja-<id> branch.
func (r Router) linksTask(ev event.Event) bool {
	sent := ev.GitHub.PullRequest
	_, ok := run.TaskIDFromBranch(sent.HeadRef)
	return ok && slices.Contains(r.Config.Policy.PullRequestAuthors, sent.Author) &&
		(slices.Contains(openActions, ev.Action) || ev.Action == closedAction)
}

func prRef(pr event.PullRequest) string { return fmt.Sprintf("%s#%d", pr.Repository, pr.Number) }

// prOpen labels the task `pr open`, moves it to Review unless it is done,
// and announces the PR once.
func (r Router) prOpen(fd *FamilyDecision, sent, pr event.PullRequest, t event.Task) {
	if pr.State != event.PullRequestOpen {
		fd.Outcome, fd.Reason = Stale, ReasonPullRequestClosed
		return
	}
	p, target := r.Config.Policy, action.VikunjaTask(t.ID)
	if !t.HasLabel(p.PROpenLabel) {
		fd.Actions = append(fd.Actions, action.New(action.AddLabel, target, p.PROpenLabel))
	}
	if !t.Done && !t.InBucket(p.ReviewBucket) {
		fd.Actions = append(fd.Actions, action.New(action.MoveBucket, target, p.ReviewBucket))
	}
	fd.Actions = append(fd.Actions, action.Notice(target, NoticePROpen, prRef(sent)))
	fd.Outcome = Act
}

// mergeClose finishes the task of a merged PR (done, lifecycle labels
// off, the merged notice once) or clears the PR labels of one closed
// unmerged; a PR reopened since is stale. A merge cannot be undone, so
// the PR is merged when the delivery or the fresh PR says so: a late
// unmerged-close delivery of a PR merged since still finishes the task.
func (r Router) mergeClose(fd *FamilyDecision, sent, pr event.PullRequest, t event.Task) {
	if pr.State == event.PullRequestOpen {
		fd.Outcome, fd.Reason = Stale, ReasonReopened
		return
	}
	p, target := r.Config.Policy, action.VikunjaTask(t.ID)
	labels, merged := p.CloseLabels, sent.Merged || pr.Merged
	if merged {
		labels = p.MergeCloseLabels
		if !t.Done {
			fd.Actions = append(fd.Actions, action.New(action.MarkDone, target, ""))
		}
	}
	for _, l := range labels {
		if t.HasLabel(l) {
			fd.Actions = append(fd.Actions, action.New(action.RemoveLabel, target, l))
		}
	}
	if merged {
		fd.Actions = append(fd.Actions, action.Notice(target, NoticeMerged, prRef(sent)))
	}
	fd.Outcome = Act
}
