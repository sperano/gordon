package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

const costGen = 41

// costBodies is a cost notice store: the latest text and revision of
// each generation.
type costBodies map[string]struct {
	body string
	rev  int
}

func (c costBodies) CostBody(_ context.Context, name string, inboxID int64) (string, int, error) {
	b, ok := c[fmt.Sprintf("%s@%d", name, inboxID)]
	if !ok {
		return "", 0, store.ErrNoCostNotice
	}
	return b.body, b.rev, nil
}

func (c costBodies) set(body string, rev int) {
	c[fmt.Sprintf("%s@%d", runName, costGen)] = struct {
		body string
		rev  int
	}{body, rev}
}

func costExecutor(t *testing.T) (*fakeVikunja, *Executor, costBodies) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, startRun())
	ex.Policy.VikunjaServiceUser = gordon
	bodies := costBodies{}
	ex.Costs = bodies
	return f, ex, bodies
}

func TestPublishCostPostsThenEditsInPlace(t *testing.T) {
	f, ex, bodies := costExecutor(t)
	bodies.set("<p>rev one</p>", 1)
	first := action.CostNotice(5, runName, costGen, 1)
	if err := ex.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	want1 := "<p>rev one</p>" + notice.CostRevisionMarker(runName, costGen, 1)
	if cs := f.comments[5]; len(cs) != 1 || cs[0].Comment != want1 {
		t.Fatalf("comments %+v, want %q", cs, want1)
	}
	bodies.set("<p>rev two</p>", 2)
	if err := ex.Execute(ctx, action.CostNotice(5, runName, costGen, 2)); err != nil {
		t.Fatal(err)
	}
	want2 := "<p>rev two</p>" + notice.CostRevisionMarker(runName, costGen, 2)
	if cs := f.comments[5]; len(cs) != 1 || cs[0].Comment != want2 {
		t.Fatalf("comments %+v, want the one comment edited to %q", cs, want2)
	}
	// The first revision, retried, finds a later one shown: no write.
	writes := len(f.writes)
	if err := ex.Execute(ctx, first); err != nil || len(f.writes) != writes {
		t.Fatalf("old revision: %v, writes %v", err, f.writes[writes:])
	}
}

// TestPublishCostWritesTheLatestRevision: a revision queued behind a
// later one writes the latest text, and the later one is then applied.
func TestPublishCostWritesTheLatestRevision(t *testing.T) {
	f, ex, bodies := costExecutor(t)
	bodies.set("<p>rev three</p>", 3)
	if err := ex.Execute(ctx, action.CostNotice(5, runName, costGen, 1)); err != nil {
		t.Fatal(err)
	}
	if cs := f.comments[5]; len(cs) != 1 || !strings.Contains(cs[0].Comment, "rev=3 -->") {
		t.Fatalf("comments %+v, want revision 3", cs)
	}
	applied, err := ex.Recover(ctx, action.CostNotice(5, runName, costGen, 3))
	if err != nil || !applied {
		t.Fatalf("Recover(rev 3) = %v, %v", applied, err)
	}
}

// TestAmbiguousCostPostIsRecovered: a post whose answer was lost is
// found by its revision marker; the retry posts nothing.
func TestAmbiguousCostPostIsRecovered(t *testing.T) {
	f, ex, bodies := costExecutor(t)
	bodies.set("<p>rev one</p>", 1)
	a := action.CostNotice(5, runName, costGen, 1)
	f.failNext["PUT "+`^/tasks/(\d+)/comments$`] = true
	if err := ex.Execute(ctx, a); !worker.IsAmbiguous(err) {
		t.Fatalf("lost answer: %v, want ambiguous", err)
	}
	if applied, err := ex.Recover(ctx, a); err != nil || !applied {
		t.Fatalf("Recover = %v, %v, want applied", applied, err)
	}
	if err := ex.Execute(ctx, a); err != nil {
		t.Fatal(err)
	}
	if len(f.comments[5]) != 1 {
		t.Fatalf("comments %+v, want one", f.comments[5])
	}
}

// TestAmbiguousCostEditIsRecovered: an edit whose answer was lost is
// found applied by the revision it shows.
func TestAmbiguousCostEditIsRecovered(t *testing.T) {
	f, ex, bodies := costExecutor(t)
	bodies.set("<p>rev one</p>", 1)
	if err := ex.Execute(ctx, action.CostNotice(5, runName, costGen, 1)); err != nil {
		t.Fatal(err)
	}
	bodies.set("<p>rev two</p>", 2)
	a := action.CostNotice(5, runName, costGen, 2)
	if applied, err := ex.Recover(ctx, a); err != nil || applied {
		t.Fatalf("Recover before the edit = %v, %v, want not applied", applied, err)
	}
	f.failNext["POST "+`^/tasks/(\d+)/comments/(\d+)$`] = true
	if err := ex.Execute(ctx, a); !worker.IsAmbiguous(err) {
		t.Fatalf("lost answer: %v, want ambiguous", err)
	}
	if applied, err := ex.Recover(ctx, a); err != nil || !applied {
		t.Fatalf("Recover = %v, %v, want applied", applied, err)
	}
	if cs := f.comments[5]; len(cs) != 1 || !strings.HasPrefix(cs[0].Comment, "<p>rev two</p>") {
		t.Fatalf("comments %+v", cs)
	}
}

// TestCostMarkerOnlyCountsOnGordonsComments: a copied marker in someone
// else's comment, or another generation's notice, is not this notice.
func TestCostMarkerOnlyCountsOnGordonsComments(t *testing.T) {
	f, ex, bodies := costExecutor(t)
	bodies.set("<p>rev one</p>", 1)
	f.comments[5] = []Comment{
		{ID: 1, Comment: notice.CostRevisionMarker(runName, costGen, 9), Author: User{ID: 2, Username: "eric"}},
		{ID: 2, Comment: notice.CostRevisionMarker(runName, costGen+1, 9), Author: User{ID: 80, Username: gordon}},
	}
	if err := ex.Execute(ctx, action.CostNotice(5, runName, costGen, 1)); err != nil {
		t.Fatal(err)
	}
	cs := f.comments[5]
	if len(cs) != 3 || cs[2].Comment != "<p>rev one</p>"+notice.CostRevisionMarker(runName, costGen, 1) ||
		cs[1].Comment != notice.CostRevisionMarker(runName, costGen+1, 9) {
		t.Fatalf("comments %+v, want a new notice and the others untouched", cs)
	}
}

func TestPublishCostRefusals(t *testing.T) {
	_, ex, _ := costExecutor(t)
	for name, a := range map[string]action.Action{
		"no stored text": action.CostNotice(5, runName, costGen, 1),
		"bad detail":     {Key: "k", Kind: action.PublishCost, Target: action.VikunjaTask(5), Value: runName, Detail: "x"},
		"no revision":    {Key: "k", Kind: action.PublishCost, Target: action.VikunjaTask(5), Value: runName, Detail: "41:0"},
		"no run":         {Key: "k", Kind: action.PublishCost, Target: action.VikunjaTask(5), Detail: "41:1"},
	} {
		if err := ex.Execute(ctx, a); !worker.IsPermanent(err) {
			t.Errorf("%s: %v, want permanent", name, err)
		}
	}
	ex.Costs = nil
	if err := ex.Execute(ctx, action.CostNotice(5, runName, costGen, 1)); !worker.IsPermanent(err) {
		t.Errorf("no store: %v, want permanent", err)
	}
}
