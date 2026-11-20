package intake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/store"
)

var testKey = []byte("intake-test-key")

const (
	testSlots    = 4
	testDeadline = 5 * time.Second
	commentBody  = `{"event_name":"task.comment.created","data":{"task":{"id":5,"project_id":7},` +
		`"comment":{"id":9,"comment":"/gordon secret-prompt ghp_abcdefghijklmnopqrstuvwxyz"},"doer":{"username":"eric"}}}`
)

// memInbox stores deliveries like the real inbox: once per delivery key.
type memInbox struct {
	mu   sync.Mutex
	rows []store.Delivery
	err  error
}

func (m *memInbox) Accept(_ context.Context, d store.Delivery) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, false, m.err
	}
	for _, r := range m.rows {
		if r.DeliveryKey == d.DeliveryKey || (d.BodyDigest != nil && bytes.Equal(r.BodyDigest, d.BodyDigest)) {
			return 0, true, nil
		}
	}
	m.rows = append(m.rows, d)
	return int64(len(m.rows)), false, nil
}

type harness struct {
	rc    *Receiver
	inbox *memInbox
	logs  *bytes.Buffer
}

func newHarness() harness {
	inbox, logs := &memInbox{}, &bytes.Buffer{}
	keys := auth.Keys{event.SourceVikunja: testKey, event.SourceGitHub: testKey,
		event.SourceWikiComment: testKey, event.SourceImageBuild: testKey}
	rc := &Receiver{Inbox: inbox, Keys: func() auth.Keys { return keys }, Slots: NewSlots(testSlots),
		Metrics: metrics.New(), Log: logging.New(logs, slog.LevelDebug), Deadline: testDeadline}
	return harness{rc, inbox, logs}
}

func (h harness) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(Path, h.rc)
	return mux
}

func signed(t *testing.T, source event.Source, body string, headers map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/"+string(source), strings.NewReader(body))
	name, value, err := auth.Sign(source, testKey, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(name, value)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func (h harness) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.mux().ServeHTTP(rec, req)
	return rec
}

func (h harness) intake(source, result, reason string) float64 {
	return testutil.ToFloat64(h.rc.Metrics.Intake.WithLabelValues(source, result, reason))
}

func TestAcceptsAndStoresVerifiedDelivery(t *testing.T) {
	h := newHarness()
	rec := h.do(signed(t, event.SourceVikunja, commentBody, nil))
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != "accepted" {
		t.Fatalf("answer %d %q, want 202 accepted", rec.Code, rec.Body.String())
	}
	if len(h.inbox.rows) != 1 {
		t.Fatalf("%d rows stored, want 1", len(h.inbox.rows))
	}
	got := h.inbox.rows[0]
	if got.DeliveryKey != "vikunja/comment/9" || got.RequestKey != "vikunja/comment/9" ||
		got.Kind != event.VikunjaTaskCommentCreated || got.Scope != "vikunja/project/7" ||
		string(got.Body) != commentBody || len(got.Headers) != 0 {
		t.Fatalf("stored %+v, want the comment keys, scope, body and no signature header", got)
	}
	if h.intake("vikunja", ResultAccepted, "") != 1 {
		t.Fatal("accepted delivery not counted")
	}
	for _, leak := range []string{"secret-prompt", "ghp_", "/gordon"} {
		if strings.Contains(h.logs.String(), leak) {
			t.Fatalf("log line leaks %q: %s", leak, h.logs.String())
		}
	}
	if !strings.Contains(h.logs.String(), `"delivery_key":"vikunja/comment/9"`) ||
		!strings.Contains(h.logs.String(), `"inbox_id":1`) {
		t.Fatalf("log line lacks the correlation keys: %s", h.logs.String())
	}
}

func TestRedeliveryIsDuplicateWithoutSecondRow(t *testing.T) {
	h := newHarness()
	h.do(signed(t, event.SourceVikunja, commentBody, nil))
	rec := h.do(signed(t, event.SourceVikunja, commentBody, nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "duplicate" || len(h.inbox.rows) != 1 {
		t.Fatalf("redelivery answered %d %q with %d rows, want 200 duplicate and 1 row", rec.Code, rec.Body.String(), len(h.inbox.rows))
	}
}

func TestGitHubKeepsParsingHeaders(t *testing.T) {
	h := newHarness()
	body := `{"action":"opened","repository":{"full_name":"sperano/gordon"},"pull_request":{"number":3,"head":{"sha":"abc"}}}`
	rec := h.do(signed(t, event.SourceGitHub, body, map[string]string{
		event.GitHubEventHeader: "pull_request", event.GitHubDeliveryHeader: "guid-1", "X-Other": "dropped"}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("answer %d %q", rec.Code, rec.Body.String())
	}
	got := h.inbox.rows[0]
	if got.DeliveryKey != "github/delivery/guid-1" || got.Scope != "github/sperano/gordon" ||
		len(got.Headers) != 2 || got.Headers[event.GitHubEventHeader] != "pull_request" ||
		got.Headers[event.GitHubDeliveryHeader] != "guid-1" {
		t.Fatalf("stored %+v", got)
	}
}

func TestGitHubBodyResentUnderNewDeliveryIsDuplicate(t *testing.T) {
	h := newHarness()
	body := `{"action":"opened","repository":{"full_name":"sperano/gordon"},"pull_request":{"number":3,"head":{"sha":"abc"}}}`
	send := func(guid string) *httptest.ResponseRecorder {
		return h.do(signed(t, event.SourceGitHub, body, map[string]string{
			event.GitHubEventHeader: "pull_request", event.GitHubDeliveryHeader: guid}))
	}
	if rec := send("guid-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery %d", rec.Code)
	}
	rec := send("guid-2")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "duplicate" || len(h.inbox.rows) != 1 {
		t.Fatalf("replay under a new id answered %d %q with %d rows, want 200 duplicate and 1 row",
			rec.Code, rec.Body.String(), len(h.inbox.rows))
	}
	sum := sha256.Sum256([]byte(body))
	if !bytes.Equal(h.inbox.rows[0].BodyDigest, sum[:]) {
		t.Fatalf("body digest %x, want the body's SHA-256", h.inbox.rows[0].BodyDigest)
	}
}

func TestVikunjaDeliveryHasNoBodyDigest(t *testing.T) {
	h := newHarness()
	h.do(signed(t, event.SourceVikunja, commentBody, nil))
	if d := h.inbox.rows[0].BodyDigest; d != nil {
		t.Fatalf("vikunja body digest %x, want none (its key comes from the signed body)", d)
	}
}

func TestDispatchSourcesKeyedByBodyDigest(t *testing.T) {
	h := newHarness()
	h.do(signed(t, event.SourceWikiComment, `{"page":1}`, nil))
	h.do(signed(t, event.SourceWikiComment, `{"page":2}`, nil))
	rec := h.do(signed(t, event.SourceWikiComment, `{"page":1}`, nil))
	if rec.Code != http.StatusOK || len(h.inbox.rows) != 2 {
		t.Fatalf("third delivery %d with %d rows, want the repeat a duplicate of 2 rows", rec.Code, len(h.inbox.rows))
	}
	if k := h.inbox.rows[0].DeliveryKey; !strings.HasPrefix(k, "wiki-comment/body/") || len(k) != len("wiki-comment/body/")+2*bodyDigestBytes {
		t.Fatalf("delivery key %q", k)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name   string
		req    func(t *testing.T) *http.Request
		status int
		result string
		reason string
	}{
		{"unknown source", func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/webhook/gitlab", strings.NewReader("{}"))
		}, http.StatusNotFound, ResultRejected, ReasonUnknown},
		{"wrong method", func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet, "/webhook/vikunja", nil)
		}, http.StatusMethodNotAllowed, ResultRejected, ReasonMethod},
		{"no length", func(t *testing.T) *http.Request {
			r := signed(t, event.SourceVikunja, commentBody, nil)
			r.ContentLength = -1
			return r
		}, http.StatusLengthRequired, ResultRejected, ReasonLength},
		{"over cap", func(t *testing.T) *http.Request {
			r := signed(t, event.SourceVikunja, commentBody, nil)
			r.ContentLength = auth.MaxVikunjaBodyBytes + 1
			r.Body = io.NopCloser(failingReader{t})
			return r
		}, http.StatusRequestEntityTooLarge, ResultRejected, ReasonTooLarge},
		{"cut short", func(t *testing.T) *http.Request {
			r := signed(t, event.SourceVikunja, commentBody, nil)
			r.ContentLength = int64(len(commentBody) + 10)
			return r
		}, http.StatusBadRequest, ResultRejected, ReasonIncomplete},
		{"unsigned", func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/webhook/vikunja", strings.NewReader(commentBody))
		}, http.StatusUnauthorized, ResultRejected, "signature-missing"},
		{"bad signature", func(t *testing.T) *http.Request {
			r := signed(t, event.SourceVikunja, commentBody, nil)
			r.Header.Set(auth.VikunjaHeader, strings.Repeat("0", 64))
			return r
		}, http.StatusForbidden, ResultRejected, "signature-invalid"},
		{"signed for another body", func(t *testing.T) *http.Request {
			r := signed(t, event.SourceVikunja, `{}`, nil)
			r.Body, r.ContentLength = io.NopCloser(strings.NewReader(commentBody)), int64(len(commentBody))
			return r
		}, http.StatusForbidden, ResultRejected, "signature-invalid"},
		{"malformed", func(t *testing.T) *http.Request {
			return signed(t, event.SourceVikunja, `{"event_name":"task.updated","data":{"task":{}}}`, nil)
		}, http.StatusBadRequest, ResultRejected, ReasonMalformed},
		{"github without delivery id", func(t *testing.T) *http.Request {
			return signed(t, event.SourceGitHub, `{}`, map[string]string{event.GitHubEventHeader: "push"})
		}, http.StatusBadRequest, ResultRejected, ReasonMalformed},
		{"unsupported event", func(t *testing.T) *http.Request {
			return signed(t, event.SourceVikunja, `{"event_name":"project.created","data":{}}`, nil)
		}, http.StatusAccepted, ResultIgnored, ReasonUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			rec := h.do(tc.req(t))
			if rec.Code != tc.status {
				t.Fatalf("status %d (%q), want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if len(h.inbox.rows) != 0 {
				t.Fatalf("%d rows stored for a refused delivery", len(h.inbox.rows))
			}
			src := strings.TrimPrefix(tc.req(t).URL.Path, "/webhook/")
			if h.intake(metrics.Bound(src, metrics.Sources), tc.result, tc.reason) != 1 {
				t.Fatalf("not counted as %s/%s", tc.result, tc.reason)
			}
		})
	}
}

// failingReader fails the test when the receiver reads a body it should
// have refused from its declared length alone.
type failingReader struct{ t *testing.T }

func (f failingReader) Read([]byte) (int, error) {
	f.t.Error("body read despite its declared length over the cap")
	return 0, io.EOF
}

func TestStoreFailureIsNotAcknowledged(t *testing.T) {
	h := newHarness()
	h.inbox.err = errors.New("connection refused")
	rec := h.do(signed(t, event.SourceVikunja, commentBody, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("answer %d with the store down, want 503 so the sender retries", rec.Code)
	}
	if h.intake("vikunja", ResultUnavailable, ReasonStore) != 1 {
		t.Fatal("store failure not counted")
	}
	h.inbox.err = nil
	if rec := h.do(signed(t, event.SourceVikunja, commentBody, nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("retry after the outage answered %d, want 202", rec.Code)
	}
}

func TestBusyWhenPeerShareIsHeld(t *testing.T) {
	h := newHarness()
	h.rc.Deadline = 50 * time.Millisecond
	req := signed(t, event.SourceVikunja, commentBody, nil)
	p := h.rc.Proxies.Peer(req)
	held := max(1, testSlots/PeerShares)
	for range held {
		if !h.rc.Slots.Acquire(context.Background(), p) {
			t.Fatal("could not take the peer's share")
		}
	}
	if rec := h.do(req); rec.Code != http.StatusServiceUnavailable || len(h.inbox.rows) != 0 {
		t.Fatalf("answer %d with the peer's share held, want 503 and nothing stored", rec.Code)
	}
	if h.intake("vikunja", ResultUnavailable, ReasonBusy) != 1 {
		t.Fatal("busy refusal not counted")
	}
}

func TestPeerShareIsPerClientBehindTrustedProxy(t *testing.T) {
	h := newHarness()
	h.rc.Deadline = 50 * time.Millisecond
	proxies, err := ParseProxies("10.42.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	h.rc.Proxies = proxies
	via := func(client string) *http.Request {
		req := signed(t, event.SourceVikunja, commentBody, nil)
		req.RemoteAddr = "10.42.0.9:40000" // the ingress proxy
		req.Header.Set(ForwardedForHeader, client)
		return req
	}
	for range max(1, testSlots/PeerShares) {
		if !h.rc.Slots.Acquire(context.Background(), "203.0.113.1") {
			t.Fatal("could not take the slow client's share")
		}
	}
	if rec := h.do(via("203.0.113.1")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("slow client's next request answered %d, want 503 busy", rec.Code)
	}
	if rec := h.do(via("198.51.100.2")); rec.Code != http.StatusAccepted || len(h.inbox.rows) != 1 {
		t.Fatalf("another client through the same proxy answered %d, want 202 accepted", rec.Code)
	}
}
