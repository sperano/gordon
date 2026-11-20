package kelos_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
)

const ns = "kelos-agents"

type observed struct {
	mu  sync.Mutex
	got []string
}

func (o *observed) observe(op, result string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, op+"="+result)
}

func (o *observed) list() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.got, ",")
}

func client(t *testing.T, srv *kelostest.Server) (*kelos.Client, *observed) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &observed{}
	return &kelos.Client{BaseURL: srv.URL, Namespace: ns, TokenFile: token, HTTP: srv.Client(), Observe: o.observe}, o
}

func task(name string) kelos.Task {
	return kelos.Task{Metadata: kelos.Metadata{Name: name, Labels: map[string]string{"v": "a"}},
		Spec: kelos.Spec{Prompt: "p", Branch: "kelos/vikunja-1", Worker: &kelos.Worker{Type: "claude-code",
			WorkspaceRef: &kelos.Ref{Name: "gordon"}}}}
}

func TestCreateGetList(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.PageSize = 1
	c, o := client(t, srv)
	ctx := context.Background()
	created, err := c.Create(ctx, task("a"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Metadata.UID == "" || created.APIVersion != kelos.APIVersion || created.Metadata.Namespace != ns {
		t.Fatalf("created %+v", created.Metadata)
	}
	if srv.Authorization() != "Bearer sa-token" {
		t.Fatalf("Authorization %q, want the trimmed token file", srv.Authorization())
	}
	sent := srv.Creates()[0]
	if sent.Kind != kelos.KindTask || sent.APIVersion != kelos.APIVersion || sent.Metadata.Namespace != ns {
		t.Fatalf("create body %+v", sent)
	}
	b := task("b")
	b.Metadata.Labels = map[string]string{"v": "b"}
	if _, err := c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, raw, err := c.Get(ctx, "a")
	if err != nil || got.Metadata.UID != created.Metadata.UID || !strings.Contains(string(raw), `"name":"a"`) {
		t.Fatalf("Get = %+v, %s, %v", got.Metadata, raw, err)
	}
	all, err := c.List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d tasks, %v; want both, across two pages", len(all), err)
	}
	only, err := c.List(ctx, "v=b")
	if err != nil || len(only) != 1 || only[0].Metadata.Name != "b" {
		t.Fatalf("List(v=b) = %+v, %v", only, err)
	}
	if want := "create=ok,create=ok,get=ok,list=ok,list=ok,list=ok"; o.list() != want {
		t.Fatalf("observed %s, want %s", o.list(), want)
	}
}

func TestErrorKinds(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.Admit = func(t kelos.Task) error {
		if t.Metadata.Name == "denied" {
			return errors.New("profile default does not grant Secrets x")
		}
		return nil
	}
	c, o := client(t, srv)
	ctx := context.Background()
	if _, err := c.Create(ctx, task("a")); err != nil {
		t.Fatal(err)
	}
	_, err := c.Create(ctx, task("a"))
	if !kelos.IsAlreadyExists(err) || kelos.IsRefused(err) || kelos.IsAmbiguous(err) {
		t.Fatalf("second create = %v, want AlreadyExists only", err)
	}
	_, err = c.Create(ctx, task("denied"))
	if !kelos.IsRefused(err) || kelos.IsAmbiguous(err) || !strings.Contains(err.Error(), "does not grant Secrets x") {
		t.Fatalf("denied create = %v, want a refusal quoting admission", err)
	}
	srv.FailCreates(http.StatusServiceUnavailable)
	if _, err = c.Create(ctx, task("b")); !kelos.IsAmbiguous(err) || kelos.IsRefused(err) {
		t.Fatalf("503 create = %v, want ambiguous", err)
	}
	srv.FailCreates(http.StatusTooManyRequests)
	if _, err = c.Create(ctx, task("b")); kelos.IsAmbiguous(err) || kelos.IsRefused(err) || err == nil {
		t.Fatalf("429 create = %v, want a plain retry", err)
	}
	srv.FailCreates(kelostest.DropResponse)
	if _, err = c.Create(ctx, task("lost")); !kelos.IsAmbiguous(err) {
		t.Fatalf("dropped response = %v, want ambiguous", err)
	}
	if _, ok := srv.Task("lost"); !ok {
		t.Fatal("the dropped create was not applied")
	}
	if _, _, err = c.Get(ctx, "nope"); !kelos.IsNotFound(err) {
		t.Fatalf("Get missing = %v, want NotFound", err)
	}
	want := "create=ok,create=refused,create=refused,create=error,create=error,create=error,get=not_found"
	if o.list() != want {
		t.Fatalf("observed %s, want %s", o.list(), want)
	}
}

func TestDialFailureIsNotSent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c := &kelos.Client{BaseURL: "http://" + addr, Namespace: ns, HTTP: &http.Client{Timeout: time.Second}}
	_, err = c.Create(context.Background(), task("a"))
	var e *kelos.Error
	if !errors.As(err, &e) || e.Sent || kelos.IsAmbiguous(err) {
		t.Fatalf("Create to a closed port = %v, want a not-sent error", err)
	}
}

func TestTokenFileRequired(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	for name, content := range map[string]string{"missing": "", "empty": "\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if content != "" {
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, TokenFile: path, HTTP: srv.Client()}
			_, err := c.Create(context.Background(), task("a"))
			if err == nil || !strings.Contains(err.Error(), "token") || len(srv.Creates()) != 0 {
				t.Fatalf("Create = %v with %d requests sent, want a token error before sending", err, len(srv.Creates()))
			}
		})
	}
}

func TestOversizedAndUndecodableAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/big") {
			fmt.Fprintf(w, `{"metadata":{"name":"%s"}}`, strings.Repeat("x", 5<<20))
			return
		}
		fmt.Fprint(w, `not json`)
	}))
	defer srv.Close()
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	if _, _, err := c.Get(context.Background(), "big"); err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("Get big = %v, want the size cap", err)
	}
	_, _, err := c.Get(context.Background(), "garbage")
	var e *kelos.Error
	if !errors.As(err, &e) || e.Status != http.StatusOK || !e.Sent || e.Err == nil {
		t.Fatalf("Get of a non-JSON answer = %v, want a decode failure of the 200 answer", err)
	}
}

func TestNewHTTPClient(t *testing.T) {
	if _, err := kelos.NewHTTPClient(filepath.Join(t.TempDir(), "none"), time.Second); err == nil {
		t.Fatal("missing CA file accepted")
	}
	bad := filepath.Join(t.TempDir(), "ca")
	os.WriteFile(bad, []byte("not a pem"), 0o600) //nolint:errcheck // read next
	if _, err := kelos.NewHTTPClient(bad, time.Second); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("NewHTTPClient(bad) = %v", err)
	}
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	writePEM(t, ca, srv)
	hc, err := kelos.NewHTTPClient(ca, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("TLS with the server's CA: %v", err)
	}
	resp.Body.Close()
}

func writePEM(t *testing.T, path string, srv *httptest.Server) {
	t.Helper()
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
}

// apiMessageCap is the client's cap on a kept API server message.
const apiMessageCap = 512

func TestErrorMessageCutOnARuneBoundary(t *testing.T) {
	cases := map[string]struct{ message, want string }{
		"short":           {"refused", "refused"},
		"ascii":           {strings.Repeat("x", apiMessageCap+10), strings.Repeat("x", apiMessageCap) + "…"},
		"rune across cap": {strings.Repeat("x", apiMessageCap-1) + "é tail", strings.Repeat("x", apiMessageCap-1) + "…"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]string{"kind": "Status", "reason": "Invalid", "message": tc.message})
			}))
			defer srv.Close()
			c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
			_, _, err := c.Get(context.Background(), "a")
			var e *kelos.Error
			if !errors.As(err, &e) || e.Message != tc.want || !utf8.ValidString(e.Message) {
				t.Fatalf("Get = %v, want message %q", err, tc.want)
			}
		})
	}
}

func TestIsRefusedStatuses(t *testing.T) {
	cases := map[int]bool{
		http.StatusBadRequest: true, http.StatusUnauthorized: true, http.StatusForbidden: true,
		http.StatusNotFound: true, http.StatusMethodNotAllowed: true, http.StatusUnprocessableEntity: true,
		http.StatusRequestEntityTooLarge: true,
		http.StatusConflict:              false, http.StatusTooManyRequests: false, http.StatusInternalServerError: false,
	}
	for status, want := range cases {
		if got := kelos.IsRefused(&kelos.Error{Status: status, Sent: true}); got != want {
			t.Errorf("IsRefused(%d) = %v, want %v", status, got, want)
		}
	}
	if kelos.IsRefused(errors.New("plain")) {
		t.Error("IsRefused of a non-Kelos error")
	}
}

func TestCancelledCreateIsNotSent(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	c, _ := client(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Create(ctx, task("a"))
	var e *kelos.Error
	if !errors.As(err, &e) || e.Sent || kelos.IsAmbiguous(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Create with an ended context = %v, want unsent and not ambiguous", err)
	}
	if n := len(srv.Creates()); n != 0 {
		t.Fatalf("%d creates reached the server", n)
	}
}

func TestUnreadableCreateAnswerIsAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `not json`)
	}))
	defer srv.Close()
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	if _, err := c.Create(context.Background(), task("a")); !kelos.IsAmbiguous(err) || kelos.IsRefused(err) {
		t.Fatalf("Create answered 201 with garbage = %v, want ambiguous", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewHTTPClientCustomDefaultTransport(t *testing.T) {
	saved := http.DefaultTransport
	defer func() { http.DefaultTransport = saved }()
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") })
	if _, err := kelos.NewHTTPClient("", time.Second); err == nil || !strings.Contains(err.Error(), "not an *http.Transport") {
		t.Fatalf("NewHTTPClient = %v, want a configuration error", err)
	}
}

// listCapBytes is the client's list page cap (maxListBytes).
const listCapBytes = 32 << 20

// oversizedLists answers a list asking more than fits with a page over
// the cap, and records every limit asked.
func oversizedLists(t *testing.T, fits int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var limits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := r.URL.Query().Get("limit")
		mu.Lock()
		limits = append(limits, limit)
		mu.Unlock()
		var n int
		fmt.Sscan(limit, &n) //nolint:errcheck // an unparsed limit reads as 0, which fits any cap
		if n > fits {
			fmt.Fprintf(w, `{"items":[{"spec":{"prompt":"%s"}}]}`, strings.Repeat("x", listCapBytes))
			return
		}
		fmt.Fprintf(w, `{"items":[{"metadata":{"name":"page-of-%d"}}]}`, n)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), limits...)
	}
}

func TestListShrinksAPageOverTheCap(t *testing.T) {
	srv, limits := oversizedLists(t, 40)
	o := &observed{}
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client(), Observe: o.observe}
	got, err := c.List(context.Background(), "")
	if err != nil || len(got) != 1 || got[0].Metadata.Name != "page-of-31" {
		t.Fatalf("List = %+v, %v; want the page read at limit 31", got, err)
	}
	if l := strings.Join(limits(), ","); l != "250,125,62,31" {
		t.Fatalf("limits asked %s, want halved until the page fits", l)
	}
	if l := o.list(); l != "list=too_large,list=too_large,list=too_large,list=ok" {
		t.Fatalf("observed %s, want the retries as too_large, not error", l)
	}
}

// listPageBudget is the client's page cap (maxListPages).
const listPageBudget = 400

// TestListSizeRetriesSpendNoPages: a list of exactly the page budget
// still completes after a page read again smaller.
func TestListSizeRetriesSpendNoPages(t *testing.T) {
	const fits = 125
	var mu sync.Mutex
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscan(r.URL.Query().Get("limit"), &n) //nolint:errcheck // an unparsed limit reads as 0, which fits
		if n > fits {
			fmt.Fprintf(w, `{"items":[{"spec":{"prompt":"%s"}}]}`, strings.Repeat("x", listCapBytes))
			return
		}
		mu.Lock()
		served++
		next := ""
		if served < listPageBudget {
			next = fmt.Sprint(served)
		}
		mu.Unlock()
		fmt.Fprintf(w, `{"metadata":{"continue":%q},"items":[{"metadata":{"name":"t"}}]}`, next)
	}))
	defer srv.Close()
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	got, err := c.List(context.Background(), "")
	if err != nil || len(got) != listPageBudget {
		t.Fatalf("List = %d Tasks, %v; want all %d pages read", len(got), err, listPageBudget)
	}
}

func TestListFailsWhenTheSmallestPageIsOverTheCap(t *testing.T) {
	srv, limits := oversizedLists(t, 0)
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	if _, err := c.List(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("List = %v, want the size cap", err)
	}
	if l := strings.Join(limits(), ","); l != "250,125,62,31,15,8" {
		t.Fatalf("limits asked %s, want halved down to 8 (a page sure to fit), then given up", l)
	}
}
