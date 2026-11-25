package costs

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/sillage"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

// Defaults of the pass.
const (
	// DefaultWait paces the checks of a notice waiting for Sillage's
	// final estimate (Sillage's own grace after a run ends is 20 minutes).
	DefaultWait = 2 * time.Minute
	// DefaultDeadline is how long after its run finished a notice waits
	// for a final estimate before it is posted marked not final.
	DefaultDeadline = 6 * time.Hour
	// DefaultRefresh paces the checks of a live notice.
	DefaultRefresh = 15 * time.Minute
	// DefaultLiveFor is how long after its last revision a live notice is
	// kept current: a review or fix that comes later is in the next
	// run's notice, or in Sillage.
	DefaultLiveFor = 14 * 24 * time.Hour
	// DefaultHold is how long a pass holds the notices it took.
	DefaultHold = 5 * time.Minute
	// DefaultBatch bounds the notices one pass takes.
	DefaultBatch = 100
	// DefaultRetryBase and DefaultRetryMax bound the backoff after
	// Sillage failed to answer.
	DefaultRetryBase = time.Minute
	DefaultRetryMax  = 30 * time.Minute
)

// Results of one notice check, the cost_notice_checks_total result label.
const (
	ResultPublished    = "published"
	ResultUpdated      = "updated"
	ResultUnchanged    = "unchanged"
	ResultWaiting      = "waiting"
	ResultSettled      = "settled"
	ResultUnavailable  = "unavailable"
	ResultIncompatible = "incompatible"
	ResultRaced        = "raced"
	// ResultExpired: Sillage still failed to answer once the notice's
	// live window passed; it is checked no more.
	ResultExpired = "expired"
)

// Results lists every result label.
var Results = []string{ResultPublished, ResultUpdated, ResultUnchanged, ResultWaiting, ResultSettled,
	ResultUnavailable, ResultIncompatible, ResultRaced, ResultExpired}

// PassStore is the part of the store the pass uses.
type PassStore interface {
	CostDue(ctx context.Context, now time.Time, hold time.Duration, limit int) ([]store.CostNotice, error)
	CostCheck(ctx context.Context, n store.CostNotice, next time.Time, failed bool, reason string) (bool, error)
	CostPublish(ctx context.Context, n store.CostNotice, body, digest string, now, liveUntil, next time.Time) (bool, error)
	CostSettle(ctx context.Context, n store.CostNotice) (bool, error)
}

// Reader reads a Vikunja task's cost from Sillage.
type Reader interface {
	TaskCost(ctx context.Context, host string, id int64) (sillage.TaskCost, error)
}

// Links makes a notice's Lucarne run link and Sillage task link.
type Links interface {
	Run(uid string) string
	Activity(task int64) string
}

// Publisher is the cost notice pass (a worker.Pass).
type Publisher struct {
	Store   PassStore
	Sillage Reader
	Links   Links
	// VikunjaWeb is the Vikunja web base, whose host names the task to
	// Sillage.
	VikunjaWeb                             string
	Wait, Deadline, Refresh, LiveFor, Hold time.Duration
	Backoff                                worker.Backoff
	Batch                                  int
	Now                                    func() time.Time
	Metrics                                *metrics.Metrics
	Log                                    *slog.Logger
}

// read is one task's Sillage answer.
type read struct {
	cost sillage.TaskCost
	err  error
}

// Once checks the due notices, reading each task's cost once. A notice
// that fails does not stop the others.
func (p *Publisher) Once(ctx context.Context) error {
	host, err := p.host()
	if err != nil {
		return err
	}
	now := p.now()
	due, err := p.Store.CostDue(ctx, now, cmp.Or(p.Hold, DefaultHold), cmp.Or(p.Batch, DefaultBatch))
	if err != nil || len(due) == 0 {
		return err
	}
	reads := map[int64]read{}
	var errs []error
	for _, n := range due {
		r, ok := reads[n.TaskID]
		if !ok {
			c, err := p.Sillage.TaskCost(ctx, host, n.TaskID)
			r = read{c, err}
			reads[n.TaskID] = r
		}
		errs = append(errs, p.check(ctx, n, r, now))
	}
	return errors.Join(errs...)
}

// host is the Vikunja host Sillage keys tasks on.
func (p *Publisher) host() (string, error) {
	u, err := url.Parse(p.VikunjaWeb)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("cost notices: no Vikunja host in %q", p.VikunjaWeb)
	}
	return u.Hostname(), nil
}

// check decides one notice from its task's read.
func (p *Publisher) check(ctx context.Context, n store.CostNotice, r read, now time.Time) error {
	if r.err != nil && !errors.Is(r.err, sillage.ErrNotFound) {
		return p.failed(ctx, n, r.err, now)
	}
	run, known := r.cost.RunByUID(n.TaskUID)
	final := known && run.Final
	if n.State == store.CostWaiting && !final && now.Before(n.Finished.Add(cmp.Or(p.Deadline, DefaultDeadline))) {
		return p.recheck(ctx, n, now.Add(cmp.Or(p.Wait, DefaultWait)), ResultWaiting)
	}
	body := Render(n, r.cost, run, known, p.Links)
	digest := Digest(body)
	refresh := now.Add(cmp.Or(p.Refresh, DefaultRefresh))
	if n.State == store.CostLive && digest == n.Digest {
		if !now.Before(n.LiveUntil) {
			ok, err := p.Store.CostSettle(ctx, n)
			return p.done(n, ok, err, ResultSettled)
		}
		return p.recheck(ctx, n, refresh, ResultUnchanged)
	}
	ok, err := p.Store.CostPublish(ctx, n, body, digest, now, now.Add(cmp.Or(p.LiveFor, DefaultLiveFor)), refresh)
	result := ResultPublished
	if n.State == store.CostLive {
		result = ResultUpdated
	}
	return p.done(n, ok, err, result)
}

// failed records a read of Sillage that failed: a retry with backoff,
// until the notice's live window passed (a waiting notice's counts from
// its run's finish), when it settles as it is: a waiting one is never
// posted.
func (p *Publisher) failed(ctx context.Context, n store.CostNotice, err error, now time.Time) error {
	expiry := n.LiveUntil
	if n.State == store.CostWaiting {
		expiry = n.Finished.Add(cmp.Or(p.LiveFor, DefaultLiveFor))
	}
	if !now.Before(expiry) {
		p.Log.Warn("sillage cost read failed past the live window", "task", n.Name, "state", n.State,
			"failures", n.Failures+1, "error", err.Error())
		ok, serr := p.Store.CostSettle(ctx, n)
		return p.done(n, ok, serr, ResultExpired)
	}
	result := ResultUnavailable
	if errors.Is(err, sillage.ErrIncompatible) {
		result = ResultIncompatible
	}
	b := p.Backoff
	if b.Base <= 0 || b.Max < b.Base {
		b = worker.Backoff{Base: DefaultRetryBase, Max: DefaultRetryMax}
	}
	p.Log.Warn("sillage cost read failed", "task", n.Name, "failures", n.Failures+1, "error", err.Error())
	ok, serr := p.Store.CostCheck(ctx, n, now.Add(b.Delay(n.Failures+1)), true, err.Error())
	return p.done(n, ok, serr, result)
}

func (p *Publisher) recheck(ctx context.Context, n store.CostNotice, next time.Time, result string) error {
	ok, err := p.Store.CostCheck(ctx, n, next, false, "")
	return p.done(n, ok, err, result)
}

// done counts a check: raced when another pass changed the row first.
func (p *Publisher) done(n store.CostNotice, ok bool, err error, result string) error {
	if err != nil {
		return fmt.Errorf("cost notice of %s: %w", n.Name, err)
	}
	if !ok {
		result = ResultRaced
	}
	p.Metrics.CostChecks.WithLabelValues(result).Inc()
	p.Log.Debug("cost notice checked", "task", n.Name, "result", result)
	return nil
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Digest identifies a notice's text.
func Digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
