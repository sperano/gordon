package worker

import (
	"context"
	"errors"
	"sync"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
)

// TaskReader reads a fresh Vikunja task and its comments.
type TaskReader interface {
	Task(ctx context.Context, id int64) (event.Task, error)
	TaskComments(ctx context.Context, id int64) ([]event.TaskComment, error)
}

// PullRequestReader reads a fresh GitHub pull request.
type PullRequestReader interface {
	PullRequest(ctx context.Context, repo string, number int, withFiles bool) (event.PullRequest, error)
}

// HistoryReader reads what the review-round caps count: a pull request's
// reviews and conversation comments.
type HistoryReader interface {
	History(ctx context.Context, repo string, number int) ([]event.Review, []event.IssueComment, error)
}

// errNoHistory is a history read with no reader configured.
var errNoHistory = errors.New("no pull-request history reader")

// StateReader reads the fresh upstream state a delivery's decision needs:
// only what the router would read (route.Router.Reads), so a delivery the
// router refuses before looking at the state (an unconfigured repository,
// a comment by someone other than the owner) or an owner request already
// handled costs no API call. A failed read leaves that part of the state
// nil, which the router answers with retry.
type StateReader struct {
	Config  profile.Config
	Tasks   TaskReader
	PRs     PullRequestReader
	History HistoryReader
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
	if need.History {
		errs = append(errs, s.readHistory(ctx, ev, &st))
	}
	if need.Task != 0 {
		if t, err := s.Tasks.Task(ctx, need.Task); err != nil {
			errs = append(errs, err)
		} else {
			st.Task = &t
		}
	}
	if need.Chain && st.Task != nil && (route.Router{Config: s.Config}).ChainTask(*st.Task) {
		errs = append(errs, s.readChain(ctx, &st))
	}
	return st, errors.Join(errs...)
}

// readHistory reads the pull request's reviews and comments; a failure
// leaves the history unread, which the router answers with retry.
func (s StateReader) readHistory(ctx context.Context, ev event.Event, st *route.State) error {
	if s.History == nil {
		return errNoHistory
	}
	gh := ev.GitHub
	reviews, comments, err := s.History.History(ctx, gh.Repository, gh.PullRequest.Number)
	if err != nil {
		return err
	}
	st.History = &route.PRHistory{Reviews: reviews, Comments: comments}
	return nil
}

// chainReadFanOut bounds the subtask reads of one chain decision in
// flight at once, so a long chain fits the read deadline without one
// decision taking every upstream slot.
const chainReadFanOut = 4

// readChain reads a chain task's subtasks and comments. Any failure
// leaves the task unread: a chain decided on part of its subtasks could
// start one out of order, so the router answers retry instead.
func (s StateReader) readChain(ctx context.Context, st *route.State) error {
	t := st.Task
	subs, err := s.readSubtasks(ctx, t.Relations.Subtasks)
	if err != nil {
		st.Task = nil
		return err
	}
	comments, err := s.Tasks.TaskComments(ctx, t.ID)
	if err != nil {
		st.Task = nil
		return err
	}
	st.Subtasks, st.Comments = subs, comments
	return nil
}

// readSubtasks reads the subtasks concurrently, at most chainReadFanOut
// at a time, in relation order; the errors are joined.
func (s StateReader) readSubtasks(ctx context.Context, rels []event.Related) ([]event.Task, error) {
	subs, errs := make([]event.Task, len(rels)), make([]error, len(rels))
	slots := make(chan struct{}, chainReadFanOut)
	var wg sync.WaitGroup
	for i, rel := range rels {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			subs[i], errs[i] = s.Tasks.Task(ctx, rel.ID)
		})
	}
	wg.Wait()
	return subs, errors.Join(errs...)
}
