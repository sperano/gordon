package route

import (
	"regexp"
	"slices"
	"strconv"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
)

// Review-round cap reasons (cap_review_rounds.py), decided before the
// submission instead of in the pod's init container.
const (
	ReasonRoundCap       = "review-round-cap"
	ReasonSuggestionsCap = "suggestions-round-cap"
)

// The round-cap notices.
const (
	NoticeReviewCap      = notice.ReviewCap
	NoticeSuggestionsCap = notice.SuggestionsCap
)

// reviewRunName is cap_review_rounds.REVIEW_TASK_NAME: a review follow-up
// run (the webhook's -r, the poller's -p), restarts included.
var reviewRunName = regexp.MustCompile(`^pr-followup-.+-(\d+)-[rp](\d+)(?:-retry\d+)?$`)

// startedRun is cap_review_rounds.START_COMMENT_TASK for one pull request.
func startedRun(number int) *regexp.Regexp {
	return regexp.MustCompile("`pr-followup-[^`]+-" + strconv.Itoa(number) + "-[rp](\\d+)(?:-retry\\d+)?`")
}

// capsOn reports whether a review-round cap is configured.
func (r Router) capsOn() bool {
	p := r.Config.Policy
	return p.MaxRoundsPerPR > 0 || r.suggestionsLimit() > 0
}

// suggestionsLimit is cap_review_rounds.suggestions_max_from: no
// suggestions cap without the fix-suggestions label.
func (r Router) suggestionsLimit() int {
	if r.Config.Policy.FixSuggestionsLabel == "" {
		return 0
	}
	return r.Config.Policy.SuggestionsMaxRounds
}

// roundCap stops a review bot's request once the pull request has taken
// its rounds (cap_review_rounds.check): a round is a bot review whose
// follow-up ran, named by a start notice from a pull-request author or by
// a run in the ledger (submitted, not announced yet). It reports whether
// the request is capped, the family then carrying the cap's writes: the
// notice once per pull request, and at the suggestions cap the removal
// of the fix-suggestions label. Nothing is drafted for a capped request,
// so there is no draft to release.
func (r Router) roundCap(fd *FamilyDecision, ev event.Event, h *PRHistory, led Ledger, proj profile.Project) bool {
	p, gh := r.Config.Policy, ev.GitHub
	bots := r.botPatterns()
	byID := map[int64]event.Review{}
	for _, rv := range h.Reviews {
		if _, ok := bots[rv.Author]; ok {
			byID[rv.ID] = rv
		}
	}
	current, ok := byID[gh.CommentID]
	if !ok { // not listed yet: the delivery is the review
		current = event.Review{ID: gh.CommentID, Author: ev.Actor, Body: gh.Body, CommitID: gh.ReviewCommitID}
	}
	taken := r.taken(h, led, proj, gh.PullRequest.Number, byID)
	delete(taken, gh.CommentID)
	target := action.PullRequest(gh.Repository, gh.PullRequest.Number)
	if p.MaxRoundsPerPR > 0 && len(taken) >= p.MaxRoundsPerPR {
		fd.Outcome, fd.Reason = Act, ReasonRoundCap
		fd.Actions = append(fd.Actions, action.Notice(target, NoticeReviewCap, ""))
		return true
	}
	limit := r.suggestionsLimit()
	if limit == 0 || !suggestionsOnly(current, bots[ev.Actor]) {
		return false
	}
	cycles := map[string]bool{}
	for id := range taken {
		if rv := byID[id]; suggestionsOnly(rv, bots[rv.Author]) {
			cycles[head(rv)] = true
		}
	}
	delete(cycles, current.CommitID)
	if len(cycles) < limit {
		return false
	}
	fd.Outcome, fd.Reason = Act, ReasonSuggestionsCap
	fd.Actions = append(fd.Actions, action.New(action.RemoveLabel, target, p.FixSuggestionsLabel),
		action.Notice(target, NoticeSuggestionsCap, ""))
	return true
}

// head is the push a review reviewed; a review without one is a cycle of
// its own (cap_review_rounds.suggestions_cycles).
func head(rv event.Review) string {
	if rv.CommitID != "" {
		return rv.CommitID
	}
	return "review-" + strconv.FormatInt(rv.ID, 10)
}

// taken are the bot reviews of the pull request whose follow-up ran.
func (r Router) taken(h *PRHistory, led Ledger, proj profile.Project, number int, bots map[int64]event.Review) map[int64]bool {
	out := map[int64]bool{}
	add := func(id string) {
		n, err := strconv.ParseInt(id, 10, 64)
		if _, bot := bots[n]; err == nil && bot {
			out[n] = true
		}
	}
	started := startedRun(number)
	for _, c := range h.Comments {
		if !slices.Contains(r.Config.Policy.PullRequestAuthors, c.Author) {
			continue
		}
		for _, m := range started.FindAllStringSubmatch(c.Body, -1) {
			add(m[1])
		}
	}
	for _, rec := range led.PRRuns(proj.Workspace, number) {
		if m := reviewRunName.FindStringSubmatch(rec.Name); m != nil && m[1] == strconv.Itoa(number) {
			add(m[2])
		}
	}
	return out
}

// botPattern is a review bot's compiled patterns.
type botPattern struct{ findings, suggestions *regexp.Regexp }

// botPatterns compiles the review bots' patterns; a bot whose pattern
// does not compile counts no suggestions (its own review is refused by
// classify).
func (r Router) botPatterns() map[string]botPattern {
	out := map[string]botPattern{}
	for _, b := range r.Config.Policy.ReviewBots {
		f, err := regexp.Compile(b.FindingsPattern)
		if err != nil {
			out[b.Login] = botPattern{}
			continue
		}
		bp := botPattern{findings: f}
		if s, err := regexp.Compile(b.SuggestionsPattern); err == nil && b.SuggestionsPattern != "" {
			bp.suggestions = s
		}
		out[b.Login] = bp
	}
	return out
}

// suggestionsOnly is cap_review_rounds.is_suggestions: the bot's
// suggestions pattern matches and its findings pattern does not.
func suggestionsOnly(rv event.Review, bp botPattern) bool {
	return bp.suggestions != nil && bp.findings != nil &&
		bp.suggestions.MatchString(rv.Body) && !bp.findings.MatchString(rv.Body)
}
