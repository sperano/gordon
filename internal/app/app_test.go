package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/worker"
)

const (
	rolePassword = "role-password"
	waitFor      = 30 * time.Second
	pollEvery    = 20 * time.Millisecond
	testGrace    = 5 * time.Second
	// A task.updated of an eligible Hollingsworth (project 3) task.
	taskBody = `{"event_name":"task.updated","data":{"task":{"id":4101,"project_id":3,"done":false,` +
		`"labels":[{"title":"feature"}],"assignees":[{"username":"claude-opus"}]},"doer":{"username":"eric"}}}`
)

// methods records the upstream requests a fake provider receives.
type methods struct {
	mu   sync.Mutex
	seen []string
}

func (m *methods) add(r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, r.Method+" "+r.URL.Path)
}

func (m *methods) list() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// roles migrates a new database as its owner, granting two new login
// roles, and returns their connection URLs.
func roles(t *testing.T) (receiverURL, workerURL, ownerURL string) {
	t.Helper()
	ownerURL = storetest.Database(t)
	suffix := storetest.Random(t)
	recv, work := "gordon_receiver_"+suffix, "gordon_worker_"+suffix
	for _, r := range []string{recv, work} {
		storetest.Exec(t, ownerURL, "CREATE ROLE "+r+" LOGIN PASSWORD '"+rolePassword+"'")
		t.Cleanup(func() {
			storetest.Exec(t, ownerURL, "DROP OWNED BY "+r)
			storetest.Exec(t, storetest.AdminURL(t), "DROP ROLE IF EXISTS "+r)
		})
	}
	var logs bytes.Buffer
	if err := Migrate(context.Background(), config.Migrate{Common: config.Common{DatabaseURL: ownerURL},
		ReceiverRole: recv, WorkerRole: work}, logging.New(&logs, slog.LevelInfo)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if want := fmt.Sprintf(`"applied":%d`, store.LatestVersion()); !strings.Contains(logs.String(), want) {
		t.Fatalf("migrate log %s", logs.String())
	}
	return storetest.WithUser(t, ownerURL, recv, rolePassword), storetest.WithUser(t, ownerURL, work, rolePassword), ownerURL
}

// addrs collects the servers' bound addresses.
type addrs struct {
	mu sync.Mutex
	m  map[string]string
	ch chan struct{}
}

func newAddrs(want int) *addrs { return &addrs{m: map[string]string{}, ch: make(chan struct{}, want)} }

func (a *addrs) on(name string, addr net.Addr) {
	a.mu.Lock()
	a.m[name] = addr.String()
	a.mu.Unlock()
	a.ch <- struct{}{}
}

func (a *addrs) wait(t *testing.T, n int) map[string]string {
	t.Helper()
	for range n {
		select {
		case <-a.ch:
		case <-time.After(waitFor):
			t.Fatal("servers did not start")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.m
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(pollEvery)
	}
}

// service is a receiver and a shadow worker on their own database
// roles, with fake GitHub and Vikunja behind the worker.
type service struct {
	ownerURL string
	key      []byte
	upstream *methods
	receiver map[string]string
	worker   map[string]string
	logs     *syncBuffer
	cancel   context.CancelFunc
	done     chan error
}

// startService starts both modes; each edit adjusts the worker's
// configuration first.
func startService(t *testing.T, edits ...func(*config.Worker)) *service {
	t.Helper()
	receiverURL, workerURL, ownerURL := roles(t)
	dir := t.TempDir()
	svc := &service{ownerURL: ownerURL, key: []byte("e2e-vikunja-key"), upstream: &methods{}, logs: &syncBuffer{},
		done: make(chan error, 2)}
	vikunja := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.upstream.add(r)
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if r.URL.Query().Get("page") != "1" {
				fmt.Fprint(w, `[]`) // past the last page
				return
			}
			fmt.Fprint(w, `[{"id":1,"comment":"<p>earlier</p>"}]`)
			return
		}
		fmt.Fprint(w, `{"id":4101,"project_id":3,"title":"Ship it","description":"<p>now</p>","labels":[{"title":"feature"}],
			"assignees":[{"username":"claude-opus"}],"buckets":[{"title":"Doing"}]}`)
	}))
	t.Cleanup(vikunja.Close)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.upstream.add(r)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(github.Close)
	rcfg := config.Receiver{Common: config.Common{DatabaseURL: receiverURL, AdminAddr: "127.0.0.1:0"},
		ListenAddr: "127.0.0.1:0", KeyFiles: map[event.Source]string{event.SourceVikunja: writeFile(t, dir, "vk", string(svc.key))},
		MaxInFlight: config.DefaultMaxInFlight, RequestDeadline: config.DefaultRequestDeadline, ShutdownGrace: testGrace}
	wcfg := config.Worker{Common: config.Common{DatabaseURL: workerURL, AdminAddr: "127.0.0.1:0"}, Shadow: true, ID: "e2e",
		VikunjaURL: vikunja.URL, VikunjaTokenFile: writeFile(t, dir, "vikunja-token", "vt"),
		GitHubAPIURL: github.URL, GitHubTokenFile: writeFile(t, dir, "github-token", "gt"),
		UpstreamLimit: map[string]int{"vikunja": 1, "github": 1}, Concurrency: 2, LeaseTTL: time.Minute,
		PollInterval: pollEvery, MaxAttempts: 3, ProjectLimit: 1, DeferDelay: time.Second,
		ReconcileInterval: pollEvery, BodyRetention: time.Hour, UpstreamTimeout: 5 * time.Second, ShutdownGrace: testGrace,
		Routing: profile.Defaults(), Backoff: worker.Backoff{Base: time.Second, Max: time.Minute}}
	for _, edit := range edits {
		edit(&wcfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc.cancel = cancel
	t.Cleanup(cancel)
	log := logging.New(svc.logs, slog.LevelDebug)
	ra, wa := newAddrs(2), newAddrs(1)
	go func() { svc.done <- Receiver(ctx, rcfg, log.With("mode", "receiver"), ra.on) }()
	go func() { svc.done <- Worker(ctx, wcfg, log.With("mode", "worker"), wa.on) }()
	svc.receiver, svc.worker = ra.wait(t, 2), wa.wait(t, 1)
	return svc
}

func (svc *service) post(t *testing.T, body string) int {
	t.Helper()
	sig, value, _ := auth.Sign(event.SourceVikunja, svc.key, []byte(body))
	req, _ := http.NewRequest(http.MethodPost, "http://"+svc.receiver[ServerPublic]+"/webhook/vikunja", strings.NewReader(body))
	req.Header.Set(sig, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (svc *service) count(t *testing.T, sql string) int {
	t.Helper()
	var n int
	storetest.QueryRow(t, svc.ownerURL, sql, nil, &n)
	return n
}

func TestServiceEndToEndInShadow(t *testing.T) {
	svc := startService(t)
	if code := svc.post(t, taskBody); code != http.StatusAccepted {
		t.Fatalf("receiver answered %d, want 202", code)
	}
	eventually(t, "the decision", func() bool { return svc.count(t, `SELECT count(*) FROM decisions`) == 1 })
	if n := svc.count(t, `SELECT count(*) FROM decisions WHERE outcome = 'dispatch' AND NOT executed`); n != 1 {
		t.Fatal("decision is not a recorded-only dispatch")
	}
	if n := svc.count(t, `SELECT (SELECT count(*) FROM actions) + (SELECT count(*) FROM runs)`); n != 0 {
		t.Fatalf("shadow queued %d actions or runs", n)
	}
	if calls := svc.upstream.list(); len(calls) != 1 || calls[0] != "GET /tasks/4101" {
		t.Fatalf("upstream calls %v, want only the one task read", calls)
	}
	for _, addr := range []string{svc.receiver[ServerAdmin], svc.worker[ServerAdmin]} {
		if code, _ := httpGet(t, "http://"+addr+"/readyz"); code != http.StatusOK {
			t.Fatalf("readyz on %s = %d", addr, code)
		}
	}
	_, rm := httpGet(t, "http://"+svc.receiver[ServerAdmin]+"/metrics")
	if !strings.Contains(rm, `gordon_intake_requests_total{reason="",result="accepted",source="vikunja"} 1`) {
		t.Fatal("receiver metrics lack the accepted delivery")
	}
	eventually(t, "worker metrics", func() bool {
		_, wm := httpGet(t, "http://"+svc.worker[ServerAdmin]+"/metrics")
		return strings.Contains(wm, `gordon_decisions_total{executed="false",family="vikunja-implement",outcome="dispatch",source="vikunja"} 1`) &&
			strings.Contains(wm, `gordon_upstream_requests_total{provider="vikunja",result="ok"} 1`) &&
			strings.Contains(wm, `gordon_reconcile_runs_total{result="ok"}`)
	})
	svc.cancel()
	for range 2 {
		if err := <-svc.done; err != nil {
			t.Fatalf("mode exited with %v, want a clean shutdown", err)
		}
	}
	if strings.Contains(svc.logs.String(), "Bearer") || strings.Contains(svc.logs.String(), rolePassword) {
		t.Fatal("a credential reached the log")
	}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// syncBuffer is a bytes.Buffer safe for concurrent loggers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
