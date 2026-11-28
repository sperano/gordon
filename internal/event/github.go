package event

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// GitHub delivery headers.
const (
	GitHubEventHeader    = "X-GitHub-Event"
	GitHubDeliveryHeader = "X-GitHub-Delivery"
	// SyntheticHeader marks a delivery the worker queued itself; no
	// webhook carries it (the receiver stores none it does not set).
	SyntheticHeader = "X-Gordon-Synthetic"
)

// ActionConflict is the pull_request action of the conflict
// reconciliation's synthetic delivery: a conflict it confirmed. GitHub
// sends no such action, and only a synthetic delivery is routed with it.
const ActionConflict = "gordon-conflict"

// ActionReadiness is the pull_request action of the readiness
// reconciliation's synthetic delivery: an open agent pull request whose
// head is due a readiness verdict. Only a synthetic delivery is routed
// with it.
const ActionReadiness = "gordon-readiness"

// ActionCompleted is a check_run's completed action, the only one the
// readiness family routes.
const ActionCompleted = "completed"

// Legacy follow-up Task-name letters by comment kind.
const (
	CommentKindIssue  byte = 'c'
	CommentKindInline byte = 'l'
	CommentKindReview byte = 'r'
)

type githubUser struct {
	Login string `json:"login"`
}

type githubBody struct {
	Action     string     `json:"action"`
	Sender     githubUser `json:"sender"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue *struct {
		Number int `json:"number"`
		// PullRequest marks an issue that is a pull request; decoding it
		// as an object makes any other value malformed.
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Comment     *githubComment     `json:"comment"`
	Review      *githubComment     `json:"review"`
	PullRequest *githubPullRequest `json:"pull_request"`
	CheckRun    *struct {
		HeadSHA string `json:"head_sha"`
	} `json:"check_run"`
	// A push's fields.
	Ref     string         `json:"ref"`
	After   string         `json:"after"`
	Deleted bool           `json:"deleted"`
	Commits []githubCommit `json:"commits"`
	// Gordon is what a synthetic delivery adds: the Workspace whose
	// conflict reconciliation found the conflict.
	Gordon *struct {
		Workspace string `json:"workspace"`
	} `json:"gordon"`
}

// githubCommit is one commit of a push.
type githubCommit struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
}

// githubComment is a comment or a submitted review.
type githubComment struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id"`
	Body     string `json:"body"`
	HTMLURL  string `json:"html_url"`
	ReviewID int64  `json:"pull_request_review_id"`
	CommitID string `json:"commit_id"`
}

type githubPullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	// Merged is a pointer so that a closed delivery without it is
	// refused instead of read as unmerged.
	Merged *bool      `json:"merged"`
	User   githubUser `json:"user"`
	Head   githubRef  `json:"head"`
	Base   githubRef  `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type githubRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

// closedAction is the pull_request action whose merged flag decides the
// merge-close path.
const closedAction = "closed"

var githubKinds = map[string]Kind{
	"issue_comment":               GitHubIssueComment,
	"pull_request_review":         GitHubReview,
	"pull_request_review_comment": GitHubReviewComment,
	"pull_request":                GitHubPullRequest,
	"check_run":                   GitHubCheckRun,
	"push":                        GitHubPush,
}

// ParseGitHub parses an authenticated GitHub delivery. Its delivery key is
// the X-GitHub-Delivery GUID, which a redelivery from GitHub keeps; a
// comment's or review's request key is its id, so the same request seen
// through another delivery is still one request.
func ParseGitHub(d Delivery) (Event, error) {
	kind, ok := githubKinds[d.Header(GitHubEventHeader)]
	if !ok {
		return Event{}, fmt.Errorf("%w: github %q", ErrUnsupported, d.Header(GitHubEventHeader))
	}
	guid := d.Header(GitHubDeliveryHeader)
	if guid == "" {
		return Event{}, fmt.Errorf("%w: %s missing", ErrMalformed, GitHubDeliveryHeader)
	}
	var raw githubBody
	if err := json.Unmarshal(d.Body, &raw); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if raw.Repository.FullName == "" {
		return Event{}, fmt.Errorf("%w: repository.full_name missing", ErrMalformed)
	}
	gh := &GitHub{Repository: raw.Repository.FullName, Synthetic: d.Header(SyntheticHeader) != ""}
	if gh.Synthetic && raw.Gordon != nil {
		gh.ConflictWorkspace = raw.Gordon.Workspace
	}
	ev := Event{
		Source: SourceGitHub, Kind: kind, Action: raw.Action,
		DeliveryKey: "github/delivery/" + guid, Actor: raw.Sender.Login, GitHub: gh,
	}
	if raw.PullRequest != nil {
		if raw.PullRequest.Number <= 0 {
			return Event{}, fmt.Errorf("%w: pull_request.number missing", ErrMalformed)
		}
		gh.OnPullRequest = true
		gh.PullRequest = pullRequest(raw.Repository.FullName, raw.PullRequest)
	}
	if err := request(&ev, raw); err != nil {
		return Event{}, err
	}
	return ev, nil
}

func pullRequest(repo string, p *githubPullRequest) PullRequest {
	pr := PullRequest{
		Repository: repo, Number: p.Number, Title: p.Title, URL: p.HTMLURL, State: p.State, Draft: p.Draft,
		Merged: p.Merged != nil && *p.Merged, Author: p.User.Login, HeadRef: p.Head.Ref,
		HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref, BaseSHA: p.Base.SHA,
	}
	if p.Head.Repo != nil {
		pr.HeadRepo = p.Head.Repo.FullName
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr
}

// request fills the comment fields and the request key of a comment or
// review event.
func request(ev *Event, raw githubBody) error {
	gh := ev.GitHub
	switch ev.Kind {
	case GitHubIssueComment:
		if raw.Comment == nil || raw.Issue == nil {
			return fmt.Errorf("%w: issue_comment without comment or issue", ErrMalformed)
		}
		gh.CommentKind, gh.CommentID, gh.CommentNodeID, gh.Body = CommentKindIssue, raw.Comment.ID, raw.Comment.NodeID, raw.Comment.Body
		gh.CommentURL = raw.Comment.HTMLURL
		gh.OnPullRequest = raw.Issue.PullRequest != nil
		gh.PullRequest = PullRequest{Repository: gh.Repository, Number: raw.Issue.Number}
	case GitHubReviewComment:
		if raw.Comment == nil || raw.PullRequest == nil {
			return fmt.Errorf("%w: review comment without comment or pull_request", ErrMalformed)
		}
		gh.CommentKind, gh.CommentID, gh.CommentNodeID, gh.Body = CommentKindInline, raw.Comment.ID, raw.Comment.NodeID, raw.Comment.Body
		gh.CommentURL, gh.ReviewID = raw.Comment.HTMLURL, raw.Comment.ReviewID
	case GitHubReview:
		if raw.Review == nil || raw.PullRequest == nil {
			return fmt.Errorf("%w: review without review or pull_request", ErrMalformed)
		}
		gh.CommentKind, gh.CommentID, gh.CommentNodeID, gh.Body = CommentKindReview, raw.Review.ID, raw.Review.NodeID, raw.Review.Body
		gh.CommentURL, gh.ReviewCommitID = raw.Review.HTMLURL, raw.Review.CommitID
	case GitHubCheckRun:
		if raw.CheckRun == nil || raw.CheckRun.HeadSHA == "" {
			return fmt.Errorf("%w: check_run without head_sha", ErrMalformed)
		}
		gh.CheckRunHeadSHA = raw.CheckRun.HeadSHA
		return nil
	case GitHubPush:
		return push(gh, raw)
	case GitHubPullRequest:
		if raw.PullRequest == nil {
			return fmt.Errorf("%w: pull_request event without pull_request", ErrMalformed)
		}
		if raw.Action == closedAction && raw.PullRequest.Merged == nil {
			return fmt.Errorf("%w: closed pull_request without merged", ErrMalformed)
		}
		ev.RequestKey = fmt.Sprintf("github/%s/pull/%d/%s/%s", gh.Repository,
			gh.PullRequest.Number, raw.Action, gh.PullRequest.HeadSHA)
		return nil
	default:
		return nil
	}
	if gh.CommentID <= 0 || gh.PullRequest.Number <= 0 {
		return fmt.Errorf("%w: %s without a comment, review or issue id", ErrMalformed, ev.Kind)
	}
	ev.RequestKey = fmt.Sprintf("github/%s/%c/%d", gh.Repository, gh.CommentKind, gh.CommentID)
	return nil
}

// commitSHA is a full commit id: SHA-1 or SHA-256, lowercase hex.
var commitSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// push fills a push's ref, commit and changed files. A push is no
// request: its delivery key alone deduplicates it.
func push(gh *GitHub, raw githubBody) error {
	if raw.Ref == "" || !commitSHA.MatchString(raw.After) {
		return fmt.Errorf("%w: push without a ref or a full after commit", ErrMalformed)
	}
	p := &Push{Ref: raw.Ref, After: raw.After, Deleted: raw.Deleted}
	seen := map[string]bool{}
	for _, c := range raw.Commits {
		for _, batch := range [][]string{c.Added, c.Removed, c.Modified} {
			for _, f := range batch {
				if !seen[f] {
					seen[f] = true
					p.Files = append(p.Files, f)
				}
			}
		}
	}
	gh.Push = p
	return nil
}
