package ghlife

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

var (
	ctx = context.Background()
	pr4 = action.PullRequest("sperano/vigie", 4)
)

func react() action.Action {
	a := action.New(action.React, action.GitHubComment("sperano/vigie", "c", 77), "eyes")
	a.Detail = "IC_77"
	return a
}

func TestReactionOnceAndRecovered(t *testing.T) {
	f := newFake(t)
	e, _ := executor(f, nil)
	f.failNext["react"] = true
	err := e.Execute(ctx, react())
	if !worker.IsAmbiguous(err) {
		t.Fatalf("a 500 after the mutation is %v, want ambiguous", err)
	}
	if ok, err := e.Recover(ctx, react()); !ok || err != nil {
		t.Fatalf("recover %v %v, want the reaction found", ok, err)
	}
	if err := e.Execute(ctx, react()); err != nil {
		t.Fatal(err)
	}
	if got := f.wrote(); !slices.Equal(got, []string{"react IC_77"}) {
		t.Errorf("writes %v, want one reaction", got)
	}
	if !slices.Equal(f.tokens[:1], []string{"vigie:pull_requests"}) {
		t.Errorf("reaction token scopes %v, want pull_requests only", f.tokens)
	}
	noNode := react()
	noNode.Detail = ""
	if err := e.Execute(ctx, noNode); !worker.IsPermanent(err) {
		t.Errorf("reaction without a node: %v, want permanent", err)
	}
}

func draftActions() (action.Action, action.Action) {
	label := action.New(action.AddLabel, pr4, "kelos:follow-up")
	return label, action.New(action.ConvertDraft, pr4, "").WaitFor(label)
}

func TestDraftLabelsThenConverts(t *testing.T) {
	f := newFake(t)
	e, _ := executor(f, nil)
	label, draft := draftActions()
	for _, a := range []action.Action{label, draft, label, draft} {
		if err := e.Execute(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.wrote(); !slices.Equal(got, []string{"label " + labelID, "draft " + prNode}) {
		t.Errorf("writes %v, want one label and one conversion", got)
	}
	if !slices.Contains(f.tokens, "vigie:contents,pull_requests") {
		t.Errorf("token scopes %v: the conversion needs contents", f.tokens)
	}
}

func TestDraftLeavesADraftAlone(t *testing.T) {
	f := newFake(t)
	f.draft = true
	e, _ := executor(f, nil)
	label, draft := draftActions()
	for _, a := range []action.Action{label, draft} {
		if err := e.Execute(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.wrote(); len(got) != 0 {
		t.Errorf("writes %v on a draft, want none", got)
	}
}

func TestDraftLabelMissingFromTheRepository(t *testing.T) {
	f := newFake(t)
	f.repoLabel = false
	e, _ := executor(f, nil)
	label, _ := draftActions()
	err := e.Execute(ctx, label)
	if !worker.IsPermanent(err) || !strings.Contains(err.Error(), `label "kelos:follow-up" missing from sperano/vigie, not drafted`) {
		t.Errorf("missing label: %v, want permanent", err)
	}
}

func TestRemoveLabel(t *testing.T) {
	f := newFake(t)
	f.labels = []string{"kelos:fix-suggestions", "other"}
	e, _ := executor(f, nil)
	a := action.New(action.RemoveLabel, pr4, "kelos:fix-suggestions")
	for range 2 {
		if err := e.Execute(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.wrote(); !slices.Equal(got, []string{"unlabel kelos:fix-suggestions"}) || !slices.Equal(f.labels, []string{"other"}) {
		t.Errorf("writes %v labels %v", got, f.labels)
	}
	// Removed by someone between the read and the write: a 404 is done.
	if err := e.removeLabel(ctx, a, Target{Repo: Repo{owner, repoNm}, Kind: "pull", N: 4}); err != nil {
		t.Errorf("label gone meanwhile: %v", err)
	}
}

func TestSkippedChecks(t *testing.T) {
	f := newFake(t)
	e, _ := executor(f, nil)
	a := action.New(action.SkippedChecks, pr4, "review/pr-challenger@0123abcd")
	a.Detail = "author:renovate[bot]"
	for range 2 {
		if err := e.Execute(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	runs := f.checks["0123abcd"]
	if len(runs) != 1 {
		t.Fatalf("%d check runs, want 1", len(runs))
	}
	out := runs[0]["output"].(map[string]any)
	if runs[0]["conclusion"] != "skipped" || runs[0]["status"] != "completed" || out["title"] != "Not reviewed" ||
		out["summary"] != "Pull requests opened by `renovate[bot]` are not reviewed. This check is skipped so that the required `review/pr-challenger` check does not hold the merge." {
		t.Errorf("check run %v", runs[0])
	}
	if got := SkippedSummary("review/x", "label", "kelos:no-review"); got != "Pull requests labelled `kelos:no-review` are not reviewed. This check is skipped so that the required `review/x` check does not hold the merge." {
		t.Errorf("label summary %q", got)
	}
	bad := a
	bad.Detail = "nothing"
	bad.Value = "review/x@feedbeef"
	if err := e.Execute(ctx, bad); !worker.IsPermanent(err) {
		t.Errorf("bad reason: %v, want permanent", err)
	}
}

const runName = "pr-followup-vigie-4-c77"

func runs() fakeRuns {
	return fakeRuns{runName: store.RunInfo{Name: runName, Family: run.PRFollowUp, Model: "claude-opus-5-5",
		Effort: "high", TaskUID: "uid-1"}}
}

func start() action.Action { return action.Notice(pr4, notice.Start, runName) }

func TestStartNoticeOnAPublicRepository(t *testing.T) {
	f := newFake(t)
	e, l := executor(f, runs())
	for range 2 {
		if err := e.Execute(ctx, start()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.comments) != 1 {
		t.Fatalf("%d comments, want 1", len(f.comments))
	}
	want := notice.PRStartText(notice.PRStartInfo{Task: runName, Model: "claude-opus-5-5", Effort: "high"}) + "\n" +
		notice.ActionMarker(start().Key)
	if f.comments[0].Body != want {
		t.Errorf("notice\n%q\nwant\n%q", f.comments[0].Body, want)
	}
	if !slices.Equal(l.targets, []string{string(pr4)}) {
		t.Errorf("locks %v, want the PR's once", l.targets)
	}
}

func TestStartNoticeOnAPrivateRepositoryLinks(t *testing.T) {
	f := newFake(t)
	f.private = true
	e, _ := executor(f, runs())
	if err := e.Execute(ctx, start()); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"[lucarne](https://lucarne.example/runs/hollingsworth/kelos-agents/uid-1)",
		"[sillage](https://sillage.example/open?url=https%3A%2F%2Fgithub.com%2Fsperano%2Fvigie%2Fpull%2F4)"} {
		if !strings.Contains(f.comments[0].Body, link) {
			t.Errorf("private notice lacks %s:\n%s", link, f.comments[0].Body)
		}
	}
}

func TestAmbiguousNoticeIsRecovered(t *testing.T) {
	f := newFake(t)
	e, _ := executor(f, runs())
	f.failNext["comment"] = true
	if err := e.Execute(ctx, start()); !worker.IsAmbiguous(err) {
		t.Fatalf("a 500 after posting is %v, want ambiguous", err)
	}
	if ok, err := e.Recover(ctx, start()); !ok || err != nil {
		t.Fatalf("recover %v %v, want the notice found by its marker", ok, err)
	}
}

func TestLegacyNoticeCountsOnlyFromAnAuthor(t *testing.T) {
	f := newFake(t)
	legacy := "BACK ON THE CASE!\n\n" + notice.Marker(notice.Start, runName)
	f.addComment("mallory", legacy)
	e, _ := executor(f, runs())
	if ok, _ := e.Recover(ctx, start()); ok {
		t.Fatal("a stranger's copy of the legacy marker suppressed the notice")
	}
	f.addComment("sperano", legacy)
	if ok, err := e.Recover(ctx, start()); !ok || err != nil {
		t.Errorf("the legacy start comment of the run was not recognised: %v", err)
	}
	if ok, _ := e.Recover(ctx, action.Notice(pr4, notice.Start, "pr-followup-vigie-4-c78")); ok {
		t.Error("another run's start comment counted")
	}
}

func TestCapNotices(t *testing.T) {
	f := newFake(t)
	e, _ := executor(f, nil)
	e.Policy.MaxRoundsPerPR = 50
	for _, a := range []action.Action{action.Notice(pr4, notice.ReviewCap, ""), action.Notice(pr4, notice.SuggestionsCap, "")} {
		if err := e.Execute(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	p := e.Policy
	want := []string{notice.ReviewCapText([]string{"cubic-dev-ai[bot]", "gordon-cole-ai[bot]"}, 50, "/gordon"),
		notice.SuggestionsCapText(p.SuggestionsMaxRounds, p.FixSuggestionsLabel, "/gordon")}
	for i, w := range want {
		if !strings.HasPrefix(f.comments[i].Body, w+"\n") {
			t.Errorf("notice %d:\n%s\nwant\n%s", i, f.comments[i].Body, w)
		}
	}
	// A cap notice the legacy script posted is not repeated.
	g := newFake(t)
	g.addComment(appBot, notice.ReviewCapText([]string{"x"}, 3, "/gordon"))
	e2, _ := executor(g, nil)
	if err := e2.Execute(ctx, action.Notice(pr4, notice.ReviewCap, "")); err != nil || len(g.wrote()) != 0 {
		t.Errorf("legacy cap notice repeated: %v %v", err, g.wrote())
	}
}

func TestTargetsAndKindsRefused(t *testing.T) {
	f := newFake(t)
	e, _ := executor(f, nil)
	for _, a := range []action.Action{
		action.New(action.AddLabel, action.VikunjaTask(4), "x"),
		action.New(action.AddLabel, "github/sperano/vigie/pull/x", "x"),
		action.New(action.AddLabel, "github/../vigie/pull/4", "x"),
		action.New(action.AddLabel, action.GitHubComment("sperano/vigie", "c", 7), "x"),
		action.New(action.MarkDone, pr4, ""),
		action.Notice(pr4, "unknown", ""),
	} {
		if err := e.Execute(ctx, a); !worker.IsPermanent(err) {
			t.Errorf("%s %s: %v, want permanent", a.Kind, a.Target, err)
		}
	}
}
