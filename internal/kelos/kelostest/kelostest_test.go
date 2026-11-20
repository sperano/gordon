package kelostest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
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

// admitGuard bounds Admit's read of the server: past it, the read is
// taken for a deadlock on the server's lock and the test fails, while
// Admit returns so the handler, and srv.Close, can finish.
const admitGuard = 5 * time.Second

// TestAdmitMayReadTheServer: Admit runs outside the server's lock.
func TestAdmitMayReadTheServer(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	var ran, existed atomic.Bool
	srv.Admit = func(kelos.Task) error {
		ran.Store(true)
		read := make(chan bool, 1)
		go func() { _, ok := srv.Task("a"); read <- ok }()
		select {
		case ok := <-read:
			existed.Store(ok)
			return nil
		case <-time.After(admitGuard):
			t.Error("Admit's read of the server deadlocked")
			return errors.New("deadlock")
		}
	}
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	if _, err := c.Create(context.Background(), named("a")); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Task("a"); !ok || !ran.Load() || existed.Load() {
		t.Fatalf("created %v, admission ran %v and saw the Task already %v", ok, ran.Load(), existed.Load())
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
	// Admit is set before the first request and switched through refuse:
	// writing srv.Admit after a dropped create races its handler's read.
	var refuse atomic.Bool
	srv.Admit = func(kelos.Task) error {
		if refuse.Load() {
			return errors.New("refused by policy")
		}
		return nil
	}
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	srv.FailCreates(kelostest.DropResponse)
	if _, err := c.Create(context.Background(), named("taken")); err == nil || kelos.IsAlreadyExists(err) || !kelos.IsAmbiguous(err) {
		t.Fatalf("dropped create of a taken name = %v, want a lost response", err)
	}
	refuse.Store(true)
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

// TestCreateAssignsTheUID: as the API server, a create ignores a
// client-supplied UID and stores one of its own.
func TestCreateAssignsTheUID(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	sent := named("a")
	sent.Metadata.UID = "client-uid"
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	got, err := c.Create(context.Background(), sent)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := srv.Task("a")
	if got.Metadata.UID != "uid-1" || stored.Metadata.UID != "uid-1" {
		t.Fatalf("created UID %q, stored %q, want uid-1 from the server", got.Metadata.UID, stored.Metadata.UID)
	}
}

// TestListRefusesInvalidContinueTokens: a continue token the server did
// not hand out answers 400 instead of a page (or a panic).
func TestListRefusesInvalidContinueTokens(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	srv.PageSize = 1
	srv.Put(named("a"))
	srv.Put(named("b"))
	listPage(t, srv, "") // snapshot 0, two items
	for _, token := range []string{"junk", "0", "0/-1", "0/3", "0/x", "1/0", "-1/0"} {
		u := srv.URL + "/apis/" + kelos.APIVersion + "/namespaces/" + ns + "/tasks?continue=" + url.QueryEscape(token)
		resp, err := srv.Client().Get(u)
		if err != nil {
			t.Fatal(err)
		}
		var st struct{ Reason, Message string }
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		want := "continue key \"" + token + "\" is not valid"
		if err != nil || resp.StatusCode != http.StatusBadRequest || st.Reason != "BadRequest" || st.Message != want {
			t.Errorf("continue %q = %d %+v (%v), want 400 BadRequest %q", token, resp.StatusCode, st, err, want)
		}
	}
}

// TestCreateRefusesAnotherNamespace: as the API server, a create whose
// body names another namespace than the request path answers 400 and
// stores nothing; a body without a namespace is created.
func TestCreateRefusesAnotherNamespace(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	u := srv.URL + "/apis/" + kelos.APIVersion + "/namespaces/" + ns + "/tasks"
	post := func(body string) (int, string) {
		resp, err := srv.Client().Post(u, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st struct{ Message string }
		_ = json.NewDecoder(resp.Body).Decode(&st) //nolint:errcheck // a 201 has no message
		return resp.StatusCode, st.Message
	}
	code, msg := post(`{"metadata":{"name":"a","namespace":"other"},"spec":{"prompt":"p"}}`)
	want := "the namespace of the provided object does not match the namespace sent on the request"
	if _, stored := srv.Task("a"); code != http.StatusBadRequest || msg != want || stored || len(srv.Creates()) != 0 {
		t.Fatalf("create in another namespace = %d %q (stored %v, creates %d), want 400 %q and nothing kept", code, msg, stored, len(srv.Creates()), want)
	}
	if code, msg := post(`{"metadata":{"name":"b"},"spec":{"prompt":"p"}}`); code != http.StatusCreated {
		t.Fatalf("create without a namespace = %d %q, want 201", code, msg)
	}
	if got, _ := srv.Task("b"); got.Metadata.Namespace != ns {
		t.Fatalf("stored namespace %q, want %q", got.Metadata.Namespace, ns)
	}
}

// TestRepeatedSelectorKeysAllApply: k=a,k=b requires both values, so it
// matches no Task, and k=a,k=a matches the Task labelled k=a.
func TestRepeatedSelectorKeysAllApply(t *testing.T) {
	srv := kelostest.New(ns)
	defer srv.Close()
	for name, v := range map[string]string{"ta": "a", "tb": "b"} {
		task := named(name)
		task.Metadata.Labels = map[string]string{"k": v}
		srv.Put(task)
	}
	c := &kelos.Client{BaseURL: srv.URL, Namespace: ns, HTTP: srv.Client()}
	if got, err := c.List(context.Background(), "k=a,k=b"); err != nil || len(got) != 0 {
		t.Fatalf("List(\"k=a,k=b\") = %+v, %v; want no Task", got, err)
	}
	got, err := c.List(context.Background(), "k=a,k=a")
	if err != nil || len(got) != 1 || got[0].Metadata.Name != "ta" {
		t.Fatalf("List(\"k=a,k=a\") = %+v, %v; want only ta", got, err)
	}
}
