package ghlife

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/worker"
)

// Executor performs the GitHub actions (the outbox's Executor).
type Executor struct {
	W      *Writer
	Policy profile.Policy
	Runs   lifecycle.Runs
	Lock   lifecycle.Locker
	Links  lifecycle.Links
}

// handler is one kind: applied checks whether the write is already
// there (nothing left to do), write makes it.
type handler struct {
	applied func(ctx context.Context, a action.Action, t Target) (bool, error)
	write   func(ctx context.Context, a action.Action, t Target) error
}

func (e *Executor) handler(k action.Kind) (handler, bool) {
	switch k {
	case action.React:
		return handler{e.reactApplied, e.react}, true
	case action.AddLabel:
		return handler{e.labelApplied, e.addLabel}, true
	case action.ConvertDraft:
		return handler{e.draftApplied, e.convertDraft}, true
	case action.RemoveLabel:
		return handler{e.unlabelApplied, e.removeLabel}, true
	case action.SkippedChecks:
		return handler{e.checkApplied, e.postCheck}, true
	case action.PostNotice:
		return handler{e.noticeApplied, e.postNotice}, true // checks again under the target lock
	}
	return handler{}, false
}

// Execute makes the write unless it is already there.
func (e *Executor) Execute(ctx context.Context, a action.Action) error {
	h, ok := e.handler(a.Kind)
	if !ok {
		return worker.Permanent(fmt.Errorf("no GitHub writer for %s", a.Kind))
	}
	t, err := ParseTarget(a)
	if err != nil {
		return err
	}
	done, err := h.applied(ctx, a, t)
	if err != nil || done {
		return err
	}
	return h.write(ctx, a, t)
}

// Recover reports whether the write is there.
func (e *Executor) Recover(ctx context.Context, a action.Action) (bool, error) {
	h, ok := e.handler(a.Kind)
	if !ok {
		return false, nil
	}
	t, err := ParseTarget(a)
	if err != nil {
		return false, err
	}
	return h.applied(ctx, a, t)
}

func pullTarget(a action.Action, t Target) error {
	if t.Kind != "pull" {
		return worker.Permanent(fmt.Errorf("%s needs a pull request target: %s", a.Kind, a.Target))
	}
	return nil
}

// reactions maps the router's reaction names to GraphQL's content.
var reactions = map[string]string{"eyes": "EYES"}

const reactedQuery = `query($id: ID!) { node(id: $id) { ... on Reactable { reactionGroups { content viewerHasReacted } } } }`

const addReaction = `mutation($subject: ID!, $content: ReactionContent!) { addReaction(input: {subjectId: $subject, content: $content}) { reaction { content } } }`

func reaction(a action.Action) (string, error) {
	content, ok := reactions[a.Value]
	if !ok || a.Detail == "" {
		return "", worker.Permanent(fmt.Errorf("reaction %q on node %q", a.Value, a.Detail))
	}
	return content, nil
}

// reactApplied reports the App's reaction on the comment's or review's
// node (its Detail): addReaction is a no-op once it is there.
func (e *Executor) reactApplied(ctx context.Context, a action.Action, t Target) (bool, error) {
	content, err := reaction(a)
	if err != nil {
		return false, err
	}
	var out struct {
		Node *struct {
			ReactionGroups []reactionGroup `json:"reactionGroups"`
		} `json:"node"`
	}
	if err := e.W.GraphQL(ctx, t.Repo, githubapp.PullRequests, reactedQuery, map[string]any{"id": a.Detail}, &out, false); err != nil {
		return false, err
	}
	if out.Node == nil {
		return false, worker.Permanent(fmt.Errorf("no reactable node %s", a.Detail))
	}
	return slices.ContainsFunc(out.Node.ReactionGroups, func(g reactionGroup) bool {
		return g.Content == content && g.ViewerHasReacted
	}), nil
}

type reactionGroup struct {
	Content          string `json:"content"`
	ViewerHasReacted bool   `json:"viewerHasReacted"`
}

func (e *Executor) react(ctx context.Context, a action.Action, t Target) error {
	content, _ := reaction(a)
	return e.W.GraphQL(ctx, t.Repo, githubapp.PullRequests, addReaction,
		map[string]any{"subject": a.Detail, "content": content}, nil, true)
}

const prStateQuery = `query($owner: String!, $name: String!, $number: Int!, $label: String!) { repository(owner: $owner, name: $name) { ` +
	`pullRequest(number: $number) { id isDraft labels(first: 100) { nodes { name } } } label(name: $label) { id } } }`

// prState is a pull request's draft flag and labels, and the id of a
// repository label (github_reactor.PULL_REQUEST_STATE).
type prState struct {
	ID      string
	Draft   bool
	Labels  []string
	LabelID string
}

func (e *Executor) state(ctx context.Context, t Target, label string, perms map[string]string) (prState, error) {
	var out struct {
		Repository *struct {
			PullRequest *struct {
				ID      string `json:"id"`
				IsDraft bool   `json:"isDraft"`
				Labels  struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"pullRequest"`
			Label *struct {
				ID string `json:"id"`
			} `json:"label"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": t.Owner, "name": t.Name, "number": t.N, "label": label}
	if err := e.W.GraphQL(ctx, t.Repo, perms, prStateQuery, vars, &out, false); err != nil {
		return prState{}, err
	}
	if out.Repository == nil || out.Repository.PullRequest == nil {
		return prState{}, worker.Permanent(fmt.Errorf("pull request %s#%d not found", t.FullName(), t.N))
	}
	pr := out.Repository.PullRequest
	st := prState{ID: pr.ID, Draft: pr.IsDraft}
	for _, l := range pr.Labels.Nodes {
		st.Labels = append(st.Labels, l.Name)
	}
	if out.Repository.Label != nil {
		st.LabelID = out.Repository.Label.ID
	}
	return st, nil
}

// labelApplied: the label is on, or the PR is a draft already, which the
// reactor leaves alone (github_reactor.GitHub.draft), label included.
func (e *Executor) labelApplied(ctx context.Context, a action.Action, t Target) (bool, error) {
	if err := pullTarget(a, t); err != nil {
		return false, err
	}
	st, err := e.state(ctx, t, a.Value, githubapp.PullRequests)
	return err == nil && (st.Draft || slices.Contains(st.Labels, a.Value)), err
}

const addLabels = `mutation($pull: ID!, $label: ID!) { addLabelsToLabelable(input: {labelableId: $pull, labelIds: [$label]}) { clientMutationId } }`

// addLabel adds a label the repository already has: REST would create a
// missing one, the reactor refuses instead (and drafts nothing).
func (e *Executor) addLabel(ctx context.Context, a action.Action, t Target) error {
	st, err := e.state(ctx, t, a.Value, githubapp.PullRequests)
	if err != nil {
		return err
	}
	if st.LabelID == "" {
		return worker.Permanent(fmt.Errorf("label %q missing from %s, not drafted", a.Value, t.FullName()))
	}
	return e.W.GraphQL(ctx, t.Repo, githubapp.PullRequests, addLabels, map[string]any{"pull": st.ID, "label": st.LabelID}, nil, true)
}

func (e *Executor) draftApplied(ctx context.Context, a action.Action, t Target) (bool, error) {
	if err := pullTarget(a, t); err != nil {
		return false, err
	}
	st, err := e.state(ctx, t, "", githubapp.Draft)
	return err == nil && st.Draft, err
}

const convertToDraft = `mutation($pull: ID!) { convertPullRequestToDraft(input: {pullRequestId: $pull}) { pullRequest { isDraft } } }`

func (e *Executor) convertDraft(ctx context.Context, a action.Action, t Target) error {
	st, err := e.state(ctx, t, "", githubapp.Draft)
	if err != nil {
		return err
	}
	return e.W.GraphQL(ctx, t.Repo, githubapp.Draft, convertToDraft, map[string]any{"pull": st.ID}, nil, true)
}

func (e *Executor) unlabelApplied(ctx context.Context, a action.Action, t Target) (bool, error) {
	if err := pullTarget(a, t); err != nil {
		return false, err
	}
	st, err := e.state(ctx, t, a.Value, githubapp.PullRequests)
	return err == nil && !slices.Contains(st.Labels, a.Value), err
}

// removeLabel removes the label; one the PR lacks is no error
// (cap_review_rounds.GitHub.remove_label).
func (e *Executor) removeLabel(ctx context.Context, a action.Action, t Target) error {
	p := path("repos", t.Owner, t.Name, "issues", fmt.Sprint(t.N), "labels", a.Value)
	err := e.W.REST(ctx, t.Repo, githubapp.PullRequests, http.MethodDelete, p, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// checkSpec parses a skipped check's value, <check>@<head sha>.
func checkSpec(a action.Action) (string, string, error) {
	i := strings.LastIndex(a.Value, "@")
	if i <= 0 || i == len(a.Value)-1 {
		return "", "", worker.Permanent(fmt.Errorf("skipped check %q", a.Value))
	}
	return a.Value[:i], a.Value[i+1:], nil
}

// checkApplied: a completed run of that check is on the head already. A
// redelivery to the reactor posted it again; GitHub keeps the latest run
// of a name, so one is enough.
func (e *Executor) checkApplied(ctx context.Context, a action.Action, t Target) (bool, error) {
	if err := pullTarget(a, t); err != nil {
		return false, err
	}
	check, sha, err := checkSpec(a)
	if err != nil {
		return false, err
	}
	var out struct {
		CheckRuns []checkRun `json:"check_runs"`
	}
	p := path("repos", t.Owner, t.Name, "commits", sha, "check-runs") + "?check_name=" + url.QueryEscape(check)
	if err := e.W.REST(ctx, t.Repo, githubapp.Checks, http.MethodGet, p, nil, &out); err != nil {
		return false, err
	}
	return slices.ContainsFunc(out.CheckRuns, func(r checkRun) bool { return r.Status == checkCompleted }), nil
}

type checkRun struct {
	Status string `json:"status"`
}

// checkCompleted is a finished check run's status.
const checkCompleted = "completed"

// postCheck posts review_skip.check_run_body.
func (e *Executor) postCheck(ctx context.Context, a action.Action, t Target) error {
	check, sha, _ := checkSpec(a)
	kind, value, ok := strings.Cut(a.Detail, ":")
	if !ok || value == "" {
		return worker.Permanent(fmt.Errorf("skipped check reason %q", a.Detail))
	}
	body := map[string]any{"name": check, "head_sha": sha, "status": checkCompleted, "conclusion": "skipped",
		"output": map[string]string{"title": "Not reviewed", "summary": SkippedSummary(check, kind, value)}}
	return e.W.REST(ctx, t.Repo, githubapp.Checks, http.MethodPost, path("repos", t.Owner, t.Name, "check-runs"), body, nil)
}

// SkippedSummary is review_skip.check_run_body's summary.
func SkippedSummary(check, kind, value string) string {
	which := fmt.Sprintf("labelled `%s`", value)
	if kind == "author" {
		which = fmt.Sprintf("opened by `%s`", value)
	}
	return fmt.Sprintf("Pull requests %s are not reviewed. This check is skipped so that the required `%s` check does not hold the merge.",
		which, check)
}
