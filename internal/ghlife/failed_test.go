package ghlife

import (
	"context"
	"errors"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
)

const conflictRun = "pr-conflict-0123abcd-4-89abcdef-01234567"

func failedRuns() fakeRuns {
	r := runs()
	r[conflictRun] = store.RunInfo{Name: conflictRun, Family: run.PRConflict, TaskUID: "uid-2"}
	return r
}

// TestFailedNotices: a failed PR run's notice quotes its Task's message,
// links its run only on a private repository, tells a conflict's retry
// from a follow-up's, and is posted once.
func TestFailedNotices(t *testing.T) {
	message := func(_ context.Context, name string) (string, error) { return "backoff limit of " + name, nil }
	cases := []struct {
		name    string
		run     string
		private bool
		want    notice.PRFailedInfo
	}{
		{"public follow-up", runName, false, notice.PRFailedInfo{Task: runName, Message: "backoff limit of " + runName,
			Trigger: "/gordon", Namespace: "kelos-agents"}},
		{"private conflict", conflictRun, true, notice.PRFailedInfo{Task: conflictRun, Message: "backoff limit of " + conflictRun,
			Trigger: "/gordon", Conflict: true, Namespace: "kelos-agents",
			RunURL: "https://lucarne.example/runs/hollingsworth/kelos-agents/uid-2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			f.private = c.private
			e, _ := executor(f, failedRuns())
			e.Messages = message
			a := action.Notice(pr4, notice.Failed, c.run)
			for range 2 {
				if err := e.Execute(ctx, a); err != nil {
					t.Fatal(err)
				}
			}
			want := notice.PRFailedText(c.want) + "\n" + notice.ActionMarker(a.Key)
			if len(f.comments) != 1 || f.comments[0].Body != want {
				t.Fatalf("comments %+v\nwant one %q", f.comments, want)
			}
		})
	}
}

// TestFailedNoticeWaitsForItsReads: a failed message read posts nothing,
// so the action retries instead of losing the last word; without a
// reader the notice has none.
func TestFailedNoticeWaitsForItsReads(t *testing.T) {
	f := newFake(t)
	e, _ := executor(f, failedRuns())
	readErr := errors.New("kelos down")
	e.Messages = func(context.Context, string) (string, error) { return "", readErr }
	a := action.Notice(pr4, notice.Failed, runName)
	if err := e.Execute(ctx, a); !errors.Is(err, readErr) || len(f.comments) != 0 {
		t.Fatalf("failed read: %v, %d comments", err, len(f.comments))
	}
	e.Messages = nil
	if err := e.Execute(ctx, a); err != nil {
		t.Fatal(err)
	}
	want := notice.PRFailedText(notice.PRFailedInfo{Task: runName, Trigger: "/gordon", Namespace: "kelos-agents"}) + "\n" +
		notice.ActionMarker(a.Key)
	if len(f.comments) != 1 || f.comments[0].Body != want {
		t.Fatalf("comments %+v, want one %q", f.comments, want)
	}
	if err := e.Execute(ctx, action.Notice(pr4, notice.Failed, "pr-followup-vigie-4-c1")); !errors.Is(err, store.ErrUnknownRun) {
		t.Errorf("unknown run: %v", err)
	}
}
