package route

import (
	"regexp"
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// GitHub routing reasons.
const (
	ReasonRepositoryNotConfigured = "repository-not-configured"
	ReasonNotAPullRequest         = "not-a-pull-request"
	ReasonAuthorNotAllowed        = "pull-request-author-not-allowed"
	ReasonPullRequestClosed       = "pull-request-closed"
	ReasonMultipleVariantLabels   = "multiple-variant-labels"
	ReasonActionNotRouted         = "action-not-routed"
	// ReasonBadBotPattern refuses a review bot's review whose configured
	// pattern does not compile, instead of panicking on the delivery.
	ReasonBadBotPattern = "review-bot-pattern-invalid"
)

// requestActions are the comment and review actions that carry a request.
var requestActions = map[event.Kind]string{
	event.GitHubIssueComment:  "created",
	event.GitHubReviewComment: "created",
	event.GitHubReview:        "submitted",
}

// prComment routes a PR follow-up request: the owner's `/gordon` comment,
// inline comment or review, or a review bot's findings
// (followupspawners.yaml).
func (r Router) prComment(ev event.Event, st State, led Ledger) Decision {
	gh := ev.GitHub
	proj, ok := r.Config.PRProject(gh.Repository)
	switch {
	case !ok:
		return Decision{Outcome: Ignore, Reason: ReasonRepositoryNotConfigured}
	case requestActions[ev.Kind] != ev.Action:
		return Decision{Outcome: Ignore, Reason: ReasonActionNotRouted}
	case !gh.OnPullRequest:
		return Decision{Outcome: Ignore, Reason: ReasonNotAPullRequest}
	}
	owner, why := r.classify(ev, st)
	if why != "" {
		o := Ignore
		if why == ReasonNotOwner || why == ReasonBadBotPattern {
			o = Reject
		}
		return Decision{Outcome: o, Reason: why}
	}
	if led.HandledRequest(ev.RequestKey) {
		return Decision{Outcome: Duplicate, Reason: ReasonRequestHandled}
	}
	if st.PullRequest == nil {
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	}
	fd := r.family(run.PRFollowUp, proj.Name)
	pr := *st.PullRequest
	variant, ok := r.prGate(&fd, gh.PullRequest, pr)
	if !ok {
		return Decision{Families: []FamilyDecision{fd}}
	}
	kind := string(gh.CommentKind)
	id := run.Identity{Family: run.PRFollowUp, Project: proj.Name, Workspace: proj.Workspace, Variant: variant,
		PR: pr.Number, CommentKind: kind, CommentID: gh.CommentID, HeadRef: pr.HeadRef}
	if r.submit(&fd, led, id, profile.Request{Project: proj.Name, Variant: variant, Mode: profile.ModeFollowUp}) {
		r.followUpWrites(&fd, owner, gh, pr)
	}
	return Decision{Families: []FamilyDecision{fd}}
}

// classify tells an owner request (true) from a bot's findings (false);
// a non-empty reason means the event is no request.
func (r Router) classify(ev event.Event, st State) (bool, string) {
	p, gh := r.Config.Policy, ev.GitHub
	if isRequest(gh.Body, p.Trigger, false) {
		if ev.Actor != p.GitHubOwner {
			return false, ReasonNotOwner
		}
		return true, ""
	}
	if ev.Kind != event.GitHubReview {
		return false, ReasonNotARequest
	}
	labels := gh.PullRequest.Labels
	if st.PullRequest != nil {
		labels = st.PullRequest.Labels
	}
	for _, bot := range p.ReviewBots {
		if bot.Login != ev.Actor {
			continue
		}
		found, err := botFindings(bot, gh.Body, slices.Contains(labels, p.FixSuggestionsLabel))
		switch {
		case err != nil:
			return false, ReasonBadBotPattern
		case found:
			return false, ""
		}
	}
	return false, ReasonNotARequest
}

func botFindings(bot profile.Bot, body string, fixSuggestions bool) (bool, error) {
	found, err := matches(bot.FindingsPattern, body)
	if err != nil || found || !fixSuggestions || bot.SuggestionsPattern == "" {
		return found, err
	}
	return matches(bot.SuggestionsPattern, body)
}

func matches(pattern, text string) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(text), nil
}

// prGate checks the fresh pull request and picks its variant.
func (r Router) prGate(fd *FamilyDecision, sent, pr event.PullRequest) (string, bool) {
	p := r.Config.Policy
	switch {
	case !slices.Contains(p.PullRequestAuthors, pr.Author):
		fd.Outcome, fd.Reason = Reject, ReasonAuthorNotAllowed
		return "", false
	case pr.State != event.PullRequestOpen:
		fd.Outcome, fd.Reason = Ignore, ReasonPullRequestClosed
		if sent.State == "" || sent.State == event.PullRequestOpen {
			fd.Outcome = Stale
		}
		return "", false
	}
	variant, ok := r.prVariant(pr)
	if !ok {
		fd.Outcome, fd.Reason = Conflict, ReasonMultipleVariantLabels
	}
	return variant, ok
}

// prVariant is the PR's one non-default `kelos:<variant>` label, else the
// default variant. Two such labels are a conflict: legacy started
// whichever spawner won the Task-name race.
func (r Router) prVariant(pr event.PullRequest) (string, bool) {
	var found []string
	for _, l := range pr.Labels {
		name, ok := strings.CutPrefix(l, r.Config.Policy.VariantLabelPrefix)
		if ok && name != r.Config.DefaultVariant && r.Config.IsAgent(name) {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return r.Config.DefaultVariant, true
	case 1:
		return found[0], true
	default:
		return "", false
	}
}

// followUpWrites adds the reactor's writes: 👀 on an owner request, and
// the draft label and draft conversion while the follow-up runs. A review
// gets its 👀 too: the REST API has no reactions on a submitted review,
// so the worker uses GraphQL addReaction on its node id, as
// github_reactor.py does.
func (r Router) followUpWrites(fd *FamilyDecision, owner bool, gh *event.GitHub, pr event.PullRequest) {
	if owner {
		target := action.GitHubComment(gh.Repository, string(gh.CommentKind), gh.CommentID)
		fd.Actions = append(fd.Actions, action.New(action.React, target, reactionEyes))
	}
	if label := r.Config.Policy.DraftLabel; label != "" && !pr.Draft {
		target := action.PullRequest(gh.Repository, pr.Number)
		fd.Actions = append(fd.Actions, action.New(action.AddLabel, target, label),
			action.New(action.ConvertDraft, target, ""))
	}
}
