package ghlife

import (
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/worker"
)

// TestNoticeListingAtThePageCap: a PR with exactly as many comments as
// one listing reads is checked and gets its notice; one with more cannot
// be checked, which no retry changes.
func TestNoticeListingAtThePageCap(t *testing.T) {
	f := newFake(t)
	for range maxCommentPages * commentsPerPage {
		f.addComment(appBot, "an earlier notice")
	}
	e, _ := executor(f, runs())
	if err := e.Execute(ctx, start()); err != nil {
		t.Fatalf("exactly at the cap: %v, want the notice posted", err)
	}
	if want := []string{"comment"}; !slices.Equal(f.writes, want) {
		t.Fatalf("writes %v, want %v", f.writes, want)
	}
	f.addComment(appBot, "one more")
	_, err := e.Recover(ctx, start())
	if !worker.IsPermanent(err) || err.Error() != "permanent: comments of sperano/vigie#4: over 10000, the notice cannot be checked" {
		t.Errorf("over the cap: %v, want the permanent over-the-cap refusal", err)
	}
}
