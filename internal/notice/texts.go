package notice

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"
)

// Gordon Cole's voice: the legacy scripts' exact lines
// (wait_for_blockers.py, drag_cancel.py, unanswered_notice.py,
// subtask_chain.py, report_failures.py, pr_open.py, merge_close.py), so the owner, vigie and sillage
// read the same notices whoever posts them.
var (
	TaskStartedLines = []string{
		"THIS ONE HAS THE STUFF!",
		"I'VE GOT MY BEST PEOPLE ON THIS.",
		"SENDING IN THE BIG GUNS!",
		"THIS IS A JOB FOR A REAL AGENT.",
	}
	FollowUpStartedLines = []string{
		"HEARD YOU LOUD AND CLEAR!",
		"GOT IT! HEARING AIDS ARE CRANKED UP TO THE MAX!",
		"MESSAGE RECEIVED! NO NEED TO SHOUT!",
		"I HEARD THAT ONE!",
	}
)

const (
	startCloser   = "COFFEE TIME! REPORT TO FOLLOW."
	discussLine   = "DISCUSS MODE. I'M OLD SCHOOL. YOU KNOW THAT. THE AGENT WILL ANSWER HERE; NO CODE CHANGES."
	setbackCloser = "WE'VE ALL HAD OUR SOCKS TOSSED AROUND."
	labelCell     = "width: 1%; white-space: nowrap"
	unansweredMk  = "NO AGENT WILL ANSWER"
	agentVerdict  = "THAT'S NO BACKEND HICCUP. READ THE LOGS BEFORE YOU RETRY"
	// maxFailureChars is report_failures.MAX_ERROR_CHARS.
	maxFailureChars = 300
)

// escape is Python's html.escape(text, quote=True), so a text matches the
// legacy one byte for byte.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(s)
}

func code(s string) string { return "<code>" + escape(s) + "</code>" }

// PickLine is wait_for_blockers.pick_line: the line of lines for the Task
// name, the same for every retry of it.
func PickLine(lines []string, name string) string {
	sum := sha256.Sum256([]byte(stripRetry(name)))
	n := new(big.Int).SetBytes(sum[:])
	return lines[new(big.Int).Mod(n, big.NewInt(int64(len(lines)))).Int64()]
}

func stripRetry(name string) string {
	if i := strings.LastIndex(name, "-retry"); i >= 0 && isDigits(name[i+len("-retry"):]) {
		return name[:i]
	}
	return name
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// StartInfo is what a start notice shows.
type StartInfo struct {
	Task, Model, Effort string
	FollowUp, Discuss   bool
	// SourceComment is a discuss follow-up's request comment, whose
	// marker the start comment keeps.
	SourceComment       int64
	RunURL, ActivityURL string
}

// StartText is wait_for_blockers.started_comment without its NODE row:
// Gordon announces at submission, before Kelos schedules the pod.
func StartText(s StartInfo) string {
	lines := TaskStartedLines
	if s.FollowUp {
		lines = FollowUpStartedLines
	}
	var b strings.Builder
	b.WriteString("<p>" + PickLine(lines, s.Task) + "</p>")
	if s.Discuss {
		b.WriteString("<p>" + discussLine + "</p>")
	}
	b.WriteString("<table><tbody>")
	for _, row := range [][2]string{{"MODEL", s.Model}, {"EFFORT", s.Effort}, {"KELOS TASK", s.Task}} {
		if row[1] != "" {
			fmt.Fprintf(&b, `<tr><td style="%s">%s</td><td>%s</td></tr>`, labelCell, row[0], code(row[1]))
		}
	}
	b.WriteString("</tbody></table>")
	b.WriteString(links(s.RunURL, s.ActivityURL))
	b.WriteString("<p>" + startCloser + "</p>")
	if s.Discuss && s.SourceComment > 0 {
		b.WriteString(DiscussMarker(s.SourceComment))
	}
	return b.String() + Marker(Start, s.Task)
}

func links(run, activity string) string {
	var out []string
	if run != "" {
		out = append(out, `<a href="`+escape(run)+`">lucarne</a>`)
	}
	if activity != "" {
		out = append(out, `<a href="`+escape(activity)+`">sillage</a>`)
	}
	if len(out) == 0 {
		return ""
	}
	return "<p>" + strings.Join(out, " / ") + "</p>"
}

// DiscussFallbackText is discuss_fallback.fallback_text: the one line a
// discuss follow-up's start comment begins as.
func DiscussFallbackText(comment int64) string {
	return "<p>" + discussLine + "</p>" + DiscussMarker(comment)
}

// StopText is drag_cancel.stopped_comment.
func StopText(backlog, start string, names []string, labelNote string) string {
	tasks := make([]string, len(names))
	for i, n := range names {
		tasks[i] = code(n)
	}
	return fmt.Sprintf("<p>THE CARD MOVED TO %s. THE AGENT IS OFF THE CASE. KELOS TASKS DELETED: %s.%s "+
		"MOVE THE CARD BACK TO %s TO START A NEW AGENT. ANYTHING THE STOPPED ONE PUSHED STAYS ON THE "+
		"TASK'S BRANCH. CATCH YOU LATER.</p>", code(backlog), strings.Join(tasks, ", "), labelNote, code(start)) +
		Marker(Stop, "")
}

// LabelKeptNote is drag_cancel's sentence when the label stayed.
func LabelKeptNote(label string) string {
	return fmt.Sprintf(" THE %s LABEL COULD NOT BE REMOVED; REMOVE IT BEFORE MOVING THE CARD BACK.", code(label))
}

// UnansweredText is unanswered_notice.notice_text with its reasons.
func UnansweredText(trigger string, comment int64, reasons []string) string {
	var items strings.Builder
	for _, r := range reasons {
		items.WriteString("<li>" + r + "</li>")
	}
	t := code(trigger)
	return fmt.Sprintf(`<p>%s <a href="#comment-%d">THIS %s COMMENT</a>:</p><ul>%s</ul>`+
		"<p>FIX THAT, THEN SPEAK UP AGAIN: POST THE REQUEST AS A NEW %s COMMENT. EDITING THE OLD "+
		"COMMENT STARTS NOTHING. I DON'T HEAR EDITS. LONG STORY.</p>", unansweredMk, comment, t, items.String(), t) +
		Marker(Unanswered, "")
}

// FailedInfo is what a failure notice shows.
type FailedInfo struct {
	Task, Message, InProgressLabel, ErrorLabel, Namespace string
	FollowUp                                              bool
	RunURL                                                string
}

// FailedText is report_failures.failed_comment from what Gordon knows:
// the Task's status message stands for the pod log's last error, which
// the worker cannot read.
func FailedText(f FailedInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>KELOS TASK %s FAILED.", code(f.Task))
	if f.ErrorLabel != "" {
		fmt.Fprintf(&b, " LABELLED %s UNTIL THE NEXT ATTEMPT STARTS.", code(f.ErrorLabel))
	}
	b.WriteString("</p>")
	if msg := truncate(f.Message, maxFailureChars); msg != "" {
		fmt.Fprintf(&b, "<p>THE LAST WORD: %s</p>", code(msg))
	}
	fmt.Fprintf(&b, "<p>%s. TO RETRY: %s.</p>", agentVerdict, retrySteps(f))
	if f.RunURL != "" {
		fmt.Fprintf(&b, `<p>THE RUN IS ON <a href="%s">LUCARNE</a>. SIGN IN AND YOU LAND RIGHT ON IT.</p>`, escape(f.RunURL))
	}
	return b.String() + "<p>" + setbackCloser + "</p>" + Marker(Failed, "")
}

func retrySteps(f FailedInfo) string {
	if f.FollowUp {
		return "COMMENT AGAIN: A NEW COMMENT STARTS A NEW FOLLOW-UP"
	}
	edit := "EDIT THIS TASK"
	if f.InProgressLabel != "" {
		edit = fmt.Sprintf("REMOVE %s FROM THIS TASK (EDIT IT IF IT HAS NONE)", code(f.InProgressLabel))
	}
	del := code(fmt.Sprintf("kubectl -n %s delete task %s", f.Namespace, f.Task))
	return fmt.Sprintf("DELETE THE FAILED TASK (%s, OR WAIT FOR ITS TTL), THEN %s. MOVING THE CARD STARTS "+
		"NOTHING WHILE THE FAILED TASK IS THERE", del, edit)
}

// truncate is report_failures.truncate: whitespace collapsed, cut with "…".
func truncate(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return text
}
