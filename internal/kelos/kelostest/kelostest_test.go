package kelostest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
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

// TestListPagesReadOneSnapshot: a Task deleted between two pages of a
// list still shows on the later page, and the next list no longer has it.
func TestListPagesReadOneSnapshot(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.PageSize = 1
	for _, n := range []string{"a", "b", "c"} {
		srv.Put(named(n))
	}
	first := listPage(t, srv, "")
	srv.Delete("b")
	second := listPage(t, srv, first.Metadata.Continue)
	third := listPage(t, srv, second.Metadata.Continue)
	got := []string{first.name(t), second.name(t), third.name(t)}
	if !slices.Equal(got, []string{"a", "b", "c"}) || third.Metadata.Continue != "" {
		t.Fatalf("pages = %v (last continue %q), want a b c from the first page's list", got, third.Metadata.Continue)
	}
	again := listPage(t, srv, "")
	if next := listPage(t, srv, again.Metadata.Continue); next.name(t) != "c" {
		t.Fatalf("a new list's second page = %s, want c with b deleted", next.name(t))
	}
}

type listResponse struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []kelos.Task `json:"items"`
}

func (l listResponse) name(t *testing.T) string {
	t.Helper()
	if len(l.Items) != 1 {
		t.Fatalf("page of %d items, want 1", len(l.Items))
	}
	return l.Items[0].Metadata.Name
}

func listPage(t *testing.T, srv *kelostest.Server, token string) listResponse {
	t.Helper()
	u := srv.URL + "/apis/" + kelos.APIVersion + "/namespaces/" + ns + "/tasks?continue=" + url.QueryEscape(token)
	resp, err := srv.Client().Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out listResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRawIsACopy: changing the returned JSON leaves the stored Task as it
// was.
func TestRawIsACopy(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.Put(named("a"))
	want := string(srv.Raw("a"))
	got := srv.Raw("a")
	for i := range got {
		got[i] = ' '
	}
	if string(srv.Raw("a")) != want {
		t.Fatalf("Raw after a caller's change = %s, want %s", srv.Raw("a"), want)
	}
}

// TestListRefusesUnsupportedSelectors: a selector the fake cannot match
// exactly answers 400, never a wrong, empty list.
func TestListRefusesUnsupportedSelectors(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.Put(named("a"))
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	for _, sel := range []string{"k!=v", "k==v", "k in (a,b)", "k", "a=b,", "=v"} {
		_, err := c.List(context.Background(), sel)
		var e *kelos.Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || !strings.Contains(e.Message, "unsupported label selector") {
			t.Errorf("List(%q) = %v, want 400 unsupported label selector", sel, err)
		}
	}
	got, err := c.List(context.Background(), "")
	if err != nil || len(got) != 1 {
		t.Fatalf("List(\"\") = %d Tasks, %v; want the one Task", len(got), err)
	}
}

// TestInjectedFailuresCarryTheAPIServerReason: an injected 409 is
// AlreadyExists and a 422 Invalid, so reason-based checks see them as
// they see the API server's.
func TestInjectedFailuresCarryTheAPIServerReason(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	for code, want := range map[int]string{
		http.StatusConflict:            kelos.ReasonAlreadyExists,
		http.StatusUnprocessableEntity: "Invalid",
		http.StatusInternalServerError: "InternalError",
	} {
		srv.FailCreates(code)
		_, err := c.Create(context.Background(), named("a"))
		var e *kelos.Error
		if !errors.As(err, &e) || e.Status != code || e.Reason != want {
			t.Errorf("injected %d = %v, want reason %s", code, err, want)
		}
	}
	srv.FailCreates(http.StatusConflict)
	if _, err := c.Create(context.Background(), named("a")); !kelos.IsAlreadyExists(err) {
		t.Fatalf("injected 409 = %v, want AlreadyExists", err)
	}
}

// TestDropResponseCutsARefusedCreate: a queued DropResponse drops the
// connection of the next create even when admission or the name check
// refuses it, and is used up by it.
func TestDropResponseCutsARefusedCreate(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.Put(named("taken"))
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	srv.FailCreates(kelostest.DropResponse)
	if _, err := c.Create(context.Background(), named("taken")); err == nil || kelos.IsAlreadyExists(err) || !kelos.IsAmbiguous(err) {
		t.Fatalf("dropped create of a taken name = %v, want a lost response", err)
	}
	srv.Admit = func(kelos.Task) error { return errors.New("refused by policy") }
	srv.FailCreates(kelostest.DropResponse)
	if _, err := c.Create(context.Background(), named("new")); err == nil || kelos.IsRefused(err) || !kelos.IsAmbiguous(err) {
		t.Fatalf("dropped refused create = %v, want a lost response", err)
	}
	if _, ok := srv.Task("new"); ok {
		t.Fatal("a refused create was stored")
	}
	_, err := c.Create(context.Background(), named("new"))
	var e *kelos.Error
	if !errors.As(err, &e) || e.Status != http.StatusUnprocessableEntity {
		t.Fatalf("create after the drop = %v, want the 422 answered", err)
	}
}

// TestEmptySelectorValueNeedsTheLabel: k= matches a Task whose label k
// is empty, not one without the label.
func TestEmptySelectorValueNeedsTheLabel(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	with := named("with")
	with.Metadata.Labels = map[string]string{"k": ""}
	srv.Put(with)
	srv.Put(named("without"))
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	got, err := c.List(context.Background(), "k=")
	if err != nil || len(got) != 1 || got[0].Metadata.Name != "with" {
		t.Fatalf("List(\"k=\") = %+v, %v; want only the Task labelled k", got, err)
	}
}
