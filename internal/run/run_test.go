package run

import "testing"

func TestNames(t *testing.T) {
	cases := []struct {
		id     Identity
		name   string
		branch string
	}{
		{Identity{Family: VikunjaImplement, Project: "vigie", Variant: "gpt-sol", TaskID: 12}, "vikunja-12-vigie-sol", "kelos/vikunja-12"},
		{Identity{Family: VikunjaDiscuss, Project: "vigie", Variant: "gpt-sol", TaskID: 12}, "vikunja-discuss-12-vigie-sol", "kelos/vikunja-12"},
		{Identity{Family: VikunjaFollowUp, Project: "vigie", Variant: "gpt-sol", TaskID: 12, CommentID: 3}, "vikunja-fu-vigie-12-c3", "kelos/vikunja-12"},
		{Identity{Family: VikunjaFollowUpAnswer, Project: "vigie", TaskID: 12, CommentID: 3}, "vikunja-fu-vigie-12-c3", ""},
		{Identity{Family: VikunjaFollowUpDiscuss, Project: "vigie", TaskID: 12, CommentID: 3}, "vikunja-fu-vigie-12-c3", ""},
		{Identity{Family: PRFollowUp, Workspace: "vigie", PR: 4, CommentKind: "l", CommentID: 99, HeadRef: "kelos/vikunja-12"}, "pr-followup-vigie-4-l99", "kelos/vikunja-12"},
		{Identity{Family: PRReview, Workspace: "vigie", Reviewer: "idiomatic-go", PR: 4, HeadSHA: "0123456789abcdef"}, "review-vigie-idiomatic-go-4-01234567", ""},
		{Identity{Family: PRReview, Workspace: "vigie", Reviewer: "pr-challenger", PR: 4, HeadSHA: "abc"}, "review-vigie-pr-challenger-4-abc", ""},
		{Identity{Family: PRConflict, Workspace: "vigie", PR: 4, Episode: "0123abcd-4-89abcdef-01234567", HeadRef: "kelos/vikunja-12"},
			"pr-conflict-0123abcd-4-89abcdef-01234567", "kelos/vikunja-12"},
		{Identity{Family: VikunjaImplement, Project: "vigie", Variant: "gpt-sol", TaskID: 12, Retry: 2}, "vikunja-12-vigie-sol-retry2", "kelos/vikunja-12"},
		{Identity{Family: VikunjaImplement, Project: "hollingsworth", Variant: "claude-opus", TaskID: 12, Ops: true},
			"vikunja-ops-12-hollingsworth-opus", "kelos/vikunja-12"},
		{Identity{Family: VikunjaImplement, Project: "hollingsworth", Variant: "claude-opus", TaskID: 12, Ops: true, Retry: 1},
			"vikunja-ops-12-hollingsworth-opus-retry1", "kelos/vikunja-12"},
		{Identity{Family: PRFollowUp, Workspace: "vigie", PR: 4, CommentKind: "r", CommentID: 9, HeadRef: "b", Retry: 1}, "pr-followup-vigie-4-r9-retry1", "b"},
		{Identity{Family: PROpen, TaskID: 12, Retry: 1}, "", ""},
		{Identity{Family: PROpen, TaskID: 12}, "", ""},
		{Identity{Family: MergeClose, TaskID: 12}, "", ""},
	}
	for _, c := range cases {
		if got := c.id.Name(); got != c.name {
			t.Errorf("%s Name = %q, want %q", c.id.Family, got, c.name)
		}
		if got := c.id.Branch(); got != c.branch {
			t.Errorf("%s Branch = %q, want %q", c.id.Family, got, c.branch)
		}
	}
}

func TestTaskIDFromBranch(t *testing.T) {
	cases := map[string]int64{"kelos/vikunja-1621": 1621, "kelos/vikunja-": 0, "kelos/vikunja-12a": 0,
		"kelos/wiki-5": 0, "feature/kelos/vikunja-3": 0, "kelos/vikunja-99999999999999999999": 0}
	for branch, want := range cases {
		got, ok := TaskIDFromBranch(branch)
		if got != want || ok != (want != 0) {
			t.Errorf("TaskIDFromBranch(%q) = %d, %v; want %d", branch, got, ok, want)
		}
	}
}

func TestStripRetry(t *testing.T) {
	cases := map[string]string{
		"vikunja-12-vigie-sol-retry2":    "vikunja-12-vigie-sol",
		"vikunja-12-vigie-sol":           "vikunja-12-vigie-sol",
		"pr-followup-vigie-4-c9-retry10": "pr-followup-vigie-4-c9",
		"a-retry2-b":                     "a-retry2-b",
	}
	for in, want := range cases {
		if got := StripRetry(in); got != want {
			t.Errorf("StripRetry(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreviousName(t *testing.T) {
	cases := []struct {
		id   Identity
		want string
	}{
		{Identity{Family: VikunjaImplement, Project: "vigie", Variant: "gpt-sol", TaskID: 12}, "vikunja-vigie-gpt-sol-12"},
		{Identity{Family: VikunjaImplement, Project: "vigie", Variant: "gpt-sol", TaskID: 12, Retry: 2}, "vikunja-vigie-gpt-sol-12-retry2"},
		{Identity{Family: VikunjaImplement, Project: "hollingsworth", Variant: "claude-opus", TaskID: 12, Ops: true},
			"vikunja-ops-hollingsworth-claude-opus-12"},
		{Identity{Family: VikunjaDiscuss, Project: "vigie", Variant: "gpt-sol", TaskID: 12}, "vikunja-discuss-vigie-gpt-sol-12"},
		{Identity{Family: VikunjaFollowUp, Project: "vigie", TaskID: 12, CommentID: 3}, ""},
		{Identity{Family: PRFollowUp, Workspace: "vigie", PR: 4, CommentKind: "c", CommentID: 9, Retry: 1}, ""},
		{Identity{Family: PROpen, TaskID: 12}, ""},
	}
	for _, c := range cases {
		if got := c.id.PreviousName(); got != c.want {
			t.Errorf("%s PreviousName = %q, want %q", c.id.Family, got, c.want)
		}
	}
}

// TestShortVariant: a run name drops the variant's vendor prefix and keeps
// a variant without one whole.
func TestShortVariant(t *testing.T) {
	cases := map[string]string{
		"claude-opus":   "opus",
		"gpt-sol":       "sol",
		"claude-next-6": "next-6",
		"opus":          "opus",
		"claude-":       "claude-",
		"":              "",
	}
	for variant, want := range cases {
		if got := ShortVariant(variant); got != want {
			t.Errorf("ShortVariant(%q) = %q, want %q", variant, got, want)
		}
	}
}
