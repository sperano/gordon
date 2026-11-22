package route

import (
	"fmt"
	"slices"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// Readiness routing reasons (reconcile_pr_ready.py's skips and verdicts).
const (
	ReasonNotATaskBranch        = "not-a-task-branch"
	ReasonReadinessUnconfigured = "readiness-not-configured"
	ReasonNoPullRequest         = "no-open-pull-request"
	ReasonAnnouncementActive    = "announcement-tasks-active"
	ReasonAlreadyAnnounced      = "already-announced"
	ReasonRoundLimitAnnounced   = "round-limit-announced"
	ReasonReady                 = "ready"
	ReasonRoundLimit            = "round-limit"
)

// Readiness is the fresh state of one pull request's readiness verdict:
// the pull request (with its files, mergeability and creation time),
// its head's check runs, reviews, conversation and review threads, the
// Kelos Tasks of its repository's Workspaces and of its Vikunja task,
// that task with its comments and reactions, and when it was all read.
type Readiness struct {
	PullRequest event.PullRequest    `json:"pull_request"`
	CheckRuns   []event.CheckRun     `json:"check_runs"`
	Reviews     []event.Review       `json:"reviews"`
	Comments    []event.IssueComment `json:"comments"`
	Threads     []event.ReviewThread `json:"threads"`
	// HeadCommitted is the head commit's committer date, read only when
	// a CI check is missing on a head old enough for its notice.
	HeadCommitted time.Time           `json:"head_committed,omitzero"`
	Tasks         []KelosTask         `json:"tasks"`
	Task          event.Task          `json:"task"`
	TaskComments  []event.TaskComment `json:"task_comments"`
	// Reactions are the reaction values on the task (the ntfy
	// bookkeeping), read only with ntfy on.
	Reactions []string  `json:"reactions,omitempty"`
	ReadAt    time.Time `json:"read_at"`
}

// KelosTask is what the readiness gate reads of a Kelos Task: its name,
// phase, labels and annotations.
type KelosTask struct {
	Name        string            `json:"name"`
	Phase       string            `json:"phase,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ReadinessCandidate is reconcile_pr_ready.candidate_problem's none
// without a head: an open, non-draft pull request of an allowed author,
// from a kelos/vikunja-<id> branch of the repository itself.
func (r Router) ReadinessCandidate(pr event.PullRequest) bool {
	return r.readinessProblem(pr, "") == ""
}

// readinessProblem is candidate_problem: why the pull request is no
// candidate for head (any head for ""), with the reason's outcome.
func (r Router) readinessProblem(pr event.PullRequest, head string) string {
	_, task := run.TaskIDFromBranch(pr.HeadRef)
	switch {
	case pr.State != event.PullRequestOpen:
		return ReasonPullRequestClosed
	case pr.Draft:
		return ReasonDraft
	case !slices.Contains(r.Config.Policy.PullRequestAuthors, pr.Author):
		return ReasonAuthorNotAllowed
	case pr.HeadRepo != pr.Repository:
		return ReasonForeignHead
	case head != "" && pr.HeadSHA != head:
		return ReasonHeadMoved
	case !task:
		return ReasonNotATaskBranch
	}
	return ""
}

// candidateOutcomes are the outcomes of readinessProblem's reasons: a
// pull request that changed since the event is stale, any other is not
// the family's.
var candidateOutcomes = map[string]Outcome{
	ReasonPullRequestClosed: Stale, ReasonDraft: Stale, ReasonHeadMoved: Stale,
}

// readiness decides a readiness delivery: the readiness reconciliation's
// synthetic pull_request (one pull request, any head), or a completed
// check run (every open candidate whose head it ran on).
func (r Router) readiness(ev event.Event, st State, led Ledger, head string) Decision {
	gh := ev.GitHub
	proj, ok := r.Config.PRProject(gh.Repository)
	switch {
	case !ok:
		return Decision{Outcome: Ignore, Reason: ReasonRepositoryNotConfigured}
	case !st.ReadinessRead:
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	case len(st.Readiness) == 0:
		return Decision{Outcome: Ignore, Reason: ReasonNoPullRequest}
	}
	var out []FamilyDecision
	for i := range st.Readiness {
		out = append(out, r.readyPR(proj, &st.Readiness[i], led, head))
	}
	return Decision{Families: out}
}

// readyPR is reconcile_pr_ready.reconcile_pull for one pull request.
func (r Router) readyPR(proj profile.Project, rd *Readiness, led Ledger, head string) FamilyDecision {
	fd := r.family(run.PRReadiness, proj.Name)
	pr := rd.PullRequest
	if why := r.readinessProblem(pr, head); why != "" {
		fd.Outcome, fd.Reason = Ignore, why
		if o, ok := candidateOutcomes[why]; ok {
			fd.Outcome = o
		}
		return fd
	}
	if _, ok := r.Config.Policy.Readiness.CIChecks[pr.Repository]; !ok {
		fd.Outcome, fd.Reason = Reject, ReasonReadinessUnconfigured
		return fd
	}
	taskID, _ := run.TaskIDFromBranch(pr.HeadRef)
	if rd.Task.ID != taskID {
		fd.Outcome, fd.Reason = Retry, ReasonStateUnavailable
		return fd
	}
	if len(announcementTasks(rd)) > 0 {
		fd.Outcome, fd.Reason = Ignore, ReasonAnnouncementActive
		return fd
	}
	g := r.gate(proj, rd, led)
	g.decide(&fd)
	return fd
}

// gate is one pull request's readiness gate and the writes it decides.
type gate struct {
	r       Router
	proj    profile.Project
	rd      *Readiness
	led     Ledger
	head    notice.Head
	target  action.Target
	trusted []event.TaskComment
	actions []action.Action
	// created is the pr-open notice this decision posts, which every
	// readiness notice waits for.
	created *action.Action
}

func (r Router) gate(proj profile.Project, rd *Readiness, led Ledger) *gate {
	pr := rd.PullRequest
	return &gate{r: r, proj: proj, rd: rd, led: led,
		head:    notice.Head{Repo: pr.Repository, Number: pr.Number, SHA: pr.HeadSHA},
		target:  action.VikunjaTask(rd.Task.ID),
		trusted: notice.Trusted(rd.TaskComments, r.Config.Policy.VikunjaServiceUser)}
}

// add queues a, after the pr-open notice when this decision posts one.
func (g *gate) add(a action.Action) action.Action {
	if g.created != nil && a.After == "" {
		a = a.WaitFor(*g.created)
	}
	g.actions = append(g.actions, a)
	return a
}

// posted reports a trusted notice of kind about this head.
func (g *gate) posted(kind string) bool {
	return slices.ContainsFunc(g.trusted, func(c event.TaskComment) bool { return notice.HeadPosted(c.Body, kind, g.head) })
}

// decide fills the family decision: the pr-open announcement it
// re-checks (pr_open.announce_created), then the announced head's labels,
// ntfy and merge, or the verdict of a head not announced yet.
func (g *gate) decide(fd *FamilyDecision) {
	pr := g.rd.PullRequest
	url := fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repository, pr.Number)
	if !slices.ContainsFunc(g.trusted, func(c event.TaskComment) bool { return notice.PROpenPosted(c.Body, url) }) {
		created := action.Notice(g.target, NoticePROpen, prRef(pr))
		g.actions = append(g.actions, created)
		g.created = &created
	}
	reason := g.verdictWrites()
	fd.Actions = g.actions
	fd.Outcome, fd.Reason = Act, reason
	if len(fd.Actions) == 0 {
		fd.Outcome = Ignore
		if reason == ReasonAlreadyAnnounced || reason == ReasonRoundLimitAnnounced {
			fd.Outcome = Duplicate
		}
	}
}

// verdictWrites adds the writes of the head's verdict and returns the
// reason the decision records.
func (g *gate) verdictWrites() string {
	if g.posted(notice.Ready) {
		g.announcedWrites()
		return ReasonAlreadyAnnounced
	}
	v := g.verdict()
	switch {
	case v.problem != "":
		g.labels(verdictNotReady, nil)
		g.missingChecks(v.missing)
		return v.problem
	case len(v.limited) > 0:
		return g.roundLimit(v.limited)
	}
	ready := g.add(action.Notice(g.target, notice.Ready, g.head.Detail()))
	g.notify(&ready)
	g.labels(verdictReady, &ready)
	g.merge(&ready)
	return ReasonReady
}

// announcedWrites are reconcile_pull's announced branch: the labels follow
// whether the head still is ready (announced_head_ready); a ready one is
// notified and merged once the full gate passes again.
func (g *gate) announcedWrites() {
	if !g.announcedReady() {
		g.labels(verdictNotReady, nil)
		return
	}
	g.labels(verdictReady, nil)
	if g.r.notifies() && !g.delivered() && g.verdict().ready() {
		g.notify(nil)
	}
	if g.mergeWanted() && g.verdict().ready() {
		g.merge(nil)
	}
}

// announcedReady is announced_head_ready: GitHub reports no conflict and,
// with a ready or auto-merge label configured, no agent Task is on it.
func (g *gate) announcedReady() bool {
	pr, p := g.rd.PullRequest, g.r.Config.Policy
	if pr.Mergeable != nil && !*pr.Mergeable {
		return false
	}
	if p.ReadyToMergeLabel == "" && p.AutoMergeLabel == "" {
		return true
	}
	return len(g.activeTasks()) == 0
}

// roundLimit is announce_round_limit: the notice once per head (first,
// the deduplication key), then the labels.
func (g *gate) roundLimit(limited []notice.Limited) string {
	reason := ReasonRoundLimitAnnounced
	var posted *action.Action
	if !g.posted(notice.RoundLimit) {
		h := g.head
		h.Extra = notice.LimitedExtra(limited)
		a := g.add(action.Notice(g.target, notice.RoundLimit, h.Detail()))
		posted, reason = &a, ReasonRoundLimit
	}
	g.labels(verdictRoundLimit, posted)
	return reason
}

// checkRun decides a check_run delivery: a completed one is the readiness
// family's, for every open candidate on its head (the legacy event
// spawner's filter).
func (r Router) checkRun(ev event.Event, st State, led Ledger) Decision {
	if _, ok := r.Config.PRProject(ev.GitHub.Repository); !ok {
		return Decision{Outcome: Ignore, Reason: ReasonRepositoryNotConfigured}
	}
	if ev.Action != event.ActionCompleted {
		return Decision{Outcome: Ignore, Reason: ReasonActionNotRouted}
	}
	return r.readiness(ev, st, led, ev.GitHub.CheckRunHeadSHA)
}
