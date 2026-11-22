package notice

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Kinds of Gordon's GitHub notices (notice_kind.py).
const (
	ReviewCap      = "review-cap"
	SuggestionsCap = "suggestions-cap"
)

// Gordon Cole's voice on a pull request (announce_github_start.py).
var (
	PRFollowUpStartedLines = []string{
		"A KELOS AGENT HAS STARTED THIS FOLLOW-UP ROUND.",
		"BACK ON THE CASE! ANOTHER ROUND COMING UP!",
		"YOUR NOTES ARE IN. WE'RE ON IT!",
		"ANOTHER ROUND! THIS ONE STILL HAS THE STUFF!",
	}
	ConflictStartedLines = []string{
		"PUT CAUTION IN THE SHOTGUN SEAT!",
		"TWO ROADS CROSSED ON THIS ONE. EASY DOES IT!",
		"WE'VE GOT A COLLISION! STEADY HANDS!",
		"MAIN MOVED ON US. TIME TO CATCH UP!",
	}
)

const (
	// capPhrase and suggestionsCapPhrase identify the cap notices posted
	// before the markers (cap_review_rounds.CAP_MARKER, SUGGESTIONS_CAP_MARKER).
	capPhrase            = "HAS REACHED ITS REVIEW-BOT ROUND LIMIT"
	suggestionsCapPhrase = "HAS REACHED ITS FIX-SUGGESTIONS ROUND LIMIT"
	capConfig            = "followUp.maxRoundsPerPR"
	suggestionsCapConfig = "followUp.fixSuggestions.maxRounds"
	sillageOpenPath      = "open"
)

// conflictRunName is announce_github_start.CONFLICT_NAME.
var conflictRunName = regexp.MustCompile(`^pr-conflict-[0-9a-f]{8}-(\d+)-[0-9a-f]{8}-[0-9a-f]{8}(?:-retry\d+)?$`)

// MarkMarkdown is notice_kind.mark_markdown: a Markdown notice with its
// marker as a last block of its own, which GitHub does not render.
func MarkMarkdown(body, kind, task string) string {
	return strings.TrimRight(body, " \t\n\r\f\v") + "\n\n" + Marker(kind, task)
}

// IsKind is notice_kind.is_kind: by the marker when text carries any
// marker, else by the phrase of a notice posted before the markers.
func IsKind(text, kind, phrase string) bool {
	if len(Kinds(text)) > 0 {
		return HasKind(text, kind, "")
	}
	return phrase != "" && strings.Contains(strings.ToUpper(text), phrase)
}

// ReviewCapPosted and SuggestionsCapPosted recognise the cap notices.
func ReviewCapPosted(text string) bool { return IsKind(text, ReviewCap, capPhrase) }
func SuggestionsCapPosted(text string) bool {
	return IsKind(text, SuggestionsCap, suggestionsCapPhrase)
}

// ReviewCapText is cap_review_rounds.cap_comment.
func ReviewCapText(bots []string, maxRounds int, trigger string) string {
	plural := "S"
	if maxRounds == 1 {
		plural = ""
	}
	sorted := append([]string(nil), bots...)
	sort.Strings(sorted)
	names := make([]string, len(sorted))
	for i, b := range sorted {
		names[i] = "`" + b + "`"
	}
	return MarkMarkdown(fmt.Sprintf("THIS PULL REQUEST %s: REVIEWS BY %s HAVE STARTED "+
		"KELOS FOLLOW-UP AGENTS %d TIME%s, THE CONFIGURED MAXIMUM (`%s`). LATER REVIEWS BY THESE BOTS DO "+
		"NOT START AN AGENT HERE. DON'T LET 'EM RATTLE YOU. TO CONTINUE, COMMENT ON THE PULL REQUEST "+
		"WITH A REQUEST STARTING WITH `%s`, FOR EXAMPLE `%s address the latest review findings`.",
		capPhrase, strings.Join(names, ", "), maxRounds, plural, capConfig, trigger, trigger), ReviewCap, "")
}

// SuggestionsCapText is cap_review_rounds.suggestions_cap_comment.
func SuggestionsCapText(maxRounds int, label, trigger string) string {
	plural := "ES"
	if maxRounds == 1 {
		plural = ""
	}
	return MarkMarkdown(fmt.Sprintf("THIS PULL REQUEST %s: SUGGESTIONS-ONLY REVIEWS "+
		"OF %d PUSH%s HAVE STARTED KELOS FOLLOW-UP AGENTS, THE CONFIGURED MAXIMUM (`%s`). THE `%s` "+
		"LABEL IS REMOVED: LATER SUGGESTIONS STAY IN THE REVIEWS FOR YOU, AND "+
		"ONLY BLOCKING FINDINGS START AN AGENT. ADDING THE LABEL BACK DOES NOT "+
		"LIFT THE LIMIT. TO HAVE SUGGESTIONS FIXED, COMMENT ON THE PULL "+
		"REQUEST WITH A REQUEST STARTING WITH `%s`, FOR EXAMPLE "+
		"`%s fix the latest review's suggestions`.",
		suggestionsCapPhrase, maxRounds, plural, suggestionsCapConfig, label, trigger, trigger), SuggestionsCap, "")
}

// PRStartInfo is what a pull request's start notice shows.
type PRStartInfo struct {
	Task, Model, Effort string
	// RunURL and ActivityURL are the private links, set only on a private
	// repository.
	RunURL, ActivityURL string
}

// PRStartText is announce_github_start.started_comment, announced at
// submission: no JOB UID and NODE rows (the run ledger, not the comment,
// deduplicates Gordon's runs, and no pod exists yet).
func PRStartText(s PRStartInfo) string {
	lines := PRFollowUpStartedLines
	if conflictRunName.MatchString(s.Task) {
		lines = ConflictStartedLines
	}
	rows := []string{"| | |", "|---|---|"}
	if s.Model != "" {
		rows = append(rows, "| MODEL | `"+s.Model+"` |")
	}
	if s.Effort != "" {
		rows = append(rows, "| EFFORT | `"+s.Effort+"` |")
	}
	rows = append(rows, "| KELOS TASK | `"+s.Task+"` |")
	var links []string
	if s.RunURL != "" {
		links = append(links, "[lucarne]("+s.RunURL+")")
	}
	if s.ActivityURL != "" {
		links = append(links, "[sillage]("+s.ActivityURL+")")
	}
	linkBlock := ""
	if len(links) > 0 {
		linkBlock = strings.Join(links, " / ") + "\n\n"
	}
	return MarkMarkdown(PickLine(lines, s.Task)+"\n\n"+strings.Join(rows, "\n")+"\n\n"+linkBlock+startCloser, Start, s.Task)
}

// SillagePRURL is announce_github_start.sillage_pr_url: Sillage's link
// resolving a pull request to its story.
func SillagePRURL(base, prURL string) string {
	return strings.TrimRight(base, "/") + "/" + sillageOpenPath + "?" + url.Values{"url": {prURL}}.Encode()
}
