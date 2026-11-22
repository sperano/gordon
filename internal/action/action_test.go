package action

import "testing"

func TestKeysAreStableAndDistinct(t *testing.T) {
	cases := map[string]Action{
		"submit-task:kelos/kelos-agents/vikunja-vigie-gpt-sol-12": New(SubmitTask, KelosTask("kelos-agents", "vikunja-vigie-gpt-sol-12"), ""),
		"react:vikunja/comment/9:eyes":                            New(React, VikunjaComment(9), "eyes"),
		"add-label:github/sperano/vigie/pull/4:kelos:follow-up":   New(AddLabel, PullRequest("sperano/vigie", 4), "kelos:follow-up"),
		"react:github/sperano/vigie/l/7:eyes":                     New(React, GitHubComment("sperano/vigie", "l", 7), "eyes"),
		"post-notice:vikunja/task/12:merged:sperano/vigie#4":      Notice(VikunjaTask(12), "merged", "sperano/vigie#4"),
		"post-notice:vikunja/comment/9:discuss-start":             Notice(VikunjaComment(9), "discuss-start", ""),
	}
	for want, a := range cases {
		if a.Key != want {
			t.Errorf("key %q, want %q", a.Key, want)
		}
	}
	n := Notice(VikunjaComment(9), "unanswered", "task-done")
	if n.Kind != PostNotice || n.Value != "unanswered" || n.Detail != "task-done" {
		t.Errorf("notice %+v", n)
	}
	if Notice(VikunjaComment(9), "unanswered", "task-done").Key == Notice(VikunjaComment(9), "unanswered", "no-agent-label").Key {
		t.Error("two notice reasons share a key")
	}
}

func TestCostNoticeKeyPerRevision(t *testing.T) {
	a := CostNotice(5, "vikunja-gordon-claude-opus-5", 41, 2)
	if a.Key != "publish-cost:vikunja/task/5:vikunja-gordon-claude-opus-5:41:2" || a.Kind != PublishCost ||
		a.Target != "vikunja/task/5" || a.Value != "vikunja-gordon-claude-opus-5" || a.Detail != "41:2" {
		t.Errorf("CostNotice = %+v", a)
	}
	if b := CostNotice(5, "vikunja-gordon-claude-opus-5", 42, 2); b.Key == a.Key {
		t.Error("two generations share a key")
	}
}
