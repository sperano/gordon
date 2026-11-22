package upstream

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
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
