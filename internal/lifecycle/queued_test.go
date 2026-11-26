package lifecycle

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/worker"
)

// queueLog records the actions queued per inbox row; HoldNotices
// counts its hold notices as the store does, one per key.
type queueLog struct {
	calls     []queuedCall
	err       error
	noticeErr error
}

type queuedCall struct {
	inbox   int64
	family  run.Family
	project string
	actions []action.Action
}

func (q *queueLog) QueueRunActions(_ context.Context, inboxID int64, family run.Family, project string, actions []action.Action) error {
	q.calls = append(q.calls, queuedCall{inboxID, family, project, actions})
	return q.err
}

func (q *queueLog) HoldNotices(_ context.Context, inboxID int64, name, kind string) (int, string, error) {
	keys, last := map[string]bool{}, ""
	for _, c := range q.calls {
		for _, a := range c.actions {
			if c.inbox == inboxID && a.Kind == action.PostNotice && a.Value == kind &&
				strings.HasPrefix(a.Detail, name+":") && !keys[a.Key] {
				keys[a.Key], last = true, a.Detail
			}
		}
	}
	return len(keys), last, q.noticeErr
}

func queuedPolicy() profile.Policy {
	p := profile.Defaults().Policy
	p.QueuedLabel = queuedLabel
	return p
}

var limitHold = notice.Hold{Run: runName, Reason: notice.HoldLimit, Variant: "claude-opus", Running: 2, Limit: 2}

func TestHoldsQueueTheHoldOnItsRun(t *testing.T) {
	q := &queueLog{}
	h := &Holds{Store: q, Policy: queuedPolicy()}
	r := store.SubmitRun{Name: runName, InboxID: 9, Family: run.VikunjaFollowUp, Project: "hollingsworth", TaskID: 5}
	if err := h.Held(ctx, r, limitHold); err != nil {
		t.Fatal(err)
	}
	want := []queuedCall{{9, run.VikunjaQueued, "hollingsworth", route.HoldActions(queuedPolicy(), 5, limitHold, 0)}}
	if !reflect.DeepEqual(q.calls, want) {
		t.Fatalf("queued %+v, want %+v", q.calls, want)
	}
	q.err = errors.New("store down")
	if err := h.Held(ctx, r, limitHold); !errors.Is(err, q.err) {
		t.Fatalf("a failed queue: %v, want the store's error", err)
	}
	q.noticeErr = errors.New("read refused")
	if err := h.Held(ctx, r, limitHold); !errors.Is(err, q.noticeErr) || len(q.calls) != 2 {
		t.Fatalf("a failed notice count: %v after %d queues, want the store's error and no queue", err, len(q.calls))
	}
}

var branchHold = notice.Hold{Run: runName, Reason: notice.HoldBranch, Branch: "kelos/vikunja-5", Holder: "other-1"}

// TestHoldsNumberEachChangedHold: the same hold again keeps its notice's
// seq; a new reason, back to an earlier one included, takes the next.
func TestHoldsNumberEachChangedHold(t *testing.T) {
	q := &queueLog{}
	h := &Holds{Store: q, Policy: queuedPolicy()}
	r := store.SubmitRun{Name: runName, InboxID: 9, Family: run.VikunjaImplement, Project: "hollingsworth", TaskID: 5}
	busier := limitHold
	busier.Running = 3
	steps := []struct {
		hold notice.Hold
		seq  int
	}{{limitHold, 0}, {busier, 0}, {branchHold, 1}, {branchHold, 1}, {limitHold, 2}}
	for i, s := range steps {
		if err := h.Held(ctx, r, s.hold); err != nil {
			t.Fatal(err)
		}
		if got, want := q.calls[i].actions, route.HoldActions(queuedPolicy(), 5, s.hold, s.seq); !reflect.DeepEqual(got, want) {
			t.Fatalf("deferral %d queued\n%+v\nwant\n%+v", i, got, want)
		}
	}
}

// TestChangedHoldPostsANewNotice: limit, branch, then limit again post
// three hold notices, the newest saying why the run waits now; the same
// hold again posts none.
func TestChangedHoldPostsANewNotice(t *testing.T) {
	q := &queueLog{}
	h := &Holds{Store: q, Policy: queuedPolicy()}
	ex, f := heldExecutor(t, store.RunSubmitting)
	r := store.SubmitRun{Name: runName, InboxID: 9, Family: run.VikunjaImplement, TaskID: 5}
	var notes []action.Action
	for _, hold := range []notice.Hold{limitHold, branchHold, branchHold, limitHold} {
		if err := h.Held(ctx, r, hold); err != nil {
			t.Fatal(err)
		}
		note := q.calls[len(q.calls)-1].actions[1]
		if err := ex.Execute(ctx, note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	want := []string{
		notice.QueuedText(limitHold, queuedLabel) + notice.ActionMarker(notes[0].Key),
		notice.QueuedText(branchHold, queuedLabel) + notice.ActionMarker(notes[1].Key),
		notice.QueuedText(limitHold, queuedLabel) + notice.ActionMarker(notes[3].Key),
	}
	var got []string
	for _, c := range f.comments[5] {
		got = append(got, c.Comment)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("hold notices\n%q\nwant\n%q", got, want)
	}
	if notes[2] != notes[1] || notes[3].Key == notes[0].Key {
		t.Fatalf("notice keys %q, want the repeated branch hold's the same and the limit's again a new one", notes)
	}
}

func TestHoldsSkipRunsOfNoTask(t *testing.T) {
	q := &queueLog{}
	runs := []store.SubmitRun{
		{Name: "review", InboxID: 9, Family: run.PRReview, TaskID: 5},
		{Name: runName, InboxID: 9, Family: run.VikunjaImplement},
	}
	for _, r := range runs {
		if err := (&Holds{Store: q, Policy: queuedPolicy()}).Held(ctx, r, limitHold); err != nil {
			t.Fatal(err)
		}
		if err := (&Holds{Store: q, Policy: queuedPolicy()}).Unsubmitted(ctx, store.ObservedRun{Name: r.Name,
			InboxID: r.InboxID, Family: r.Family, TaskID: r.TaskID}); err != nil {
			t.Fatal(err)
		}
	}
	implement := store.SubmitRun{Name: runName, InboxID: 9, Family: run.VikunjaImplement, TaskID: 5}
	off := &Holds{Store: q, Policy: profile.Defaults().Policy}
	if err := off.Held(ctx, implement, limitHold); err != nil {
		t.Fatal(err)
	}
	if err := off.Unsubmitted(ctx, store.ObservedRun{Name: runName, InboxID: 9, Family: run.VikunjaImplement, TaskID: 5}); err != nil {
		t.Fatal(err)
	}
	if len(q.calls) != 0 {
		t.Fatalf("queued %+v for runs of no task or with no queued label, want nothing", q.calls)
	}
}

func TestHoldsReleaseAnUnsubmittedRun(t *testing.T) {
	q := &queueLog{}
	h := &Holds{Store: q, Policy: queuedPolicy()}
	if err := h.Unsubmitted(ctx, store.ObservedRun{Name: runName, InboxID: 9, Family: run.VikunjaDiscuss,
		Project: "hollingsworth", TaskID: 5}); err != nil {
		t.Fatal(err)
	}
	release, _ := route.ReleaseAction(queuedPolicy(), 5, runName)
	want := []queuedCall{{9, run.VikunjaQueued, "hollingsworth", []action.Action{release}}}
	if !reflect.DeepEqual(q.calls, want) {
		t.Fatalf("queued %+v, want %+v", q.calls, want)
	}
}

// heldExecutor is the Vikunja executor with the queued label on, task 5
// open and the run in state.
func heldExecutor(t *testing.T, state string) (*Executor, *fakeVikunja) {
	f := newFake(t)
	f.board()
	f.labels = append(f.labels, Label{ID: queuedLabelID, Title: queuedLabel})
	f.put(Task{ID: 5, ProjectID: 3})
	rs := runs{}
	if state != "" {
		rs[runName] = store.RunInfo{Name: runName, Family: run.VikunjaImplement, State: state}
	}
	ex := executor(f, rs)
	ex.Policy.QueuedLabel = queuedLabel
	return ex, f
}

func TestHeldLabelOnlyWhileTheRunIsHeld(t *testing.T) {
	label := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[0]
	cases := map[string]struct {
		state string
		done  bool
		want  []string
	}{
		"held":        {store.RunSubmitting, false, []string{queuedLabel}},
		"started":     {"active", false, nil},
		"stopped":     {store.RunFinished, false, nil},
		"unknown run": {"", false, nil},
		"done task":   {store.RunSubmitting, true, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ex, f := heldExecutor(t, c.state)
			f.tasks[5].Done = c.done
			if err := ex.Execute(ctx, label); err != nil {
				t.Fatal(err)
			}
			if got := labels(f.get(5)); !slices.Equal(got, c.want) {
				t.Fatalf("labels %v, want %v", got, c.want)
			}
		})
	}
}

// startingRuns is runs whose run reads as submitting once, then as
// started: its Task created while the queued label was being added.
type startingRuns struct {
	runs
	reads int
}

func (r *startingRuns) RunInfo(ctx context.Context, name string) (store.RunInfo, error) {
	i, err := r.runs.RunInfo(ctx, name)
	if r.reads++; r.reads > 1 {
		i.State = "active"
	}
	return i, err
}

// TestHeldLabelTakenOffWhenTheRunStartsMeanwhile: a run started while
// its queued label was added loses the label again (its removal may have
// run first), unless another run of the task is still held.
func TestHeldLabelTakenOffWhenTheRunStartsMeanwhile(t *testing.T) {
	label := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[0]
	other := store.RunInfo{Name: "vikunja-fu-hollingsworth-5-c9", Family: run.VikunjaFollowUp,
		Identity: run.Identity{TaskID: 5}, State: store.RunSubmitting}
	for name, c := range map[string]struct {
		others runs
		want   []string
	}{
		"alone":              {runs{}, nil},
		"another run held":   {runs{other.Name: other}, []string{queuedLabel}},
		"another task's run": {runs{other.Name: {Name: other.Name, Family: run.VikunjaFollowUp, Identity: run.Identity{TaskID: 6}, State: store.RunSubmitting}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			ex, f := heldExecutor(t, store.RunSubmitting)
			rs := ex.Runs.(runs)
			maps.Copy(rs, c.others)
			ex.Runs = &startingRuns{runs: rs}
			if err := ex.Execute(ctx, label); err != nil {
				t.Fatal(err)
			}
			if got := labels(f.get(5)); !slices.Equal(got, c.want) {
				t.Fatalf("labels %v, want %v", got, c.want)
			}
		})
	}
}

// TestUnheldRemovalWaitsForTheOtherHeldRun: a held run's removal leaves
// the queued label while another run of the task is still held.
func TestUnheldRemovalWaitsForTheOtherHeldRun(t *testing.T) {
	removal := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[2]
	release, _ := route.ReleaseAction(queuedPolicy(), 5, runName)
	other := "vikunja-fu-hollingsworth-5-c9"
	for name, c := range map[string]struct {
		state string
		want  []string
	}{
		"other held":    {store.RunSubmitting, []string{queuedLabel}},
		"other started": {"active", nil},
		"no other run":  {"", nil},
	} {
		for _, a := range []action.Action{removal, release} {
			t.Run(name+" "+a.Key, func(t *testing.T) {
				ex, f := heldExecutor(t, "active")
				f.tasks[5].Labels = []Label{{ID: queuedLabelID, Title: queuedLabel}}
				if c.state != "" {
					ex.Runs.(runs)[other] = store.RunInfo{Name: other, Family: run.VikunjaFollowUp,
						Identity: run.Identity{TaskID: 5}, State: c.state}
				}
				if err := ex.Execute(ctx, a); err != nil {
					t.Fatal(err)
				}
				if got := labels(f.get(5)); !slices.Equal(got, c.want) {
					t.Fatalf("labels %v, want %v", got, c.want)
				}
			})
		}
	}
}

func TestHeldNoticeOnlyWhileTheRunIsHeld(t *testing.T) {
	note := route.HoldActions(queuedPolicy(), 5, limitHold, 0)[1]
	ex, f := heldExecutor(t, store.RunSubmitting)
	if err := ex.Execute(ctx, note); err != nil {
		t.Fatal(err)
	}
	want := notice.QueuedText(limitHold, queuedLabel) + notice.ActionMarker(note.Key)
	if cs := f.comments[5]; len(cs) != 1 || cs[0].Comment != want {
		t.Fatalf("comments %+v, want the one hold notice %q", cs, want)
	}
	if err := ex.Execute(ctx, note); err != nil || len(f.comments[5]) != 1 {
		t.Fatalf("a repeated hold notice: %v, %d comments; want still one", err, len(f.comments[5]))
	}
	started, f := heldExecutor(t, "active")
	if err := started.Execute(ctx, note); err != nil || len(f.comments[5]) != 0 {
		t.Fatalf("the notice of a started run: %v, %d comments; want none", err, len(f.comments[5]))
	}
	bad := note
	bad.Key, bad.Detail = note.Key+":bad", "nonsense"
	if err := ex.Execute(ctx, bad); !worker.IsPermanent(err) || !strings.Contains(err.Error(), "bad hold detail") {
		t.Fatalf("a bad hold detail: %v, want a permanent refusal", err)
	}
}
