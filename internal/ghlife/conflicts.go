package ghlife

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

// ConflictRequeue is how long one queued observation of a conflict
// stands: within it the same episode is queued once; after it, an
// episode whose run is gone (its Task's TTL, or deleted) and that is
// still in conflict is queued again, as the legacy Task name's TTL
// re-armed it.
const ConflictRequeue = time.Hour

// ConflictLister lists a repository's open pull requests and reads one
// fresh, with its mergeability, with the read-only token.
type ConflictLister interface {
	OpenPullRequests(ctx context.Context, repo string) ([]upstream.ListedPR, error)
	PullRequest(ctx context.Context, repo string, number int, withFiles bool) (event.PullRequest, error)
}

// ConflictStore queues the conflicts found and tells the runs of their
// episodes that are taken (still running, or their Task still present).
type ConflictStore interface {
	Enqueue(ctx context.Context, ds []store.Delivery) (int, error)
	RunsTaken(ctx context.Context, names []string) (map[string]bool, error)
}

// ConflictReconciler is reconcile_conflicts.py, a pass: for each
// Workspace of a project where the pr-conflict family is not legacy's, it
// lists the repository's open pull requests, picks the agent PRs based
// on the Workspace's ref or stacked on another such PR, reads each fresh
// and, for each GitHub confirms in conflict (mergeable false, dirty)
// whose episode has no run taken (not running and its Task gone),
// queues a synthetic delivery the router
// decides from the fresh state. A PR whose mergeability GitHub has not
// computed yet is left to the next pass (the read asks GitHub for it).
// Push and pull_request events start no scan: the pass's interval is the
// detection delay, where legacy also scanned on events.
type ConflictReconciler struct {
	GitHub    ConflictLister
	Store     ConflictStore
	Config    profile.Config
	Ownership rollout.Table
	Now       func() time.Time
	Metrics   *metrics.Metrics
	Log       *slog.Logger
}

// workspaceScope is one Workspace's conflict scan.
type workspaceScope struct {
	repo, workspace, base string
	// others are the refs of the repository's other Workspaces: never a
	// stack head here (reconcile_conflicts.Config.other_base_refs).
	others []string
}

// Once runs one pass, counted and logged. A Workspace that fails does
// not stop the others.
func (c *ConflictReconciler) Once(ctx context.Context) error {
	n, err := c.pass(ctx)
	c.Metrics.ConflictReconcileQueued.Add(float64(n))
	if err != nil {
		c.Metrics.ConflictReconciles.WithLabelValues(lifecycle.ReconcileError).Inc()
		c.Log.Warn("conflict reconciliation failed", "error", err)
		return err
	}
	c.Metrics.ConflictReconciles.WithLabelValues(lifecycle.ReconcileOK).Inc()
	if n > 0 {
		c.Log.Info("conflict reconciliation queued", "conflicts", n)
	}
	return nil
}

// scopes are the Workspaces scanned, once each, in project order.
func (c *ConflictReconciler) scopes() []workspaceScope {
	var out []workspaceScope
	seen := map[[2]string]bool{}
	for _, p := range c.Config.Projects {
		key := [2]string{p.Repository, p.Workspace}
		if p.Repository == "" || seen[key] {
			continue
		}
		seen[key] = true
		// The family's owner is the Workspace's first project's, as the
		// router routes its conflicts.
		if c.Ownership.Owner(p.Name, run.PRConflict) == rollout.Legacy {
			continue
		}
		out = append(out, workspaceScope{repo: p.Repository, workspace: p.Workspace, base: p.Base()})
	}
	for i := range out {
		for _, p := range c.Config.Projects {
			if p.Repository == out[i].repo && p.Workspace != out[i].workspace && !slices.Contains(out[i].others, p.Base()) {
				out[i].others = append(out[i].others, p.Base())
			}
		}
	}
	return out
}

func (c *ConflictReconciler) pass(ctx context.Context) (int, error) {
	listed := map[string][]upstream.ListedPR{}
	var found []conflict
	var errs []error
	for _, s := range c.scopes() {
		prs, ok := listed[s.repo]
		if !ok {
			var err error
			if prs, err = c.GitHub.OpenPullRequests(ctx, s.repo); err != nil {
				errs = append(errs, fmt.Errorf("%s: open pull requests: %w", s.repo, err))
				continue
			}
			listed[s.repo] = prs
		}
		cs, err := c.scope(ctx, s, prs)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", s.repo, s.workspace, err))
		}
		found = append(found, cs...)
	}
	ds, err := c.untaken(ctx, found)
	if err != nil {
		return 0, errors.Join(append(errs, err)...)
	}
	n, err := c.Store.Enqueue(ctx, ds)
	return n, errors.Join(append(errs, err)...)
}

// conflict is one confirmed conflict and the run its episode names.
type conflict struct {
	delivery store.Delivery
	run      string
}

// scope finds one Workspace's confirmed conflicts; what it found before a
// read failed is kept.
func (c *ConflictReconciler) scope(ctx context.Context, s workspaceScope, prs []upstream.ListedPR) ([]conflict, error) {
	bases := c.bases(s, prs)
	var out []conflict
	var errs []error
	for _, listed := range prs {
		if !c.eligible(listed.PullRequest, s, bases) {
			continue
		}
		fresh, err := c.GitHub.PullRequest(ctx, s.repo, listed.Number, false)
		if err != nil {
			errs = append(errs, fmt.Errorf("pull request %d: %w", listed.Number, err))
			continue
		}
		if fresh.Mergeable == nil {
			c.Log.Debug("conflict mergeability not computed yet", "repository", s.repo, "pr", fresh.Number)
			continue
		}
		if !c.eligible(fresh, s, bases) || !fresh.Conflicting() {
			continue
		}
		cf, err := c.conflict(s, fresh)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, cf)
	}
	return out, errors.Join(errs...)
}

// bases are the Workspace's ref and the heads of the open agent PRs
// stacked on it (reconcile_conflicts.agent_head_refs): a PR stacked on
// one of them conflicts with that branch as a PR on the ref does.
func (c *ConflictReconciler) bases(s workspaceScope, prs []upstream.ListedPR) []string {
	reachable := []string{s.base}
	for pending := []string{s.base}; len(pending) > 0; {
		base := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, pr := range prs {
			head := pr.HeadRef
			if pr.State != event.PullRequestOpen || !c.agentPR(pr.PullRequest, s) || slices.Contains(s.others, head) ||
				pr.BaseRef != base || slices.Contains(reachable, head) {
				continue
			}
			reachable = append(reachable, head)
			pending = append(pending, head)
		}
	}
	return reachable
}

// agentPR is reconcile_conflicts.origin_problem's none: a pull-request
// author's PR from a branch of the repository itself.
func (c *ConflictReconciler) agentPR(pr event.PullRequest, s workspaceScope) bool {
	return slices.Contains(c.Config.Policy.PullRequestAuthors, pr.Author) && pr.HeadRepo == s.repo
}

// eligible is reconcile_conflicts.candidate_route with a variant: an open,
// non-draft agent PR on one of the bases, with a `kelos:` label and at
// most one non-default variant label.
func (c *ConflictReconciler) eligible(pr event.PullRequest, s workspaceScope, bases []string) bool {
	if pr.State != event.PullRequestOpen || pr.Draft || !slices.Contains(bases, pr.BaseRef) || !c.agentPR(pr, s) {
		return false
	}
	_, ok := c.variant(pr)
	return ok
}

// variant is the run's variant, as the router picks it.
func (c *ConflictReconciler) variant(pr event.PullRequest) (string, bool) {
	return route.Router{Config: c.Config}.ConflictVariant(pr)
}

func (c *ConflictReconciler) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// conflict is the confirmed conflict's synthetic delivery and its run:
// the delivery's key names the episode and the requeue window, so one
// window queues it once.
func (c *ConflictReconciler) conflict(s workspaceScope, pr event.PullRequest) (conflict, error) {
	variant, _ := c.variant(pr)
	episode := route.Episode(s.repo, s.workspace, variant, pr.Number, pr.BaseSHA, pr.HeadSHA)
	p := pullOf(upstream.ListedPR{PullRequest: pr})
	p.Base.SHA = pr.BaseSHA
	body := payload{Action: event.ActionConflict, Sender: user{pr.Author}, Repository: repoName{s.repo}, PullRequest: &p,
		Gordon: &gordonJSON{Workspace: s.workspace}}
	guid := fmt.Sprintf("gordon-conflict/%s/%s/%s/%d", s.repo, s.workspace, episode, c.now().Unix()/int64(ConflictRequeue/time.Second))
	d, err := store.GitHubSynthetic(eventPullRequest, guid, body)
	if err != nil {
		return conflict{}, err
	}
	return conflict{delivery: d, run: (run.Identity{Family: run.PRConflict, Episode: episode}).Name()}, nil
}

// untaken are the conflicts' deliveries whose run is not taken: running,
// or its Task still present (the legacy name's deduplication).
func (c *ConflictReconciler) untaken(ctx context.Context, cs []conflict) ([]store.Delivery, error) {
	if len(cs) == 0 {
		return nil, nil
	}
	names := make([]string, len(cs))
	for i, cf := range cs {
		names[i] = cf.run
	}
	taken, err := c.Store.RunsTaken(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("conflict runs: %w", err)
	}
	var out []store.Delivery
	for _, cf := range cs {
		if !taken[cf.run] {
			out = append(out, cf.delivery)
		}
	}
	slices.SortFunc(out, func(a, b store.Delivery) int { return strings.Compare(a.DeliveryKey, b.DeliveryKey) })
	return out, nil
}
