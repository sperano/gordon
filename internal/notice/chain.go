package notice

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/event"
)

// The chain notices' opening phrases (subtask_chain.py).
const (
	chainStartedMark = "NEXT UP IN THE CHAIN"
	chainWaitingMark = "THE CHAIN WAITS ON YOU"
	chainStoppedMark = "THE CHAIN STOPS HERE"
	chainNoAgentMark = "THIS CHAIN HAS NO AGENT"
	chainDoneMark    = "CHAIN COMPLETE"
)

// Chain is what a chain notice names.
type Chain struct {
	Web, StartBucket, ReviewBucket, NoAgentLabel string
	AgentUsers                                   []string
}

// TaskLink is subtask_chain.task_link.
func (c Chain) TaskLink(t event.Task) string {
	n := t.Index
	if n == 0 {
		n = t.ID
	}
	label := strings.TrimSpace(fmt.Sprintf("#%d %s", n, t.Title))
	return `<a href="` + TaskHref(c.Web, t.ID) + `">` + escape(label) + "</a>"
}

// StartedText is subtask_chain.started_comment.
func (c Chain) StartedText(t event.Task, agent string, nested bool) string {
	who := fmt.Sprintf("AGENT %s TAKES IT FROM %s.", code(agent), code(c.StartBucket))
	if nested {
		who = fmt.Sprintf("IT HAS SUBTASKS OF ITS OWN, SO IT RUNS AS A CHAIN TOO: AGENT %s TAKES THEM ONE BY ONE FROM %s.",
			code(agent), code(c.StartBucket))
	}
	return fmt.Sprintf("<p>%s: %s. %s ONE CASE AT A TIME. THE NEXT ONE STARTS WHEN THIS ONE IS DONE.</p>",
		chainStartedMark, c.TaskLink(t), who) + Marker(ChainStarted, "")
}

// WaitingText is subtask_chain.waiting_comment.
func (c Chain) WaitingText(t event.Task) string {
	return fmt.Sprintf("<p>%s: %s. IT CARRIES %s, SO NO AGENT TOUCHES IT. DO IT, MARK IT DONE, AND THE CHAIN "+
		"MOVES ON. I'LL KEEP THE COFFEE WARM.</p>", chainWaitingMark, c.TaskLink(t), code(c.NoAgentLabel)) +
		Marker(ChainWaiting, "")
}

// StoppedText is subtask_chain.stopped_comment.
func (c Chain) StoppedText(t event.Task) string {
	b := code(c.StartBucket)
	return fmt.Sprintf("<p>%s: %s. YOU PULLED IT OFF %s, SO I WON'T PUSH IT BACK. DRAG IT TO %s TO RETRY, OR "+
		"MARK IT DONE TO SKIP IT, AND THE CHAIN MOVES ON.</p>", chainStoppedMark, c.TaskLink(t), b, b) +
		Marker(ChainStopped, "")
}

// NoAgentText is subtask_chain.no_agent_comment.
func (c Chain) NoAgentText() string {
	users := make([]string, len(c.AgentUsers))
	for i, u := range c.AgentUsers {
		users[i] = code(u)
	}
	return fmt.Sprintf("<p>%s. ASSIGN THIS TASK TO EXACTLY ONE OF %s: ITS SUBTASKS GET THAT AGENT, UNLESS ONE HAS "+
		"ITS OWN. THE FIRST ONE STARTS ON THE NEXT EDIT OF THIS TASK OR THE NEXT RECONCILE.</p>",
		chainNoAgentMark, strings.Join(users, ", ")) + Marker(ChainNoAgent, "")
}

// DoneText is subtask_chain.done_comment.
func (c Chain) DoneText(prs []string) string {
	var items strings.Builder
	for _, pr := range prs {
		fmt.Fprintf(&items, `<li><a href="%s">%s</a></li>`, escape(pr), escape(pr))
	}
	if len(prs) == 0 {
		items.WriteString("<li>NONE ON RECORD.</li>")
	}
	where := ""
	if c.ReviewBucket != "" {
		where = fmt.Sprintf(" THE CARD GOES TO %s.", code(c.ReviewBucket))
	}
	return fmt.Sprintf("<p>%s. EVERY SUBTASK IS DONE.%s LOOK IT OVER AND CLOSE IT YOURSELF.</p><p>MERGED:</p><ul>%s</ul>",
		chainDoneMark, where, items.String()) + Marker(ChainDone, "")
}

var (
	anyLink    = regexp.MustCompile(`<a href="([^"]+)"`)
	mergedLink = regexp.MustCompile(`(?:MERGED|Merged): <a href="([^"]+)"`)
	doneLink   = regexp.MustCompile(`<li><a href="([^"]+)"`)
)

// MergedPRs is subtask_chain.merged_prs over the subtasks' trusted
// comments, in chain order: each merge notice's PR link and a nested
// chain's done list.
func MergedPRs(comments [][]event.TaskComment, service string) []string {
	var out []string
	for _, cs := range comments {
		for _, c := range Trusted(cs, service) {
			for _, u := range commentPRs(c.Body) {
				if u = unescape(u); !slices.Contains(out, u) {
					out = append(out, u)
				}
			}
		}
	}
	return out
}

func commentPRs(text string) []string {
	pick := func(re *regexp.Regexp) []string {
		var out []string
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
		return out
	}
	switch {
	case HasKind(text, ChainDone, ""):
		return pick(doneLink)
	case HasKind(text, Merged, ""):
		return pick(anyLink)
	case len(Kinds(text)) == 0 && mergedLink.MatchString(text):
		return pick(mergedLink)
	}
	return nil
}

func unescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#x27;", "'", "&#39;", "'").Replace(s)
}

// UnansweredReason is one unanswered_notice.reasons item for a router
// reason code; assigned are the agent users on the task.
func UnansweredReason(reason, noAgentLabel string, agents, assigned []string) string {
	switch reason {
	case "project-not-configured":
		return "ITS PROJECT HAS NO KELOS SPAWNER: ADD IT TO " + code("vikunjaSpawners.projects")
	case "task-done":
		return "IT IS DONE: OPEN A NEW TASK FOR MORE WORK"
	case "no-agent-label":
		return fmt.Sprintf("IT CARRIES %s, WHICH KEEPS EVERY AGENT OFF IT", code(noAgentLabel))
	case "no-agent-assignee":
		return "IT IS ASSIGNED TO NO AGENT USER: ASSIGN ONE OF " + codes(agents)
	case "multiple-agent-assignees":
		return fmt.Sprintf("IT IS ASSIGNED TO SEVERAL AGENT USERS (%s): UNASSIGN ALL BUT ONE", codes(assigned))
	}
	return code(reason)
}

func codes(items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = code(s)
	}
	return strings.Join(out, ", ")
}
