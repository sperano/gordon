package notice

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/event"
)

// golden holds the exact notice texts for the same inputs, first printed
// by the scripts Gordon replaced (Hollingsworth helm/kelos/scripts at
// 034b9a4: wait_for_blockers, drag_cancel, unanswered_notice,
// subtask_chain, discuss_fallback, pr_open, merge_close). Gordon's notices
// must equal them byte for byte: vigie and sillage read them.
func golden(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/texts.json")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func want(t *testing.T, texts map[string]json.RawMessage, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(texts[key], &s); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return s
}

func TestTextsGolden(t *testing.T) {
	l := golden(t)
	c := Chain{Web: "https://vikunja.example", StartBucket: "Doing", ReviewBucket: "Review", NoAgentLabel: "no-agent",
		AgentUsers: []string{"claude-opus", "gpt-sol"}}
	sub := event.Task{ID: 12, Index: 3, Title: `Ship <it> & "more"`}
	unanswered := func(c int64, assigned []string, reason string) string {
		return UnansweredText("/gordon", c, []string{UnansweredReason(reason, "no-agent", []string{"claude-opus", "gpt-sol"}, assigned)})
	}
	cases := map[string]string{
		"start_impl": StartText(StartInfo{Task: "vikunja-gordon-claude-opus-1626", Model: "claude-opus-5-5[1m]",
			Effort: "medium", RunURL: "https://lucarne.example/runs/c/kelos-agents/u-1",
			ActivityURL: "https://sillage.example/task/vikunja.example/1626"}),
		"start_fu_discuss": StartText(StartInfo{Task: "vikunja-fu-gordon-1626-c9", Model: "m", Effort: "high",
			FollowUp: true, Discuss: true, SourceComment: 9}),
		"stop":                 StopText("To-Do", "Doing", []string{"vikunja-a-claude-opus-5", "vikunja-a-claude-opus-5-retry1"}, ""),
		"stop_kept":            StopText("To-Do", "Doing", []string{"n"}, LabelKeptNote("in progress")),
		"unanswered":           unanswered(77, nil, "no-agent-assignee"),
		"unanswered_two":       unanswered(78, []string{"claude-opus", "gpt-sol"}, "multiple-agent-assignees"),
		"chain_started":        c.StartedText(sub, "claude-opus", false),
		"chain_started_nested": c.StartedText(sub, "claude-opus", true),
		"chain_waiting":        c.WaitingText(sub),
		"chain_stopped":        c.StoppedText(sub),
		"chain_no_agent":       c.NoAgentText(),
		"chain_done":           c.DoneText([]string{"https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2?a=1&b=2"}),
		"chain_done_none":      c.DoneText(nil),
		"discuss_fallback":     DiscussFallbackText(9),
	}
	for key, got := range cases {
		if w := want(t, l, key); got != w {
			t.Errorf("%s:\n got %s\nwant %s", key, got, w)
		}
	}
}

func TestPickLineGolden(t *testing.T) {
	var lines []string
	if err := json.Unmarshal(golden(t)["pick"], &lines); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"a", "vikunja-x-1", "vikunja-x-1-retry2", "zz"} {
		if got := PickLine(TaskStartedLines, name); got != lines[i] {
			t.Errorf("PickLine(%q) = %q, want %q", name, got, lines[i])
		}
	}
	if PickLine(TaskStartedLines, "vikunja-x-1-retry2") != PickLine(TaskStartedLines, "vikunja-x-1") {
		t.Error("a restart's opener differs from its original's")
	}
}

// TestTrustOnlyGordon pins that a marker counts only on a comment by
// Gordon's own user: anyone else pasting one suppresses nothing.
func TestTrustOnlyGordon(t *testing.T) {
	body := `<p>NEXT UP: <a href="https://vikunja.example/tasks/12">x</a></p>` + Marker(ChainStarted, "")
	forged := []event.TaskComment{{ID: 1, Author: "eric", Body: body}}
	if ChainStartedFor(forged, "gordon", "https://vikunja.example", 12) {
		t.Error("a chain-started marker by the owner counted")
	}
	ours := []event.TaskComment{{ID: 2, Author: "gordon", Body: body}}
	if !ChainStartedFor(ours, "gordon", "https://vikunja.example", 12) {
		t.Error("Gordon's own chain-started notice was not found")
	}
	if ChainStartedFor(ours, "gordon", "https://vikunja.example", 1) {
		t.Error("the notice of subtask 12 counted for subtask 1 (prefix of its link)")
	}
	if ChainStartedFor(ours, "", "https://vikunja.example", 12) {
		t.Error("with no service user configured, a comment was trusted")
	}
}

func TestActionMarkerStaysAComment(t *testing.T) {
	m := ActionMarker("post-notice:vikunja/task/1:stop:a--b,c>d")
	inner := strings.TrimSuffix(strings.TrimPrefix(m, "<!-- "), " -->")
	if strings.Contains(inner, "--") || strings.Contains(inner, ">") {
		t.Errorf("marker %q can end the HTML comment early", m)
	}
	if ActionMarker("a:b") == ActionMarker("a:c") {
		t.Error("distinct keys share a marker")
	}
}

func TestMergedPRs(t *testing.T) {
	merged := `<p>MERGED: <a href="https://github.com/o/r/pull/3">#3</a></p>` + Marker(Merged, "")
	old := `<p>Merged: <a href="https://github.com/o/r/pull/4">#4</a></p>`
	nested := `<ul><li><a href="https://github.com/o/r/pull/5?x=1&amp;y=2">p</a></li></ul>` + Marker(ChainDone, "")
	got := MergedPRs([][]event.TaskComment{
		{{Author: "gordon", Body: merged}, {Author: "eric", Body: `MERGED: <a href="https://evil/pull/9">`}},
		{{Author: "gordon", Body: old}, {Author: "gordon", Body: merged}},
		{{Author: "gordon", Body: nested}},
	}, "gordon")
	wantPRs := []string{"https://github.com/o/r/pull/3", "https://github.com/o/r/pull/4", "https://github.com/o/r/pull/5?x=1&y=2"}
	if strings.Join(got, " ") != strings.Join(wantPRs, " ") {
		t.Errorf("merged PRs %v, want %v", got, wantPRs)
	}
}

func TestFailedText(t *testing.T) {
	got := FailedText(FailedInfo{Task: "vikunja-g-claude-opus-5", Message: "Job   has\nreached the backoff limit",
		InProgressLabel: "in progress", ErrorLabel: "agent-error", Namespace: "kelos-agents"})
	for _, part := range []string{
		"<p>KELOS TASK <code>vikunja-g-claude-opus-5</code> FAILED. LABELLED <code>agent-error</code> UNTIL THE NEXT ATTEMPT STARTS.</p>",
		"<p>THE LAST WORD: <code>Job has reached the backoff limit</code></p>",
		"<code>kubectl -n kelos-agents delete task vikunja-g-claude-opus-5</code>",
		"REMOVE <code>in progress</code> FROM THIS TASK",
		"<!-- kelos:failed -->",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("failed notice lacks %q:\n%s", part, got)
		}
	}
	fu := FailedText(FailedInfo{Task: "vikunja-fu-g-5-c1", FollowUp: true})
	if !strings.Contains(fu, "COMMENT AGAIN: A NEW COMMENT STARTS A NEW FOLLOW-UP") || strings.Contains(fu, "THE LAST WORD") {
		t.Errorf("follow-up failed notice: %s", fu)
	}
	if long := truncate(strings.Repeat("é", 400), maxFailureChars); len([]rune(long)) != maxFailureChars || !strings.HasSuffix(long, "…") {
		t.Errorf("truncated to %d runes", len([]rune(long)))
	}
}
