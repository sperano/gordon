package route

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
)

// suggestionMarker starts a review thread that is only a suggestion
// (poll_review_bot.SUGGESTION_FINDING_MARKER).
const suggestionMarker = "**Suggestion**"

// botSuffix is the suffix GitHub's REST logins of an App carry and its
// GraphQL logins lack.
const botSuffix = "[bot]"

// findings is review_findings.assess: whether a bot review still waits for
// its follow-up (pending), else the reviews whose findings a round limit
// left unaddressed. Nothing without review bots.
func (g *gate) findings() (bool, []notice.Limited) {
	if len(g.r.Config.Policy.ReviewBots) == 0 {
		return false, nil
	}
	f := g.newFindings()
	if f.pending() {
		return true, nil
	}
	return false, f.roundLimited()
}

// reviewState holds one pull request's review-bot state.
type reviewState struct {
	g        *gate
	pr       event.PullRequest
	bots     map[string]botPattern
	byID     map[int64]event.Review
	history  *PRHistory
	capped   bool // the suggestions cap notice is on the PR
	suggests bool // suggestions count: capped or the fix-suggestions label
}

func (g *gate) newFindings() *reviewState {
	rd := g.rd
	f := &reviewState{g: g, pr: rd.PullRequest, bots: g.r.botPatterns(), byID: map[int64]event.Review{},
		history: &PRHistory{Reviews: rd.Reviews, Comments: rd.Comments}}
	for _, rv := range rd.Reviews {
		if _, ok := f.bots[rv.Author]; ok {
			f.byID[rv.ID] = rv
		}
	}
	authors := g.r.Config.Policy.PullRequestAuthors
	f.capped = slices.ContainsFunc(rd.Comments, func(c event.IssueComment) bool {
		return slices.Contains(authors, c.Author) && notice.SuggestionsCapPosted(c.Body)
	})
	f.suggests = f.capped || g.r.suggestionsOn(f.pr)
	return f
}

// suggestionsOn is poll_review_bot.suggestions_enabled.
func (r Router) suggestionsOn(pr event.PullRequest) bool {
	l := r.Config.Policy.FixSuggestionsLabel
	return l != "" && pr.HasLabel(l)
}

// latestReview is poll_review_bot.latest_review: the author's newest
// submitted review with a body.
func latestReview(reviews []event.Review, author string) (event.Review, bool) {
	var out event.Review
	found := false
	for _, rv := range reviews {
		if rv.Author != author || rv.Body == "" || rv.SubmittedAt.IsZero() {
			continue
		}
		if !found || rv.SubmittedAt.After(out.SubmittedAt) {
			out, found = rv, true
		}
	}
	return out, found
}

// followUpExists is review_followup_exists: a Kelos Task or a Gordon run
// of the PR's follow-up for the review.
func (f *reviewState) followUpExists(review int64) bool {
	names := make([]string, 0, len(f.g.rd.Tasks))
	for _, t := range f.g.rd.Tasks {
		names = append(names, t.Name)
	}
	for _, w := range f.g.r.workspaces(f.pr.Repository) {
		for _, rec := range f.g.led.PRRuns(w, f.pr.Number) {
			names = append(names, rec.Name)
		}
	}
	want := [2]string{fmt.Sprint(f.pr.Number), fmt.Sprint(review)}
	return slices.ContainsFunc(names, func(n string) bool {
		m := reviewRunName.FindStringSubmatch(n)
		return m != nil && [2]string{m[1], m[2]} == want
	})
}

// announced is review_was_announced: a start notice by a pull-request
// author names the review's follow-up in any of the repository's
// Workspaces.
func (f *reviewState) announced(review int64) bool {
	authors := f.g.r.Config.Policy.PullRequestAuthors
	for _, w := range f.g.r.workspaces(f.pr.Repository) {
		re := regexp.MustCompile(fmt.Sprintf("`pr-followup-%s-%d-[rp]%d(?:-retry\\d+)?`", regexp.QuoteMeta(w), f.pr.Number, review))
		if slices.ContainsFunc(f.history.Comments, func(c event.IssueComment) bool {
			return slices.Contains(authors, c.Author) && re.MatchString(c.Body)
		}) {
			return true
		}
	}
	return false
}

// sharedCap is review_round_cap_reached: the PR's review-bot rounds, the
// review left out, reached followUp.maxRoundsPerPR.
func (f *reviewState) sharedCap(review int64) bool {
	limit := f.g.r.Config.Policy.MaxRoundsPerPR
	if limit <= 0 {
		return false
	}
	taken := f.g.r.taken(f.history, f.g.led, f.g.proj, f.pr.Number, f.byID)
	delete(taken, review)
	return len(taken) >= limit
}

// hasFindings is poll_review_bot.has_findings.
func (f *reviewState) hasFindings(rv event.Review, bp botPattern) bool {
	if bp.findings != nil && bp.findings.MatchString(rv.Body) {
		return true
	}
	return bp.suggestions != nil && f.g.r.suggestionsOn(f.pr) && bp.suggestions.MatchString(rv.Body)
}

// threads is poll_review_bot.unresolved_bot_threads: the unresolved
// threads the bot started, a suggestion only when suggestions count.
func (f *reviewState) threads(bot profile.Bot, bp botPattern, suggestions bool) int {
	n := 0
	for _, t := range f.g.rd.Threads {
		if t.Resolved || strings.TrimSuffix(t.Author, botSuffix) != strings.TrimSuffix(bot.Login, botSuffix) {
			continue
		}
		if !suggestions && bp.suggestions != nil && strings.HasPrefix(t.Body, suggestionMarker) {
			continue
		}
		n++
	}
	return n
}

// pending is pending_followup: the first bot whose latest review has no
// follow-up, no start notice, no cap, and findings or unresolved threads.
func (f *reviewState) pending() bool {
	for _, bot := range f.g.r.Config.Policy.ReviewBots {
		rv, ok := latestReview(f.history.Reviews, bot.Login)
		if !ok || f.followUpExists(rv.ID) || f.announced(rv.ID) || f.sharedCap(rv.ID) {
			continue
		}
		bp := f.bots[bot.Login]
		if f.hasFindings(rv, bp) || f.threads(bot, bp, f.g.r.suggestionsOn(f.pr)) > 0 {
			return true
		}
	}
	return false
}

// roundLimited is review_findings.round_limited: no limit applies
// without a shared cap or the suggestions cap notice.
func (f *reviewState) roundLimited() []notice.Limited {
	if f.g.r.Config.Policy.MaxRoundsPerPR <= 0 && !f.capped {
		return nil
	}
	var out []notice.Limited
	for _, bot := range f.g.r.Config.Policy.ReviewBots {
		for _, id := range f.botLimited(bot) {
			out = append(out, notice.Limited{Author: bot.Login, Review: id})
		}
	}
	return out
}

// headReviews is review_findings.head_reviews: the bot's reviews of the
// head, else its latest one.
func (f *reviewState) headReviews(author string) []event.Review {
	var own, onHead []event.Review
	for _, rv := range f.history.Reviews {
		if rv.Author == author && rv.Body != "" && !rv.SubmittedAt.IsZero() {
			own = append(own, rv)
			if rv.CommitID == f.pr.HeadSHA {
				onHead = append(onHead, rv)
			}
		}
	}
	if len(onHead) > 0 {
		return onHead
	}
	if latest, ok := latestReview(own, author); ok {
		return []event.Review{latest}
	}
	return nil
}

// bodyLimited is Limits.body_limited.
func (f *reviewState) bodyLimited(rv event.Review, bp botPattern) bool {
	if bp.findings != nil && bp.findings.MatchString(rv.Body) {
		return f.sharedCap(rv.ID)
	}
	suggests := f.suggests && bp.suggestions != nil && bp.suggestions.MatchString(rv.Body)
	return suggests && (f.capped || f.sharedCap(rv.ID))
}

// botLimited is review_findings.bot_limited: the bot's reviews whose
// body findings a limit left, else its latest review when the limit left
// its unresolved threads.
func (f *reviewState) botLimited(bot profile.Bot) []int64 {
	bp := f.bots[bot.Login]
	reviews := f.headReviews(bot.Login)
	var found []int64
	for _, rv := range reviews {
		if !f.announced(rv.ID) && f.bodyLimited(rv, bp) {
			found = append(found, rv.ID)
		}
	}
	if len(found) > 0 || len(reviews) == 0 {
		return found
	}
	latest, _ := latestReview(reviews, bot.Login)
	if f.announced(latest.ID) || !(f.capped || f.sharedCap(latest.ID)) || f.threads(bot, bp, f.suggests) == 0 {
		return nil
	}
	return []int64{latest.ID}
}
