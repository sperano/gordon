package notice

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	prCreatedCloser = "THE AGENT'S LOCAL CHECKS PASSED; GITHUB CHECKS AND AUTOMATED REVIEWS MAY STILL BE RUNNING."
	prReadyPromise  = " I'LL SPEAK UP AGAIN WHEN IT'S READY TO MERGE."
	mergedCloser    = "WHAT A BEAUTY. CASE CLOSED."
)

// PRURL is the GitHub URL of a pull request named owner/repo#number (a
// pr-open or merged notice's detail).
func PRURL(ref string) (string, error) {
	repo, num, ok := strings.Cut(ref, "#")
	n, err := strconv.Atoi(num)
	owner, name, _ := strings.Cut(repo, "/")
	if !ok || err != nil || n <= 0 || strings.Count(repo, "/") != 1 || owner == "" || name == "" {
		return "", fmt.Errorf("pull request reference %q", ref)
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n), nil
}

func prLabel(url string) string {
	return strings.Replace(strings.TrimPrefix(url, "https://github.com/"), "/pull/", "#", 1)
}

// PROpenText is pr_open.created_comment: the PR's provisional
// announcement, promising a ready notice when readiness is enabled.
func PROpenText(url string, readiness bool) string {
	text := fmt.Sprintf(`<p>PR CREATED: <a href="%s">%s</a>. %s`, escape(url), escape(prLabel(url)), prCreatedCloser)
	if readiness {
		text += prReadyPromise
	}
	return text + "</p>" + Marker(PROpen, "")
}

// MergedText is merge_close.merged_comment.
func MergedText(url string) string {
	u := escape(url)
	return fmt.Sprintf(`<p>MERGED: <a href="%s">%s</a>. %s</p>`, u, u, mergedCloser) + Marker(Merged, "")
}

// PROpenPosted reports whether text announces the PR's creation
// (pr_open.created_signature, on the HTML the legacy script posts).
func PROpenPosted(text, url string) bool {
	return strings.Contains(text, fmt.Sprintf(`PR CREATED: <a href="%s">%s</a>.`, escape(url), escape(prLabel(url))))
}

// MergedPosted is merge_close.is_merged_comment: a merged notice (its
// marker, else the opening of one posted before the markers) linking the
// PR.
func MergedPosted(text, url string) bool {
	link := `<a href="` + escape(url) + `"`
	if !strings.Contains(text, link) {
		return false
	}
	if len(Kinds(text)) > 0 {
		return HasKind(text, Merged, "")
	}
	return strings.Contains(text, "Merged: "+link) || strings.Contains(text, "MERGED: "+link)
}
