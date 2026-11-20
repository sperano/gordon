package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

type lister struct {
	open, done  []upstream.ListedTask
	comments    map[int64][]event.TaskComment
	commentErrs map[int64]error
	filters     []string
}

func (l *lister) Tasks(_ context.Context, filter string, _ bool) ([]upstream.ListedTask, error) {
	l.filters = append(l.filters, filter)
	if strings.HasPrefix(filter, "done = true") {
		return l.done, nil
	}
	return l.open, nil
}

func (l *lister) TaskComments(_ context.Context, id int64) ([]event.TaskComment, error) {
	return l.comments[id], l.commentErrs[id]
}

func (l *lister) LabelID(_ context.Context, title string) (int64, bool, error) {
	return map[string]int64{"in progress": 1}[title], title == "in progress", nil
}

// inbox is an in-memory Inbox that, like the store, keeps each key once.
type inbox struct {
	keys    map[string]store.Delivery
	known   []string
	handled []string
}

func (i *inbox) Enqueue(_ context.Context, ds []store.Delivery) (int, error) {
	n := 0
	for _, d := range ds {
		if _, ok := i.keys[d.DeliveryKey]; !ok {
			i.keys[d.DeliveryKey], n = d, n+1
		}
	}
	return n, nil
}

func set(keys, of []string) map[string]bool {
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = slices.Contains(of, k)
	}
	return out
}

func (i *inbox) KnownDeliveries(_ context.Context, keys []string) (map[string]bool, error) {
	return set(keys, i.known), nil
}

func (i *inbox) HandledRequests(_ context.Context, keys []string) (map[string]bool, error) {
	return set(keys, i.handled), nil
}

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func reconciler(l *lister, in *inbox) *Reconciler {
	return &Reconciler{Tasks: l, Inbox: in, Config: profile.Defaults(), Lookback: DefaultLookback,
		After: DefaultRedispatchAge, Now: func() time.Time { return now }, Metrics: metrics.New(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func request(id int64, author string, age time.Duration) event.TaskComment {
	return event.TaskComment{ID: id, Author: author, Body: "<p>/gordon go</p>", Created: now.Add(-age)}
}

func queuedTasks(in *inbox) []string {
	var out []string
	for _, d := range in.keys {
		var body struct {
			Event string `json:"event_name"`
			Data  struct {
				Task    struct{ ID int64 }
				Comment *struct{ ID int64 }
			}
		}
		json.Unmarshal(d.Body, &body) //nolint:errcheck // built by store
		if body.Data.Comment != nil {
			out = append(out, "comment/"+itoa(body.Data.Comment.ID))
		} else {
			out = append(out, "task/"+itoa(body.Data.Task.ID))
		}
	}
	slices.Sort(out)
	return out
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestReconcileQueuesCandidatesOnce(t *testing.T) {
	agent := upstream.ListedTask{Task: event.Task{ID: 1, ProjectID: 3, Assignees: []string{"claude-opus"}}}
	human := upstream.ListedTask{Task: event.Task{ID: 2, ProjectID: 3, Assignees: []string{"eric"}}}
	chain := upstream.ListedTask{Task: event.Task{ID: 3, ProjectID: 3, Labels: []string{"chain"}}}
	doneLabelled := upstream.ListedTask{Task: event.Task{ID: 4, ProjectID: 3, Done: true, Labels: []string{"in progress"}}}
	l := &lister{open: []upstream.ListedTask{agent, human, chain}, done: []upstream.ListedTask{doneLabelled}}
	in := &inbox{keys: map[string]store.Delivery{}}
	r := reconciler(l, in)
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queuedTasks(in); !slices.Equal(got, []string{"task/1", "task/3", "task/4"}) {
		t.Fatalf("queued %v, want the agent, chain and done-labelled tasks", got)
	}
	if !slices.Contains(l.filters, "done = true && labels in 1 && project in "+r.projectIDs()) {
		t.Errorf("filters %q lack the done sweep", l.filters)
	}
	n, err := r.pass(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("an unchanged second pass queued %d (%v)", n, err)
	}
	l.open[0].Task.Relations.BlockedBy = []event.Related{{ID: 9, Done: true}}
	if n, _ := r.pass(context.Background()); n != 1 {
		t.Fatalf("a blocker done since queued %d, want the task again", n)
	}
	for _, d := range in.keys {
		if d.Headers[store.SyntheticHeader] != store.SyntheticReconcile {
			t.Errorf("synthetic header %q", d.Headers[store.SyntheticHeader])
		}
	}
}

func TestReconcileRecoversLostRequests(t *testing.T) {
	cancelled := request(14, "eric", time.Hour)
	cancelled.Reactions = map[string][]string{"👎": {"eric"}}
	task := upstream.ListedTask{Task: event.Task{ID: 1, ProjectID: 3, Assignees: []string{"eric"}},
		Comments: []event.TaskComment{
			request(10, "eric", time.Hour),    // lost: recovered
			request(11, "eric", time.Minute),  // the webhook may be on its way
			request(12, "eric", 13*time.Hour), // out of the window
			request(13, "mallory", time.Hour), // not the owner
			cancelled,                         // withdrawn
			request(15, "eric", time.Hour),    // its webhook arrived
			request(16, "eric", time.Hour),    // handled
			{ID: 17, Author: "eric", Body: "<p>thanks</p>", Created: now.Add(-time.Hour)}, // no request
		}}
	cut := upstream.ListedTask{Task: event.Task{ID: 2, ProjectID: 3}, CommentCount: 60}
	l := &lister{open: []upstream.ListedTask{task, cut},
		comments: map[int64][]event.TaskComment{2: {request(20, "eric", time.Hour)}}}
	in := &inbox{keys: map[string]store.Delivery{}, known: []string{"vikunja/comment/15"}, handled: []string{"vikunja/comment/16"}}
	if err := reconciler(l, in).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queuedTasks(in); !slices.Equal(got, []string{"comment/10", "comment/20"}) {
		t.Fatalf("recovered %v, want comments 10 and 20", got)
	}
	for _, d := range in.keys {
		ev, err := event.ParseVikunja(d.Body)
		if err != nil || ev.Actor != "eric" || ev.RequestKey == "" || !strings.Contains(ev.DeliveryKey, "/redispatch/") {
			t.Errorf("recovered delivery %+v (%v): want the owner's request under a redispatch key", ev, err)
		}
	}
}

// TestReconcileRequeuesAChainWhenASubtaskChanges: a subtask's blocker
// getting done changes nothing on its chain parent, so the parent's state
// key covers its open subtasks.
func TestReconcileRequeuesAChainWhenASubtaskChanges(t *testing.T) {
	parent := upstream.ListedTask{Task: event.Task{ID: 3, ProjectID: 3, Labels: []string{"chain"},
		Relations: event.Relations{Subtasks: []event.Related{{ID: 5}}}}}
	sub := upstream.ListedTask{Task: event.Task{ID: 5, ProjectID: 3,
		Relations: event.Relations{BlockedBy: []event.Related{{ID: 9}}}}}
	l := &lister{open: []upstream.ListedTask{parent, sub}}
	r := reconciler(l, &inbox{keys: map[string]store.Delivery{}})
	if n, err := r.pass(context.Background()); err != nil || n != 1 {
		t.Fatalf("first pass queued %d (%v), want the chain parent", n, err)
	}
	l.open[1].Task.Relations.BlockedBy[0].Done = true
	if n, err := r.pass(context.Background()); err != nil || n != 1 {
		t.Fatalf("a subtask's blocker done since queued %d (%v), want the parent again", n, err)
	}
}

// TestReconcileSkipsOnlyATaskWhoseCommentsFail: one task's comment read
// failing still queues the rest of the pass.
func TestReconcileSkipsOnlyATaskWhoseCommentsFail(t *testing.T) {
	broken := upstream.ListedTask{Task: event.Task{ID: 1, ProjectID: 3, Assignees: []string{"claude-opus"}}, CommentCount: 60}
	cut := upstream.ListedTask{Task: event.Task{ID: 2, ProjectID: 3}, CommentCount: 60}
	l := &lister{open: []upstream.ListedTask{broken, cut},
		comments:    map[int64][]event.TaskComment{2: {request(20, "eric", time.Hour)}},
		commentErrs: map[int64]error{1: errors.New("vikunja: unavailable")}}
	in := &inbox{keys: map[string]store.Delivery{}}
	if err := reconciler(l, in).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queuedTasks(in); !slices.Equal(got, []string{"comment/20", "task/1"}) {
		t.Fatalf("queued %v, want task 1's update and task 2's request", got)
	}
}
