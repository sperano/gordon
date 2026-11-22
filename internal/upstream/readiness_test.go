package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGitHubPullRequestCreatedAt(t *testing.T) {
	c, _ := client(t, ProviderGitHub, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":7,"state":"open","created_at":"2026-11-22T10:00:00Z"}`)
	})
	pr, err := GitHub{c}.PullRequest(ctx, "sperano/gordon", 7, false)
	if err != nil || !pr.CreatedAt.Equal(time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v, %v", pr.CreatedAt, err)
	}
}

func TestVikunjaTaskLabelCreators(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":42,"labels":[{"title":"auto-merge","created_by":{"username":"eric"}},
			{"title":"auto-merge","created_by":{"username":"mallory"}},{"title":"in progress","created_by":null},
			{"title":"pr open"}]}`)
	})
	got, err := Vikunja{c}.Task(ctx, 42)
	want := map[string][]string{"auto-merge": {"eric", "mallory"}}
	if err != nil || !reflect.DeepEqual(got.LabelCreators, want) ||
		!slices.Equal(got.Labels, []string{"auto-merge", "auto-merge", "in progress", "pr open"}) {
		t.Fatalf("Task = %+v, %v", got, err)
	}
	if !got.LabelBy("auto-merge", "eric") || got.LabelBy("in progress", "eric") || got.LabelBy("auto-merge", "") {
		t.Error("LabelBy")
	}
}

func TestVikunjaTaskReactions(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/tasks/42/reactions" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `{"🔔01234567":[{"username":"gordon"}],"⏳01234567-1":[{"username":"gordon"}]}`)
	})
	got, err := Vikunja{c}.TaskReactions(ctx, 42)
	if err != nil || !slices.Equal(got, []string{"⏳01234567-1", "🔔01234567"}) {
		t.Errorf("TaskReactions = %v, %v", got, err)
	}
	none, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `null`) })
	if got, err := (Vikunja{none}).TaskReactions(ctx, 42); err != nil || len(got) != 0 {
		t.Errorf("no reactions: %v, %v", got, err)
	}
	if _, err := (Vikunja{none}).TaskReactions(ctx, 0); ClassOf(err) != Invalid {
		t.Errorf("task 0: %v", err)
	}
}

// A token without reactions.read_all is refused only on the reactions
// route; the error names that route so the missing permission shows in
// the worker's retry log.
func TestRefusedReadNamesItsRoute(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tasks/42/reactions" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"id":42}`)
	})
	if _, err := (Vikunja{c}).Task(ctx, 42); err != nil {
		t.Fatalf("Task = %v, want the task route allowed", err)
	}
	_, err := Vikunja{c}.TaskReactions(ctx, 42)
	var e *Error
	if !errors.As(err, &e) || e.Class != Unauthorized || e.Status != http.StatusUnauthorized ||
		e.Path != "/tasks/42/reactions" {
		t.Fatalf("TaskReactions = %#v, want unauthorized on /tasks/42/reactions", err)
	}
	if want := "vikunja: unauthorized (HTTP 401) on GET /tasks/42/reactions"; err.Error() != want {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}
}

// A read that fails before any response (here its token file) still names
// the route it was for; one refused before a request is built names none.
func TestFailedReadPathBeforeAnyResponse(t *testing.T) {
	c := &Client{Provider: ProviderVikunja, BaseURL: "http://127.0.0.1:1",
		TokenFile: filepath.Join(t.TempDir(), "missing"), HTTP: http.DefaultClient}
	_, err := Vikunja{c}.Task(ctx, 7)
	var e *Error
	if !errors.As(err, &e) || e.Class != Unavailable || e.Status != 0 || e.Path != "/tasks/7" ||
		!strings.HasPrefix(err.Error(), "vikunja: unavailable on GET /tasks/7: token: ") {
		t.Fatalf("missing token file = %v, want unavailable on GET /tasks/7", err)
	}
	_, err = Vikunja{c}.Task(ctx, 0)
	if !errors.As(err, &e) || e.Path != "" || err.Error() != "vikunja: invalid: task id 0" {
		t.Fatalf("Task(0) = %v, want an invalid read naming no route", err)
	}
}
