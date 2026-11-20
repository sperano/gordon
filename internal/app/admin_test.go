package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/store"
)

type fakeSchema struct {
	pingErr    error
	version    int
	versionErr error
}

func (f fakeSchema) Ping(context.Context) error { return f.pingErr }
func (f fakeSchema) SchemaVersion(context.Context) (int, error) {
	return f.version, f.versionErr
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, strings.TrimSpace(string(body))
}

func TestReadiness(t *testing.T) {
	latest := store.LatestVersion()
	cases := []struct {
		name   string
		schema fakeSchema
		drain  bool
		status int
		body   string
	}{
		{"ready", fakeSchema{version: latest}, false, http.StatusOK, "ready"},
		{"schema ahead", fakeSchema{version: latest + 1}, false, http.StatusOK, "ready"},
		{"database down", fakeSchema{pingErr: errors.New("refused")}, false, http.StatusServiceUnavailable, "database unavailable"},
		{"never migrated", fakeSchema{versionErr: errors.New("no table")}, false, http.StatusServiceUnavailable, "schema not migrated"},
		{"schema behind", fakeSchema{version: latest - 1}, false, http.StatusServiceUnavailable, "schema version"},
		{"draining", fakeSchema{version: latest}, true, http.StatusServiceUnavailable, "draining"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Admin{Store: tc.schema, Metrics: metrics.New()}
			if tc.drain {
				a.Drain()
			}
			status, body := get(t, a.Handler(), "/readyz")
			if status != tc.status || !strings.HasPrefix(body, tc.body) {
				t.Fatalf("readyz = %d %q, want %d %q", status, body, tc.status, tc.body)
			}
			if status, body := get(t, a.Handler(), "/healthz"); status != http.StatusOK || body != "ok" {
				t.Fatalf("healthz = %d %q, want 200 ok whatever readiness says", status, body)
			}
		})
	}
}

func TestMetricsEndpoint(t *testing.T) {
	m := metrics.New()
	m.Intake.WithLabelValues("vikunja", "accepted", "").Inc()
	m.SetQueues(nil)
	status, body := get(t, (&Admin{Store: fakeSchema{}, Metrics: m}).Handler(), "/metrics")
	if status != http.StatusOK {
		t.Fatalf("metrics = %d", status)
	}
	for _, want := range []string{
		`gordon_intake_requests_total{reason="",result="accepted",source="vikunja"} 1`,
		`gordon_queue_depth{queue="actions",state="ambiguous"} 0`,
		`gordon_queue_oldest_age_seconds{queue="inbox",state="pending"} 0`,
		"go_goroutines", "process_",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}
