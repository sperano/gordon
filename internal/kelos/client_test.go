package kelos_test

import (
	"context"
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
				os.WriteFile(path, []byte(content), 0o600) //nolint:errcheck // checked by the request
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
	if _, _, err := c.Get(context.Background(), "big"); err == nil || !strings.Contains(err.Error(), "response over") {
		t.Fatalf("Get big = %v, want the size cap", err)
	}
	if _, _, err := c.Get(context.Background(), "garbage"); err == nil {
		t.Fatal("Get of a non-JSON answer succeeded")
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
