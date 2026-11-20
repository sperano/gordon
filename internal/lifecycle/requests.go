package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

// candidateRequest is an owner request a pass may recover.
type candidateRequest struct {
	task    event.Task
	comment event.TaskComment
}

// requests are the synthetic deliveries of the owner `/gordon` comments of
// the window on open tasks of the configured projects that no delivery
// brought (its webhook was lost) and that were not handled, nor withdrawn
// with the owner's cancel reaction. Tasks a follow-up cannot take are
// recovered too: the router then answers with the unanswered notice the
// lost webhook would have got.
func (r *Reconciler) requests(ctx context.Context, open []upstream.ListedTask) ([]store.Delivery, error) {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	var found []candidateRequest
	for _, lt := range open {
		comments := lt.Comments
		if lt.CommentCount > len(comments) {
			newest, err := r.Tasks.NewestComments(ctx, lt.Task.ID)
			if err != nil {
				return nil, fmt.Errorf("comments of task %d: %w", lt.Task.ID, err)
			}
			comments = newest
		}
		for _, c := range comments {
			if r.recoverable(c, now) {
				found = append(found, candidateRequest{lt.Task, c})
			}
		}
	}
	return r.unseen(ctx, found)
}

func (r *Reconciler) recoverable(c event.TaskComment, now time.Time) bool {
	p := r.Config.Policy
	age := now.Sub(c.Created)
	return c.Author == p.VikunjaOwner && route.IsVikunjaRequest(c.Body, p.Trigger) && age >= r.After && age < r.Lookback &&
		!c.ReactedBy(p.CancelReaction, p.VikunjaOwner)
}

// unseen keeps the requests whose webhook delivery the inbox never got
// and whose request no decision handled.
func (r *Reconciler) unseen(ctx context.Context, found []candidateRequest) ([]store.Delivery, error) {
	if len(found) == 0 {
		return nil, nil
	}
	keys := make([]string, len(found))
	for i, f := range found {
		keys[i] = fmt.Sprintf("vikunja/comment/%d", f.comment.ID)
	}
	known, err := r.Inbox.KnownDeliveries(ctx, keys)
	if err != nil {
		return nil, err
	}
	handled, err := r.Inbox.HandledRequests(ctx, keys)
	if err != nil {
		return nil, err
	}
	var out []store.Delivery
	for i, f := range found {
		if known[keys[i]] || handled[keys[i]] {
			continue
		}
		out = append(out, store.CommentCreated(f.task.ID, f.task.ProjectID, f.comment,
			event.Synthetic{Reconcile: "request"}))
	}
	return out, nil
}
