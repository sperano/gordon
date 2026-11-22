package submit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

// Labels and annotation of a review Task (reviewspawners.yaml,
// supersede_reviews.py).
const (
	WorkspaceLabel = "vikunja.spe.quebec/workspace"
	ReviewerLabel  = "vikunja.spe.quebec/reviewer"
	PRLabel        = "vikunja.spe.quebec/pr"
	ReviewRole     = "pr-review"
	// RestartOf marks a limit-reaper restart: newer than every Task of
	// its PR, it reviews an older push and supersedes nothing.
	RestartOf = "kelos.spe.quebec/limit-restart-of"
)

// SupersedeStore is the part of the store a supersede uses.
type SupersedeStore interface {
	CancelStore
	RunInfo(ctx context.Context, name string) (store.RunInfo, error)
	OlderReviews(ctx context.Context, name string) ([]string, error)
}

// Superseder stops the reviews a newer push to the same pull request
// supersedes (action.SupersedeReviews, supersede_reviews.py), once the
// new review's Task exists. Holding the admission lock, it finishes
// Gordon's older queued or running review runs of the pull request by
// the same reviewer (a queued submission is never created) and lists
// every unfinished review Task of the pull request by that reviewer
// created before the new one, a legacy spawner's included, which it
// deletes after releasing the lock. Finished
// Tasks stay: their logs may matter, and Kelos's TTL removes them.
type Superseder struct {
	Store SupersedeStore
	Kelos Deleter
}

func (s *Superseder) run(a action.Action) (string, error) {
	parts := strings.Split(string(a.Target), "/")
	if a.Kind != action.SupersedeReviews || len(parts) != 3 || parts[0] != "kelos" || parts[2] == "" {
		return "", worker.Permanent(fmt.Errorf("not a review supersede: %s %s", a.Kind, a.Target))
	}
	return parts[2], nil
}

// older lists the live review Tasks the run supersedes: none when its
// own Task is not listed (gone already) or is a limit restart.
func (s *Superseder) older(ctx context.Context, name string) ([]kelos.Metadata, error) {
	info, err := s.Store.RunInfo(ctx, name)
	if err != nil {
		return nil, err
	}
	id := info.Identity
	tasks, err := s.Kelos.List(ctx, fmt.Sprintf("%s=%s,%s=%s,%s=%s", RoleLabel, ReviewRole,
		WorkspaceLabel, id.Workspace, ReviewerLabel, id.Reviewer))
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(tasks, func(t kelos.Task) bool { return t.Metadata.Name == name })
	if i < 0 {
		return nil, nil
	}
	own := tasks[i].Metadata
	if _, restart := own.Annotations[RestartOf]; restart {
		return nil, nil
	}
	created, err := time.Parse(time.RFC3339, own.CreationTimestamp)
	if err != nil {
		return nil, fmt.Errorf("Task %s creationTimestamp %q: %w", name, own.CreationTimestamp, err)
	}
	var out []kelos.Metadata
	for _, t := range tasks {
		m := t.Metadata
		at, err := time.Parse(time.RFC3339, m.CreationTimestamp)
		if m.Name == name || m.Labels[PRLabel] != own.Labels[PRLabel] || t.Finished() || err != nil || !at.Before(created) {
			continue
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b kelos.Metadata) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// cancelOlder finishes Gordon's older review runs of the pull request.
func (s *Superseder) cancelOlder(ctx context.Context, name string) error {
	names, err := s.Store.OlderReviews(ctx, name)
	if err != nil || len(names) == 0 {
		return err
	}
	_, err = s.Store.CancelRuns(ctx, names)
	return err
}

// Execute cancels the older runs under the admission lock, then deletes
// the older live Tasks it listed there once the lock is released: the
// Kelos deletes would hold every submission behind them.
func (s *Superseder) Execute(ctx context.Context, a action.Action) error {
	name, err := s.run(a)
	if err != nil {
		return err
	}
	var live []kelos.Metadata
	err = s.Store.WithAdmission(ctx, func(ctx context.Context) error {
		if err := s.cancelOlder(ctx, name); err != nil {
			return err
		}
		var err error
		if live, err = s.older(ctx, name); err != nil {
			return refusedPermanent(err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// By uid: a Task recreated under a listed name meanwhile is refused
	// (a Conflict, retried), never deleted unseen.
	for _, m := range live {
		if err := s.Kelos.Delete(ctx, m.Name, m.UID); err != nil && !kelos.IsNotFound(err) {
			return refusedPermanent(err)
		}
	}
	return nil
}

// Recover cancels the older runs, as Execute does, then reports the
// supersede applied once no older live Task is left.
func (s *Superseder) Recover(ctx context.Context, a action.Action) (bool, error) {
	name, err := s.run(a)
	if err != nil {
		return false, err
	}
	applied := false
	err = s.Store.WithAdmission(ctx, func(ctx context.Context) error {
		if err := s.cancelOlder(ctx, name); err != nil {
			return err
		}
		live, err := s.older(ctx, name)
		applied = err == nil && len(live) == 0
		return err
	})
	return applied, err
}
