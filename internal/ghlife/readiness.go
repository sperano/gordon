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

// OpenPullRequests lists a repository's open pull requests.
type OpenPullRequests interface {
	OpenPullRequests(ctx context.Context, repo string) ([]upstream.ListedPR, error)
}

// ReadinessInbox queues the readiness reconciliation's deliveries.
type ReadinessInbox interface {
	Enqueue(ctx context.Context, ds []store.Delivery) (int, error)
}

// ReadinessReconciler is the readiness backstop (the legacy */5 cron
// TaskSpawner): each pass lists the open pull requests of every
// repository whose PR project does not leave pr-readiness to legacy, and
// queues one synthetic delivery per readiness candidate, which the router
// decides from the fresh state. Its delivery key names the head and the
// pass's interval window, so one window queues a head once. Completed
// check runs decide their heads between passes; the legacy event waiter's
// polling (WAIT_SECONDS, POLL_SECONDS) is the pass's interval.
type ReadinessReconciler struct {
	GitHub    OpenPullRequests
	Inbox     ReadinessInbox
	Config    profile.Config
	Ownership rollout.Table
	// Interval is the pass's interval: the delivery key's window.
	Interval time.Duration
	Now      func() time.Time
	Metrics  *metrics.Metrics
	Log      *slog.Logger
}

// Once runs one pass, counted and logged. A repository that fails does
// not stop the others.
func (c *ReadinessReconciler) Once(ctx context.Context) error {
	n, err := c.pass(ctx)
	c.Metrics.ReadinessReconcileQueued.Add(float64(n))
	if err != nil {
		c.Metrics.ReadinessReconciles.WithLabelValues(lifecycle.ReconcileError).Inc()
		c.Log.Warn("readiness reconciliation failed", "error", err)
		return err
	}
	c.Metrics.ReadinessReconciles.WithLabelValues(lifecycle.ReconcileOK).Inc()
	if n > 0 {
		c.Log.Info("readiness reconciliation queued", "pull_requests", n)
	}
	return nil
}

// repositories are the repositories scanned, once each, in project order:
// the family's owner is the repository's PR project's, as the router
// decides it.
func (c *ReadinessReconciler) repositories() []string {
	var out []string
	for _, p := range c.Config.Projects {
		if p.Repository == "" || slices.Contains(out, p.Repository) {
			continue
		}
		if proj, _ := c.Config.PRProject(p.Repository); proj.Name != p.Name {
			continue
		}
		if c.Ownership.Owner(p.Name, run.PRReadiness) != rollout.Legacy {
			out = append(out, p.Repository)
		}
	}
	return out
}

func (c *ReadinessReconciler) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *ReadinessReconciler) pass(ctx context.Context) (int, error) {
	r := route.Router{Config: c.Config}
	var ds []store.Delivery
	var errs []error
	for _, repo := range c.repositories() {
		prs, err := c.GitHub.OpenPullRequests(ctx, repo)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: open pull requests: %w", repo, err))
			continue
		}
		for _, pr := range prs {
			if !r.ReadinessCandidate(pr.PullRequest) {
				continue
			}
			d, err := c.delivery(pr)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			ds = append(ds, d)
		}
	}
	slices.SortFunc(ds, func(a, b store.Delivery) int { return strings.Compare(a.DeliveryKey, b.DeliveryKey) })
	n, err := c.Inbox.Enqueue(ctx, ds)
	return n, errors.Join(append(errs, err)...)
}

// window is the pass's interval window of now: one per interval.
func (c *ReadinessReconciler) window() int64 {
	step := int64(c.Interval / time.Second)
	if step <= 0 {
		step = 1
	}
	return c.now().Unix() / step
}

// delivery is the candidate's synthetic readiness delivery.
func (c *ReadinessReconciler) delivery(pr upstream.ListedPR) (store.Delivery, error) {
	p := pullOf(pr)
	body := payload{Action: event.ActionReadiness, Sender: user{pr.Author}, Repository: repoName{pr.Repository}, PullRequest: &p}
	guid := fmt.Sprintf("gordon-readiness/%s/%d/%s/%d", pr.Repository, pr.Number, pr.HeadSHA, c.window())
	return store.GitHubSynthetic(eventPullRequest, guid, body)
}
