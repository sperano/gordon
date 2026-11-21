package ghlife

import (
	"fmt"

	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

// The GitHub events the reconciliation recovers.
const (
	eventIssueComment  = "issue_comment"
	eventReview        = "pull_request_review"
	eventReviewComment = "pull_request_review_comment"
	eventPullRequest   = "pull_request"
)

// recoveryGUID names a recovery: one fixed key per recovered webhook, so
// the same recovery is queued once whichever pass finds it.
func recoveryGUID(repo, what string, id int64) string {
	return fmt.Sprintf("gordon-reconcile/%s/%s/%d", repo, what, id)
}

type user struct {
	Login string `json:"login"`
}

type label struct {
	Name string `json:"name"`
}

type ref struct {
	Ref  string    `json:"ref"`
	SHA  string    `json:"sha,omitempty"`
	Repo *repoName `json:"repo,omitempty"`
}

type repoName struct {
	FullName string `json:"full_name"`
}

type pullJSON struct {
	Number int     `json:"number"`
	Title  string  `json:"title,omitempty"`
	URL    string  `json:"html_url,omitempty"`
	State  string  `json:"state"`
	Draft  bool    `json:"draft"`
	Merged bool    `json:"merged"`
	User   user    `json:"user"`
	Head   ref     `json:"head"`
	Base   ref     `json:"base"`
	Labels []label `json:"labels"`
}

// pullOf is a listed pull request as a webhook payload carries it.
func pullOf(pr upstream.ListedPR) pullJSON {
	p := pullJSON{Number: pr.Number, Title: pr.Title, URL: pr.URL, State: pr.State, Draft: pr.Draft, Merged: pr.Merged,
		User: user{pr.Author}, Head: ref{Ref: pr.HeadRef, SHA: pr.HeadSHA}, Base: ref{Ref: pr.BaseRef}, Labels: []label{}}
	if pr.HeadRepo != "" {
		p.Head.Repo = &repoName{pr.HeadRepo}
	}
	for _, l := range pr.Labels {
		p.Labels = append(p.Labels, label{l})
	}
	return p
}

type commentJSON struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id,omitempty"`
	Body     string `json:"body"`
	URL      string `json:"html_url,omitempty"`
	CommitID string `json:"commit_id,omitempty"`
	ReviewID int64  `json:"pull_request_review_id,omitempty"`
}

type payload struct {
	Action      string       `json:"action"`
	Sender      user         `json:"sender"`
	Repository  repoName     `json:"repository"`
	Issue       *issueJSON   `json:"issue,omitempty"`
	Comment     *commentJSON `json:"comment,omitempty"`
	Review      *commentJSON `json:"review,omitempty"`
	PullRequest *pullJSON    `json:"pull_request,omitempty"`
}

type issueJSON struct {
	Number      int      `json:"number"`
	PullRequest struct{} `json:"pull_request"`
}

// reviewDelivery is a submitted review's pull_request_review delivery.
func reviewDelivery(pr upstream.ListedPR, rv upstream.ListedReview) (store.Delivery, error) {
	p := pullOf(pr)
	body := payload{Action: "submitted", Sender: user{rv.Author}, Repository: repoName{pr.Repository}, PullRequest: &p,
		Review: &commentJSON{ID: rv.ID, NodeID: rv.NodeID, Body: rv.Body, URL: rv.URL, CommitID: rv.CommitID}}
	return store.GitHubSynthetic(eventReview, recoveryGUID(pr.Repository, "r", rv.ID), body)
}

// commentDelivery is an owner comment's issue_comment delivery, or an
// inline comment's pull_request_review_comment delivery.
func commentDelivery(pr upstream.ListedPR, c upstream.ListedComment, inline bool) (store.Delivery, error) {
	body := payload{Action: "created", Sender: user{c.Author}, Repository: repoName{pr.Repository},
		Comment: &commentJSON{ID: c.ID, NodeID: c.NodeID, Body: c.Body, URL: c.URL, ReviewID: c.ReviewID}}
	if inline {
		p := pullOf(pr)
		body.PullRequest = &p
		return store.GitHubSynthetic(eventReviewComment, recoveryGUID(pr.Repository, "l", c.ID), body)
	}
	body.Issue = &issueJSON{Number: pr.Number}
	return store.GitHubSynthetic(eventIssueComment, recoveryGUID(pr.Repository, "c", c.ID), body)
}

// closedDelivery is a closed pull request's delivery, merged or not.
func closedDelivery(pr upstream.ListedPR) (store.Delivery, error) {
	p := pullOf(pr)
	body := payload{Action: "closed", Sender: user{pr.Author}, Repository: repoName{pr.Repository}, PullRequest: &p}
	return store.GitHubSynthetic(eventPullRequest, recoveryGUID(pr.Repository, "closed", int64(pr.Number)), body)
}
