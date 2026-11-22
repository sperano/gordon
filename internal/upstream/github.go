package upstream

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/event"
)

// GitHub reads pull requests.
type GitHub struct{ Client *Client }

// GitHubAPIVersion is the REST API version Gordon reads.
const GitHubAPIVersion = "2022-11-28"

// GitHubHeaders are the headers every GitHub read sends.
var GitHubHeaders = map[string]string{
	"Accept":               "application/vnd.github+json",
	"X-GitHub-Api-Version": GitHubAPIVersion,
}

// filesPerPage is the page size of the files list; maxFilePages stops at
// GitHub's own limit of 3000 files, past which the list is incomplete and
// the read fails rather than decide on part of it.
const (
	filesPerPage = 100
	maxFilePages = 30
)

// repository is an owner/name pair; anything else (a path segment, a
// query) is refused before it reaches a URL.
var repository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// SafeRepository reports an owner/name pair neither of whose segments is
// a dot segment ("." or ".."), which the URL path would resolve away.
func SafeRepository(repo string) bool {
	if !repository.MatchString(repo) {
		return false
	}
	owner, name, _ := strings.Cut(repo, "/")
	for _, seg := range []string{owner, name} {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

type githubPR struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	HTMLURL      string `json:"html_url"`
	ChangedFiles int    `json:"changed_files"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	Merged       bool   `json:"merged"`
	User         struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// Mergeable is null while GitHub computes it (a single pull request's
	// read only: listings never carry it).
	Mergeable      *bool     `json:"mergeable"`
	MergeableState string    `json:"mergeable_state"`
	CreatedAt      time.Time `json:"created_at"`
}

// PullRequest reads a pull request; withFiles also lists its changed
// files (the reviewers' file patterns need them).
func (g GitHub) PullRequest(ctx context.Context, repo string, number int, withFiles bool) (event.PullRequest, error) {
	if !SafeRepository(repo) || number <= 0 {
		return event.PullRequest{}, &Error{Provider: ProviderGitHub, Class: Invalid,
			Err: fmt.Errorf("pull request %q#%d", repo, number)}
	}
	base := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	var raw githubPR
	err := g.Client.get(ctx, base, nil, &raw, func() error {
		if raw.Number != number {
			return fmt.Errorf("asked for pull request %s#%d, got %d", repo, number, raw.Number)
		}
		return nil
	})
	if err != nil {
		return event.PullRequest{}, err
	}
	pr := event.PullRequest{Repository: repo, Number: raw.Number, Title: raw.Title, URL: raw.HTMLURL,
		State: raw.State, Draft: raw.Draft,
		Merged: raw.Merged, Author: raw.User.Login, HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA,
		BaseRef: raw.Base.Ref, BaseSHA: raw.Base.SHA, Mergeable: raw.Mergeable, MergeableState: raw.MergeableState,
		CreatedAt: raw.CreatedAt}
	if raw.Head.Repo != nil {
		pr.HeadRepo = raw.Head.Repo.FullName
	}
	for _, l := range raw.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	if !withFiles {
		return pr, nil
	}
	files, err := g.files(ctx, base)
	if err != nil {
		return event.PullRequest{}, err
	}
	if len(files) < raw.ChangedFiles {
		return event.PullRequest{}, &Error{Provider: ProviderGitHub, Class: Invalid,
			Err: fmt.Errorf("pull request %s#%d lists %d of its %d files", repo, number, len(files), raw.ChangedFiles)}
	}
	pr.Files = files
	return pr, nil
}

func (g GitHub) files(ctx context.Context, base string) ([]string, error) {
	var out []string
	for page := 1; page <= maxFilePages; page++ {
		var batch []struct {
			Filename string `json:"filename"`
		}
		q := url.Values{"per_page": {strconv.Itoa(filesPerPage)}, "page": {strconv.Itoa(page)}}
		if err := g.Client.get(ctx, base+"/files", q, &batch, nil); err != nil {
			return nil, err
		}
		for _, f := range batch {
			out = append(out, f.Filename)
		}
		if len(batch) < filesPerPage {
			break
		}
	}
	return out, nil
}
