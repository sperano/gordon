package inventory

import (
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/run"
)

func TestEntriesAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Entries() {
		if seen[e.ID] {
			t.Errorf("duplicate entry %s", e.ID)
		}
		seen[e.ID] = true
		if e.Trigger == "" || len(e.Legacy) == 0 || e.Kind == "" {
			t.Errorf("%s: missing trigger, legacy files or kind", e.ID)
		}
		checkDecision(t, e)
	}
}

// checkDecision keeps owner, decision and stages consistent: a migration
// is built at one stage and cut over at a later one, a retention moves
// nothing, a retirement names its removal stage.
func checkDecision(t *testing.T, e Entry) {
	t.Helper()
	switch e.Decision {
	case Migrate:
		if e.Owner != Gordon || e.Implement < StageIntake || e.Cutover <= e.Implement || e.Cutover > StageRetire {
			t.Errorf("%s: migrate to %s at %d → %d", e.ID, e.Owner, e.Implement, e.Cutover)
		}
	case Retain:
		if e.Owner != Kelos && e.Owner != Hollingsworth || e.Implement != 0 || e.Cutover != 0 || len(e.Families) != 0 {
			t.Errorf("%s: retain by %s with stages %d → %d", e.ID, e.Owner, e.Implement, e.Cutover)
		}
	case Retire:
		if e.Owner != Nobody || e.Cutover == 0 || len(e.Families) != 0 {
			t.Errorf("%s: retire by %s at %d", e.ID, e.Owner, e.Cutover)
		}
	default:
		t.Errorf("%s: unknown decision %q", e.ID, e.Decision)
	}
}

// TestEveryRoutedFamilyHasAnEntry fails when the router gains a family
// the inventory does not explain.
func TestEveryRoutedFamilyHasAnEntry(t *testing.T) {
	covered := map[run.Family]bool{}
	for _, e := range Entries() {
		for _, f := range e.Families {
			covered[f] = true
		}
	}
	for _, f := range run.Families {
		if !covered[f] {
			t.Errorf("family %s has no inventory entry", f)
		}
	}
}

// TestDispatchFamiliesDecided pins the decisions the task requires: the
// legacy dispatch families and the two explicit calls (wiki follow-ups
// retired, model-free image builds retained with Kelos).
func TestDispatchFamiliesDecided(t *testing.T) {
	want := map[string]Decision{
		"vikunja-implement": Migrate, "vikunja-discuss": Migrate, "vikunja-followup": Migrate,
		"vikunja-followup-answer": Migrate, "vikunja-followup-discuss": Migrate, "pr-followup": Migrate,
		"pr-review": Migrate, "pr-conflict": Migrate, "pr-open": Migrate, "merge-close": Migrate,
		"pr-readiness": Migrate, "review-poller": Migrate, "subtask-chain": Migrate, "drag-cancel": Migrate,
		"limit-reaper": Migrate, "wiki-followup": Retire, "wiki-comment-poller": Retire,
		"image-build-request": Retain, "image-build-push": Retain, "cluster-triage": Retain, "kelos-branch-lock": Retain,
	}
	got := map[string]Entry{}
	for _, e := range Entries() {
		got[e.ID] = e
	}
	for id, d := range want {
		if e, ok := got[id]; !ok || e.Decision != d {
			t.Errorf("%s: decision %q, want %q", id, e.Decision, d)
		}
	}
	if got["image-build-request"].Owner != Kelos {
		t.Errorf("image builds owned by %s, want kelos", got["image-build-request"].Owner)
	}
}

func TestStagesCoverTheChain(t *testing.T) {
	stages := Stages()
	var numbers []int
	for _, s := range stages {
		numbers = append(numbers, s.Number)
		if s.Title == "" || len(s.Hollingsworth) == 0 {
			t.Errorf("stage %d: missing title or Hollingsworth paths", s.Number)
		}
	}
	want := []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	if !slices.Equal(numbers, want) {
		t.Errorf("stages %v, want %v", numbers, want)
	}
}

func TestMarkdown(t *testing.T) {
	got := Markdown([]Entry{
		{ID: "a", Kind: Spawner, Trigger: "x|<y>", Owner: Gordon, Decision: Migrate, Implement: 6, Cutover: 7,
			Legacy: []string{"p1", "p2"}, Note: "n"},
		{ID: "b", Kind: Cron, Trigger: "t", Owner: Nobody, Decision: Retire, Cutover: 9, Legacy: []string{"p"}},
		{ID: "c", Kind: Cron, Trigger: "t", Owner: Kelos, Decision: Retain, Legacy: []string{"p"}},
	})
	want := strings.Join([]string{
		"| Legacy entry | Kind | Trigger | Owner | Decision | Stages | Legacy files |",
		"|---|---|---|---|---|---|---|",
		"| `a` | taskspawner | x\\|&lt;y&gt; | gordon | migrate | 06 → 07 | `p1`<br>`p2` |",
		"| | | ↳ n | | | | |",
		"| `b` | cronjob | t | retired | retire | → 09 | `p` |",
		"| `c` | cronjob | t | kelos | retain | – | `p` |",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Markdown:\n%s\nwant:\n%s", got, want)
	}
}
