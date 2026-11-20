package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/worker"
)

// TestPullRequestNotices: the router's pr-open and merged notices post
// the legacy texts once, and a legacy script's notice of the same PR
// counts as posted.
func TestPullRequestNotices(t *testing.T) {
	const url = "https://github.com/sperano/vigie/pull/4"
	cases := []struct {
		kind, want string
		readiness  bool
	}{
		{notice.PROpen, notice.PROpenText(url, false), false},
		{notice.PROpen, notice.PROpenText(url, true), true},
		{notice.Merged, notice.MergedText(url), false},
	}
	for _, c := range cases {
		f := newFake(t)
		f.put(Task{ID: 5, ProjectID: 3})
		ex := executor(f, nil)
		ex.Policy.PRReadiness = c.readiness
		a := action.Notice(action.VikunjaTask(5), c.kind, "sperano/vigie#4")
		for range 2 {
			if err := ex.Execute(ctx, a); err != nil {
				t.Fatalf("%s: %v", c.kind, err)
			}
		}
		if cs := f.comments[5]; len(cs) != 1 || cs[0].Comment != c.want+notice.ActionMarker(a.Key) {
			t.Fatalf("%s comments %+v, want one %q", c.kind, cs, c.want)
		}
		legacy := newFake(t)
		legacy.put(Task{ID: 5, ProjectID: 3})
		legacy.comments[5] = []Comment{{ID: 1, Comment: c.want, Author: User{Username: gordon}}}
		if err := executor(legacy, nil).Execute(ctx, a); err != nil || len(legacy.comments[5]) != 1 {
			t.Fatalf("%s after the legacy notice: %v, %d comments", c.kind, err, len(legacy.comments[5]))
		}
	}
	bad := action.Notice(action.VikunjaTask(5), notice.Merged, "vigie")
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	if err := executor(f, nil).Execute(ctx, bad); !worker.IsPermanent(err) {
		t.Fatalf("malformed PR reference: %v, want permanent", err)
	}
}

// TestFailedNoticeWaitsForTheMessage: the failed notice quotes the
// Task's status message, and a failed read of it posts nothing (the
// action retries) instead of a notice without its last word.
func TestFailedNoticeWaitsForTheMessage(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, startRun())
	readErr := errors.New("kelos down")
	ex.Messages = func(context.Context, string) (string, error) { return "", readErr }
	a := action.Notice(action.VikunjaTask(5), notice.Failed, runName)
	if err := ex.Execute(ctx, a); !errors.Is(err, readErr) || len(f.comments[5]) != 0 {
		t.Fatalf("failed read: %v, %d comments", err, len(f.comments[5]))
	}
	ex.Messages = func(_ context.Context, name string) (string, error) { return "backoff limit of " + name, nil }
	if err := ex.Execute(ctx, a); err != nil || len(f.comments[5]) != 1 ||
		!strings.Contains(f.comments[5][0].Comment, "backoff limit of "+runName) {
		t.Fatalf("failed notice: %v, %+v", err, f.comments[5])
	}
}
