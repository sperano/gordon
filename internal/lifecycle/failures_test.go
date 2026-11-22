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
	// A review run keeps its task id: only the family guard skips it.
	review := failed
	review.Family = run.PRReview
	untasked := failed
	untasked.TaskID = 0
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
		{"run without a task", untasked, kelos.PhaseFailed, owns(run.VikunjaFailure), false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &queued{newer: c.newer}
			f := &Failures{Store: q, Policy: profile.Defaults().Policy, Ownership: c.owner}
			if err := f.Finished(context.Background(), c.r, kelos.Task{Status: &kelos.Status{Phase: c.phase}}); err != nil {
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

type familyQueue struct {
	queued
	families []run.Family
}

func (q *familyQueue) QueueRunActions(ctx context.Context, inboxID int64, family run.Family, p string, actions []action.Action) error {
	q.families = append(q.families, family)
	return q.queued.QueueRunActions(ctx, inboxID, family, p, actions)
}

// TestFailuresSkipLimitStops: a Task the subscription limit stopped is
// the limit restarts', whoever owns them; legacy's reporter skips it too.
func TestFailuresSkipLimitStops(t *testing.T) {
	q := &queued{}
	f := &Failures{Store: q, Policy: profile.Defaults().Policy, Ownership: owns(run.VikunjaFailure)}
	r := store.ObservedRun{Name: "vikunja-gordon-claude-opus-5", InboxID: 3, Family: run.VikunjaImplement, Project: "gordon", TaskID: 5}
	stop := kelos.Task{Status: &kelos.Status{Phase: kelos.PhaseFailed, Results: map[string]string{kelos.ResultLimitType: "weekly"}}}
	if err := f.Finished(context.Background(), r, stop); err != nil {
		t.Fatal(err)
	}
	if len(q.actions) != 0 {
		t.Errorf("a limit stop queued %v", q.actions)
	}
}

func TestPRRunFailures(t *testing.T) {
	followUp := store.ObservedRun{Name: "pr-followup-gordon-7-c5", InboxID: 4, Family: run.PRFollowUp, Project: "gordon", PR: 7}
	conflict := store.ObservedRun{Name: "pr-conflict-0123abcd-7-89abcdef-01234567", InboxID: 4, Family: run.PRConflict,
		Project: "gordon", PR: 7}
	noPR := followUp
	noPR.PR = 0
	unknown := followUp
	unknown.Project = "elsewhere"
	failed := kelos.Task{Status: &kelos.Status{Phase: kelos.PhaseFailed}}
	limitStop := kelos.Task{Status: &kelos.Status{Phase: kelos.PhaseFailed, Results: map[string]string{kelos.ResultLimitType: "weekly"}}}
	cases := []struct {
		name  string
		r     store.ObservedRun
		task  kelos.Task
		owner rollout.Table
		want  []string
	}{
		{"failed follow-up", followUp, failed, owns(run.PRFollowUp),
			[]string{"post-notice:github/sperano/gordon/pull/7:failed:pr-followup-gordon-7-c5|pr-followup-gordon-7-c5"}},
		{"failed conflict", conflict, failed, owns(run.PRConflict),
			[]string{"post-notice:github/sperano/gordon/pull/7:failed:" + conflict.Name + "|" + conflict.Name}},
		// The limit restarts report a PR run's limit stop, not this.
		{"limit-stopped follow-up", followUp, limitStop, owns(run.PRFollowUp), nil},
		{"limit-stopped conflict", conflict, limitStop, owns(run.PRConflict), nil},
		{"succeeded follow-up", followUp, kelos.Task{Status: &kelos.Status{Phase: kelos.PhaseSucceeded}}, owns(run.PRFollowUp), nil},
		{"legacy owns the follow-ups", followUp, failed, owns(run.VikunjaFailure), nil},
		{"conflict family not owned", conflict, failed, owns(run.PRFollowUp), nil},
		{"no pull request", noPR, failed, owns(run.PRFollowUp), nil},
		{"project not configured", unknown, failed, owns(run.PRFollowUp), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &familyQueue{}
			f := &Failures{Store: q, Policy: profile.Defaults().Policy, Ownership: c.owner, Projects: profile.Defaults().Projects}
			if err := f.Finished(context.Background(), c.r, c.task); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(q.actions, c.want) {
				t.Errorf("queued %v, want %v", q.actions, c.want)
			}
			if c.want != nil && (q.inbox != 4 || !slices.Equal(q.families, []run.Family{c.r.Family})) {
				t.Errorf("queued on inbox %d as %v", q.inbox, q.families)
			}
		})
	}
}
