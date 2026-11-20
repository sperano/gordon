package submit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

func (e *env) shadow(window time.Duration) *Shadow {
	return &Shadow{Store: e.store, Kelos: e.api, Window: window, Metrics: e.metrics,
		Log: logging.New(e.logs, slog.LevelDebug)}
}

func (e *env) shadowRow(t *testing.T) (result, diff string) {
	t.Helper()
	storetest.QueryRow(t, e.dbURL, `SELECT result, coalesce(diff::text, '') FROM shadow_tasks`, nil, &result, &diff)
	return result, diff
}

func (e *env) compared(result string) float64 {
	return testutil.ToFloat64(e.metrics.ShadowComparisons.WithLabelValues("vikunja-implement", result))
}

// legacyTwin is the Task the legacy spawner made for the shadow run: the
// recorded Task without Gordon's own keys.
func (e *env) legacyTwin(t *testing.T, name string) kelos.Task {
	t.Helper()
	var tk kelos.Task
	storetest.QueryRow(t, e.dbURL, `SELECT task FROM shadow_tasks WHERE name = $1`, []any{name}, &tk)
	delete(tk.Metadata.Labels, dispatch.ManagedLabel)
	delete(tk.Metadata.Annotations, dispatch.InboxAnnotation)
	delete(tk.Metadata.Annotations, dispatch.VersionAnnotation)
	tk.Metadata.Labels["kelos.dev/taskspawner"] = "vikunja-gordon-claude-opus"
	return tk
}

func TestShadowRunsSubmitNothingAndMatchLegacy(t *testing.T) {
	e := newEnv(t, true, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	if n := count(t, e, `SELECT (SELECT count(*) FROM actions) + (SELECT count(*) FROM runs)`); n != 0 {
		t.Fatalf("shadow queued %d actions or runs", n)
	}
	sh := e.shadow(time.Hour)
	if err := sh.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if result, _ := e.shadowRow(t); result != store.ShadowPending {
		t.Fatalf("no legacy Task yet: %s, want still pending", result)
	}
	e.kelos.Put(e.legacyTwin(t, name))
	if err := sh.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if result, diff := e.shadowRow(t); result != store.ShadowMatch || diff != "" || e.compared(store.ShadowMatch) != 1 {
		t.Fatalf("shadow comparison %s %s, want a match", result, diff)
	}
	if len(e.kelos.Creates()) != 0 {
		t.Fatal("shadow created a Task")
	}
}

func TestShadowDiffRecordsThePaths(t *testing.T) {
	e := newEnv(t, true, dispatch.Limits{})
	name := e.implement(t, implementID, "claude-opus")
	twin := e.legacyTwin(t, name)
	twin.Spec.Worker.Model, twin.Spec.Prompt = "claude-sonnet-5", "other"
	e.kelos.Put(twin)
	if err := e.shadow(time.Hour).Once(ctx); err != nil {
		t.Fatal(err)
	}
	if result, diff := e.shadowRow(t); result != store.ShadowDiffer || diff != `["spec.prompt", "spec.worker.model"]` ||
		e.compared(store.ShadowDiffer) != 1 {
		t.Fatalf("shadow comparison %s %s", result, diff)
	}
}

func TestShadowMissingAfterTheWindow(t *testing.T) {
	e := newEnv(t, true, dispatch.Limits{})
	e.implement(t, implementID, "claude-opus")
	if err := e.shadow(time.Nanosecond).Once(ctx); err != nil {
		t.Fatal(err)
	}
	if result, diff := e.shadowRow(t); result != store.ShadowMissing || diff != "" || e.compared(store.ShadowMissing) != 1 {
		t.Fatalf("shadow comparison %s %s, want missing", result, diff)
	}
}

type failingGet struct{ TaskAPI }

func (failingGet) Get(context.Context, string) (kelos.Task, json.RawMessage, error) {
	return kelos.Task{}, nil, errors.New("kelos unavailable")
}

func TestShadowReadFailureKeepsPending(t *testing.T) {
	e := newEnv(t, true, dispatch.Limits{})
	e.implement(t, implementID, "claude-opus")
	sh := e.shadow(time.Nanosecond)
	sh.Kelos = failingGet{e.api}
	if err := sh.Once(ctx); err == nil {
		t.Fatal("a failed read was not reported")
	}
	if result, _ := e.shadowRow(t); result != store.ShadowPending {
		t.Fatalf("after a failed read: %s, want pending", result)
	}
}

func count(t *testing.T, e *env, sql string) int {
	t.Helper()
	var n int
	storetest.QueryRow(t, e.dbURL, sql, nil, &n)
	return n
}
