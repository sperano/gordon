package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

// CostBodies reads the stored text of a run generation's cost notice.
type CostBodies interface {
	CostBody(ctx context.Context, name string, inboxID int64) (string, int, error)
}

// costDetail parses a publish-cost action's "<generation>:<revision>".
func costDetail(a action.Action) (generation int64, rev int, err error) {
	g, r, ok := strings.Cut(a.Detail, ":")
	generation, gerr := strconv.ParseInt(g, 10, 64)
	rev, rerr := strconv.Atoi(r)
	if !ok || gerr != nil || rerr != nil || generation <= 0 || rev <= 0 || a.Value == "" {
		return 0, 0, worker.Permanent(fmt.Errorf("publish-cost %q detail %q", a.Value, a.Detail))
	}
	return generation, rev, nil
}

// costComment is the trusted comment carrying the generation's cost
// notice and the revision it shows.
func (e *Executor) costComment(ctx context.Context, task int64, name string, generation int64) (event.TaskComment, int, bool, error) {
	cs, err := e.trusted(ctx, task)
	if err != nil {
		return event.TaskComment{}, 0, false, err
	}
	for _, c := range cs {
		if rev, ok := notice.CostRevision(c.Body, name, generation); ok {
			return c, rev, true, nil
		}
	}
	return event.TaskComment{}, 0, false, nil
}

// costApplied reports a comment showing the action's revision or a
// later one: an ambiguous post or edit that landed is not made again.
func (e *Executor) costApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := taskTarget(a)
	if err != nil {
		return false, err
	}
	generation, rev, err := costDetail(a)
	if err != nil {
		return false, err
	}
	_, shown, ok, err := e.costComment(ctx, id, a.Value, generation)
	return ok && shown >= rev, err
}

// publishCost writes the latest stored revision of the notice, holding
// the target lock: an edit of the comment that carries the generation's
// notice, or a new comment when none does yet. A later revision queued
// meanwhile finds itself applied.
func (e *Executor) publishCost(ctx context.Context, a action.Action) error {
	id, _ := taskTarget(a)
	generation, _, err := costDetail(a)
	if err != nil {
		return err
	}
	if e.Costs == nil {
		return worker.Permanent(errors.New("publish-cost: no cost notice store"))
	}
	body, latest, err := e.Costs.CostBody(ctx, a.Value, generation)
	switch {
	case errors.Is(err, store.ErrNoCostNotice):
		return worker.Permanent(fmt.Errorf("publish-cost %s: %w", a.Value, err))
	case err != nil:
		return err
	}
	text := body + notice.CostRevisionMarker(a.Value, generation, latest)
	ctx, cancel := context.WithTimeout(ctx, noticeLockTimeout)
	defer cancel()
	return e.Lock.WithTargetLock(ctx, string(a.Target), func(ctx context.Context) error {
		c, shown, ok, err := e.costComment(ctx, id, a.Value, generation)
		switch {
		case err != nil:
			return err
		case ok && shown >= latest:
			return nil
		case ok:
			return e.W.UpdateComment(ctx, id, c.ID, text)
		}
		_, err = e.W.Comment(ctx, id, text)
		return err
	})
}
