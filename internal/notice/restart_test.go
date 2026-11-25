package notice

import (
	"encoding/json"
	"os"
	"testing"
)

// restart_texts.json holds the exact restart notice texts for these
// inputs, first printed by the script Gordon replaced
// (limit_reaper.restarted_comment and gave_up_comment, Hollingsworth
// helm/kelos/scripts at e60c284).
func TestRestartTextsGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/restart_texts.json")
	if err != nil {
		t.Fatal(err)
	}
	var l map[string]string
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"restart_link": RestartText(RestartInfo{Stopped: "vikunja-vigie-claude-opus-12", Restart: "vikunja-vigie-claude-opus-12-retry1",
			LimitType: "five_hour<x>", Attempt: 1, Max: 3, RunURL: "https://lucarne.example/runs/c/kelos-agents/uid-1"}),
		"restart_bare": RestartText(RestartInfo{Stopped: "pr-followup-vigie-4-r9-retry1", Restart: "pr-followup-vigie-4-r9-retry2",
			Attempt: 2, Max: 3}),
		"gave_up":      GaveUpText("vikunja-vigie-claude-opus-12", "five_hour<x>", 3),
		"gave_up_bare": GaveUpText("pr-followup-vigie-4-r9-retry1", "", 1),
	}
	if len(cases) != len(l) {
		t.Fatalf("%d cases for %d golden texts", len(cases), len(l))
	}
	for key, got := range cases {
		if got != l[key] {
			t.Errorf("%s:\n got %s\nwant %s", key, got, l[key])
		}
	}
}

func TestGaveUpPosted(t *testing.T) {
	text := GaveUpText("vikunja-a-1", "", 3)
	cases := []struct {
		text, stopped string
		want          bool
	}{
		{text, "vikunja-a-1", true},
		{text, "vikunja-a-2", false},
		{text, "", false},
		{"<p>KELOS TASK <code>vikunja-a-1</code></p>", "vikunja-a-1", false},
		{RestartText(RestartInfo{Stopped: "vikunja-a-1", Restart: "vikunja-a-1-retry1", Attempt: 1, Max: 3}), "vikunja-a-1", false},
	}
	for i, c := range cases {
		if got := GaveUpPosted(c.text, c.stopped); got != c.want {
			t.Errorf("case %d: GaveUpPosted = %v, want %v", i, got, c.want)
		}
	}
}
