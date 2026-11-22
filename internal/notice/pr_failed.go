package notice

import (
	"fmt"
	"strings"
)

// PRFailedInfo is what a pull request's failure notice shows.
type PRFailedInfo struct {
	// Task is the failed run, Message its Task's status message.
	Task, Message string
	// Trigger is the owner's request prefix a follow-up retry starts with.
	Trigger string
	// Conflict is a conflict run: the next scan retries it, not a comment.
	Conflict bool
	// Namespace is the Kelos namespace the retry's delete names.
	Namespace string
	// RunURL is the run's Lucarne link, set only on a private repository.
	RunURL string
}

// PRFailedText is a failed PR follow-up's or conflict run's notice on its
// pull request, in Markdown. Legacy reported neither (report_failures.py
// reports Tasks of a Vikunja task only); the wording follows the Vikunja
// failure notice's.
func PRFailedText(f PRFailedInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "KELOS TASK %s FAILED.", mdCode(f.Task))
	if msg := truncate(f.Message, maxFailureChars); msg != "" {
		fmt.Fprintf(&b, "\n\nTHE LAST WORD: %s", mdCode(msg))
	}
	fmt.Fprintf(&b, "\n\n%s. TO RETRY: %s.", agentVerdict, prRetry(f))
	if f.RunURL != "" {
		fmt.Fprintf(&b, "\n\nTHE RUN IS ON [LUCARNE](%s). %s", f.RunURL, lucarneSignIn)
	}
	b.WriteString("\n\n" + setbackCloser)
	return MarkMarkdown(b.String(), Failed, "")
}

func prRetry(f PRFailedInfo) string {
	if f.Conflict {
		return fmt.Sprintf("DELETE THE FAILED TASK (%s, OR WAIT FOR ITS TTL); THE NEXT CONFLICT SCAN STARTS A NEW ONE "+
			"WHILE THE CONFLICT LASTS", mdCode(fmt.Sprintf("kubectl -n %s delete task %s", f.Namespace, f.Task)))
	}
	return fmt.Sprintf("COMMENT AGAIN WITH A REQUEST STARTING WITH %s: A NEW COMMENT STARTS A NEW FOLLOW-UP. "+
		"IF THIS FOLLOW-UP DRAFTED THE PULL REQUEST, IT STAYS A DRAFT UNTIL THE NEXT ONE MARKS IT READY, OR YOU DO",
		mdCode(f.Trigger))
}

// mdCode is a Markdown code span; a backtick in s, which would end it, is
// replaced by an apostrophe.
func mdCode(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }
