package ghlife

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/restart"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/worker"
)

// commentsPerPage is GitHub's largest page; maxCommentPages bounds one
// listing (10,000 comments).
const (
	commentsPerPage = 100
	maxCommentPages = 100
)

// noticeLockTimeout bounds the check and the post held under a notice's
// target lock (lifecycle's), so a slow GitHub cannot keep the lock's pool
// connection idle for longer. A post cut short is ambiguous and
// recovered by its marker.
const noticeLockTimeout = time.Minute

type comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// trusted lists the pull request's conversation comments by a
// pull-request author: the App's bot or a Workspace token's user, the
// legacy start comments' authors (START_COMMENT_AUTHORS). A marker in
// anyone else's comment suppresses nothing.
func (e *Executor) trusted(ctx context.Context, t Target) ([]comment, error) {
	var out []comment
	// The page after the cap tells a full last page (exactly the cap)
	// from more comments than one listing reads.
	for page := 1; page <= maxCommentPages+1; page++ {
		var batch []comment
		p := fmt.Sprintf("%s?per_page=%d&page=%d", path("repos", t.Owner, t.Name, "issues", fmt.Sprint(t.N), "comments"),
			commentsPerPage, page)
		if err := e.W.REST(ctx, t.Repo, githubapp.Comments, http.MethodGet, p, nil, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		if page > maxCommentPages {
			break
		}
		for _, c := range batch {
			if slices.Contains(e.Policy.PullRequestAuthors, c.User.Login) {
				out = append(out, c)
			}
		}
		if len(batch) < commentsPerPage {
			return out, nil
		}
	}
	// Retrying reads the same comments: the notice cannot be checked.
	return nil, worker.Permanent(fmt.Errorf("comments of %s#%d: over %d, the notice cannot be checked",
		t.FullName(), t.N, maxCommentPages*commentsPerPage))
}

// legacyPosted reports a notice of the same intent posted by the legacy
// script it replaces: the start comment of that Task, the PR's one cap
// notice of each kind.
func legacyPosted(a action.Action, body string) bool {
	switch a.Value {
	case notice.Start:
		return notice.HasKind(body, notice.Start, a.Detail)
	case notice.ReviewCap:
		return notice.ReviewCapPosted(body)
	case notice.SuggestionsCap:
		return notice.SuggestionsCapPosted(body)
	}
	return false
}

func (e *Executor) noticeApplied(ctx context.Context, a action.Action, t Target) (bool, error) {
	if err := pullTarget(a, t); err != nil {
		return false, err
	}
	cs, err := e.trusted(ctx, t)
	if err != nil {
		return false, err
	}
	marker := notice.ActionMarker(a.Key)
	return slices.ContainsFunc(cs, func(c comment) bool {
		return strings.Contains(c.Body, marker) || legacyPosted(a, c.Body)
	}), nil
}

// postNotice renders the notice, then, holding the target lock, checks
// again that it is missing and posts it with its action marker. The
// render (the run's row) happens before the lock, as the Vikunja
// notices' do: the lock's transaction holds a pool connection.
func (e *Executor) postNotice(ctx context.Context, a action.Action, t Target) error {
	text, err := e.render(ctx, a, t)
	if err != nil {
		return err
	}
	text += "\n" + notice.ActionMarker(a.Key)
	ctx, cancel := context.WithTimeout(ctx, noticeLockTimeout)
	defer cancel()
	return e.Lock.WithTargetLock(ctx, string(a.Target), func(ctx context.Context) error {
		if done, err := e.noticeApplied(ctx, a, t); err != nil || done {
			return err
		}
		p := path("repos", t.Owner, t.Name, "issues", fmt.Sprint(t.N), "comments")
		return e.W.REST(ctx, t.Repo, githubapp.Comments, http.MethodPost, p, map[string]string{"body": text}, nil)
	})
}

func (e *Executor) bots() []string {
	out := make([]string, len(e.Policy.ReviewBots))
	for i, b := range e.Policy.ReviewBots {
		out[i] = b.Login
	}
	return out
}

// render is the notice's Markdown text.
func (e *Executor) render(ctx context.Context, a action.Action, t Target) (string, error) {
	p := e.Policy
	switch a.Value {
	case notice.Start:
		return e.startText(ctx, a.Detail, t)
	case notice.ReviewCap:
		return notice.ReviewCapText(e.bots(), p.MaxRoundsPerPR, p.Trigger), nil
	case notice.SuggestionsCap:
		return notice.SuggestionsCapText(p.SuggestionsMaxRounds, p.FixSuggestionsLabel, p.Trigger), nil
	case notice.Failed:
		return e.failedText(ctx, a.Detail, t)
	case notice.RestartGaveUp:
		return e.gaveUpText(ctx, a.Detail)
	}
	return "", worker.Permanent(fmt.Errorf("no GitHub notice %q", a.Value))
}

// startText is the run's start notice; the private links only on a
// private repository (announce_github_start.private_links): they name the
// cluster, the agents' namespace and private hosts.
func (e *Executor) startText(ctx context.Context, name string, t Target) (string, error) {
	r, err := e.Runs.RunInfo(ctx, name)
	if err != nil {
		return "", err
	}
	s := notice.PRStartInfo{Task: name, Model: r.Model, Effort: r.Effort}
	if e.Links.Run(r.TaskUID) != "" || e.Links.Sillage != "" {
		repo, err := e.visibility(ctx, t)
		if err != nil {
			return "", err
		}
		if repo.Private {
			s.RunURL = e.Links.Run(r.TaskUID)
			if e.Links.Sillage != "" && repo.HTMLURL != "" {
				s.ActivityURL = notice.SillagePRURL(e.Links.Sillage, fmt.Sprintf("%s/pull/%d", repo.HTMLURL, t.N))
			}
		}
	}
	return notice.PRStartText(s), nil
}

// repoVisibility is the part of a repository its notices' links need.
type repoVisibility struct {
	Private bool   `json:"private"`
	HTMLURL string `json:"html_url"`
}

// visibility reads whether the repository is private; one GitHub refuses
// for good counts as public (no private links), a transient failure is
// returned and retried.
func (e *Executor) visibility(ctx context.Context, t Target) (repoVisibility, error) {
	var repo repoVisibility
	err := e.W.REST(ctx, t.Repo, githubapp.Comments, http.MethodGet, path("repos", t.Owner, t.Name), nil, &repo)
	switch {
	case worker.IsPermanent(err):
		return repoVisibility{}, nil
	case err != nil:
		return repoVisibility{}, err
	}
	return repo, nil
}

// failedText is a failed PR run's notice: its Task's status message and
// the run link on a private repository only, as for the start notice: the
// message is the agent's or the cluster's text.
func (e *Executor) failedText(ctx context.Context, name string, t Target) (string, error) {
	r, err := e.Runs.RunInfo(ctx, name)
	if err != nil {
		return "", err
	}
	f := notice.PRFailedInfo{Task: name, Trigger: e.Policy.Trigger, Conflict: r.Family == run.PRConflict,
		Namespace: e.Links.Namespace}
	url := e.Links.Run(r.TaskUID)
	if e.Messages == nil && url == "" {
		return notice.PRFailedText(f), nil
	}
	repo, err := e.visibility(ctx, t)
	if err != nil || !repo.Private {
		return notice.PRFailedText(f), err
	}
	f.RunURL = url
	if e.Messages != nil {
		if f.Message, err = e.Messages(ctx, name); err != nil {
			return "", err
		}
	}
	return notice.PRFailedText(f), nil
}

// gaveUpText is a PR run's gave-up notice, the Vikunja task's text:
// the limit that stopped it, and the restarts allowed.
func (e *Executor) gaveUpText(ctx context.Context, name string) (string, error) {
	lt := ""
	if e.Limits != nil {
		var err error
		if lt, err = e.Limits(ctx, name); err != nil {
			return "", err
		}
	}
	return notice.GaveUpText(name, lt, cmp.Or(e.MaxRestarts, restart.DefaultMaxRestarts)), nil
}
