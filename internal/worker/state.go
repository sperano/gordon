package worker

import (
	"context"
	"errors"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// TaskReader reads a fresh Vikunja task.
type TaskReader interface {
	Task(ctx context.Context, id int64) (event.Task, error)
}

// PullRequestReader reads a fresh GitHub pull request.
type PullRequestReader interface {
	PullRequest(ctx context.Context, repo string, number int, withFiles bool) (event.PullRequest, error)
}

// StateReader reads the fresh upstream state a delivery's decision needs.
// It reads only what the router would use: a delivery the router refuses
// before looking at the state (an unconfigured repository, a comment by
// someone other than the owner) costs no API call. A failed read leaves
// that part of the state nil, which the router answers with retry.
type StateReader struct {
	Config profile.Config
	Tasks  TaskReader
	PRs    PullRequestReader
}

// Read returns the state and the reads' errors, joined.
func (s StateReader) Read(ctx context.Context, ev event.Event) (route.State, error) {
	var st route.State
	var errs []error
	task := func(id int64) {
		t, err := s.Tasks.Task(ctx, id)
		if err != nil {
			errs = append(errs, err)
			return
		}
		st.Task = &t
	}
	pr := func(files bool) {
		gh := ev.GitHub
		p, err := s.PRs.PullRequest(ctx, gh.Repository, gh.PullRequest.Number, files)
		if err != nil {
			errs = append(errs, err)
			return
		}
		st.PullRequest = &p
	}
	switch ev.Kind {
	case event.VikunjaTaskUpdated:
		if _, ok := s.Config.Project(ev.Vikunja.Task.ProjectID); ok {
			task(ev.Vikunja.Task.ID)
		}
	case event.VikunjaTaskCommentCreated:
		if ev.Actor == s.Config.Policy.VikunjaOwner {
			task(ev.Vikunja.Task.ID)
		}
	case event.GitHubIssueComment, event.GitHubReview, event.GitHubReviewComment:
		if _, ok := s.Config.PRProject(ev.GitHub.Repository); ok && ev.GitHub.OnPullRequest {
			pr(false)
		}
	case event.GitHubPullRequest:
		if _, ok := s.Config.PRProject(ev.GitHub.Repository); ok {
			pr(true)
			if id, linked := run.TaskIDFromBranch(ev.GitHub.PullRequest.HeadRef); linked {
				task(id)
			}
		}
	}
	return st, errors.Join(errs...)
}
