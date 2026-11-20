package worker

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/upstream"
)

func chainUpdate(id int64) event.Event {
	return event.Event{Kind: event.VikunjaTaskUpdated, RequestKey: "vikunja/task/1",
		Vikunja: &event.Vikunja{Task: event.Task{ID: id, ProjectID: 3}}}
}

// TestStateReaderReadsAChainsSubtasks: a task carrying the chain label is
// read with its subtasks and comments; any other task alone; a failed
// subtask read leaves the whole state unread (the router retries).
func TestStateReaderReadsAChainsSubtasks(t *testing.T) {
	parent := event.Task{ID: 1, ProjectID: 3, Labels: []string{"chain"},
		Relations: event.Relations{Subtasks: []event.Related{{ID: 2}, {ID: 3}}}}
	tasks := &fakeTasks{tasks: map[int64]event.Task{1: parent, 2: {ID: 2}, 3: {ID: 3}, 4: {ID: 4, ProjectID: 3}},
		comments: map[int64][]event.TaskComment{1: {{ID: 9, Author: "gordon"}}}}
	sr := StateReader{Config: profile.Defaults(), Tasks: tasks}
	st, err := sr.Read(context.Background(), chainUpdate(1), replay.Ledger{})
	if err != nil || st.Task == nil || len(st.Subtasks) != 2 || st.Subtasks[1].ID != 3 || len(st.Comments) != 1 {
		t.Fatalf("chain state %+v (%v)", st, err)
	}
	tasks.reads = 0
	if st, err := sr.Read(context.Background(), chainUpdate(4), replay.Ledger{}); err != nil || st.Subtasks != nil || tasks.reads != 1 {
		t.Fatalf("a plain task read %d times, subtasks %v (%v)", tasks.reads, st.Subtasks, err)
	}
	delete(tasks.tasks, 3)
	st, err = sr.Read(context.Background(), chainUpdate(1), replay.Ledger{})
	var ue *upstream.Error
	if st.Task != nil || !errors.As(err, &ue) || ue.Class != upstream.NotFound {
		t.Fatalf("missing subtask: task %v, err %v, want no state and the not-found read", st.Task, err)
	}
}

type recordingExecutor struct{ executed []string }

func (r *recordingExecutor) Execute(_ context.Context, a action.Action) error {
	r.executed = append(r.executed, a.Key)
	return nil
}

func (r *recordingExecutor) Recover(context.Context, action.Action) (bool, error) { return true, nil }

// TestByProviderDefersAProviderWithoutWriter: a kind shared by Vikunja and
// GitHub targets runs the Vikunja writer and keeps a GitHub write queued.
func TestByProviderDefersAProviderWithoutWriter(t *testing.T) {
	v := &recordingExecutor{}
	b := ByProvider{Executors: map[string]Executor{upstream.ProviderVikunja: v}, Delay: time.Hour}
	vik := action.New(action.AddLabel, action.VikunjaTask(1), "x")
	if err := b.Execute(context.Background(), vik); err != nil || len(v.executed) != 1 {
		t.Fatalf("vikunja write: %v, executed %v", err, v.executed)
	}
	gh := action.New(action.AddLabel, action.PullRequest("o/r", 1), "x")
	err := b.Execute(context.Background(), gh)
	var d deferredError
	if !errors.As(err, &d) || d.delay != time.Hour || len(v.executed) != 1 {
		t.Fatalf("github write: %v, want deferred for an hour", err)
	}
	if applied, err := b.Recover(context.Background(), gh); applied || err != nil {
		t.Fatalf("Recover without a writer = %v, %v, want nothing applied", applied, err)
	}
}

// TestDoneBlockerStartsTheTaskItHeld: the done blocker's delivery queues
// a requeue of the task it blocked; that synthetic delivery is decided
// from the blocked task's fresh state and dispatches its run, executed
// with its claim waiting for the submission.
func TestDoneBlockerStartsTheTaskItHeld(t *testing.T) {
	e := newEnv(t, gordonOwns(run.VikunjaImplement))
	blocked := eligible()
	blocked.Relations.BlockedBy = []event.Related{{ID: 9, ProjectID: 136, Done: true}}
	e.tasks.tasks[blocked.ID] = blocked
	e.tasks.tasks[9] = event.Task{ID: 9, ProjectID: 136, Done: true, Buckets: []string{"Done"},
		Relations: event.Relations{Blocking: []event.Related{{ID: blocked.ID, ProjectID: 3}}}}
	body := `{"event_name":"task.updated","data":{"task":{"id":9,"project_id":136,"done":true}}}`
	e.accept(t, storedDelivery(t, event.SourceVikunja, nil, body))
	if n := e.tick(t); n != 1 {
		t.Fatalf("decided %d deliveries, want the blocker's", n)
	}
	if n := e.count(t, `SELECT count(*) FROM inbox WHERE headers->>'X-Gordon-Synthetic' = 'requeue' AND state = 'pending'`); n != 1 {
		t.Fatalf("%d requeues pending, want the blocked task's", n)
	}
	if n := e.tick(t); n != 1 {
		t.Fatalf("decided %d deliveries, want the requeue", n)
	}
	name := "vikunja-hollingsworth-claude-opus-" + strconv.FormatInt(blocked.ID, 10)
	if n := e.count(t, `SELECT count(*) FROM runs WHERE name = $1`, name); n != 1 {
		t.Fatalf("run %s not recorded", name)
	}
	if n := e.count(t, `SELECT count(*) FROM actions WHERE after_key = $1`, "submit-task:kelos/kelos-agents/"+name); n != 3 {
		t.Fatalf("%d claim actions wait for the submission, want 3", n)
	}
}
