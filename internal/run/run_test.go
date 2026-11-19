package run

import "testing"

func TestNamesMatchLegacyTemplates(t *testing.T) {
	cases := []struct {
		id     Identity
		name   string
		branch string
	}{
		{Identity{Family: VikunjaImplement, Project: "vigie", Variant: "gpt-sol", TaskID: 12}, "vikunja-vigie-gpt-sol-12", "kelos/vikunja-12"},
		{Identity{Family: VikunjaDiscuss, Project: "vigie", Variant: "gpt-sol", TaskID: 12}, "vikunja-discuss-vigie-gpt-sol-12", "kelos/vikunja-12"},
		{Identity{Family: VikunjaFollowUp, Project: "vigie", Variant: "gpt-sol", TaskID: 12, CommentID: 3}, "vikunja-fu-vigie-12-c3", "kelos/vikunja-12"},
		{Identity{Family: VikunjaFollowUpAnswer, Project: "vigie", TaskID: 12, CommentID: 3}, "vikunja-fu-vigie-12-c3", ""},
		{Identity{Family: VikunjaFollowUpDiscuss, Project: "vigie", TaskID: 12, CommentID: 3}, "vikunja-fu-vigie-12-c3", ""},
		{Identity{Family: PRFollowUp, Workspace: "vigie", PR: 4, CommentKind: "l", CommentID: 99, HeadRef: "kelos/vikunja-12"}, "pr-followup-vigie-4-l99", "kelos/vikunja-12"},
		{Identity{Family: PRReview, Workspace: "vigie", Reviewer: "idiomatic-go", PR: 4, HeadSHA: "0123456789abcdef"}, "review-vigie-idiomatic-go-4-01234567", ""},
		{Identity{Family: PRReview, Workspace: "vigie", Reviewer: "pr-challenger", PR: 4, HeadSHA: "abc"}, "review-vigie-pr-challenger-4-abc", ""},
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
		"vikunja-vigie-gpt-sol-12-retry2": "vikunja-vigie-gpt-sol-12",
		"vikunja-vigie-gpt-sol-12":        "vikunja-vigie-gpt-sol-12",
		"pr-followup-vigie-4-c9-retry10":  "pr-followup-vigie-4-c9",
		"a-retry2-b":                      "a-retry2-b",
	}
	for in, want := range cases {
		if got := StripRetry(in); got != want {
			t.Errorf("StripRetry(%q) = %q, want %q", in, got, want)
		}
	}
}
