package kelostest_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
)

const ns = "kelos-agents"

func named(name string) kelos.Task {
	return kelos.Task{Metadata: kelos.Metadata{Name: name}, Spec: kelos.Spec{Prompt: "p"}}
}

// TestAdmissionRunsBeforeTheNameCheck: as in the API server, a refused
// create of a taken name answers 422, not AlreadyExists.
func TestAdmissionRunsBeforeTheNameCheck(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.Put(named("a"))
	srv.Admit = func(kelos.Task) error { return errors.New("refused by policy") }
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	_, err := c.Create(context.Background(), named("a"))
	var e *kelos.Error
	if !errors.As(err, &e) || e.Status != http.StatusUnprocessableEntity || !strings.Contains(e.Message, "refused by policy") {
		t.Fatalf("refused create of a taken name = %v, want 422 from admission", err)
	}
}

// admitGuard ends a create whose admission deadlocks on the server.
const admitGuard = 5 * time.Second

// TestAdmitMayReadTheServer: Admit runs outside the server's lock.
func TestAdmitMayReadTheServer(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	ran, existed := false, false
	srv.Admit = func(kelos.Task) error {
		ran = true
		_, existed = srv.Task("a")
		return nil
	}
	hc := srv.Client()
	hc.Timeout = admitGuard
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: hc}
	if _, err := c.Create(context.Background(), named("a")); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Task("a"); !ok || !ran || existed {
		t.Fatalf("created %v, admission ran %v and saw the Task already %v", ok, ran, existed)
	}
}
