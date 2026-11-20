package lifecycle

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/worker"
)

// Links are the private links a start or failure notice carries.
type Links struct {
	// Lucarne and Cluster make the run link
	// (<lucarne>/runs/<cluster>/<namespace>/<Task uid>, run_link.run_url).
	Lucarne, Cluster, Namespace string
	// Sillage and VikunjaWeb make the activity link
	// (<sillage>/task/<Vikunja host>/<task id>, sillage_story_url).
	Sillage, VikunjaWeb string
}

// Run is the run's Lucarne link, "" without a base, a cluster or a uid.
func (l Links) Run(uid string) string {
	if l.Lucarne == "" || l.Cluster == "" || uid == "" {
		return ""
	}
	parts := []string{strings.TrimRight(l.Lucarne, "/"), "runs", url.PathEscape(l.Cluster),
		url.PathEscape(l.Namespace), url.PathEscape(uid)}
	return strings.Join(parts, "/")
}

// Activity is the task's Sillage story link, "" without a base.
func (l Links) Activity(task int64) string {
	u, err := url.Parse(l.VikunjaWeb)
	if l.Sillage == "" || err != nil || u.Hostname() == "" {
		return ""
	}
	return fmt.Sprintf("%s/task/%s/%d", strings.TrimRight(l.Sillage, "/"), url.PathEscape(u.Hostname()), task)
}

func (e *Executor) trusted(ctx context.Context, task int64) ([]event.TaskComment, error) {
	cs, err := e.W.Comments(ctx, task)
	if err != nil {
		return nil, err
	}
	out := make([]event.TaskComment, len(cs))
	for i, c := range cs {
		out[i] = event.TaskComment{ID: c.ID, Author: c.Author.Username, Body: c.Comment}
	}
	return notice.Trusted(out, e.Policy.VikunjaServiceUser), nil
}

// legacyPosted reports a notice of the same intent already on the task,
// posted by Gordon's user before Gordon kept action markers, or by the
// legacy script it replaces (the chain's notify_once, the start
// announcement of that Task, the discuss start comment of that request).
func (e *Executor) legacyPosted(a action.Action, body string) bool {
	switch a.Value {
	case notice.Start:
		return notice.HasKind(body, notice.Start, a.Detail)
	case notice.DiscussStart:
		c, err := strconv.ParseInt(a.Detail, 10, 64)
		return err == nil && strings.Contains(body, notice.DiscussMarker(c))
	case notice.ChainStarted, notice.ChainWaiting, notice.ChainStopped:
		sub, err := strconv.ParseInt(a.Detail, 10, 64)
		return err == nil && notice.AboutTask(body, a.Value, e.Policy.VikunjaWebURL, sub)
	case notice.ChainNoAgent, notice.ChainDone:
		return notice.HasKind(body, a.Value, "")
	}
	return false
}

func (e *Executor) noticeApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := taskTarget(a)
	if err != nil {
		return false, err
	}
	cs, err := e.trusted(ctx, id)
	if err != nil {
		return false, err
	}
	marker := notice.ActionMarker(a.Key)
	return slices.ContainsFunc(cs, func(c event.TaskComment) bool {
		return strings.Contains(c.Body, marker) || e.legacyPosted(a, c.Body)
	}), nil
}

// noticeLockTimeout bounds the check and the post held under a notice's
// target lock, so a slow Vikunja cannot keep the lock's pool connection
// idle in its transaction for longer. A post cut short is ambiguous and
// recovered by its marker.
const noticeLockTimeout = time.Minute

// postNotice renders the notice, then, holding the target lock, checks
// again that it is missing and posts it with its action marker. The
// render (the run's row, the subtasks of a done chain) happens before the
// lock: the lock's transaction holds a pool connection, and a read needing
// a second one while every connection is so held would wait for ever.
func (e *Executor) postNotice(ctx context.Context, a action.Action) error {
	id, _ := taskTarget(a)
	text, replace, err := e.render(ctx, a, id)
	if err != nil {
		return err
	}
	text += notice.ActionMarker(a.Key)
	ctx, cancel := context.WithTimeout(ctx, noticeLockTimeout)
	defer cancel()
	return e.Lock.WithTargetLock(ctx, string(a.Target), func(ctx context.Context) error {
		if done, err := e.noticeApplied(ctx, a); err != nil || done {
			return err
		}
		return e.post(ctx, id, text, replace)
	})
}

// post adds the comment; a discuss follow-up's start notice replaces the
// fallback line its request (comment replace) got.
func (e *Executor) post(ctx context.Context, id int64, text string, replace int64) error {
	if replace > 0 {
		cs, err := e.trusted(ctx, id)
		if err != nil {
			return err
		}
		if c, ok := notice.Find(cs, e.Policy.VikunjaServiceUser, func(b string) bool {
			return strings.Contains(b, notice.DiscussMarker(replace))
		}); ok {
			return e.W.UpdateComment(ctx, id, c.ID, text)
		}
	}
	_, err := e.W.Comment(ctx, id, text)
	return err
}

// render is the notice's text and, for a discuss follow-up's start, the
// request comment whose fallback line it replaces.
func (e *Executor) render(ctx context.Context, a action.Action, task int64) (string, int64, error) {
	p := e.Policy
	switch a.Value {
	case notice.Start:
		return e.startText(ctx, a.Detail, task)
	case notice.DiscussStart:
		c, err := strconv.ParseInt(a.Detail, 10, 64)
		if err != nil {
			return "", 0, worker.Permanent(fmt.Errorf("discuss-start detail %q", a.Detail))
		}
		return notice.DiscussFallbackText(c), 0, nil
	case notice.Unanswered:
		text, err := e.unansweredText(ctx, a.Detail, task)
		return text, 0, err
	case notice.Stop:
		t, err := e.W.Task(ctx, task)
		if err != nil {
			return "", 0, err
		}
		note := ""
		if hasLabel(t, p.InProgressLabel) {
			note = notice.LabelKeptNote(p.InProgressLabel)
		}
		return notice.StopText(p.BacklogBucket, p.DoingBucket, strings.Split(a.Detail, ","), note), 0, nil
	case notice.Failed:
		text, err := e.failedText(ctx, a.Detail)
		return text, 0, err
	}
	text, err := e.chainText(ctx, a, task)
	return text, 0, err
}

var followUps = []run.Family{run.VikunjaFollowUp, run.VikunjaFollowUpAnswer, run.VikunjaFollowUpDiscuss}

func (e *Executor) startText(ctx context.Context, name string, task int64) (string, int64, error) {
	r, err := e.Runs.RunInfo(ctx, name)
	if err != nil {
		return "", 0, err
	}
	s := notice.StartInfo{Task: name, Model: r.Model, Effort: r.Effort,
		FollowUp: slices.Contains(followUps, r.Family),
		Discuss:  r.Family == run.VikunjaDiscuss || r.Family == run.VikunjaFollowUpDiscuss,
		RunURL:   e.Links.Run(r.TaskUID), ActivityURL: e.Links.Activity(task)}
	if r.Family == run.VikunjaFollowUpDiscuss {
		s.SourceComment = r.Identity.CommentID
	}
	return notice.StartText(s), s.SourceComment, nil
}

func (e *Executor) unansweredText(ctx context.Context, detail string, task int64) (string, error) {
	cs, reason, ok := strings.Cut(detail, ":")
	comment, err := strconv.ParseInt(cs, 10, 64)
	if !ok || err != nil {
		return "", worker.Permanent(fmt.Errorf("unanswered detail %q", detail))
	}
	t, err := e.W.Task(ctx, task)
	if err != nil {
		return "", err
	}
	var assigned []string
	for _, u := range t.Assignees {
		if slices.Contains(e.Agents, u.Username) {
			assigned = append(assigned, u.Username)
		}
	}
	item := notice.UnansweredReason(reason, e.Policy.NoAgentLabel, e.Agents, assigned)
	return notice.UnansweredText(e.Policy.Trigger, comment, []string{item}), nil
}

func (e *Executor) failedText(ctx context.Context, name string) (string, error) {
	r, err := e.Runs.RunInfo(ctx, name)
	if err != nil {
		return "", err
	}
	msg := ""
	if e.Messages != nil {
		msg = e.Messages(ctx, name)
	}
	return notice.FailedText(notice.FailedInfo{Task: name, Message: msg, InProgressLabel: e.Policy.InProgressLabel,
		ErrorLabel: e.Policy.AgentErrorLabel, Namespace: e.Namespace, FollowUp: slices.Contains(followUps, r.Family),
		RunURL: e.Links.Run(r.TaskUID)}), nil
}

func (e *Executor) chain() notice.Chain {
	p := e.Policy
	return notice.Chain{Web: p.VikunjaWebURL, StartBucket: p.DoingBucket, ReviewBucket: p.ReviewBucket,
		NoAgentLabel: p.NoAgentLabel, AgentUsers: e.Agents}
}

func eventTask(t Task) event.Task { return event.Task{ID: t.ID, Title: t.Title, Index: t.Index} }

func (e *Executor) chainText(ctx context.Context, a action.Action, parent int64) (string, error) {
	c := e.chain()
	switch a.Value {
	case notice.ChainNoAgent:
		return c.NoAgentText(), nil
	case notice.ChainDone:
		return e.doneText(ctx, parent)
	case notice.ChainStarted, notice.ChainWaiting, notice.ChainStopped:
	default:
		return "", worker.Permanent(fmt.Errorf("no text for notice kind %q", a.Value))
	}
	sub, err := strconv.ParseInt(a.Detail, 10, 64)
	if err != nil {
		return "", worker.Permanent(fmt.Errorf("%s detail %q", a.Value, a.Detail))
	}
	t, err := e.W.Task(ctx, sub)
	if err != nil {
		return "", err
	}
	switch a.Value {
	case notice.ChainWaiting:
		return c.WaitingText(eventTask(t)), nil
	case notice.ChainStopped:
		return c.StoppedText(eventTask(t)), nil
	}
	agent := ""
	if i := slices.IndexFunc(t.Assignees, func(u User) bool { return slices.Contains(e.Agents, u.Username) }); i >= 0 {
		agent = t.Assignees[i].Username
	}
	return c.StartedText(eventTask(t), agent, hasLabel(t, e.Policy.ChainLabel)), nil
}

// doneText lists the PRs the subtasks' merge notices (and nested chains'
// done notices) name, in chain order.
func (e *Executor) doneText(ctx context.Context, parent int64) (string, error) {
	pt, err := e.W.Task(ctx, parent)
	if err != nil {
		return "", err
	}
	var subs []Task
	for _, rel := range pt.RelatedTasks["subtask"] {
		t, err := e.W.Task(ctx, rel.ID)
		if err != nil {
			return "", err
		}
		subs = append(subs, t)
	}
	slices.SortFunc(subs, func(a, b Task) int { return cmp.Or(cmp.Compare(a.Index, b.Index), cmp.Compare(a.ID, b.ID)) })
	var comments [][]event.TaskComment
	for _, t := range subs {
		cs, err := e.trusted(ctx, t.ID)
		if err != nil {
			return "", err
		}
		comments = append(comments, cs)
	}
	return e.chain().DoneText(notice.MergedPRs(comments, e.Policy.VikunjaServiceUser)), nil
}
