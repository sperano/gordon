package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/worker"
)

// The ntfy bookkeeping reactions on a task (pr_ready_notify.py): a
// reservation before each send, its failure, and the delivery. A task has
// one open PR branch at a time, so the short head SHA is identity enough.
const (
	PendingReaction = "⏳"
	RetryReaction   = "🔁"
)

// DeliveredWriteAttempts bounds the tries at recording a send that
// succeeded: the send itself is never repeated for it.
const DeliveredWriteAttempts = 3

// errNtfySend is a request to ntfy that got no answer. The transport's
// error is dropped: a url.Error carries the endpoint, whose path is the
// private topic.
var errNtfySend = errors.New("ntfy: request failed")

// MaxUnconfirmedSends bounds the sends whose outcome could not be
// recorded (pr_ready_notify.MAX_UNCONFIRMED_ATTEMPTS).
const MaxUnconfirmedSends = 3

// Ntfy publishes to a private ntfy topic with a bearer token, over HTTPS
// only, never following a redirect (which would carry the token away).
type Ntfy struct {
	URL, Topic, Token string
	HTTP              *http.Client
}

// ErrNtfyConfig is an ntfy publisher missing a setting or not on HTTPS.
var ErrNtfyConfig = errors.New("ntfy configuration")

// valid is pr_ready_notify.validate_config.
func (n *Ntfy) valid() error {
	if n == nil || n.URL == "" || n.Topic == "" || n.Token == "" || n.HTTP == nil {
		return fmt.Errorf("%w: missing URL, topic or token", ErrNtfyConfig)
	}
	if u, err := url.Parse(n.URL); err != nil || u.Scheme != "https" {
		return fmt.Errorf("%w: the URL must use HTTPS", ErrNtfyConfig)
	}
	return nil
}

// Publish is pr_ready_notify.publish: the PR, its full head SHA and URL.
// The answer's body is never read.
func (n *Ntfy) Publish(ctx context.Context, h notice.Head) error {
	if err := n.valid(); err != nil {
		return err
	}
	endpoint := strings.TrimRight(n.URL, "/") + "/" + url.PathEscape(n.Topic)
	ref := fmt.Sprintf("%s#%d", h.Repo, h.Number)
	body := fmt.Sprintf("PR ready: %s\nHead: %s\n%s", ref, h.SHA, h.URL())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		return worker.Permanent(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("Title", "PR ready: "+ref)
	req.Header.Set("Click", h.URL())
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	client := *n.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errNtfySend
	}
	resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}

// sendState is a head's ntfy bookkeeping, from the task's reactions.
type sendState struct {
	delivered         bool
	attempts, retries map[int]bool
	next              int
}

func (s sendState) unconfirmed() int {
	n := 0
	for a := range s.attempts {
		if !s.retries[a] {
			n++
		}
	}
	return n
}

// attemptNumbers is pr_ready_notify.attempt_numbers.
func attemptNumbers(values []string, prefix, short string) map[int]bool {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(prefix+short+"-") + `(\d+)$`)
	out := map[int]bool{}
	for _, v := range values {
		if m := re.FindStringSubmatch(v); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				out[n] = true
			}
		}
	}
	return out
}

func (e *Executor) sendState(ctx context.Context, task int64, sha string) (sendState, error) {
	rs, err := e.W.TaskReactions(ctx, task)
	if err != nil {
		return sendState{}, err
	}
	values := make([]string, 0, len(rs))
	for v := range rs {
		values = append(values, v)
	}
	short := notice.ShortSHA(sha)
	s := sendState{attempts: attemptNumbers(values, PendingReaction, short), retries: attemptNumbers(values, RetryReaction, short)}
	for _, v := range values {
		s.delivered = s.delivered || v == route.DeliveredReaction+short
	}
	for a := range s.attempts {
		s.next = max(s.next, a)
	}
	s.next++
	return s, nil
}

func notifyHead(a action.Action) (notice.Head, error) {
	h, err := notice.ParseHead(a.Detail)
	if err != nil || h.SHA != a.Value {
		return notice.Head{}, worker.Permanent(fmt.Errorf("notify head %q for %q", a.Detail, a.Value))
	}
	return h, nil
}

// notifyApplied reports nothing left to send: the head delivered, or
// MaxUnconfirmedSends sends whose outcome is unknown (pr_ready_notify.run
// stops there rather than notify the owner again and again).
func (e *Executor) notifyApplied(ctx context.Context, a action.Action) (bool, error) {
	id, err := taskTarget(a)
	if err != nil {
		return false, err
	}
	if _, err := notifyHead(a); err != nil {
		return false, err
	}
	s, err := e.sendState(ctx, id, a.Value)
	return s.delivered || s.unconfirmed() >= MaxUnconfirmedSends, err
}

// notify is pr_ready_notify.send_attempt, under the task's target lock
// so two decisions of one head never send it twice: the reservation is
// reacted first (a send is never made without it), then the send, then
// the delivery, or the reservation's failure for a later retry. A send
// whose delivery cannot be recorded fails permanently: a retry would
// send it again.
func (e *Executor) notify(ctx context.Context, a action.Action) error {
	id, _ := taskTarget(a)
	h, err := notifyHead(a)
	if err != nil {
		return err
	}
	if err := e.Ntfy.valid(); err != nil {
		return worker.Permanent(err)
	}
	ctx, cancel := context.WithTimeout(ctx, noticeLockTimeout)
	defer cancel()
	return e.Lock.WithTargetLock(ctx, string(a.Target), func(ctx context.Context) error {
		s, err := e.sendState(ctx, id, h.SHA)
		if err != nil || s.delivered || s.unconfirmed() >= MaxUnconfirmedSends {
			return err
		}
		short := notice.ShortSHA(h.SHA)
		attempt := fmt.Sprintf("%s-%d", short, s.next)
		if err := e.W.ReactTask(ctx, id, PendingReaction+attempt); err != nil {
			return err
		}
		if err := e.Ntfy.Publish(ctx, h); err != nil {
			return errors.Join(err, e.W.ReactTask(ctx, id, RetryReaction+attempt))
		}
		return e.recordDelivery(ctx, id, route.DeliveredReaction+short)
	})
}

// recordDelivery reacts a succeeded send's delivery, up to
// DeliveredWriteAttempts times.
func (e *Executor) recordDelivery(ctx context.Context, id int64, value string) error {
	var err error
	for range DeliveredWriteAttempts {
		if err = e.W.ReactTask(ctx, id, value); err == nil {
			return nil
		}
	}
	return worker.Permanent(fmt.Errorf("ntfy sent, delivery not recorded: %w", err))
}
