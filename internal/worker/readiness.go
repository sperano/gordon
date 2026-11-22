package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/upstream"
)

// ReadinessReader reads, as the GitHub App (the read-only token has no
// Checks permission), what the readiness gate needs beyond the pull
// request: its head's check runs and commit date, and its review threads.
type ReadinessReader interface {
	CheckRuns(ctx context.Context, repo, sha string) ([]event.CheckRun, error)
	Committed(ctx context.Context, repo, sha string) (time.Time, error)
	ReviewThreads(ctx context.Context, repo string, number int) ([]event.ReviewThread, error)
}

// KelosLister lists the Kelos Tasks a label selector selects.
type KelosLister interface {
	Tasks(ctx context.Context, selector string) ([]route.KelosTask, error)
}

// ReactionReader reads the reaction values on a Vikunja task.
type ReactionReader interface {
	TaskReactions(ctx context.Context, id int64) ([]string, error)
}

// OpenLister lists a repository's open pull requests.
type OpenLister interface {
	OpenPullRequests(ctx context.Context, repo string) ([]upstream.ListedPR, error)
}

// Readiness is the StateReader's readiness readers; nil ones fail every
// readiness read, which the router answers with retry.
type Readiness struct {
	GitHub    ReadinessReader
	Kelos     KelosLister
	Reactions ReactionReader
	Open      OpenLister
	// Now is when the state is read; nil is time.Now.
	Now func() time.Time
}

// errNoReadiness is a readiness read with a reader missing.
var errNoReadiness = errors.New("no readiness reader")

func (r Readiness) ok() bool {
	return r.GitHub != nil && r.Kelos != nil && r.Reactions != nil && r.Open != nil
}

func (r Readiness) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// readReadiness reads the readiness state of the synthetic delivery's
// pull request, or of every open candidate on the check run's head; any
// failure leaves it unread.
func (s StateReader) readReadiness(ctx context.Context, ev event.Event, st *route.State) error {
	if !s.Readiness.ok() {
		return errNoReadiness
	}
	gh := ev.GitHub
	numbers := []int{gh.PullRequest.Number}
	if ev.Kind == event.GitHubCheckRun {
		var err error
		if numbers, err = s.headCandidates(ctx, gh.Repository, gh.CheckRunHeadSHA); err != nil {
			return err
		}
	}
	out := []route.Readiness{}
	for _, n := range numbers {
		rd, err := s.readOne(ctx, gh.Repository, n)
		if err != nil {
			return fmt.Errorf("readiness of %s#%d: %w", gh.Repository, n, err)
		}
		out = append(out, rd)
	}
	st.Readiness, st.ReadinessRead = out, true
	return nil
}

// headCandidates are the open readiness candidates whose head is sha.
func (s StateReader) headCandidates(ctx context.Context, repo, sha string) ([]int, error) {
	prs, err := s.Readiness.Open.OpenPullRequests(ctx, repo)
	if err != nil {
		return nil, err
	}
	r := route.Router{Config: s.Config}
	var out []int
	for _, pr := range prs {
		if pr.HeadSHA == sha && r.ReadinessCandidate(pr.PullRequest) {
			out = append(out, pr.Number)
		}
	}
	return out, nil
}

// readOne reads one pull request's readiness state. A pull request that
// is no candidate any more is returned as read, for the router to
// refuse: nothing more is read for it.
func (s StateReader) readOne(ctx context.Context, repo string, number int) (route.Readiness, error) {
	rd := route.Readiness{ReadAt: s.Readiness.now()}
	pr, err := s.PRs.PullRequest(ctx, repo, number, true)
	if err != nil {
		return rd, err
	}
	rd.PullRequest = pr
	r := route.Router{Config: s.Config}
	taskID, _ := run.TaskIDFromBranch(pr.HeadRef)
	if !r.ReadinessCandidate(pr) {
		return rd, nil
	}
	steps := []func() error{
		func() (err error) {
			rd.CheckRuns, err = s.Readiness.GitHub.CheckRuns(ctx, repo, pr.HeadSHA)
			return err
		},
		func() (err error) { rd.Task, err = s.Tasks.Task(ctx, taskID); return err },
		func() (err error) { rd.TaskComments, err = s.Tasks.TaskComments(ctx, taskID); return err },
		func() error { return s.readKelos(ctx, &rd, taskID) },
		func() error { return s.readReviews(ctx, &rd) },
		func() error { return s.readCommitted(ctx, r, &rd) },
		func() (err error) {
			if s.Config.Policy.Readiness.Notify {
				rd.Reactions, err = s.Readiness.Reactions.TaskReactions(ctx, taskID)
			}
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return rd, err
		}
	}
	return rd, nil
}

// readKelos lists the Kelos Tasks of the repository's Workspaces and of
// the Vikunja task (reconcile_pr_ready.workspace_selector,
// with_task_tasks), once each.
func (s StateReader) readKelos(ctx context.Context, rd *route.Readiness, taskID int64) error {
	var workspaces []string
	for _, p := range s.Config.Projects {
		if p.Repository == rd.PullRequest.Repository && !contains(workspaces, p.Workspace) {
			workspaces = append(workspaces, p.Workspace)
		}
	}
	selectors := []string{fmt.Sprintf("%s=%d", route.TaskLabel, taskID)}
	if len(workspaces) > 0 {
		selectors = append([]string{fmt.Sprintf("%s in (%s)", route.WorkspaceLabel, strings.Join(workspaces, ","))},
			selectors...)
	}
	seen := map[string]bool{}
	rd.Tasks = []route.KelosTask{}
	for _, sel := range selectors {
		tasks, err := s.Readiness.Kelos.Tasks(ctx, sel)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if !seen[t.Name] {
				seen[t.Name] = true
				rd.Tasks = append(rd.Tasks, t)
			}
		}
	}
	return nil
}

// readReviews reads the reviews, conversation and review threads the
// review-bot side of the gate counts, only with review bots configured.
func (s StateReader) readReviews(ctx context.Context, rd *route.Readiness) error {
	if len(s.Config.Policy.ReviewBots) == 0 {
		return nil
	}
	if s.History == nil {
		return errNoHistory
	}
	pr := rd.PullRequest
	reviews, comments, err := s.History.History(ctx, pr.Repository, pr.Number)
	if err != nil {
		return err
	}
	rd.Reviews, rd.Comments = reviews, comments
	rd.Threads, err = s.Readiness.GitHub.ReviewThreads(ctx, pr.Repository, pr.Number)
	return err
}

// readCommitted reads the head commit's date only when the CHECKS
// MISSING notice may be due: a configured CI check is missing on a pull
// request at least the notice's minutes old.
func (s StateReader) readCommitted(ctx context.Context, r route.Router, rd *route.Readiness) error {
	pr, minutes := rd.PullRequest, s.Config.Policy.Readiness.MissingCheckMinutes
	if minutes <= 0 || len(r.MissingChecks(pr.Repository, rd.CheckRuns)) == 0 {
		return nil
	}
	if !pr.CreatedAt.IsZero() && !r.MissingOverdue(pr.CreatedAt, time.Time{}, rd.ReadAt) {
		return nil
	}
	var err error
	rd.HeadCommitted, err = s.Readiness.GitHub.Committed(ctx, pr.Repository, pr.HeadSHA)
	return err
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
