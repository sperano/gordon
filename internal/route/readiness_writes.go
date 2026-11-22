package route

import (
	"slices"
	"strconv"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
)

// The verdicts the readiness labels follow (reconcile_pr_ready.READY,
// ROUND_LIMIT, NOT_READY).
type readyVerdict int

const (
	verdictNotReady readyVerdict = iota
	verdictReady
	verdictRoundLimit
)

// DeliveredReaction marks ntfy's delivery of a head on its task
// (pr_ready_notify.DELIVERED_REACTION).
const DeliveredReaction = "🔔"

// labels is mark_readiness: the other verdict's label comes off, then
// the verdict's own label goes on after the action-needed label (last,
// so a failed step is retried by the next scan, and once it is on the
// action label is not put back after the owner took it off). NOT_READY
// takes the action label off with a verdict label it revokes. Nothing
// without a verdict label configured. The additions wait for after, the
// notice the verdict posts, when it posts one.
func (g *gate) labels(v readyVerdict, after *action.Action) {
	p, t := g.r.Config.Policy, g.rd.Task
	titles := map[readyVerdict]string{verdictReady: p.ReadyToMergeLabel, verdictRoundLimit: p.RoundLimitLabel}
	if titles[verdictReady] == "" && titles[verdictRoundLimit] == "" {
		return
	}
	wanted := titles[v]
	var held []string
	for _, other := range []readyVerdict{verdictReady, verdictRoundLimit} {
		if other != v && titles[other] != "" && t.HasLabel(titles[other]) {
			held = append(held, titles[other])
		}
	}
	if len(held) > 0 && wanted == "" && p.ActionNeededLabel != "" && t.HasLabel(p.ActionNeededLabel) {
		held = append(held, p.ActionNeededLabel)
	}
	for _, l := range held {
		g.add(action.New(action.RemoveLabel, g.target, l))
	}
	if wanted == "" || t.HasLabel(wanted) {
		return
	}
	add := action.New(action.AddLabel, g.target, wanted)
	if l := p.ActionNeededLabel; l != "" && !t.HasLabel(l) {
		first := action.New(action.AddLabel, g.target, l)
		if after != nil {
			first = first.WaitFor(*after)
		}
		add = add.WaitFor(g.add(first))
	} else if after != nil {
		add = add.WaitFor(*after)
	}
	g.add(add)
}

// notifies reports ntfy on (prReadiness.notify.enabled).
func (r Router) notifies() bool { return r.Config.Policy.Readiness.Notify }

// delivered is pr_ready_notify.delivered: the head's delivered reaction
// is on the task.
func (g *gate) delivered() bool {
	return slices.Contains(g.rd.Reactions, DeliveredReaction+notice.ShortSHA(g.head.SHA))
}

// notify sends the head to ntfy unless it was (notify_newly_ready).
func (g *gate) notify(after *action.Action) {
	if !g.r.notifies() || g.delivered() {
		return
	}
	a := action.New(action.Notify, g.target, g.head.SHA)
	a.Detail = g.head.Detail()
	if after != nil {
		a = a.WaitFor(*after)
	}
	g.add(a)
}

// mergeWanted is auto_merge.wanted: an auto-merge label and its creator
// are configured, and the task carries that label, created by them.
func (g *gate) mergeWanted() bool {
	p := g.r.Config.Policy
	return p.AutoMergeLabel != "" && p.AutoMergeLabelCreator != "" && g.rd.Task.LabelBy(p.AutoMergeLabel, p.AutoMergeLabelCreator)
}

// merge merges the ready head, pinned to its SHA, when the task asks for
// it (auto_merge.run).
func (g *gate) merge(after *action.Action) {
	if !g.mergeWanted() {
		return
	}
	pr := g.rd.PullRequest
	a := action.New(action.MergePullRequest, action.PullRequest(pr.Repository, pr.Number), pr.HeadSHA)
	a.Detail = strconv.FormatInt(g.rd.Task.ID, 10)
	if after != nil {
		a = a.WaitFor(*after)
	}
	g.add(a)
}

// missingChecks is missing_checks.report: one notice per head once its
// configured CI checks are overdue, the later of the PR's creation and
// the head commit's date older than the configured minutes.
func (g *gate) missingChecks(missing []string) {
	minutes := g.r.Config.Policy.Readiness.MissingCheckMinutes
	if len(missing) == 0 || minutes <= 0 || g.posted(notice.ChecksMissing) {
		return
	}
	if !g.r.MissingOverdue(g.rd.PullRequest.CreatedAt, g.rd.HeadCommitted, g.rd.ReadAt) {
		return
	}
	h := g.head
	h.Extra = notice.MissingExtra(missing)
	g.add(action.Notice(g.target, notice.ChecksMissing, h.Detail()))
}

// MissingOverdue is missing_checks.head_since against the notice's
// minutes: the later of created and committed (either may be zero, not
// both) is at least that old at now. A PR younger than that is not
// overdue whatever its commit, so its head commit need not be read.
func (r Router) MissingOverdue(created, committed, now time.Time) bool {
	limit := now.Add(-time.Duration(r.Config.Policy.Readiness.MissingCheckMinutes) * time.Minute)
	since := created
	if committed.After(since) {
		since = committed
	}
	return !since.IsZero() && !since.After(limit)
}
