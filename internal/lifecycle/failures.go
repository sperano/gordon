package lifecycle

import (
	"context"
	"slices"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

// FailureStore is the part of the store the failure hook uses.
type FailureStore interface {
	QueueRunActions(ctx context.Context, inboxID int64, family run.Family, project string, actions []action.Action) error
	NewerRun(ctx context.Context, name string, families []run.Family) (bool, error)
}

// agentRunFamilies are the families whose runs work on a Vikunja task.
var agentRunFamilies = append(slices.Clone(run.AgentFamilies), followUps...)

// discussFamilies are the runs that answer on the task (the legacy
// vikunja.spe.quebec/discuss Tasks).
var discussFamilies = []run.Family{run.VikunjaDiscuss, run.VikunjaFollowUpDiscuss}

// Failures is the observer's hook for Gordon's own Vikunja runs
// (report_failures.py, action_needed.flag_discussions): a Failed run gets
// the failed notice, the agent-error label and the action-needed label;
// a discuss run that Succeeded gets action-needed, unless the owner asked
// again since. Only where Gordon owns the vikunja-failure family: the
// legacy reporter then skips Gordon's Tasks (gordon.spe.quebec/managed).
type Failures struct {
	Store     FailureStore
	Policy    profile.Policy
	Ownership rollout.Table
}

// Finished queues the finished run's lifecycle actions.
func (f *Failures) Finished(ctx context.Context, r store.ObservedRun, phase string) error {
	if r.TaskID <= 0 || !slices.Contains(agentRunFamilies, r.Family) || !f.Ownership.Executes(r.Project, run.VikunjaFailure) {
		return nil
	}
	actions, err := f.actions(ctx, r, phase)
	if err != nil || len(actions) == 0 {
		return err
	}
	return f.Store.QueueRunActions(ctx, r.InboxID, run.VikunjaFailure, r.Project, actions)
}

func (f *Failures) actions(ctx context.Context, r store.ObservedRun, phase string) ([]action.Action, error) {
	p, target := f.Policy, action.VikunjaTask(r.TaskID)
	ownerInbox := func() []action.Action {
		if p.ActionNeededLabel == "" {
			return nil
		}
		a := action.New(action.AddLabel, target, p.ActionNeededLabel)
		a.Detail = route.DetailUnlessDone
		return []action.Action{a}
	}
	switch {
	case phase == kelos.PhaseFailed:
		out := []action.Action{action.Notice(target, notice.Failed, r.Name)}
		if p.AgentErrorLabel != "" {
			out = append(out, action.New(action.AddLabel, target, p.AgentErrorLabel))
		}
		return append(out, ownerInbox()...), nil
	case phase == kelos.PhaseSucceeded && slices.Contains(discussFamilies, r.Family):
		newer, err := f.Store.NewerRun(ctx, r.Name, agentRunFamilies)
		if err != nil || newer {
			return nil, err
		}
		return ownerInbox(), nil
	}
	return nil, nil
}
