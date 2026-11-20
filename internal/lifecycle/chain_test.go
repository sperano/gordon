package lifecycle

import (
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
)

func TestChainNotices(t *testing.T) {
	f := newFake(t)
	f.board()
	parent := Task{ID: 40, ProjectID: 3, Title: "Parent"}
	parent.RelatedTasks = map[string][]struct {
		ID int64 `json:"id"`
	}{"subtask": {{ID: 42}, {ID: 41}}}
	f.put(parent)
	f.put(Task{ID: 41, ProjectID: 3, Index: 1, Title: "First", Done: true, Assignees: []User{{Username: "eric"}, {Username: "claude-opus"}}})
	f.put(Task{ID: 42, ProjectID: 3, Index: 2, Title: "Second", Done: true, Labels: []Label{{ID: 4, Title: "chain"}}})
	f.comments[41] = []Comment{{ID: 1, Author: User{Username: gordon},
		Comment: `<p>MERGED: <a href="https://github.com/o/r/pull/1">#1</a></p>` + notice.Marker(notice.Merged, "")}}
	f.comments[42] = []Comment{{ID: 2, Author: User{Username: gordon},
		Comment: `<ul><li><a href="https://github.com/o/r/pull/2">x</a></li></ul>` + notice.Marker(notice.ChainDone, "")}}
	ex := executor(f, nil)
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(40), notice.ChainStarted, "41")); err != nil {
		t.Fatal(err)
	}
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(40), notice.ChainDone, "")); err != nil {
		t.Fatal(err)
	}
	cs := f.comments[40]
	if len(cs) != 2 {
		t.Fatalf("%d comments on the parent", len(cs))
	}
	if !strings.Contains(cs[0].Comment, `<a href="https://vikunja.example/tasks/41">#1 First</a>. AGENT <code>claude-opus</code>`) {
		t.Errorf("started notice %s", cs[0].Comment)
	}
	if !strings.Contains(cs[1].Comment, `<li><a href="https://github.com/o/r/pull/1">https://github.com/o/r/pull/1</a></li>`+
		`<li><a href="https://github.com/o/r/pull/2">https://github.com/o/r/pull/2</a></li>`) {
		t.Errorf("done notice %s", cs[1].Comment)
	}
	// The legacy chain posted the same notice: notify_once by kind and link.
	again := action.Notice(action.VikunjaTask(40), notice.ChainStarted, "41")
	again.Key += ":other-decision"
	if err := ex.Execute(ctx, again); err != nil || len(f.comments[40]) != 2 {
		t.Fatalf("second chain-started for 41: %v, %d comments", err, len(f.comments[40]))
	}
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(40), notice.ChainStarted, "42")); err != nil || len(f.comments[40]) != 3 {
		t.Fatalf("chain-started for 42: %v, %d comments", err, len(f.comments[40]))
	}
}
