package ghlife

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

// The read scopes of the readiness reads: the read-only token lacks
// Checks, so the gate reads as the App (reconcile_pr_ready.GITHUB_PERMISSIONS).
var (
	checksRead   = map[string]string{"checks": "read"}
	contentsRead = map[string]string{"contents": "read"}
	pullsRead    = map[string]string{"pull_requests": "read"}
)

// Page sizes and bounds of the readiness reads: past maxReadPages the
// read fails rather than decide on part of the list.
const (
	readPageSize = 100
	maxReadPages = 30
)

// sha is a commit SHA as GitHub names one.
var sha = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Reads are the readiness gate's GitHub reads, as the App
// (worker.ReadinessReader).
type Reads struct{ W *Writer }

func repoOf(repo string) (Repo, error) {
	owner, name, _ := strings.Cut(repo, "/")
	if !upstream.SafeRepository(repo) {
		return Repo{}, worker.Permanent(fmt.Errorf("repository %q", repo))
	}
	return Repo{owner, name}, nil
}

func headOf(repo, commit string) (Repo, error) {
	r, err := repoOf(repo)
	if err == nil && !sha.MatchString(commit) {
		err = worker.Permanent(fmt.Errorf("commit %q", commit))
	}
	return r, err
}

type checkRunJSON struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  *string   `json:"conclusion"`
	CompletedAt time.Time `json:"completed_at"`
}

// CheckRuns lists the check runs of commit, every page
// (reconcile_pr_ready.GitHub.check_runs).
func (r Reads) CheckRuns(ctx context.Context, repo, commit string) ([]event.CheckRun, error) {
	rp, err := headOf(repo, commit)
	if err != nil {
		return nil, err
	}
	var out []event.CheckRun
	for page := 1; page <= maxReadPages; page++ {
		var answer struct {
			Total int            `json:"total_count"`
			Runs  []checkRunJSON `json:"check_runs"`
		}
		p := path("repos", rp.Owner, rp.Name, "commits", commit, "check-runs") +
			fmt.Sprintf("?per_page=%d&page=%d", readPageSize, page)
		if err := r.W.REST(ctx, rp, checksRead, http.MethodGet, p, nil, &answer); err != nil {
			return nil, err
		}
		for _, c := range answer.Runs {
			run := event.CheckRun{ID: c.ID, Name: c.Name, Status: c.Status, CompletedAt: c.CompletedAt}
			if c.Conclusion != nil {
				run.Conclusion = *c.Conclusion
			}
			out = append(out, run)
		}
		if len(answer.Runs) == 0 || len(out) >= answer.Total {
			return out, nil
		}
	}
	return nil, fmt.Errorf("check runs of %s@%s: over %d pages", repo, commit, maxReadPages)
}

// Committed is the commit's committer date (missing_checks.head_since).
func (r Reads) Committed(ctx context.Context, repo, commit string) (time.Time, error) {
	rp, err := headOf(repo, commit)
	if err != nil {
		return time.Time{}, err
	}
	var answer struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	err = r.W.REST(ctx, rp, contentsRead, http.MethodGet, path("repos", rp.Owner, rp.Name, "commits", commit), nil, &answer)
	return answer.Commit.Committer.Date, err
}

// reviewThreadsQuery is poll_review_bot.REVIEW_THREADS_QUERY.
const reviewThreadsQuery = `query($owner: String!, $repo: String!, $number: Int!,
                              $after: String, $pageSize: Int!, $commentLimit: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: $pageSize, after: $after) {
        nodes {
          isResolved
          comments(first: $commentLimit) {
            nodes { body author { login } url }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

type threadsJSON struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				Nodes []struct {
					IsResolved bool `json:"isResolved"`
					Comments   struct {
						Nodes []struct {
							Body   string `json:"body"`
							URL    string `json:"url"`
							Author *struct {
								Login string `json:"login"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// ReviewThreads lists the pull request's review threads with their first
// comment; a thread without one is left out.
func (r Reads) ReviewThreads(ctx context.Context, repo string, number int) ([]event.ReviewThread, error) {
	rp, err := repoOf(repo)
	if err != nil {
		return nil, err
	}
	out := []event.ReviewThread{}
	var cursor any
	for page := 1; page <= maxReadPages; page++ {
		var data threadsJSON
		vars := map[string]any{"owner": rp.Owner, "repo": rp.Name, "number": number, "after": cursor,
			"pageSize": readPageSize, "commentLimit": 1}
		if err := r.W.GraphQL(ctx, rp, pullsRead, reviewThreadsQuery, vars, &data, false); err != nil {
			return nil, err
		}
		threads := data.Repository.PullRequest.ReviewThreads
		for _, t := range threads.Nodes {
			if len(t.Comments.Nodes) == 0 {
				continue
			}
			c := t.Comments.Nodes[0]
			th := event.ReviewThread{Resolved: t.IsResolved, Body: c.Body, URL: c.URL}
			if c.Author != nil {
				th.Author = c.Author.Login
			}
			out = append(out, th)
		}
		if !threads.PageInfo.HasNextPage {
			return out, nil
		}
		if threads.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("review threads of %s#%d have no next cursor", repo, number)
		}
		cursor = threads.PageInfo.EndCursor
	}
	return nil, fmt.Errorf("review threads of %s#%d: over %d pages", repo, number, maxReadPages)
}
