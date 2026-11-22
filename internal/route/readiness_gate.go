package route

import (
	"slices"
	"strconv"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/run"
)

// Readiness verdict reasons (reconcile_pr_ready.readiness_verdict's
// problems, as fixed codes).
const (
	ReasonNotMergeable       = "not-mergeable"
	ReasonChecksMissing      = "checks-missing"
	ReasonChecksUnfinished   = "checks-unfinished"
	ReasonChecksUnacceptable = "checks-unacceptable"
	ReasonChecksSettling     = "checks-settling"
	ReasonTasksActive        = "tasks-active"
	ReasonFollowUpPending    = "review-follow-up-pending"
)

// CubicCheck is the Cubic reviewer's check run, ignored on a repository
// Cubic does not review (reconcile_pr_ready.CUBIC_CHECK_NAME).
const CubicCheck = "cubic · AI code reviewer"

// acceptable are the conclusions a finished expected check may have.
var acceptable = []string{"success", "neutral", "skipped"}

// The Kelos Task labels and annotations the readiness gate reads
// (reconcile_pr_ready.ROLE_LABEL and the rest).
const (
	RoleLabel            = "vikunja.spe.quebec/role"
	TaskLabel            = "vikunja.spe.quebec/task"
	WorkspaceLabel       = "vikunja.spe.quebec/workspace"
	PRLabel              = "vikunja.spe.quebec/pr"
	PRAnnotation         = "vikunja.spe.quebec/pr"
	RepositoryAnnotation = "vikunja.spe.quebec/repository"
	SHAAnnotation        = "vikunja.spe.quebec/sha"
)

// Kelos Task roles the gate reads.
const (
	rolePROpen    = "pr-open"
	roleReadiness = "pr-readiness"
)

// blockingRoles are the roles whose active Task on the pull request holds
// its readiness (BLOCKING_PR_ROLES).
var blockingRoles = []string{"pr-review", "pr-followup", "pr-conflict"}

// verdict is readiness_verdict's answer: why the head is not ready
// (problem, a reason code), else the reviews a round limit left
// (limited); with neither the head is ready. missing are the configured
// CI checks with no run on the head.
type verdict struct {
	problem string
	limited []notice.Limited
	missing []string
}

func (v verdict) ready() bool { return v.problem == "" && len(v.limited) == 0 }

// verdict is readiness_verdict for the head.
func (g *gate) verdict() verdict {
	pr := g.rd.PullRequest
	if pr.Mergeable == nil || !*pr.Mergeable {
		return verdict{problem: ReasonNotMergeable}
	}
	runs := g.r.headRuns(pr.Repository, g.rd.CheckRuns)
	latest := latestRuns(runs)
	v := verdict{missing: g.r.missingChecks(pr.Repository, latest)}
	if v.problem = checksProblem(nil, latest, g.r.Config.Policy.Readiness.SettleSeconds, g.rd.ReadAt); v.problem != "" {
		return v
	}
	expected := g.r.expectedChecks(pr)
	verdicts := checksProblem(expected, latest, 0, time.Time{})
	if verdicts != "" && !onlyVerdictsFail(expected, latest, g.r.reviewerChecks(pr.Repository)) {
		v.problem = verdicts
		return v
	}
	if len(g.activeTasks()) > 0 {
		v.problem = ReasonTasksActive
		return v
	}
	pending, limited := g.findings()
	switch {
	case pending:
		v.problem = ReasonFollowUpPending
	case len(limited) == 0:
		v.problem = verdicts
	default:
		v.limited = limited
	}
	return v
}

// headRuns are the head's check runs, without a stray Cubic check on a
// repository Cubic does not review.
func (r Router) headRuns(repo string, runs []event.CheckRun) []event.CheckRun {
	if slices.Contains(r.Config.Policy.Readiness.CubicRepos, repo) {
		return runs
	}
	return slices.DeleteFunc(slices.Clone(runs), func(c event.CheckRun) bool { return c.Name == CubicCheck })
}

// latestRuns is latest_runs_by_name: each name's run of the highest id.
func latestRuns(runs []event.CheckRun) map[string]event.CheckRun {
	out := map[string]event.CheckRun{}
	for _, c := range runs {
		if c.Name == "" {
			continue
		}
		if cur, ok := out[c.Name]; !ok || c.ID > cur.ID {
			out[c.Name] = c
		}
	}
	return out
}

// MissingChecks are the repository's configured CI checks with no run
// among runs, sorted (missing_checks.absent).
func (r Router) MissingChecks(repo string, runs []event.CheckRun) []string {
	return r.missingChecks(repo, latestRuns(r.headRuns(repo, runs)))
}

func (r Router) missingChecks(repo string, latest map[string]event.CheckRun) []string {
	var out []string
	for _, name := range r.Config.Policy.Readiness.CIChecks[repo] {
		if _, ok := latest[name]; !ok && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// checksProblem is checks_problem: an expected check with no run, an
// unfinished run, an expected check's unacceptable conclusion, or, with
// settle, a last completion younger than settle seconds at now.
func checksProblem(expected []string, latest map[string]event.CheckRun, settle int, now time.Time) string {
	for _, name := range expected {
		if _, ok := latest[name]; !ok {
			return ReasonChecksMissing
		}
	}
	for _, c := range latest {
		if c.Status != event.CheckRunCompleted {
			return ReasonChecksUnfinished
		}
	}
	for _, name := range expected {
		if !slices.Contains(acceptable, latest[name].Conclusion) {
			return ReasonChecksUnacceptable
		}
	}
	if settle <= 0 || len(latest) == 0 {
		return ""
	}
	limit := now.Add(-time.Duration(settle) * time.Second)
	for _, c := range latest {
		if c.CompletedAt.IsZero() || c.CompletedAt.After(limit) {
			return ReasonChecksSettling
		}
	}
	return ""
}

// onlyVerdictsFail is only_verdicts_fail: every expected reviewer check
// has a run and the other expected checks pass, so only reviewer
// verdicts (which a round limit may have left) fail.
func onlyVerdictsFail(expected []string, latest map[string]event.CheckRun, reviewers []string) bool {
	var others []string
	for _, name := range expected {
		if !slices.Contains(reviewers, name) {
			others = append(others, name)
			continue
		}
		if _, ok := latest[name]; !ok {
			return false
		}
	}
	return checksProblem(others, latest, 0, time.Time{}) == ""
}

// repoProjects are the projects on the repository, in configuration order.
func (r Router) repoProjects(repo string) []int {
	var out []int
	for i, p := range r.Config.Projects {
		if p.Repository == repo {
			out = append(out, i)
		}
	}
	return out
}

// expectedChecks is expected_checks: the repository's CI checks and the
// review check of every reviewer the review family would start on the
// pull request's files, for any of the repository's projects.
func (r Router) expectedChecks(pr event.PullRequest) []string {
	out := slices.Clone(r.Config.Policy.Readiness.CIChecks[pr.Repository])
	for _, i := range r.repoProjects(pr.Repository) {
		for _, rv := range r.reviewers(r.Config.Projects[i], pr.Files) {
			out = append(out, reviewCheckPrefix+rv.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// reviewerChecks is reviewer_checks: every review check any of the
// repository's projects may expect, whatever the files.
func (r Router) reviewerChecks(repo string) []string {
	var out []string
	for _, i := range r.repoProjects(repo) {
		p := r.Config.Projects[i]
		for _, rv := range r.Config.CommonReviewer {
			out = append(out, reviewCheckPrefix+rv.Name)
		}
		for _, stack := range p.AgentConfigs {
			if rv, ok := r.Config.StackReviewers[stack]; ok {
				out = append(out, reviewCheckPrefix+rv.Name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// workspaces are the repository's Workspaces, once each.
func (r Router) workspaces(repo string) []string {
	var out []string
	for _, i := range r.repoProjects(repo) {
		if w := r.Config.Projects[i].Workspace; !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func finished(phase string) bool { return phase == "Succeeded" || phase == "Failed" }

// onPR reports a Kelos Task about the pull request: the repository
// annotation and the PR label or annotation.
func onPR(t KelosTask, pr event.PullRequest) bool {
	n := t.Labels[PRLabel]
	if n == "" {
		n = t.Annotations[PRAnnotation]
	}
	return t.Annotations[RepositoryAnnotation] == pr.Repository && n == strconv.Itoa(pr.Number)
}

// activeTasks is active_task_names: the unfinished Kelos Tasks of the
// Vikunja task, or reviewing, following up or fixing a conflict of the
// pull request, and Gordon's own runs of either not finished yet (a
// queued submission has no Task yet).
func (g *gate) activeTasks() []string {
	pr, id := g.rd.PullRequest, g.rd.Task.ID
	var out []string
	for _, t := range g.rd.Tasks {
		if finished(t.Phase) {
			continue
		}
		if t.Labels[TaskLabel] == strconv.FormatInt(id, 10) || (slices.Contains(blockingRoles, t.Labels[RoleLabel]) && onPR(t, pr)) {
			out = append(out, t.Name)
		}
	}
	recs := g.led.TaskRuns(id, run.Families)
	for _, w := range g.r.workspaces(pr.Repository) {
		recs = append(recs, g.led.PRRuns(w, pr.Number)...)
	}
	for _, rec := range recs {
		if rec.Active {
			out = append(out, rec.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// announcementTasks is active_announcement_task_names for a scan: the
// unfinished legacy pr-open Task of the pull request, or a legacy
// readiness waiter of its head, which would announce it too.
func announcementTasks(rd *Readiness) []string {
	pr := rd.PullRequest
	var out []string
	for _, t := range rd.Tasks {
		if finished(t.Phase) || t.Annotations[RepositoryAnnotation] != pr.Repository {
			continue
		}
		role := t.Labels[RoleLabel]
		if (role == rolePROpen && onPR(t, pr)) || (role == roleReadiness && t.Annotations[SHAAnnotation] == pr.HeadSHA) {
			out = append(out, t.Name)
		}
	}
	return out
}
