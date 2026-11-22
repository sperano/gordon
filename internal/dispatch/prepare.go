package dispatch

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
)

// CommentReader reads a Vikunja task's comments as raw JSON, reading no
// more than maxBytes of them.
type CommentReader interface {
	Comments(ctx context.Context, taskID int64, maxBytes int) (json.RawMessage, error)
}

// Preparer builds the Task of every run a decision dispatches, executed
// or not (a shadow run's Task is what the shadow comparison diffs).
type Preparer struct {
	Builder  Builder
	Comments CommentReader
}

// Prepare sets each run's Task. A family one of whose runs cannot be
// built is a reject naming why, with no run and no write, as a profile
// refusal is: half a family is never submitted.
func (p Preparer) Prepare(ctx context.Context, inboxID int64, ev event.Event, st route.State, dec *route.Decision) {
	comments, fetched := "", false
	for i := range dec.Families {
		fd := &dec.Families[i]
		for j := range fd.Runs {
			rn := &fd.Runs[j]
			if p.Builder.Config.Comments && vikunjaFamily(rn.Identity.Family) && !fetched {
				comments, fetched = p.comments(ctx, rn.Identity.TaskID), true
			}
			t, err := p.Builder.Build(Input{Run: *rn, Event: ev, State: st, InboxID: inboxID, Comments: comments})
			if err != nil {
				reject(fd, err)
				break
			}
			rn.Task = &t
		}
	}
	if len(dec.Families) > 0 {
		dec.Outcome = route.Overall(dec.Families)
	}
}

// comments reads the task's comments; "" when the read fails or the
// answer is over the cap, as a failed legacy fetch (failurePolicy
// Ignore): the prompts tell the agent to read them itself.
func (p Preparer) comments(ctx context.Context, taskID int64) string {
	if p.Comments == nil {
		return ""
	}
	limit := p.Builder.Config.commentsMax()
	raw, err := p.Comments.Comments(ctx, taskID, limit)
	if err != nil || len(raw) > limit {
		return ""
	}
	return string(raw)
}

func reject(fd *route.FamilyDecision, err error) {
	code := CodeTemplate
	var e *Error
	if errors.As(err, &e) {
		code = e.Code
	}
	fd.Outcome, fd.Reason, fd.Runs, fd.Actions = route.Reject, ReasonPrefix+code, nil, nil
}
