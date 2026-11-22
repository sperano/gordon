package notice

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// The PR readiness notice kinds (notice_kind.py).
const (
	Ready         = "ready"
	RoundLimit    = "round-limit"
	ChecksMissing = "checks-missing"
	MergeRefused  = "merge-refused"
)

// shortSHALength is the head SHA prefix the readiness notices show.
const shortSHALength = 8

// ShortSHA is a head SHA's first shortSHALength characters.
func ShortSHA(sha string) string {
	if len(sha) < shortSHALength {
		return sha
	}
	return sha[:shortSHALength]
}

// Head names the pull-request head a readiness notice is about, with
// the notice's own payload: the limited reviews of a round-limit notice,
// the missing checks of a checks-missing one, GitHub's answer of a
// refused merge.
type Head struct {
	Repo   string
	Number int
	SHA    string
	Extra  string
}

// headDetail is the Detail of a readiness notice action:
// <owner>/<repo>#<number>@<sha>[ <extra>].
var headDetail = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]*)@([0-9a-f]+)(?: (.*))?$`)

// Detail is the head's action detail.
func (h Head) Detail() string {
	d := fmt.Sprintf("%s#%d@%s", h.Repo, h.Number, h.SHA)
	if h.Extra != "" {
		d += " " + h.Extra
	}
	return d
}

// ParseHead parses a readiness notice's detail.
func ParseHead(detail string) (Head, error) {
	m := headDetail.FindStringSubmatch(detail)
	if m == nil {
		return Head{}, fmt.Errorf("readiness notice detail %q", detail)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return Head{}, fmt.Errorf("readiness notice detail %q: %w", detail, err)
	}
	return Head{Repo: m[1], Number: n, SHA: m[3], Extra: m[4]}, nil
}

// URL is the pull request's GitHub URL.
func (h Head) URL() string { return fmt.Sprintf("https://github.com/%s/pull/%d", h.Repo, h.Number) }

// signature is the visible text that names one notice of the head:
// <HEADING>: <repo>#<n>. HEAD <sha8>.
func (h Head) signature(heading string) string {
	return fmt.Sprintf("%s: %s#%d. HEAD %s.", heading, h.Repo, h.Number, ShortSHA(h.SHA))
}

// opening is the shared start of the readiness notices.
func (h Head) opening(heading string) string {
	return fmt.Sprintf(`<p>%s: <a href="%s">%s</a>. HEAD %s. `, heading, escape(h.URL()),
		escape(fmt.Sprintf("%s#%d", h.Repo, h.Number)), code(ShortSHA(h.SHA)))
}

// The readiness notices' headings, their signatures' start.
const (
	readyHeading         = "READY TO MERGE"
	roundLimitHeading    = "ROUND LIMIT REACHED"
	checksMissingHeading = "CHECKS MISSING"
	mergeRefusedHeading  = "AUTO-MERGE REFUSED"
)

// ReadyText is reconcile_pr_ready.ready_comment.
func ReadyText(h Head) string {
	return h.opening(readyHeading) + "CHECKS AND AUTOMATED REVIEW FOLLOW-UPS ARE FINISHED.</p>" + Marker(Ready, "")
}

// Limited is one review whose findings a round limit left unaddressed.
type Limited struct {
	Author string `json:"author"`
	Review int64  `json:"review"`
}

// LimitedExtra is a round-limit notice's payload.
func LimitedExtra(limited []Limited) string {
	b, _ := json.Marshal(limited) //nolint:errcheck // strings and integers always encode
	return string(b)
}

// RoundLimitText is review_findings.round_limit_comment for the reviews
// the head's payload lists.
func RoundLimitText(h Head) (string, error) {
	var limited []Limited
	if err := json.Unmarshal([]byte(h.Extra), &limited); err != nil || len(limited) == 0 {
		return "", fmt.Errorf("round-limit notice reviews %q", h.Extra)
	}
	links := make([]string, len(limited))
	for i, l := range limited {
		links[i] = fmt.Sprintf(`<a href="%s#pullrequestreview-%d">%s review %d</a>`, escape(h.URL()), l.Review,
			escape(l.Author), l.Review)
	}
	return h.opening(roundLimitHeading) + "CHECKS ARE FINISHED, BUT THE REVIEW ROUND LIMIT STOPPED THE AUTOMATED " +
		"FOLLOW-UPS WITH FINDINGS UNADDRESSED: " + strings.Join(links, ", ") + ". REVIEW THEM, OR ASK FOR A " +
		"FOLLOW-UP ON THE PULL REQUEST, BEFORE MERGING.</p>" + Marker(RoundLimit, ""), nil
}

// MissingExtra is a checks-missing notice's payload: the missing names.
func MissingExtra(names []string) string {
	b, _ := json.Marshal(names) //nolint:errcheck // strings always encode
	return string(b)
}

// ChecksMissingText is missing_checks.notice for the checks the head's
// payload names, absent after minutes.
func ChecksMissingText(h Head, minutes int) (string, error) {
	var names []string
	if err := json.Unmarshal([]byte(h.Extra), &names); err != nil || len(names) == 0 {
		return "", fmt.Errorf("checks-missing notice checks %q", h.Extra)
	}
	coded := make([]string, len(names))
	for i, n := range names {
		coded[i] = code(n)
	}
	return h.opening(checksMissingHeading) + fmt.Sprintf("EXPECTED CI CHECKS STILL ABSENT AFTER %d MINUTES: %s. ",
		minutes, strings.Join(coded, ", ")) + "THE READY-TO-MERGE NOTICE WAITS FOR THEM. IS THE GITHUB ACTIONS " +
		"WORKFLOW DISABLED, OUT OF MINUTES OR NOT TRIGGERED? IF A CHECK IS GONE FOR GOOD, DROP IT FROM " +
		"<code>prReadiness.ciChecks</code>.</p>" + Marker(ChecksMissing, ""), nil
}

// MergeRefusedText is auto_merge.refused_comment for GitHub's answer,
// the head's payload.
func MergeRefusedText(h Head) string {
	return h.opening(mergeRefusedHeading) + "GITHUB SAID: " + code(h.Extra) +
		". EVERY SCAN TRIES AGAIN; MERGE IT BY HAND OR REMOVE THE LABEL.</p>" + Marker(MergeRefused, "")
}

// headings maps each readiness kind to its heading.
var headings = map[string]string{
	Ready: readyHeading, RoundLimit: roundLimitHeading, ChecksMissing: checksMissingHeading,
	MergeRefused: mergeRefusedHeading,
}

// HeadPosted reports whether text is the kind's notice of the head: its
// signature in the visible text, as the legacy scripts deduplicate it
// (already_announced), so a notice either posted counts.
func HeadPosted(text, kind string, h Head) bool {
	heading, ok := headings[kind]
	return ok && strings.Contains(VisibleText(text), h.signature(heading))
}

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// VisibleText is merge_close.visible_text: the text a browser shows,
// whitespace collapsed.
func VisibleText(s string) string {
	s = htmlTag.ReplaceAllString(htmlComment.ReplaceAllString(s, ""), "")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}
