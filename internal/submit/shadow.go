package submit

import (
	"context"
	"log/slog"
	"time"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

// DefaultShadowWindow is how long a shadow Task waits for the legacy Task
// of its name before it is recorded missing.
const DefaultShadowWindow = 15 * time.Minute

// gordonKeys are the metadata keys only Gordon sets: a legacy Task never
// has them, so they are no difference.
var gordonKeys = []string{dispatch.ManagedLabel, dispatch.InboxAnnotation, dispatch.VersionAnnotation}

// ShadowStore is the part of the store the comparison uses.
type ShadowStore interface {
	PendingShadow(ctx context.Context, limit int) ([]store.ShadowTask, error)
	ShadowCompared(ctx context.Context, name string, inboxID int64, result string, diff []string) error
}

// Shadow compares the Tasks shadow decisions would have submitted with
// the Tasks of the same names the legacy spawners created: a match, the
// paths that differ, or missing (no legacy Task within Window: legacy
// did not run what Gordon would have).
type Shadow struct {
	Store   ShadowStore
	Kelos   TaskAPI
	Window  time.Duration
	Batch   int
	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Once compares one batch.
func (s *Shadow) Once(ctx context.Context) error {
	batch := s.Batch
	if batch <= 0 {
		batch = DefaultObserveBatch
	}
	pending, err := s.Store.PendingShadow(ctx, batch)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if err := s.compare(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Shadow) compare(ctx context.Context, p store.ShadowTask) error {
	_, raw, err := s.Kelos.Get(ctx, p.Name)
	var result string
	var diff []string
	switch {
	case kelos.IsNotFound(err) && p.Age < s.window():
		return nil // legacy may still create it
	case kelos.IsNotFound(err):
		result = store.ShadowMissing
	case err != nil:
		return err
	default:
		if diff, err = kelos.Diff(p.Task, raw, gordonKeys...); err != nil {
			return err
		}
		result = store.ShadowMatch
		if len(diff) > 0 {
			result = store.ShadowDiffer
		}
	}
	if err := s.Store.ShadowCompared(ctx, p.Name, p.InboxID, result, diff); err != nil {
		return err
	}
	family := metrics.Bound(p.Family, familyLabels())
	s.Metrics.ShadowComparisons.WithLabelValues(family, result).Inc()
	s.Log.Info("shadow task compared", "task", p.Name, "result", result, "paths", diff)
	return nil
}

func (s *Shadow) window() time.Duration {
	if s.Window <= 0 {
		return DefaultShadowWindow
	}
	return s.Window
}

func familyLabels() []string {
	out := make([]string, len(run.Families))
	for i, f := range run.Families {
		out[i] = string(f)
	}
	return out
}
