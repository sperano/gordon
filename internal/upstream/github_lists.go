package upstream

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/event"
)

// listPerPage is GitHub's largest page; maxListPages bounds one listing
// (10,000 items), past which the read fails rather than decide on part.
const (
	listPerPage  = 100
	maxListPages = 100
)

type githubUserJSON struct {
	Login string `json:"login"`
}

type githubCommentJSON struct {
	ID        int64          `json:"id"`
	NodeID    string         `json:"node_id"`
	Body      string         `json:"body"`
	HTMLURL   string         `json:"html_url"`
	User      githubUserJSON `json:"user"`
	CreatedAt time.Time      `json:"created_at"`
	// IssueURL names an issue comment's issue or PR; PullRequestURL an
	// inline comment's PR; ReviewID the review an inline comment is in.
	IssueURL       string `json:"issue_url"`
	PullRequestURL string `json:"pull_request_url"`
	ReviewID       int64  `json:"pull_request_review_id"`
}

type githubReviewJSON struct {
	ID          int64          `json:"id"`
	NodeID      string         `json:"node_id"`
	Body        string         `json:"body"`
	HTMLURL     string         `json:"html_url"`
	CommitID    string         `json:"commit_id"`
	State       string         `json:"state"`
	User        githubUserJSON `json:"user"`
	SubmittedAt time.Time      `json:"submitted_at"`
}

// pendingReview is the state of a review not submitted yet: no request.
const pendingReview = "PENDING"

// list reads every page of a listing into a slice, stopping at a short
// page; more is told whether to read on after each page.
func list[T any](ctx context.Context, c *Client, path string, query url.Values, more func([]T) bool) ([]T, error) {
	var out []T
	for page := 1; page <= maxListPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)}}
		for k, v := range query {
			q[k] = v
		}
		var batch []T
		if err := c.get(ctx, path, q, &batch, nil); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < listPerPage || (more != nil && !more(batch)) {
			return out, nil
		}
	}
	return nil, &Error{Provider: ProviderGitHub, Class: Invalid, Err: fmt.Errorf("%s: over %d pages", path, maxListPages)}
}

func checkPR(repo string, number int) error {
	if !safeRepository(repo) || number <= 0 {
		return &Error{Provider: ProviderGitHub, Class: Invalid, Err: fmt.Errorf("pull request %q#%d", repo, number)}
	}
	return nil
}

// ListedReview is a submitted review with what a recovery needs.
type ListedReview struct {
	event.Review
	NodeID, URL string
	Submitted   time.Time
}

// Reviews lists a pull request's submitted reviews, oldest first.
func (g GitHub) Reviews(ctx context.Context, repo string, number int) ([]ListedReview, error) {
	if err := checkPR(repo, number); err != nil {
		return nil, err
	}
	raw, err := list[githubReviewJSON](ctx, g.Client, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number), nil, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ListedReview, 0, len(raw))
	for _, r := range raw {
		if r.State == pendingReview {
			continue
		}
		out = append(out, ListedReview{Review: event.Review{ID: r.ID, Author: r.User.Login, Body: r.Body,
			CommitID: r.CommitID}, NodeID: r.NodeID, URL: r.HTMLURL, Submitted: r.SubmittedAt})
	}
	return out, nil
}

// IssueComments lists a pull request's conversation comments.
func (g GitHub) IssueComments(ctx context.Context, repo string, number int) ([]event.IssueComment, error) {
	if err := checkPR(repo, number); err != nil {
		return nil, err
	}
	raw, err := list[githubCommentJSON](ctx, g.Client, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), nil, nil)
	if err != nil {
		return nil, err
	}
	out := make([]event.IssueComment, len(raw))
	for i, c := range raw {
		out[i] = event.IssueComment{ID: c.ID, Author: c.User.Login, Body: c.Body}
	}
	return out, nil
}

// History reads what the review-round caps count: the pull request's
// reviews and conversation comments.
func (g GitHub) History(ctx context.Context, repo string, number int) ([]event.Review, []event.IssueComment, error) {
	listed, err := g.Reviews(ctx, repo, number)
	if err != nil {
		return nil, nil, err
	}
	reviews := make([]event.Review, len(listed))
	for i, r := range listed {
		reviews[i] = r.Review
	}
	comments, err := g.IssueComments(ctx, repo, number)
	if err != nil {
		return nil, nil, err
	}
	return reviews, comments, nil
}

// ListedPR is a pull request of a listing, with its update and close times.
type ListedPR struct {
	event.PullRequest
	Updated, Closed time.Time
}

type githubListedPRJSON struct {
	githubPR
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
}

func listedPR(repo string, raw githubListedPRJSON) ListedPR {
	pr := event.PullRequest{Repository: repo, Number: raw.Number, Title: raw.Title, URL: raw.HTMLURL,
		State: raw.State, Draft: raw.Draft, Merged: raw.MergedAt != nil, Author: raw.User.Login,
		HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA, BaseRef: raw.Base.Ref}
	if raw.Head.Repo != nil {
		pr.HeadRepo = raw.Head.Repo.FullName
	}
	for _, l := range raw.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	out := ListedPR{PullRequest: pr, Updated: raw.UpdatedAt}
	if raw.ClosedAt != nil {
		out.Closed = *raw.ClosedAt
	}
	return out
}

func (g GitHub) pulls(ctx context.Context, repo string, q url.Values, more func([]githubListedPRJSON) bool) ([]ListedPR, error) {
	if !safeRepository(repo) {
		return nil, &Error{Provider: ProviderGitHub, Class: Invalid, Err: fmt.Errorf("repository %q", repo)}
	}
	raw, err := list(ctx, g.Client, fmt.Sprintf("/repos/%s/pulls", repo), q, more)
	if err != nil {
		return nil, err
	}
	out := make([]ListedPR, len(raw))
	for i, r := range raw {
		out[i] = listedPR(repo, r)
	}
	return out, nil
}

// OpenPullRequests lists a repository's open pull requests.
func (g GitHub) OpenPullRequests(ctx context.Context, repo string) ([]ListedPR, error) {
	return g.pulls(ctx, repo, url.Values{"state": {"open"}}, nil)
}

// ClosedPullRequests lists the pull requests closed since, newest update
// first (reconcile_merge_close.py): pages stop once a whole page was
// updated before since.
func (g GitHub) ClosedPullRequests(ctx context.Context, repo string, since time.Time) ([]ListedPR, error) {
	prs, err := g.pulls(ctx, repo, url.Values{"state": {"closed"}, "sort": {"updated"}, "direction": {"desc"}},
		func(batch []githubListedPRJSON) bool { return !batch[len(batch)-1].UpdatedAt.Before(since) })
	if err != nil {
		return nil, err
	}
	out := prs[:0]
	for _, p := range prs {
		if !p.Closed.IsZero() && !p.Closed.Before(since) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ListedComment is a repository comment of a listing: a conversation or
// inline comment, with the pull request it is on (0: an issue's).
type ListedComment struct {
	ID, ReviewID      int64
	NodeID, Body, URL string
	Author            string
	PR                int
	Created           time.Time
}

// prNumber is the number of an issue or pull-request API URL whose
// last two segments are kind ("issues", "pulls") and the number; 0 for
// any other URL.
func prNumber(u, kind string) int {
	p, err := url.Parse(u)
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.Trim(p.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != kind {
		return 0
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// CommentsSince lists the repository's conversation comments (inline
// false) or inline review comments (inline true) created or updated
// since.
func (g GitHub) CommentsSince(ctx context.Context, repo string, since time.Time, inline bool) ([]ListedComment, error) {
	if !safeRepository(repo) {
		return nil, &Error{Provider: ProviderGitHub, Class: Invalid, Err: fmt.Errorf("repository %q", repo)}
	}
	path, kind := fmt.Sprintf("/repos/%s/issues/comments", repo), "issues"
	if inline {
		path, kind = fmt.Sprintf("/repos/%s/pulls/comments", repo), "pulls"
	}
	q := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "sort": {"created"}, "direction": {"asc"}}
	raw, err := list[githubCommentJSON](ctx, g.Client, path, q, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ListedComment, len(raw))
	for i, c := range raw {
		u := c.IssueURL
		if inline {
			u = c.PullRequestURL
		}
		out[i] = ListedComment{ID: c.ID, ReviewID: c.ReviewID, NodeID: c.NodeID, Body: c.Body, URL: c.HTMLURL,
			Author: c.User.Login, PR: prNumber(u, kind), Created: c.CreatedAt}
	}
	return out, nil
}
