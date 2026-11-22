package ghlife

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

// PRLister lists what the GitHub reconciliation recovers from, with the
// read-only token.
type PRLister interface {
	OpenPullRequests(ctx context.Context, repo string) ([]upstream.ListedPR, error)
	ClosedPullRequests(ctx context.Context, repo string, since time.Time) ([]upstream.ListedPR, error)
	Reviews(ctx context.Context, repo string, number int) ([]upstream.ListedReview, error)
	CommentsSince(ctx context.Context, repo string, since time.Time, inline bool) ([]upstream.ListedComment, error)
}

// Reconciler recovers the GitHub webhooks that never arrived
// (poll_review_bot.py, reconcile_merge_close.py, and the owner's PR
// requests, which legacy did not recover): every pass lists each
// configured repository's open pull requests by a pull-request author,
// and queues a synthetic delivery for each owner `/gordon` comment,
// inline comment or review, and each review bot's latest review with a
// body, submitted in the window and not handled yet; and for each such
// pull request closed in the window from an agent branch, its closed
// delivery. The router decides them as it decides webhooks, from the
// fresh state and the ledger, so a request is never answered twice
// (its request key is the webhook's) and a recovered close writes
// nothing a webhook already wrote. Each recovery has a fixed delivery
// key, so it is queued once.
type Reconciler struct {
	GitHub PRLister
	Inbox  lifecycle.Inbox
	Config profile.Config
	// Lookback and Grace bound the window: younger than Lookback, older
	// than Grace (the webhook may still be on its way).
	Lookback, Grace time.Duration
	Now             func() time.Time
	Metrics         *metrics.Metrics
	Log             *slog.Logger
}

// Once runs one reconciliation pass, counted and logged. A repository
// that fails does not stop the others.
func (r *Reconciler) Once(ctx context.Context) error {
	n, err := r.pass(ctx)
	r.Metrics.GitHubReconcileQueued.Add(float64(n))
	if err != nil {
		r.Metrics.GitHubReconciles.WithLabelValues(lifecycle.ReconcileError).Inc()
		r.Log.Warn("github reconciliation failed", "error", err)
		return err
	}
	r.Metrics.GitHubReconciles.WithLabelValues(lifecycle.ReconcileOK).Inc()
	if n > 0 {
		r.Log.Info("github reconciliation queued", "deliveries", n)
	}
	return nil
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// repositories are the configured projects' repositories, once each.
func (r *Reconciler) repositories() []string {
	var out []string
	for _, p := range r.Config.Projects {
		if p.Repository != "" && !slices.Contains(out, p.Repository) {
			out = append(out, p.Repository)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Reconciler) pass(ctx context.Context) (int, error) {
	var ds []store.Delivery
	var errs []error
	for _, repo := range r.repositories() {
		found, err := r.repository(ctx, repo)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
		ds = append(ds, found...)
	}
	ds, err := r.unhandled(ctx, ds)
	if err != nil {
		return 0, errors.Join(append(errs, err)...)
	}
	n, err := r.Inbox.Enqueue(ctx, ds)
	return n, errors.Join(append(errs, err)...)
}

// unhandled drops the recovered requests a decision already handled.
func (r *Reconciler) unhandled(ctx context.Context, ds []store.Delivery) ([]store.Delivery, error) {
	var keys []string
	for _, d := range ds {
		if d.RequestKey != "" {
			keys = append(keys, d.RequestKey)
		}
	}
	if len(keys) == 0 {
		return ds, nil
	}
	handled, err := r.Inbox.HandledRequests(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("handled requests: %w", err)
	}
	out := ds[:0]
	for _, d := range ds {
		if !handled[d.RequestKey] {
			out = append(out, d)
		}
	}
	return out, nil
}

// window reports whether t is inside the recovery window.
func (r *Reconciler) window(t time.Time) bool {
	now := r.now()
	return !t.Before(now.Add(-r.Lookback)) && !t.After(now.Add(-r.Grace))
}

func (r *Reconciler) author(pr upstream.ListedPR) bool {
	return slices.Contains(r.Config.Policy.PullRequestAuthors, pr.Author)
}

// repository lists one repository's recoveries; what it found before an
// error is kept.
func (r *Reconciler) repository(ctx context.Context, repo string) ([]store.Delivery, error) {
	open, err := r.GitHub.OpenPullRequests(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("open pull requests: %w", err)
	}
	byNumber := map[int]upstream.ListedPR{}
	var out []store.Delivery
	var errs []error
	for _, pr := range open {
		if !r.author(pr) {
			continue
		}
		byNumber[pr.Number] = pr
		if pr.Updated.Before(r.now().Add(-r.Lookback)) {
			continue // untouched since before the window: no review in it
		}
		found, err := r.reviews(ctx, pr)
		errs = append(errs, err)
		out = append(out, found...)
	}
	found, err := r.comments(ctx, repo, byNumber)
	errs = append(errs, err)
	out = append(out, found...)
	found, err = r.closed(ctx, repo)
	errs = append(errs, err)
	return append(out, found...), errors.Join(errs...)
}

// reviews are an open pull request's owner request reviews in the window
// and each review bot's latest review with a body, if in the window
// (poll_review_bot.py: a bot's thread replies are empty reviews, which
// must not shadow its summary).
func (r *Reconciler) reviews(ctx context.Context, pr upstream.ListedPR) ([]store.Delivery, error) {
	reviews, err := r.GitHub.Reviews(ctx, pr.Repository, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("reviews of #%d: %w", pr.Number, err)
	}
	p := r.Config.Policy
	latest := map[string]upstream.ListedReview{}
	var picked []upstream.ListedReview
	for _, rv := range reviews {
		switch {
		case rv.Author == p.GitHubOwner && route.IsGitHubRequest(rv.Body, p.Trigger):
			picked = append(picked, rv)
		case r.bot(rv.Author) && rv.Body != "":
			if prev, ok := latest[rv.Author]; !ok || !rv.SubmittedAt.Before(prev.SubmittedAt) {
				latest[rv.Author] = rv
			}
		}
	}
	for _, rv := range latest {
		picked = append(picked, rv)
	}
	slices.SortFunc(picked, func(a, b upstream.ListedReview) int { return cmp.Compare(a.ID, b.ID) })
	var out []store.Delivery
	for _, rv := range picked {
		if !r.window(rv.SubmittedAt) {
			continue
		}
		d, err := reviewDelivery(pr, rv)
		if err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (r *Reconciler) bot(login string) bool {
	return slices.ContainsFunc(r.Config.Policy.ReviewBots, func(b profile.Bot) bool { return b.Login == login })
}

// comments are the owner's `/gordon` conversation and inline comments in
// the window on an open pull request by a pull-request author.
func (r *Reconciler) comments(ctx context.Context, repo string, open map[int]upstream.ListedPR) ([]store.Delivery, error) {
	p := r.Config.Policy
	var out []store.Delivery
	for _, inline := range []bool{false, true} {
		cs, err := r.GitHub.CommentsSince(ctx, repo, r.now().Add(-r.Lookback), inline)
		if err != nil {
			return out, fmt.Errorf("comments: %w", err)
		}
		for _, c := range cs {
			pr, ok := open[c.PR]
			if !ok || c.Author != p.GitHubOwner || !route.IsGitHubRequest(c.Body, p.Trigger) || !r.window(c.Created) {
				continue
			}
			d, err := commentDelivery(pr, c, inline)
			if err != nil {
				return out, err
			}
			out = append(out, d)
		}
	}
	return out, nil
}

// closed are the pull requests by a pull-request author from an agent
// branch closed in the window (reconcile_merge_close.py).
func (r *Reconciler) closed(ctx context.Context, repo string) ([]store.Delivery, error) {
	prs, err := r.GitHub.ClosedPullRequests(ctx, repo, r.now().Add(-r.Lookback))
	if err != nil {
		return nil, fmt.Errorf("closed pull requests: %w", err)
	}
	var out []store.Delivery
	for _, pr := range prs {
		if _, agent := run.TaskIDFromBranch(pr.HeadRef); !agent || !r.author(pr) || !r.window(pr.Closed) {
			continue
		}
		d, err := closedDelivery(pr)
		if err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, nil
}
