package worker

import (
	"context"
	"errors"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
)

// TaskReader reads a fresh Vikunja task.
type TaskReader interface {
	Task(ctx context.Context, id int64) (event.Task, error)
}

// PullRequestReader reads a fresh GitHub pull request.
type PullRequestReader interface {
	PullRequest(ctx context.Context, repo string, number int, withFiles bool) (event.PullRequest, error)
}

// StateReader reads the fresh upstream state a delivery's decision needs:
// only what the router would read (route.Router.Reads), so a delivery the
// router refuses before looking at the state (an unconfigured repository,
// a comment by someone other than the owner) or an owner request already
// handled costs no API call. A failed read leaves that part of the state
// nil, which the router answers with retry.
type StateReader struct {
	Config profile.Config
	Tasks  TaskReader
	PRs    PullRequestReader
}

// Read returns the state and the reads' errors, joined. led answers
// whether an owner request was already handled.
func (s StateReader) Read(ctx context.Context, ev event.Event, led route.Ledger) (route.State, error) {
	var st route.State
	need := route.Router{Config: s.Config}.Reads(ev)
	if need.OwnerRequest && ev.RequestKey != "" && led.HandledRequest(ev.RequestKey) {
		return st, nil
	}
	var errs []error
	if need.PullRequest {
		gh := ev.GitHub
		if p, err := s.PRs.PullRequest(ctx, gh.Repository, gh.PullRequest.Number, need.Files); err != nil {
			errs = append(errs, err)
		} else {
			st.PullRequest = &p
		}
	}
	if need.Task != 0 {
		if t, err := s.Tasks.Task(ctx, need.Task); err != nil {
			errs = append(errs, err)
		} else {
			st.Task = &t
		}
	}
	return st, errors.Join(errs...)
}
