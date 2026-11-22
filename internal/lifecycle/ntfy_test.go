package lifecycle

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/worker"
)

const ntfyHead = "0123456789abcdef0123456789abcdef01234567"

// legacyReadiness are the legacy scripts' texts and reaction values
// (internal/notice/testdata).
func legacyReadiness(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../notice/testdata/legacy_readiness_texts.json")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeNtfy records the requests a publisher sends; status is its answer.
type fakeNtfy struct {
	*httptest.Server
	mu       sync.Mutex
	status   int
	requests []*http.Request
	bodies   []string
}

func newNtfy(t *testing.T, status int) *fakeNtfy {
	n := &fakeNtfy{status: status}
	n.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		n.requests, n.bodies = append(n.requests, r), append(n.bodies, string(b))
		if n.status == http.StatusFound {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(n.status)
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *fakeNtfy) publisher() *Ntfy {
	return &Ntfy{URL: n.URL + "/", Topic: "pr ready/x", Token: "secret-token", HTTP: n.Client()}
}

func notifyAction() action.Action {
	a := action.New(action.Notify, action.VikunjaTask(5), ntfyHead)
	a.Detail = notice.Head{Repo: "sperano/gordon", Number: 12, SHA: ntfyHead}.Detail()
	return a
}

func reactionValues(f *fakeVikunja, task int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for v := range f.taskReactions[task] {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

func TestNotifySendsOnceWithItsBookkeeping(t *testing.T) {
	legacy := legacyReadiness(t)
	f, n := newFake(t), newNtfy(t, http.StatusOK)
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, nil)
	ex.Ntfy = n.publisher()
	for range 2 {
		if err := ex.Execute(ctx, notifyAction()); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.requests) != 1 {
		t.Fatalf("%d sends, want 1", len(n.requests))
	}
	r := n.requests[0]
	if r.Method != http.MethodPost || r.URL.EscapedPath() != "/pr%20ready%2Fx" || r.Header.Get("Authorization") != "Bearer secret-token" ||
		r.Header.Get("Title") != "PR ready: sperano/gordon#12" || r.Header.Get("Click") != "https://github.com/sperano/gordon/pull/12" ||
		r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("request %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
	}
	if want := "PR ready: sperano/gordon#12\nHead: " + ntfyHead + "\nhttps://github.com/sperano/gordon/pull/12"; n.bodies[0] != want {
		t.Errorf("body %q, want %q", n.bodies[0], want)
	}
	pending := PendingReaction + "01234567-1"
	if got := reactionValues(f, 5); !slices.Equal(got, []string{pending, legacy["ntfy_marker"]}) {
		t.Errorf("reactions %v", got)
	}
	if legacy["ntfy_pending"] != PendingReaction+"01234567-2" || legacy["ntfy_retried"] != RetryReaction+"01234567-1" {
		t.Errorf("bookkeeping values differ from legacy: %q %q", legacy["ntfy_pending"], legacy["ntfy_retried"])
	}
}

func TestNotifyFailureIsRecordedAndRetried(t *testing.T) {
	f, n := newFake(t), newNtfy(t, http.StatusBadGateway)
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, nil)
	ex.Ntfy = n.publisher()
	if err := ex.Execute(ctx, notifyAction()); err == nil || worker.IsPermanent(err) {
		t.Fatalf("failed send: %v, want a retryable error", err)
	}
	n.status = http.StatusOK
	if err := ex.Execute(ctx, notifyAction()); err != nil {
		t.Fatal(err)
	}
	want := []string{PendingReaction + "01234567-1", PendingReaction + "01234567-2", RetryReaction + "01234567-1", "🔔01234567"}
	if got := reactionValues(f, 5); !slices.Equal(got, want) || len(n.requests) != 2 {
		t.Errorf("reactions %v after %d sends, want %v after 2", got, len(n.requests), want)
	}
}

func TestNotifyStopsAtTheUnconfirmedLimit(t *testing.T) {
	f, n := newFake(t), newNtfy(t, http.StatusOK)
	f.put(Task{ID: 5, ProjectID: 3})
	me := []User{{Username: gordon}}
	f.taskReactions[5] = map[string][]User{PendingReaction + "01234567-1": me, PendingReaction + "01234567-2": me,
		RetryReaction + "01234567-1": me, PendingReaction + "fedcba98-1": me}
	ex := executor(f, nil)
	ex.Ntfy = n.publisher()
	if err := ex.Execute(ctx, notifyAction()); err != nil || len(n.requests) != 0 {
		t.Fatalf("one reservation without an outcome: %v, %d sends, want none", err, len(n.requests))
	}
	f.taskReactions[5][RetryReaction+"01234567-2"] = me
	if err := ex.Execute(ctx, notifyAction()); err != nil || len(n.requests) != 1 {
		t.Fatalf("a recorded failure frees a send: %v, %d sends", err, len(n.requests))
	}
	if got := reactionValues(f, 5); !slices.Contains(got, PendingReaction+"01234567-3") {
		t.Errorf("next attempt not numbered after the last: %v", got)
	}
}

func TestNotifyRefusesWhatWouldLeakTheToken(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	redirect := newNtfy(t, http.StatusFound)
	ex := executor(f, nil)
	ex.Ntfy = redirect.publisher()
	if err := ex.Execute(ctx, notifyAction()); err == nil || len(redirect.requests) != 1 {
		t.Errorf("redirect: %v after %d requests, want an error and no second request", err, len(redirect.requests))
	}
	for name, p := range map[string]*Ntfy{
		"cleartext":  {URL: "http://ntfy.example", Topic: "t", Token: "x", HTTP: http.DefaultClient},
		"no token":   {URL: "https://ntfy.example", Topic: "t", HTTP: http.DefaultClient},
		"no ntfy":    nil,
		"no topic":   {URL: "https://ntfy.example", Token: "x", HTTP: http.DefaultClient},
		"bad scheme": {URL: "ftp://ntfy.example", Topic: "t", Token: "x", HTTP: http.DefaultClient},
	} {
		clean := newFake(t)
		clean.put(Task{ID: 5, ProjectID: 3})
		ex := executor(clean, nil)
		ex.Ntfy = p
		if err := ex.Execute(ctx, notifyAction()); !worker.IsPermanent(err) || len(clean.writes) != 0 {
			t.Errorf("%s: %v with writes %v, want permanent and nothing reserved", name, err, clean.writes)
		}
	}
	bad := notifyAction()
	bad.Value = "fedcba9876543210"
	ex = executor(f, nil)
	ex.Ntfy = newNtfy(t, http.StatusOK).publisher()
	if err := ex.Execute(ctx, bad); !worker.IsPermanent(err) {
		t.Errorf("head and detail disagree: %v", err)
	}
}

func TestReadinessNoticesRenderAndDeduplicate(t *testing.T) {
	legacy := legacyReadiness(t)
	h := notice.Head{Repo: "sperano/gordon", Number: 12, SHA: ntfyHead}
	limited := h
	limited.Extra = notice.LimitedExtra([]notice.Limited{{Author: "gordon-cole-ai[bot]", Review: 99}})
	missing := notice.Head{Repo: "sperano/vigie", Number: 12, SHA: ntfyHead, Extra: notice.MissingExtra([]string{"check"})}
	refused := h
	refused.Extra = `PUT https://api.github.com/repos/sperano/gordon/pulls/12/merge: HTTP 405 Method Not Allowed: ` +
		`{"message":"Pull Request is not mergeable","status":"405"}`
	cases := []struct {
		kind string
		h    notice.Head
		want string
	}{
		{notice.Ready, h, legacy["ready"]},
		{notice.RoundLimit, limited, legacy["round_limit_one"]},
		{notice.ChecksMissing, missing, legacy["checks_missing_one"]},
		{notice.MergeRefused, refused, legacy["merge_refused"]},
	}
	for _, c := range cases {
		f := newFake(t)
		f.put(Task{ID: 5, ProjectID: 3})
		ex := executor(f, nil)
		ex.Policy.Readiness.MissingCheckMinutes = 30
		a := action.Notice(action.VikunjaTask(5), c.kind, c.h.Detail())
		for range 2 {
			if err := ex.Execute(ctx, a); err != nil {
				t.Fatalf("%s: %v", c.kind, err)
			}
		}
		if cs := f.comments[5]; len(cs) != 1 || cs[0].Comment != c.want+notice.ActionMarker(a.Key) {
			t.Fatalf("%s comments %+v, want one %q", c.kind, cs, c.want)
		}
		// The legacy script's notice of the head counts; one by anyone
		// else does not.
		for _, author := range []string{gordon, "eric"} {
			other := newFake(t)
			other.put(Task{ID: 5, ProjectID: 3})
			other.comments[5] = []Comment{{ID: 1, Comment: c.want, Author: User{Username: author}}}
			if err := executor(other, nil).Execute(ctx, a); err != nil {
				t.Fatal(err)
			}
			if want := map[string]int{gordon: 1, "eric": 2}[author]; len(other.comments[5]) != want {
				t.Errorf("%s after %s's notice: %d comments, want %d", c.kind, author, len(other.comments[5]), want)
			}
		}
	}
	for _, bad := range []action.Action{action.Notice(action.VikunjaTask(5), notice.Ready, "sperano/gordon#12"),
		action.Notice(action.VikunjaTask(5), notice.RoundLimit, h.Detail())} {
		f := newFake(t)
		f.put(Task{ID: 5, ProjectID: 3})
		if err := executor(f, nil).Execute(ctx, bad); !worker.IsPermanent(err) {
			t.Errorf("%s %q: %v, want permanent", bad.Value, bad.Detail, err)
		}
	}
}

func TestNotifyNeverResendsARecordedSend(t *testing.T) {
	delivered := "🔔01234567"
	for _, c := range []struct {
		name      string
		refusals  int
		permanent bool
	}{{"recorded on the last try", DeliveredWriteAttempts - 1, false}, {"never recorded", DeliveredWriteAttempts, true}} {
		f, n := newFake(t), newNtfy(t, http.StatusOK)
		f.put(Task{ID: 5, ProjectID: 3})
		f.refuseReactions[delivered] = c.refusals
		ex := executor(f, nil)
		ex.Ntfy = n.publisher()
		err := ex.Execute(ctx, notifyAction())
		if (err != nil) != c.permanent || worker.IsPermanent(err) != c.permanent || len(n.requests) != 1 {
			t.Errorf("%s: %v after %d sends, want permanent %v after 1", c.name, err, len(n.requests), c.permanent)
		}
		if got := slices.Contains(reactionValues(f, 5), delivered); got == c.permanent {
			t.Errorf("%s: delivered reaction on %v", c.name, got)
		}
		// A later decision of the head queues a new notify action.
		if err := ex.Execute(ctx, notifyAction()); err != nil || len(n.requests) != 1 {
			t.Errorf("%s: later decision: %v after %d sends, want none more", c.name, err, len(n.requests))
		}
	}
}

func TestNotifyFailureNotRecordedStopsTheHead(t *testing.T) {
	retry := RetryReaction + "01234567-1"
	for _, c := range []struct {
		name      string
		refusals  int
		permanent bool
		sends     int
	}{{"recorded on the last try", DeliveredWriteAttempts - 1, false, 2}, {"never recorded", DeliveredWriteAttempts, true, 1}} {
		f, n := newFake(t), newNtfy(t, http.StatusBadGateway)
		f.put(Task{ID: 5, ProjectID: 3})
		f.refuseReactions[retry] = c.refusals
		ex := executor(f, nil)
		ex.Ntfy = n.publisher()
		err := ex.Execute(ctx, notifyAction())
		if err == nil || worker.IsPermanent(err) != c.permanent {
			t.Errorf("%s: %v, want an error, permanent %v", c.name, err, c.permanent)
		}
		if got := slices.Contains(reactionValues(f, 5), retry); got == c.permanent {
			t.Errorf("%s: retry reaction on %v", c.name, got)
		}
		n.status = http.StatusOK
		if err := ex.Execute(ctx, notifyAction()); err != nil || len(n.requests) != c.sends {
			t.Errorf("%s: next attempt: %v after %d sends, want %d", c.name, err, len(n.requests), c.sends)
		}
	}
}

func TestNtfyTransportErrorHidesTheTopic(t *testing.T) {
	n := newNtfy(t, http.StatusOK)
	p := n.publisher()
	n.Close()
	err := p.Publish(ctx, notice.Head{Repo: "sperano/gordon", Number: 12, SHA: ntfyHead})
	if !errors.Is(err, errNtfySend) || err.Error() != "ntfy: request failed" {
		t.Errorf("transport error %v, want %v alone", err, errNtfySend)
	}
}
