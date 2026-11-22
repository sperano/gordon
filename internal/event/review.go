package event

import "time"

// Review is a submitted pull-request review, as GitHub lists it.
type Review struct {
	ID       int64  `json:"id"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	CommitID string `json:"commit_id,omitempty"`
	// SubmittedAt orders a bot's reviews (poll_review_bot.latest_review).
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
}

// IssueComment is a comment of a pull request's conversation.
type IssueComment struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}

// CheckRun is a check run on a commit, as GitHub lists it.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	// CompletedAt is zero until the run completed.
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

// CheckRunCompleted is GitHub's status of a finished check run.
const CheckRunCompleted = "completed"

// ReviewThread is a pull request's review thread with its first comment:
// who started it, what it says and its link.
type ReviewThread struct {
	Resolved bool   `json:"resolved"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	URL      string `json:"url"`
}
