package route

import (
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// Pull-request routing reasons.
const (
	ReasonDraft         = "draft"
	ReasonNoReviewLabel = "no-review-label"
	ReasonSkipAuthor    = "skip-author"
	ReasonHeadMoved     = "head-moved"
	ReasonReopened      = "pull-request-reopened"
	ReasonNoReviewers   = "no-reviewers"
)

// reviewActions start reviews (prReview.actions).
var reviewActions = []string{"opened", "synchronize", "ready_for_review"}

// openActions link a new agent PR to its task (mergespawners.yaml).
var openActions = []string{"opened", "reopened"}

// skipLabelBranches are the head-branch prefixes whose `kelos:no-review`
// PRs get skipped review checks (review_skip.LABEL_BRANCH_PREFIXES).
var skipLabelBranches = []string{"image-build/", "onboard/"}

// reviewCheckPrefix names a reviewer's required check, review/<reviewer>.
const reviewCheckPrefix = "review/"

func (r Router) pullRequest(ev event.Event, st State, led Ledger) Decision {
	proj, ok := r.Config.PRProject(ev.GitHub.Repository)
	if !ok {
		return Decision{Outcome: Ignore, Reason: ReasonRepositoryNotConfigured}
	}
	switch ev.Action {
	case event.ActionConflict:
		return r.conflict(ev, st, led)
	case event.ActionReadiness:
		if !ev.GitHub.Synthetic {
			return Decision{Outcome: Ignore, Reason: ReasonActionNotRouted}
		}
		return r.readiness(ev, st, led, "")
	}
	reviews := slices.Contains(reviewActions, ev.Action)
	if !reviews && !r.linksTask(ev) {
		return Decision{Outcome: Ignore, Reason: ReasonActionNotRouted}
	}
	if st.PullRequest == nil {
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	}
	var out []FamilyDecision
	if reviews {
		out = append(out, r.review(ev, proj, *st.PullRequest, led))
	}
	if fd, ok := r.taskLink(ev, proj, st); ok {
		out = append(out, fd)
	}
	if len(out) == 0 {
		return Decision{Outcome: Ignore, Reason: ReasonActionNotRouted}
	}
	return Decision{Families: out}
}

// review schedules one review run per applicable reviewer
// (reviewspawners.yaml), or skipped checks for a skipped PR (review_skip.py).
// The `kelos:no-review` label is read from the fresh PR: a label added or
// removed since the delivery decides, and so does the fresh author; the
// head branch and SHA the skipped checks name are the delivery's.
func (r Router) review(ev event.Event, proj profile.Project, pr event.PullRequest, led Ledger) FamilyDecision {
	p, sent := r.Config.Policy, ev.GitHub.PullRequest
	fd := r.family(run.PRReview, proj.Name)
	switch {
	case slices.Contains(p.ReviewSkipAuthors, pr.Author):
		r.skipChecks(&fd, sent, ReasonSkipAuthor, SkipByAuthor+pr.Author)
	case !slices.Contains(p.PullRequestAuthors, pr.Author):
		fd.Outcome, fd.Reason = Reject, ReasonAuthorNotAllowed
	case sent.Draft:
		fd.Outcome, fd.Reason = Ignore, ReasonDraft
	case pr.HasLabel(p.NoReviewLabel):
		r.skipLabelled(&fd, sent)
	case pr.State != event.PullRequestOpen:
		fd.Outcome, fd.Reason = Stale, ReasonPullRequestClosed
	case pr.HeadSHA != sent.HeadSHA:
		fd.Outcome, fd.Reason = Stale, ReasonHeadMoved
	case pr.Draft:
		fd.Outcome, fd.Reason = Stale, ReasonDraft
	default:
		r.reviewRuns(&fd, proj, pr, led)
	}
	return fd
}

func (r Router) reviewRuns(fd *FamilyDecision, proj profile.Project, pr event.PullRequest, led Ledger) {
	dups, active := 0, 0
	reviewers := r.reviewers(proj, pr.Files)
	for _, rv := range reviewers {
		id := run.Identity{Family: run.PRReview, Project: proj.Name, Workspace: proj.Workspace,
			Variant: rv.Variant, Reviewer: rv.Name, PR: pr.Number, HeadSHA: pr.HeadSHA, HeadRef: pr.HeadRef}
		if reason, dup := existing(led, id.Name()); dup {
			dups++
			if reason == ReasonRunActive {
				active++
			}
			continue
		}
		rn, ok := r.resolved(fd, id, profile.Request{Project: proj.Name, Variant: rv.Variant,
			Mode: profile.ModeReview, Effort: rv.Effort})
		if !ok {
			fd.Runs, fd.Actions = nil, nil
			return
		}
		fd.Runs = append(fd.Runs, rn)
		target := action.KelosTask(KelosNamespace, rn.Name)
		submit := action.New(action.SubmitTask, target, "")
		fd.Actions = append(fd.Actions, submit, action.New(action.SupersedeReviews, target, "").WaitFor(submit))
	}
	fd.Outcome = Dispatch
	switch {
	case len(reviewers) == 0:
		fd.Outcome, fd.Reason = Ignore, ReasonNoReviewers
	case dups < len(reviewers):
	case active == dups:
		fd.Outcome, fd.Reason = Duplicate, ReasonRunActive
	default:
		fd.Outcome, fd.Reason = Duplicate, ReasonRunExists
	}
}

// reviewers are the common reviewers plus each of the project's stack
// reviewers whose file patterns match a changed file.
func (r Router) reviewers(proj profile.Project, files []string) []profile.Reviewer {
	out := slices.Clone(r.Config.CommonReviewer)
	for _, stack := range proj.AgentConfigs {
		rv, ok := r.Config.StackReviewers[stack]
		if ok && anyMatch(rv.FilePatterns, files) {
			out = append(out, rv)
		}
	}
	return out
}

// requiredChecks are the review checks GitHub requires on every PR: those
// of the reviewers without file patterns.
func (r Router) requiredChecks() []string {
	var out []string
	for _, rv := range r.Config.CommonReviewer {
		if len(rv.FilePatterns) == 0 {
			out = append(out, reviewCheckPrefix+rv.Name)
		}
	}
	return out
}

// Skipped-check details: why the PR is not reviewed, which the check's
// summary names (review_skip.skip_reason).
const (
	SkipByAuthor = "author:"
	SkipByLabel  = "label:"
)

// skipChecks posts each required review check as skipped on the head;
// the action's detail is the skip reason its summary names.
func (r Router) skipChecks(fd *FamilyDecision, pr event.PullRequest, reason, why string) {
	fd.Outcome, fd.Reason = Act, reason
	target := action.PullRequest(pr.Repository, pr.Number)
	for _, check := range r.requiredChecks() {
		a := action.New(action.SkippedChecks, target, check+"@"+pr.HeadSHA)
		a.Detail = why
		fd.Actions = append(fd.Actions, a)
	}
}

// skipLabelled skips a `kelos:no-review` PR; only an image-build or
// onboarding branch of the same repository gets skipped checks, any other
// fails closed and waits for the owner.
func (r Router) skipLabelled(fd *FamilyDecision, pr event.PullRequest) {
	for _, prefix := range skipLabelBranches {
		if strings.HasPrefix(pr.HeadRef, prefix) && pr.HeadRepo == pr.Repository {
			r.skipChecks(fd, pr, ReasonNoReviewLabel, SkipByLabel+r.Config.Policy.NoReviewLabel)
			return
		}
	}
	fd.Outcome, fd.Reason = Ignore, ReasonNoReviewLabel
}
