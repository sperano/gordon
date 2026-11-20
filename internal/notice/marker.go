// Package notice holds Gordon's Vikunja notices: their hidden markers,
// which notice_kind.py defined for the legacy scripts (`<!-- kelos:<kind> -->`,
// `<!-- kelos:<kind> task=<name> -->`), the discuss start comment's
// per-request marker, Gordon's own action marker, and the texts.
//
// A notice is recognised only on a comment its author could not fake:
// one posted by Gordon's own Vikunja user. A marker pasted into anyone
// else's comment proves nothing, so it never suppresses a notice.
package notice

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sperano/gordon/internal/event"
)

// Kinds of the notices Gordon posts, notice_kind.py's names.
const (
	Start         = "start"
	Stop          = "stop"
	Discuss       = "discuss"
	Unanswered    = "unanswered"
	Failed        = "failed"
	ChainStarted  = "chain-started"
	ChainWaiting  = "chain-waiting"
	ChainStopped  = "chain-stopped"
	ChainNoAgent  = "chain-no-agent"
	ChainDone     = "chain-done"
	PROpen        = "pr-open"
	Merged        = "merged"
	DiscussStart  = "discuss-start"
	BlockerWait   = "blocker-wait"
	actionMarker  = "<!-- gordon:action="
	discussMarker = "<!-- kelos:discuss-start comment="
)

// taskName is notice_kind.TASK_NAME: a Kelos Task name in a marker.
var taskName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// markerPattern is notice_kind.MARKER.
var markerPattern = regexp.MustCompile(`<!-- kelos:([a-z][a-z-]*)(?: task=([A-Za-z0-9][A-Za-z0-9._-]*))? -->`)

// Marker is the legacy kind marker; task names the Task of a start notice.
func Marker(kind, task string) string {
	if task != "" && taskName.MatchString(task) {
		return fmt.Sprintf("<!-- kelos:%s task=%s -->", kind, task)
	}
	return fmt.Sprintf("<!-- kelos:%s -->", kind)
}

// DiscussMarker is the marker the discuss follow-up's start comment
// shares between its fallback line and its start notice
// (wait_for_blockers.discuss_marker).
func DiscussMarker(comment int64) string {
	return fmt.Sprintf("%s%d -->%s", discussMarker, comment, Marker(Discuss, ""))
}

// safeKey keeps an action key inside an HTML comment: no "--" and no ">".
var unsafeKey = regexp.MustCompile(`[^A-Za-z0-9:/._@=+-]`)

// ActionMarker is the marker of the action that posted a notice: the
// external trace an ambiguous post is recovered by.
func ActionMarker(key string) string {
	k := strings.ReplaceAll(unsafeKey.ReplaceAllString(key, "_"), "--", "-_")
	return actionMarker + k + " -->"
}

// Kinds returns the (kind, task) pairs of the legacy markers in text.
func Kinds(text string) [][2]string {
	var out [][2]string
	for _, m := range markerPattern.FindAllStringSubmatch(text, -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

// HasKind reports whether text carries a marker of kind (and of task,
// when task is set).
func HasKind(text, kind, task string) bool {
	for _, k := range Kinds(text) {
		if k[0] == kind && (task == "" || k[1] == task) {
			return true
		}
	}
	return false
}

// Trusted are the comments by Gordon's own Vikunja user.
func Trusted(comments []event.TaskComment, service string) []event.TaskComment {
	var out []event.TaskComment
	for _, c := range comments {
		if service != "" && c.Author == service {
			out = append(out, c)
		}
	}
	return out
}

// Find returns the first trusted comment for which match holds.
func Find(comments []event.TaskComment, service string, match func(string) bool) (event.TaskComment, bool) {
	for _, c := range Trusted(comments, service) {
		if match(c.Body) {
			return c, true
		}
	}
	return event.TaskComment{}, false
}

// TaskHref is the escaped link to a Vikunja task (subtask_chain.task_href).
func TaskHref(web string, id int64) string {
	return escape(fmt.Sprintf("%s/tasks/%d", strings.TrimRight(web, "/"), id))
}

// AboutTask reports whether text is a kind notice linking the task:
// subtask_chain.carries for a per-subtask chain notice.
func AboutTask(text, kind, web string, id int64) bool {
	return HasKind(text, kind, "") && strings.Contains(text, `<a href="`+TaskHref(web, id)+`"`)
}

// ChainStartedFor reports whether a trusted comment announces that the
// chain started the subtask: the chain then never moves it again.
func ChainStartedFor(comments []event.TaskComment, service, web string, subtask int64) bool {
	_, ok := Find(comments, service, func(b string) bool { return AboutTask(b, ChainStarted, web, subtask) })
	return ok
}
