// Package restart restarts Gordon's runs that a subscription limit
// stopped (limit_reaper.py), once the limit reset: a Kelos Task the limit
// stopped has failed for good, and nothing fires when the limit resets.
//
// Each pass takes the failed runs of the configured projects that it has
// not decided about yet, reads the
// namespace's Tasks once, and for each group of runs that must not run at
// the same time (one Vikunja task, one reviewer on one pull request, one
// pull request's follow-ups) with no Task still pending or running,
// restarts the newest limit stop as <name>-retry<n> (at most MaxRestarts
// in a chain), supersedes the older ones, and gives up after the last
// restart. The restart is an ordinary run of the stopped run's
// generation: the outbox submits it (its limits, its held branch), the
// observer follows it, a drag to the backlog stops it. Every decision is
// recorded on the stopped run in the transaction that queues its writes,
// so no restart is made twice.
package restart

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

// Defaults: limit_reaper.py's MAX_RESTARTS, RESET_GRACE_SECONDS and
// FALLBACK_DELAY_SECONDS, and the failed runs one pass decides about.
const (
	DefaultMaxRestarts = 3
	DefaultGrace       = 2 * time.Minute
	DefaultFallback    = time.Hour
	DefaultBatch       = 200
)

// Task labels a group is keyed on (limit_reaper.group_key).
const (
	taskLabel      = "vikunja.spe.quebec/task"
	projectLabel   = "vikunja.spe.quebec/project"
	roleLabel      = "vikunja.spe.quebec/role"
	workspaceLabel = "vikunja.spe.quebec/workspace"
	reviewerLabel  = "vikunja.spe.quebec/reviewer"
	prLabel        = "vikunja.spe.quebec/pr"
	reviewRole     = "pr-review"
)

// modelFreeRoles are limit_reaper.MODEL_FREE_ROLES: Tasks that run no
// model never stop at a limit and hold no group.
var modelFreeRoles = []string{"conflict-reconcile", "merge-close", "pr-open", "pr-readiness"}

// followUpSuffix is limit_reaper.FOLLOW_UP_SUFFIX.
var followUpSuffix = regexp.MustCompile(`-[clpr]\d+$`)

// Store is the part of the store the pass uses.
type Store interface {
	FailedUndecided(ctx context.Context, projects []string, limit int) ([]store.FailedRun, error)
	DecideFailed(ctx context.Context, f store.FailedRun, mark string, restart *route.Run, fd route.FamilyDecision) (bool, error)
}

// TaskLister lists the namespace's Kelos Tasks.
type TaskLister interface {
	List(ctx context.Context, selector string) ([]kelos.Task, error)
}

// Reaper is the limit restart pass (a worker.Pass).
type Reaper struct {
	Store  Store
	Kelos  TaskLister
	Config profile.Config
	// MaxRestarts bounds a chain; Grace is waited after the reset,
	// Fallback after the completion when no reset time is known.
	MaxRestarts     int
	Grace, Fallback time.Duration
	Batch           int
	Now             func() time.Time
	Metrics         *metrics.Metrics
	Log             *slog.Logger
}

// stopped is a failed run with its Task.
type stopped struct {
	run  store.FailedRun
	task kelos.Task
}

// Once runs one pass. A decision that fails does not stop the others.
func (r *Reaper) Once(ctx context.Context) error {
	projects := r.projects()
	if len(projects) == 0 {
		return nil
	}
	failed, err := r.Store.FailedUndecided(ctx, projects, cmp.Or(r.Batch, DefaultBatch))
	if err != nil || len(failed) == 0 {
		return err
	}
	tasks, err := r.Kelos.List(ctx, "")
	if err != nil {
		return err
	}
	groups, busy, errs := r.sort(ctx, failed, tasks)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if busy[k] {
			r.Log.Debug("limit stop waits for a running Task of its group", "task", groups[k][0].run.Name)
			continue
		}
		errs = append(errs, r.group(ctx, groups[k]))
	}
	return errors.Join(errs...)
}

// projects are the configured projects, each once: Gordon restarts their
// limit stops.
func (r *Reaper) projects() []string {
	var out []string
	for _, p := range r.Config.Projects {
		if !slices.Contains(out, p.Name) {
			out = append(out, p.Name)
		}
	}
	return out
}

// sort groups the failed runs whose Task the limit stopped, decides the
// others (none), and reports the groups with a Task still unfinished. A
// run whose Task is not listed is left for the observer (it is gone); one
// whose listed Task is another generation's or submitter's is none of
// this run's business.
func (r *Reaper) sort(ctx context.Context, failed []store.FailedRun, tasks []kelos.Task) (map[string][]stopped, map[string]bool, []error) {
	byName := map[string]kelos.Task{}
	busy := map[string]bool{}
	for _, t := range tasks {
		byName[t.Metadata.Name] = t
		if !slices.Contains(modelFreeRoles, t.Metadata.Labels[roleLabel]) && !t.Finished() {
			busy[groupKey(t)] = true
		}
	}
	groups := map[string][]stopped{}
	var errs []error
	for _, f := range failed {
		t, ok := byName[f.Name]
		switch {
		case !ok:
		case t.Metadata.Annotations[inboxAnnotation] != fmt.Sprint(f.InboxID) || !t.LimitStop():
			errs = append(errs, r.decide(ctx, f, store.LimitNone, nil, nil))
		default:
			groups[groupKey(t)] = append(groups[groupKey(t)], stopped{f, t})
		}
	}
	return groups, busy, errs
}

// groupKey is limit_reaper.group_key: one Vikunja task, one reviewer on
// one pull request (any head), or one pull request's follow-ups (the
// name without its comment or review id).
func groupKey(t kelos.Task) string {
	l := t.Metadata.Labels
	if id, ok := l[taskLabel]; ok {
		return strings.Join([]string{"vikunja", l[projectLabel], id}, "\x00")
	}
	if l[roleLabel] == reviewRole {
		return strings.Join([]string{reviewRole, l[workspaceLabel], l[reviewerLabel], l[prLabel]}, "\x00")
	}
	return "name\x00" + followUpSuffix.ReplaceAllString(run.StripRetry(t.Metadata.Name), "")
}

// group restarts the newest stop of a group and supersedes the others.
func (r *Reaper) group(ctx context.Context, g []stopped) error {
	slices.SortFunc(g, func(a, b stopped) int {
		return cmp.Or(cmp.Compare(a.task.Metadata.CreationTimestamp, b.task.Metadata.CreationTimestamp),
			cmp.Compare(a.run.Name, b.run.Name))
	})
	newest := g[len(g)-1]
	var errs []error
	for _, s := range g[:len(g)-1] {
		r.Log.Info("limit stop superseded", "task", s.run.Name, "by", newest.run.Name)
		errs = append(errs, r.decide(ctx, s.run, store.LimitSuperseded, nil, nil))
	}
	return errors.Join(append(errs, r.newest(ctx, newest))...)
}

func (r *Reaper) maxRestarts() int { return cmp.Or(r.MaxRestarts, DefaultMaxRestarts) }

func (r *Reaper) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// newest restarts the group's newest stop once its limit reset, or gives
// up after the last restart (limit_reaper.Reaper.handle_newest). A stop
// whose Task tells no completion time restarts at once: nothing else
// tells when the limit resets.
func (r *Reaper) newest(ctx context.Context, s stopped) error {
	attempt := s.run.Identity.Retry + 1
	if attempt > r.maxRestarts() {
		r.Log.Info("limit stop gave up", "task", s.run.Name, "restarts", r.maxRestarts())
		return r.decide(ctx, s.run, store.LimitGaveUp, nil, r.gaveUp(s.run))
	}
	if reset, ok := s.task.LimitReset(cmp.Or(r.Fallback, DefaultFallback)); ok {
		if due := reset.Add(cmp.Or(r.Grace, DefaultGrace)); r.now().Before(due) {
			r.Log.Debug("limit stop waits for its reset", "task", s.run.Name, "due", due.UTC().Format(time.RFC3339))
			return nil
		}
	}
	id := s.run.Identity
	id.Retry = attempt
	if name := id.Name(); len(name) > run.MaxNameLength {
		r.Log.Warn("limit restart name too long", "task", s.run.Name, "restart", name)
		return r.decide(ctx, s.run, store.LimitNameTooLong, nil, nil)
	}
	rn := route.Run{Name: id.Name(), Branch: id.Branch(), Identity: id, Profile: s.run.Profile}
	task := Task(s.task, rn.Name, attempt)
	rn.Task = &task
	r.Log.Info("limit stop restarted", "task", s.run.Name, "restart", rn.Name)
	return r.decide(ctx, s.run, store.LimitRestarted, &rn, r.restartWrites(s.run, rn))
}

// decide records one decision, counted.
func (r *Reaper) decide(ctx context.Context, f store.FailedRun, mark string, rn *route.Run, actions []action.Action) error {
	fd := route.FamilyDecision{Family: run.LimitRestart, Project: f.Project,
		Outcome: route.Act, Actions: actions}
	if rn != nil {
		fd.Outcome, fd.Runs = route.Dispatch, []route.Run{*rn}
	}
	done, err := r.Store.DecideFailed(ctx, f, mark, rn, fd)
	if err != nil {
		return fmt.Errorf("limit stop %s: %w", f.Name, err)
	}
	if done {
		r.Metrics.LimitRestarts.WithLabelValues(mark).Inc()
	}
	return nil
}
