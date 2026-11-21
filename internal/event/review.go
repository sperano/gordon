package event

// Review is a submitted pull-request review, as GitHub lists it.
type Review struct {
	ID       int64  `json:"id"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	CommitID string `json:"commit_id,omitempty"`
}

// IssueComment is a comment of a pull request's conversation.
type IssueComment struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}
