package inventory

import (
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/run"
)

// wantIDs is the complete legacy inventory: an entry dropped or added
// without updating this list fails.
var wantIDs = []string{
	"vikunja-implement", "vikunja-discuss", "vikunja-followup", "vikunja-followup-answer", "vikunja-followup-discuss",
	"webhook-enricher", "vikunja-followup-reconciler", "drag-cancel", "discuss-fallback", "unanswered-notice",
	"followup-reaction", "done-unlabel", "action-needed-unlabel", "subtask-chain", "wait-for-blockers",
	"announce-start", "sillage-blocker-span", "run-link", "failure-reporter", "fetch-attachments", "claim-task",
	"pr-followup", "github-reactor", "review-skip", "pr-review", "pr-task-dedup", "pr-review-supersede",
	"review-round-cap", "announce-github-start", "pr-conflict", "pr-open", "merge-close", "limit-reaper",
	"review-poller", "conflict-reconcile", "merge-close-reconcile", "pr-readiness", "wiki-comment-poller",
	"wiki-followup", "announce-wiki-start", "image-build-request", "image-build-push", "image-build-reconcile",
	"kelos-branch-lock", "pr-opener", "reviewer-scripts", "sidecar-guard", "test-postgres", "mactest",
	"dispatch-verifier", "webhook-proxy", "cluster-triage", "succeeded-task-cleaner", "cheatsheet",
	"kelos-telemetry", "clone-check",
}

func TestEntriesAreTheInventory(t *testing.T) {
	var got []string
	for _, e := range Entries() {
		got = append(got, e.ID)
	}
	if !slices.Equal(got, wantIDs) {
		t.Errorf("entries %v\nwant %v", got, wantIDs)
	}
}

// sharedLibraries are legacy helper modules a dozen entries' scripts
// import; no single entry owns them.
// scriptsDir holds the legacy scripts every inventory entry accounts for.
const scriptsDir = "helm/kelos/scripts/"

var sharedLibraries = []string{"helm/kelos/scripts/notice_kind.py"}

// TestStageScriptsHaveAnEntry fails when a stage changes a legacy script
// that no inventory entry accounts for.
func TestStageScriptsHaveAnEntry(t *testing.T) {
	listed := map[string]bool{}
	for _, p := range sharedLibraries {
		listed[p] = true
	}
	for _, e := range Entries() {
		for _, p := range e.Legacy {
			listed[p] = true
		}
	}
	for _, s := range Stages() {
		for _, p := range s.Hollingsworth {
			if strings.HasPrefix(p, scriptsDir) && !strings.HasSuffix(p, "/") && !listed[p] {
				t.Errorf("stage %02d: %s is in no inventory entry", s.Number, p)
			}
		}
	}
}

// TestEntryPathsAreStaged fails when a migrated or retired entry's legacy
// file is changed by no stage from its implementation to its cutover, so
// the stage table would omit work the move needs.
func TestEntryPathsAreStaged(t *testing.T) {
	for _, e := range Entries() {
		if e.Decision == Retain {
			continue
		}
		for _, p := range e.Legacy {
			if !stagedBetween(p, firstStage(e), e.Cutover) {
				t.Errorf("%s: %s is in no stage from %02d to %02d", e.ID, p, firstStage(e), e.Cutover)
			}
		}
	}
}

func TestStagedBetweenMatchesDirectories(t *testing.T) {
	if !stagedBetween("helm/kelos/templates/run-link.yaml", StageRetire, StageRetire) {
		t.Error("stage 13's templates/ directory does not cover a template")
	}
	if stagedBetween("helm/kelos/templates/run-link.yaml", StageGitHubCut, StageGitHubCut) {
		t.Error("stage 09 covers a template it does not list")
	}
}

// firstStage is the first stage that works on an entry: its
// implementation, or its cutover when nothing is built.
func firstStage(e Entry) int {
	if e.Implement != 0 {
		return e.Implement
	}
	return e.Cutover
}

// stagedBetween reports whether a stage numbered from lo to hi lists the
// path itself or a directory (ending in /) that contains it.
func stagedBetween(path string, lo, hi int) bool {
	for _, s := range Stages() {
		if s.Number < lo || s.Number > hi {
			continue
		}
		for _, q := range s.Hollingsworth {
			if q == path || strings.HasSuffix(q, "/") && strings.HasPrefix(path, q) {
				return true
			}
		}
	}
	return false
}

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

func TestStagesMarkdown(t *testing.T) {
	got := StagesMarkdown([]Stage{
		{Number: 2, Title: "a|b", Repositories: []string{"r1", "r2"}, Hollingsworth: []string{"p1", "p2"}},
		{Number: 12, Title: "c", Hollingsworth: []string{"p"}},
	})
	want := strings.Join([]string{
		"| Stage | Title | Repositories | Hollingsworth paths |",
		"|---|---|---|---|",
		"| 02 | a\\|b | r1, r2 | `p1`<br>`p2` |",
		"| 12 | c | – | `p` |",
		"",
	}, "\n")
	if got != want {
		t.Errorf("StagesMarkdown:\n%s\nwant:\n%s", got, want)
	}
}

// TestStagesWithHollingsworthPathsNameIt fails when a stage changes
// Hollingsworth paths but its repositories omit sperano/hollingsworth.
func TestStagesWithHollingsworthPathsNameIt(t *testing.T) {
	for _, s := range Stages() {
		if len(s.Hollingsworth) > 0 && !slices.Contains(s.Repositories, "sperano/hollingsworth") {
			t.Errorf("stage %02d changes %d Hollingsworth paths, repositories %v", s.Number, len(s.Hollingsworth), s.Repositories)
		}
	}
}
