package lifecycle

import (
	"context"
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

type queued struct {
	inbox   int64
	family  run.Family
	actions []string
	newer   bool
}

func (q *queued) QueueRunActions(_ context.Context, inboxID int64, family run.Family, _ string, actions []action.Action) error {
	q.inbox, q.family = inboxID, family
	for _, a := range actions {
		q.actions = append(q.actions, a.Key+"|"+a.Detail)
	}
	return nil
}

func (q *queued) NewerRun(context.Context, string, []run.Family) (bool, error) { return q.newer, nil }

func owns(f run.Family) rollout.Table {
	return rollout.Table{Rules: []rollout.Rule{{Project: "gordon", Family: f, Owner: rollout.Gordon,
		Evidence: []rollout.Gate{rollout.ReplayParity, rollout.ShadowParity, rollout.RollbackTested, rollout.LegacyDisabled}}}}
}

func TestFailures(t *testing.T) {
	failed := store.ObservedRun{Name: "vikunja-gordon-claude-opus-5", InboxID: 3, Family: run.VikunjaImplement,
		Project: "gordon", TaskID: 5}
	discuss := failed
	discuss.Name, discuss.Family = "vikunja-discuss-gordon-claude-opus-5", run.VikunjaDiscuss
	review := failed
	review.Family, review.TaskID = run.PRReview, 0
	cases := []struct {
		name  string
		r     store.ObservedRun
		phase string
		owner rollout.Table
		newer bool
		want  []string
	}{
		{"failed", failed, kelos.PhaseFailed, owns(run.VikunjaFailure), false, []string{
			"post-notice:vikunja/task/5:failed:vikunja-gordon-claude-opus-5|vikunja-gordon-claude-opus-5",
			"add-label:vikunja/task/5:agent-error|", "add-label:vikunja/task/5:action-needed|unless-done"}},
		{"succeeded implementation", failed, kelos.PhaseSucceeded, owns(run.VikunjaFailure), false, nil},
		{"answered discussion", discuss, kelos.PhaseSucceeded, owns(run.VikunjaFailure), false,
			[]string{"add-label:vikunja/task/5:action-needed|unless-done"}},
		{"discussion asked again since", discuss, kelos.PhaseSucceeded, owns(run.VikunjaFailure), true, nil},
		{"legacy owns failures", failed, kelos.PhaseFailed, rollout.Table{}, false, nil},
		{"review run", review, kelos.PhaseFailed, owns(run.VikunjaFailure), false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &queued{newer: c.newer}
			f := &Failures{Store: q, Policy: profile.Defaults().Policy, Ownership: c.owner}
			if err := f.Finished(context.Background(), c.r, c.phase); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(q.actions, c.want) {
				t.Errorf("queued %v, want %v", q.actions, c.want)
			}
			if c.want != nil && (q.inbox != 3 || q.family != run.VikunjaFailure) {
				t.Errorf("queued on inbox %d as %s", q.inbox, q.family)
			}
		})
	}
}
