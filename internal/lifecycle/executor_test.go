package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

var ctx = context.Background()

const runName = "vikunja-hollingsworth-claude-opus-5"

func startRun() runs {
	return runs{runName: {Name: runName, Family: run.VikunjaImplement, Model: "claude-opus-5-5[1m]", Effort: "medium",
		TaskUID: "u-1", Identity: run.Identity{Family: run.VikunjaImplement, TaskID: 5}}}
}

func labels(t Task) []string {
	var out []string
	for _, l := range t.Labels {
		out = append(out, l.Title)
	}
	return out
}

// TestAmbiguousNoticeIsRecovered: a notice whose post landed but whose
// answer was lost is found by its marker; the retry posts nothing.
func TestAmbiguousNoticeIsRecovered(t *testing.T) {
	f := newFake(t)
	f.board()
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, startRun())
	a := action.Notice(action.VikunjaTask(5), notice.Start, runName)
	f.failNext["PUT "+`^/tasks/(\d+)/comments$`] = true
	err := ex.Execute(ctx, a)
	if !errors.As(err, new(interface{ Unwrap() error })) || !strings.HasPrefix(err.Error(), "ambiguous:") {
		t.Fatalf("lost answer: %v, want ambiguous", err)
	}
	applied, err := ex.Recover(ctx, a)
	if err != nil || !applied {
		t.Fatalf("Recover = %v, %v, want applied", applied, err)
	}
	if err := ex.Execute(ctx, a); err != nil {
		t.Fatal(err)
	}
	cs := f.comments[5]
	if len(cs) != 1 || !strings.Contains(cs[0].Comment, notice.ActionMarker(a.Key)) ||
		!strings.Contains(cs[0].Comment, "<!-- kelos:start task="+runName+" -->") ||
		!strings.Contains(cs[0].Comment, "<code>claude-opus-5-5[1m]</code>") {
		t.Fatalf("comments %+v, want the one start notice", cs)
	}
}

// heldLock is a Locker recording whether it is held and the deadline fn
// ran under.
type heldLock struct {
	held     bool
	deadline time.Time
	bounded  bool
}

func (l *heldLock) WithTargetLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	l.deadline, l.bounded = ctx.Deadline()
	l.held = true
	defer func() { l.held = false }()
	return fn(ctx)
}

// lockFreeRuns refuses a run read made while the target lock is held.
type lockFreeRuns struct {
	runs
	lock *heldLock
}

func (r lockFreeRuns) RunInfo(ctx context.Context, name string) (store.RunInfo, error) {
	if r.lock.held {
		return store.RunInfo{}, errors.New("run read under the target lock")
	}
	return r.runs.RunInfo(ctx, name)
}

// TestNoticeReadsRunsOutsideTheLock: the start notice reads its run
// before taking the target lock (whose transaction holds a pool
// connection), and posts under a deadline.
func TestNoticeReadsRunsOutsideTheLock(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	lock := &heldLock{}
	ex := executor(f, nil)
	ex.Runs, ex.Lock = lockFreeRuns{runs: startRun(), lock: lock}, lock
	before := time.Now()
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(5), notice.Start, runName)); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(f.comments[5]) != 1 {
		t.Fatalf("%d comments, want one notice", len(f.comments[5]))
	}
	// The deadline is set between before and after (monotonic readings).
	if !lock.bounded {
		t.Fatal("lock context has no deadline, want one")
	}
	if lo, hi := before.Add(noticeLockTimeout), after.Add(noticeLockTimeout); lock.deadline.Before(lo) || lock.deadline.After(hi) {
		t.Fatalf("lock deadline %v, want between %v and %v", lock.deadline, lo, hi)
	}
}

// postOnLock is a Locker that posts the notice's twin (another worker
// winning the race) before running fn.
type postOnLock struct {
	f    *fakeVikunja
	body string
}

func (l postOnLock) WithTargetLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	l.f.mu.Lock()
	l.f.comments[5] = append(l.f.comments[5], Comment{ID: 99, Author: User{Username: gordon}, Comment: l.body})
	l.f.mu.Unlock()
	return fn(ctx)
}

// TestNoticeChecksAgainUnderTheLock: a notice another worker posted
// between the first check and the lock is not posted twice.
func TestNoticeChecksAgainUnderTheLock(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, startRun())
	a := action.Notice(action.VikunjaTask(5), notice.Start, runName)
	ex.Lock = postOnLock{f: f, body: notice.ActionMarker(a.Key)}
	if err := ex.Execute(ctx, a); err != nil || len(f.comments[5]) != 1 {
		t.Fatalf("Execute = %v, %d comments, want only the other worker's", err, len(f.comments[5]))
	}
}

// TestForgedMarkerDoesNotSuppress: the same marker in the owner's comment
// is no notice of Gordon's.
func TestForgedMarkerDoesNotSuppress(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, startRun())
	a := action.Notice(action.VikunjaTask(5), notice.Start, runName)
	f.comments[5] = []Comment{{ID: 1, Author: User{Username: "eric"},
		Comment: notice.ActionMarker(a.Key) + notice.Marker(notice.Start, runName)}}
	if applied, err := ex.Recover(ctx, a); err != nil || applied {
		t.Fatalf("Recover = %v, %v, want not applied", applied, err)
	}
	if err := ex.Execute(ctx, a); err != nil || len(f.comments[5]) != 2 {
		t.Fatalf("Execute = %v, %d comments, want Gordon's notice posted", err, len(f.comments[5]))
	}
}

// TestLegacyNoticeCounts: the start notice the legacy init container
// posted for the same Task (as Gordon's user) is not posted again.
func TestLegacyNoticeCounts(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, startRun())
	f.comments[5] = []Comment{{ID: 1, Author: User{Username: gordon}, Comment: "<p>x</p>" + notice.Marker(notice.Start, runName)}}
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(5), notice.Start, runName)); err != nil || len(f.comments[5]) != 1 {
		t.Fatalf("Execute = %v, %d comments, want none added", err, len(f.comments[5]))
	}
}

// TestDiscussStartReplacesFallback: the discuss follow-up's start notice
// edits its request's fallback line instead of adding a comment.
func TestDiscussStartReplacesFallback(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	name := "vikunja-fu-hollingsworth-5-c9"
	ex := executor(f, runs{name: {Name: name, Family: run.VikunjaFollowUpDiscuss,
		Identity: run.Identity{Family: run.VikunjaFollowUpDiscuss, TaskID: 5, CommentID: 9}}})
	fallback := action.Notice(action.VikunjaTask(5), notice.DiscussStart, "9")
	if err := ex.Execute(ctx, fallback); err != nil {
		t.Fatal(err)
	}
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(5), notice.Start, name)); err != nil {
		t.Fatal(err)
	}
	cs := f.comments[5]
	if len(cs) != 1 || !strings.Contains(cs[0].Comment, "<!-- kelos:start task="+name+" -->") ||
		!strings.Contains(cs[0].Comment, notice.DiscussMarker(9)) {
		t.Fatalf("comments %+v, want the fallback replaced by the start notice", cs)
	}
	// A late fallback after the start notice finds its marker there.
	if err := ex.Execute(ctx, fallback); err != nil || len(f.comments[5]) != 1 {
		t.Fatalf("late fallback: %v, %d comments", err, len(f.comments[5]))
	}
}

// TestClaimLabelSkipsADraggedCard: a claim's in-progress label is not
// added once the card went back to the backlog (or the task is done).
func TestClaimLabelSkipsADraggedCard(t *testing.T) {
	f := newFake(t)
	f.board()
	f.put(Task{ID: 5, ProjectID: 3, Buckets: []Bucket{f.buckets[1][0]}})
	ex := executor(f, nil)
	claim := action.New(action.AddLabel, action.VikunjaTask(5), "in progress")
	claim.Detail = route.DetailClaim
	if err := ex.Execute(ctx, claim); err != nil || len(labels(f.get(5))) != 0 {
		t.Fatalf("claim on a dragged card: %v, labels %v", err, labels(f.get(5)))
	}
	f.put(Task{ID: 5, ProjectID: 3, Buckets: []Bucket{f.buckets[1][1]}})
	if err := ex.Execute(ctx, claim); err != nil || !slices.Equal(labels(f.get(5)), []string{"in progress"}) {
		t.Fatalf("claim in Doing: %v, labels %v", err, labels(f.get(5)))
	}
	if err := ex.Execute(ctx, claim); err != nil || len(f.get(5).Labels) != 1 {
		t.Fatalf("repeated claim: %v, labels %v", err, labels(f.get(5)))
	}
	f.put(Task{ID: 5, ProjectID: 3, Done: true, Buckets: []Bucket{f.buckets[1][1]}})
	if err := ex.Execute(ctx, claim); err != nil || len(labels(f.get(5))) != 0 {
		t.Fatalf("claim on a done task in Doing: %v, labels %v", err, labels(f.get(5)))
	}
}

func TestLabelConditions(t *testing.T) {
	f := newFake(t)
	f.board()
	f.put(Task{ID: 5, ProjectID: 3, Labels: []Label{{ID: 1, Title: "in progress"}}})
	ex := executor(f, nil)
	ifDone := action.New(action.RemoveLabel, action.VikunjaTask(5), "in progress")
	ifDone.Detail = route.DetailIfDone
	if err := ex.Execute(ctx, ifDone); err != nil || len(f.get(5).Labels) != 1 {
		t.Fatalf("removal from a reopened task: %v, labels %v", err, labels(f.get(5)))
	}
	f.put(Task{ID: 5, ProjectID: 3, Done: true, Labels: []Label{{ID: 1, Title: "in progress"}, {ID: 9, Title: "in progress"}}})
	if err := ex.Execute(ctx, ifDone); err != nil || len(f.get(5).Labels) != 0 {
		t.Fatalf("removal from a done task: %v, labels %v", err, labels(f.get(5)))
	}
	unlessDone := action.New(action.AddLabel, action.VikunjaTask(5), "action-needed")
	unlessDone.Detail = route.DetailUnlessDone
	if err := ex.Execute(ctx, unlessDone); err != nil || len(f.get(5).Labels) != 0 {
		t.Fatalf("owner inbox label on a done task: %v, labels %v", err, labels(f.get(5)))
	}
	missing := action.New(action.AddLabel, action.VikunjaTask(5), "no-such-label")
	if err := ex.Execute(ctx, missing); !worker.IsPermanent(err) {
		t.Fatalf("missing label: %v, want permanent", err)
	}
}

// TestMoveInEveryManualView: the move covers each manual Kanban view,
// skips the views already right, and a title no view has is permanent.
func TestMoveInEveryManualView(t *testing.T) {
	f := newFake(t)
	f.board()
	f.put(Task{ID: 5, ProjectID: 3, Buckets: []Bucket{f.buckets[1][1], f.buckets[3][0]}})
	ex := executor(f, nil)
	a := action.New(action.MoveBucket, action.VikunjaTask(5), "Doing")
	if applied, err := ex.Recover(ctx, a); err != nil || applied {
		t.Fatalf("a card in To-Do in one view counts as moved: applied %v, err %v", applied, err)
	}
	before := len(f.writes)
	if err := ex.Execute(ctx, a); err != nil || f.inBucket(5, 1) != "Doing" || f.inBucket(5, 3) != "Doing" {
		t.Fatalf("move: %v, buckets %+v", err, f.get(5).Buckets)
	}
	if moves := len(f.writes) - before; moves != 1 {
		t.Fatalf("%d moves, want 1 (view 1 was in Doing already)", moves)
	}
	if err := ex.Execute(ctx, action.New(action.MoveBucket, action.VikunjaTask(5), "Nowhere")); !worker.IsPermanent(err) {
		t.Fatalf("unknown bucket: %v, want permanent", err)
	}
}

func TestAssign(t *testing.T) {
	f := newFake(t)
	f.board()
	f.put(Task{ID: 5, ProjectID: 3})
	ex := executor(f, nil)
	if err := ex.Execute(ctx, action.New(action.Assign, action.VikunjaTask(5), "claude-opus")); err != nil ||
		len(f.get(5).Assignees) != 1 || f.get(5).Assignees[0].Username != "claude-opus" {
		t.Fatalf("assign: %v, %+v", err, f.get(5).Assignees)
	}
	f.put(Task{ID: 6, ProjectID: 3, Assignees: []User{{Username: "gpt-sol"}}})
	before := len(f.writes)
	if err := ex.Execute(ctx, action.New(action.Assign, action.VikunjaTask(6), "claude-opus")); err != nil || len(f.writes) != before {
		t.Fatalf("a task with its own agent was reassigned: %v", err)
	}
	f.put(Task{ID: 7, ProjectID: 3})
	if err := ex.Execute(ctx, action.New(action.Assign, action.VikunjaTask(7), "nobody")); !worker.IsPermanent(err) {
		t.Fatalf("unknown user: %v, want permanent", err)
	}
}

func TestAssignIfUnassigned(t *testing.T) {
	f := newFake(t)
	f.board()
	ex := executor(f, nil)
	a := action.New(action.AssignIfUnassigned, action.VikunjaTask(5), "claude-opus")
	a.Detail = "3"
	f.put(Task{ID: 5, ProjectID: 3})
	if applied, err := ex.Recover(ctx, a); err != nil || applied {
		t.Fatalf("recover unassigned task: applied %v, err %v", applied, err)
	}
	if err := ex.Execute(ctx, a); err != nil || len(f.get(5).Assignees) != 1 || f.get(5).Assignees[0].Username != "claude-opus" {
		t.Fatalf("assign-if-unassigned: %v, %+v", err, f.get(5).Assignees)
	}
	if applied, err := ex.Recover(ctx, a); err != nil || !applied {
		t.Fatalf("recover assigned task: applied %v, err %v", applied, err)
	}
	for _, tc := range []struct {
		name string
		task Task
	}{
		{name: "assigned to another user", task: Task{ID: 5, ProjectID: 3, Assignees: []User{{Username: "eric"}}}},
		{name: "moved to another project", task: Task{ID: 5, ProjectID: 4}},
		{name: "no-agent label", task: Task{ID: 5, ProjectID: 3, Labels: []Label{{Title: "no-agent"}}}},
		{name: "chain label", task: Task{ID: 5, ProjectID: 3, Labels: []Label{{Title: "chain"}}}},
		{name: "discuss label", task: Task{ID: 5, ProjectID: 3, Labels: []Label{{Title: "discuss"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.put(tc.task)
			before := len(f.writes)
			if applied, err := ex.Recover(ctx, a); err != nil || !applied || len(f.writes) != before {
				t.Fatalf("recover: applied %v, err %v, writes %d, want %d", applied, err, len(f.writes), before)
			}
			if err := ex.Execute(ctx, a); err != nil || len(f.writes) != before {
				t.Fatalf("execute: err %v, writes %d, want %d", err, len(f.writes), before)
			}
		})
	}
}

// TestSyncLabelKeepsProvenance: the subtask gets exactly the parent's
// auto-merge label made by the trusted creator, and loses a lookalike.
func TestSyncLabelKeepsProvenance(t *testing.T) {
	f := newFake(t)
	f.board()
	f.put(Task{ID: 1, ProjectID: 3, Labels: []Label{f.labels[4]}})
	f.put(Task{ID: 2, ProjectID: 3, Labels: []Label{f.labels[5]}})
	ex := executor(f, nil)
	a := action.New(action.SyncLabel, action.VikunjaTask(2), "auto-merge")
	a.Detail = "1"
	if err := ex.Execute(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got := f.get(2).Labels; len(got) != 1 || got[0].ID != 5 {
		t.Fatalf("subtask labels %+v, want only eric's auto-merge (5)", got)
	}
	f.put(Task{ID: 1, ProjectID: 3})
	if err := ex.Execute(ctx, a); err != nil || len(f.get(2).Labels) != 0 {
		t.Fatalf("parent without the label: %v, subtask %+v", err, f.get(2).Labels)
	}
}

func TestReactOnce(t *testing.T) {
	f := newFake(t)
	ex := executor(f, nil)
	a := action.New(action.React, action.VikunjaComment(9), "eyes")
	for range 2 {
		if err := ex.Execute(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reactions[9]["👀"]; len(got) != 1 {
		t.Fatalf("reactions %+v, want one 👀", f.reactions[9])
	}
	if err := ex.Execute(ctx, action.New(action.React, action.VikunjaTask(9), "eyes")); !worker.IsPermanent(err) {
		t.Fatalf("react on a task: %v, want permanent", err)
	}
}

func TestMarkDone(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3, Title: "Ship it", Labels: []Label{{ID: 1, Title: "in progress"}}})
	ex := executor(f, nil)
	if err := ex.Execute(ctx, action.New(action.MarkDone, action.VikunjaTask(5), "")); err != nil {
		t.Fatal(err)
	}
	// The fake replaces the task with the body, as Vikunja does: the
	// title and labels survive only if MarkDone sent the whole task back.
	if got := f.get(5); !got.Done || got.Title != "Ship it" || got.ProjectID != 3 ||
		!slices.Equal(labels(got), []string{"in progress"}) {
		t.Fatalf("after mark done: %+v, want the task kept with done set", got)
	}
}

// TestUnansweredAndStop renders the notices that read the task now.
func TestUnansweredAndStop(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3, Labels: []Label{{ID: 1, Title: "in progress"}},
		Assignees: []User{{Username: "claude-opus"}, {Username: "gpt-sol"}}})
	ex := executor(f, nil)
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(5), notice.Unanswered, "9:multiple-agent-assignees")); err != nil {
		t.Fatal(err)
	}
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(5), notice.Stop, "a,b")); err != nil {
		t.Fatal(err)
	}
	cs := f.comments[5]
	if len(cs) != 2 || !strings.Contains(cs[0].Comment, `<a href="#comment-9">`) ||
		!strings.Contains(cs[0].Comment, "(<code>claude-opus</code>, <code>gpt-sol</code>)") ||
		!strings.Contains(cs[1].Comment, "<code>a</code>, <code>b</code>") ||
		!strings.Contains(cs[1].Comment, "LABEL COULD NOT BE REMOVED") {
		t.Fatalf("comments %+v", cs)
	}
	if err := ex.Execute(ctx, action.Notice(action.VikunjaTask(5), notice.Unanswered, "bad")); !worker.IsPermanent(err) {
		t.Fatalf("malformed detail: %v, want permanent", err)
	}
}

func TestStartNoticeOfUnknownRun(t *testing.T) {
	f := newFake(t)
	f.put(Task{ID: 5, ProjectID: 3})
	err := executor(f, runs{}).Execute(ctx, action.Notice(action.VikunjaTask(5), notice.Start, "gone"))
	if !errors.Is(err, store.ErrUnknownRun) {
		t.Fatalf("unknown run: %v", err)
	}
}
