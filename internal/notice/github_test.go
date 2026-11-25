package notice

import (
	"encoding/json"
	"os"
	"testing"
)

// githubTexts are the exact GitHub notice texts, first printed by the
// scripts Gordon replaced (hollingsworth bf043be, helm/kelos/scripts:
// announce_github_start.py and cap_review_rounds.py).
func githubTexts(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("testdata/github_texts.json")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGitHubTextsGolden(t *testing.T) {
	want := githubTexts(t)
	sillage := SillagePRURL("https://sillage.example/", "https://github.com/sperano/gordon/pull/12")
	got := map[string]string{
		"start_full": PRStartText(PRStartInfo{Task: "pr-followup-gordon-12-r345", Model: "claude-opus-5-5", Effort: "high",
			RunURL: "https://lucarne.example/runs/c/kelos-agents/uid-1", ActivityURL: sillage}),
		"start_bare":       PRStartText(PRStartInfo{Task: "pr-followup-gordon-12-c99"}),
		"start_conflict":   PRStartText(PRStartInfo{Task: "pr-conflict-0123abcd-7-89abcdef-01234567", Model: "m"}),
		"cap_many":         ReviewCapText([]string{"gordon-cole-ai[bot]", "cubic-dev-ai[bot]"}, 50, "/gordon"),
		"cap_one":          ReviewCapText([]string{"cubic-dev-ai[bot]"}, 1, "/gordon"),
		"suggestions_many": SuggestionsCapText(4, "kelos:fix-suggestions", "/gordon"),
		"suggestions_one":  SuggestionsCapText(1, "kelos:fix-suggestions", "/gordon"),
		"sillage":          sillage,
	}
	if len(got) != len(want) {
		t.Fatalf("%d texts, the goldens have %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s:\n got %q\nwant %q", k, got[k], w)
		}
	}
}

func TestCapNoticesRecognised(t *testing.T) {
	cases := []struct {
		name, text      string
		cap, suggestion bool
	}{
		{"cap marker", ReviewCapText([]string{"b"}, 2, "/gordon"), true, false},
		{"suggestions marker", SuggestionsCapText(2, "l", "/gordon"), false, true},
		{"cap before markers", "This pull request has reached its review-bot round limit: ...", true, false},
		{"suggestions before markers", "THIS PULL REQUEST HAS REACHED ITS FIX-SUGGESTIONS ROUND LIMIT", false, true},
		// A marked notice of another kind quoting the phrase is not a cap.
		{"other marker", "HAS REACHED ITS REVIEW-BOT ROUND LIMIT " + Marker(Start, "pr-followup-x-1-c2"), false, false},
		{"plain", "thanks", false, false},
	}
	for _, c := range cases {
		if got := ReviewCapPosted(c.text); got != c.cap {
			t.Errorf("%s: ReviewCapPosted = %v", c.name, got)
		}
		if got := SuggestionsCapPosted(c.text); got != c.suggestion {
			t.Errorf("%s: SuggestionsCapPosted = %v", c.name, got)
		}
	}
}
