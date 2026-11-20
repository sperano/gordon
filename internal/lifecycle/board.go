package lifecycle

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/worker"
)

// placement is where a move-bucket action still has to put the task: the
// manual Kanban views that have a bucket of the title the task is not in.
type placement struct {
	project int64
	moves   [][2]int64 // view, bucket
	// found is false when no manual view has a bucket of the title.
	found bool
	// stale is a move the task no longer wants (skipMove).
	stale bool
}

// skipMove reports a move that would resurrect stale work: any move into
// Doing once the task is done, and a claim's move once the card went
// back to the backlog (Executor.skip).
func (e *Executor) skipMove(t Task, a action.Action) bool {
	return (a.Value == e.Policy.DoingBucket && t.Done) || e.skip(t, a)
}

func (e *Executor) placement(ctx context.Context, a action.Action) (placement, error) {
	id, err := taskTarget(a)
	if err != nil {
		return placement{}, err
	}
	t, err := e.W.Task(ctx, id)
	if err != nil {
		return placement{}, err
	}
	views, err := e.W.Views(ctx, t.ProjectID)
	if err != nil {
		return placement{}, err
	}
	p := placement{project: t.ProjectID, stale: e.skipMove(t, a)}
	if p.stale {
		return p, nil
	}
	for _, v := range views {
		if !v.Manual() {
			continue
		}
		buckets, err := e.W.Buckets(ctx, t.ProjectID, v.ID)
		if err != nil {
			return placement{}, err
		}
		i := slices.IndexFunc(buckets, func(b Bucket) bool { return b.Title == a.Value })
		if i < 0 {
			continue
		}
		p.found = true
		if !slices.ContainsFunc(t.Buckets, func(b Bucket) bool { return b.ViewID == v.ID && b.ID == buckets[i].ID }) {
			p.moves = append(p.moves, [2]int64{v.ID, buckets[i].ID})
		}
	}
	return p, nil
}

func (e *Executor) moveApplied(ctx context.Context, a action.Action) (bool, error) {
	p, err := e.placement(ctx, a)
	return err == nil && (p.stale || p.found && len(p.moves) == 0), err
}

// move puts the task in the bucket of that title in every manual Kanban
// view of its project (wait_for_blockers.move_to_bucket), unless the
// move went stale (skipMove).
func (e *Executor) move(ctx context.Context, a action.Action) error {
	p, err := e.placement(ctx, a)
	if err != nil || p.stale {
		return err
	}
	if !p.found {
		return worker.Permanent(fmt.Errorf("no manual Kanban view has a bucket titled %q", a.Value))
	}
	id, _ := taskTarget(a)
	for _, m := range p.moves {
		if err := e.W.Move(ctx, p.project, m[0], m[1], id); err != nil {
			return err
		}
	}
	return nil
}

// assignApplied: the task already has the user or another agent user
// (wait_for_blockers.assign_agent).
func (e *Executor) assignApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := taskTarget(a)
	if err != nil {
		return false, err
	}
	t, err := e.W.Task(ctx, id)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(t.Assignees, func(u User) bool {
		return u.Username == a.Value || slices.Contains(e.Agents, u.Username)
	}), nil
}

func (e *Executor) assign(ctx context.Context, a action.Action) error {
	id, _ := taskTarget(a)
	t, err := e.W.Task(ctx, id)
	if err != nil {
		return err
	}
	user, ok, err := e.W.ProjectUser(ctx, t.ProjectID, a.Value)
	switch {
	case err != nil:
		return err
	case !ok:
		return worker.Permanent(fmt.Errorf("no Vikunja user %q with access to task %d's project", a.Value, id))
	}
	return e.W.Assign(ctx, id, user)
}

// syncPlan is what makes the target carry exactly the source's label of
// the title created by the trusted creator (subtask_chain.sync_label):
// the ids to remove and the id to add (0 for none).
type syncPlan struct {
	target int64
	remove []int64
	add    int64
}

func (e *Executor) syncPlan(ctx context.Context, a action.Action) (syncPlan, error) {
	id, err := taskTarget(a)
	if err != nil {
		return syncPlan{}, err
	}
	src, err := strconv.ParseInt(a.Detail, 10, 64)
	if err != nil {
		return syncPlan{}, worker.Permanent(fmt.Errorf("sync-label source %q: %w", a.Detail, err))
	}
	source, err := e.W.Task(ctx, src)
	if err != nil {
		return syncPlan{}, err
	}
	t, err := e.W.Task(ctx, id)
	if err != nil {
		return syncPlan{}, err
	}
	var want int64
	for _, l := range source.Labels {
		if l.Title == a.Value && l.CreatedBy.Username == e.Policy.AutoMergeLabelCreator {
			want = l.ID
			break
		}
	}
	plan, has := syncPlan{target: id}, false
	for _, l := range t.Labels {
		switch {
		case l.Title != a.Value:
		case l.ID == want:
			has = true
		default:
			plan.remove = append(plan.remove, l.ID)
		}
	}
	if want != 0 && !has {
		plan.add = want
	}
	return plan, nil
}

func (e *Executor) syncApplied(ctx context.Context, a action.Action) (bool, error) {
	p, err := e.syncPlan(ctx, a)
	return err == nil && len(p.remove) == 0 && p.add == 0, err
}

func (e *Executor) sync(ctx context.Context, a action.Action) error {
	p, err := e.syncPlan(ctx, a)
	if err != nil {
		return err
	}
	for _, id := range p.remove {
		if err := e.W.RemoveLabel(ctx, p.target, id); err != nil {
			return err
		}
	}
	if p.add != 0 {
		return e.W.AddLabel(ctx, p.target, p.add)
	}
	return nil
}

func emoji(value string) string {
	if e, ok := reactionEmoji[value]; ok {
		return e
	}
	return value
}

func (e *Executor) commentTarget(a action.Action) (int64, error) {
	kind, id, err := target(a)
	if err == nil && kind != "comment" {
		err = worker.Permanent(fmt.Errorf("react needs a comment target: %s", a.Target))
	}
	return id, err
}

// reactApplied: Gordon's user already left the reaction.
func (e *Executor) reactApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := e.commentTarget(a)
	if err != nil {
		return false, err
	}
	rs, err := e.W.Reactions(ctx, id)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(rs[emoji(a.Value)], func(u User) bool { return u.Username == e.Policy.VikunjaServiceUser }), nil
}

func (e *Executor) react(ctx context.Context, a action.Action) error {
	id, _ := e.commentTarget(a)
	return e.W.React(ctx, id, emoji(a.Value))
}
