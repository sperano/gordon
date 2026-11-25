package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/run"
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

// TestRestartNotices: a restart's notice names the Task it restarts (the
// run's previous attempt) with its limit, the restart's number of the
// configured maximum and its run link; the gave-up notice names the
// stopped Task. Each is posted once, and the legacy reaper's counts.
func TestRestartNotices(t *testing.T) {
	const restarted = "vikunja-5-gordon-opus-retry2"
	id := run.Identity{Family: run.VikunjaImplement, Project: "gordon", Variant: "claude-opus", TaskID: 5, Retry: 2}
	rs := runs{restarted: {Name: restarted, Family: run.VikunjaImplement, Identity: id, TaskUID: "uid-7"}}
	var asked []string
	limits := func(_ context.Context, name string) (string, error) {
		asked = append(asked, name)
		return "weekly", nil
	}
	cases := []struct {
		a    action.Action
		want string
	}{
		{action.Notice(action.VikunjaTask(5), notice.Restart, restarted), notice.RestartText(notice.RestartInfo{
			Stopped: "vikunja-5-gordon-opus-retry1", Restart: restarted, LimitType: "weekly", Attempt: 2, Max: 4,
			RunURL: "https://lucarne.example/runs/c/kelos-agents/uid-7"})},
		{action.Notice(action.VikunjaTask(5), notice.RestartGaveUp, "vikunja-5-gordon-opus-retry3"),
			notice.GaveUpText("vikunja-5-gordon-opus-retry3", "weekly", 4)},
	}
	for _, c := range cases {
		f := newFake(t)
		f.put(Task{ID: 5, ProjectID: 3})
		ex := executor(f, rs)
		ex.Limits, ex.MaxRestarts = limits, 4
		ex.Links = Links{Lucarne: "https://lucarne.example", Cluster: "c", Namespace: "kelos-agents"}
		for range 2 {
			if err := ex.Execute(ctx, c.a); err != nil {
				t.Fatalf("%s: %v", c.a.Value, err)
			}
		}
		if cs := f.comments[5]; len(cs) != 1 || cs[0].Comment != c.want+notice.ActionMarker(c.a.Key) {
			t.Fatalf("%s comments %+v, want one %q", c.a.Value, cs, c.want)
		}
		legacy := newFake(t)
		legacy.put(Task{ID: 5, ProjectID: 3})
		legacy.comments[5] = []Comment{{ID: 1, Comment: c.want, Author: User{Username: gordon}}}
		lex := executor(legacy, rs)
		lex.Limits = limits
		if err := lex.Execute(ctx, c.a); err != nil || len(legacy.comments[5]) != 1 {
			t.Fatalf("%s after the legacy notice: %v, %d comments", c.a.Value, err, len(legacy.comments[5]))
		}
	}
	if !slices.Contains(asked, "vikunja-5-gordon-opus-retry1") || !slices.Contains(asked, "vikunja-5-gordon-opus-retry3") {
		t.Errorf("limits read for %v", asked)
	}
}

func TestRestartNoticeFailures(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	original := run.Identity{Family: run.VikunjaImplement, Project: "gordon", Variant: "claude-opus", TaskID: 5}
	ex := executor(f, runs{"vikunja-5-gordon-opus": {Identity: original}})
	// The notice of a run that restarts nothing is refused for good.
	a := action.Notice(action.VikunjaTask(5), notice.Restart, "vikunja-5-gordon-opus")
	if err := ex.Execute(ctx, a); !worker.IsPermanent(err) {
		t.Fatalf("restart notice of an original run: %v, want permanent", err)
	}
	// A failed limit read posts nothing: the action retries.
	readErr := errors.New("kelos down")
	ex.Limits = func(context.Context, string) (string, error) { return "", readErr }
	gave := action.Notice(action.VikunjaTask(5), notice.RestartGaveUp, "vikunja-5-gordon-opus-retry3")
	if err := ex.Execute(ctx, gave); !errors.Is(err, readErr) || len(f.comments[5]) != 0 {
		t.Fatalf("failed limit read: %v, %d comments", err, len(f.comments[5]))
	}
	// Without a reader the notice names no limit, and the default maximum.
	ex.Limits = nil
	if err := ex.Execute(ctx, gave); err != nil || len(f.comments[5]) != 1 ||
		!strings.HasPrefix(f.comments[5][0].Comment, notice.GaveUpText("vikunja-5-gordon-opus-retry3", "", 3)) {
		t.Fatalf("gave-up without a limit reader: %v, %+v", err, f.comments[5])
	}
}
