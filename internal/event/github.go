package event

import (
	"encoding/json"
	"fmt"
)

// GitHub delivery headers.
const (
	GitHubEventHeader    = "X-GitHub-Event"
	GitHubDeliveryHeader = "X-GitHub-Delivery"
)

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
	Comment *struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Review *struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"review"`
	PullRequest *githubPullRequest `json:"pull_request"`
}

type githubPullRequest struct {
	Number int        `json:"number"`
	State  string     `json:"state"`
	Draft  bool       `json:"draft"`
	Merged bool       `json:"merged"`
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
	gh := &GitHub{Repository: raw.Repository.FullName}
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
		Repository: repo, Number: p.Number, State: p.State, Draft: p.Draft,
		Merged: p.Merged, Author: p.User.Login, HeadRef: p.Head.Ref,
		HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref,
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
		gh.CommentKind, gh.CommentID, gh.Body = CommentKindIssue, raw.Comment.ID, raw.Comment.Body
		gh.OnPullRequest = raw.Issue.PullRequest != nil
		gh.PullRequest = PullRequest{Repository: gh.Repository, Number: raw.Issue.Number}
	case GitHubReviewComment:
		if raw.Comment == nil || raw.PullRequest == nil {
			return fmt.Errorf("%w: review comment without comment or pull_request", ErrMalformed)
		}
		gh.CommentKind, gh.CommentID, gh.Body = CommentKindInline, raw.Comment.ID, raw.Comment.Body
	case GitHubReview:
		if raw.Review == nil || raw.PullRequest == nil {
			return fmt.Errorf("%w: review without review or pull_request", ErrMalformed)
		}
		gh.CommentKind, gh.CommentID, gh.Body = CommentKindReview, raw.Review.ID, raw.Review.Body
	case GitHubPullRequest:
		if raw.PullRequest == nil {
			return fmt.Errorf("%w: pull_request event without pull_request", ErrMalformed)
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
