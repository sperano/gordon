package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
)

func TestBackoffDoublesToItsCap(t *testing.T) {
	b := Backoff{Base: 5 * time.Second, Max: time.Minute}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := b.Delay(1000); got != time.Minute {
		t.Errorf("Delay(1000) = %v, want the cap", got)
	}
}

func TestLimiter(t *testing.T) {
	l := &Limiter{Default: 1, Limits: map[string]int{"github": 2, "free": 0}}
	if !l.TryAcquire("a") || l.TryAcquire("a") {
		t.Fatal("default limit of 1 not enforced")
	}
	if !l.TryAcquire("github") || !l.TryAcquire("github") || l.TryAcquire("github") {
		t.Fatal("per-key limit of 2 not enforced")
	}
	for range 5 {
		if !l.TryAcquire("free") {
			t.Fatal("limit 0 is not unbounded")
		}
	}
	l.Release("a")
	if !l.TryAcquire("a") {
		t.Fatal("released slot not reusable")
	}
	var none *Limiter
	if !none.TryAcquire("x") {
		t.Fatal("nil limiter refused")
	}
	none.Release("x")
}

func TestLoopFinishesInFlightBatchWithinGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var workErr error
	ticks := 0
	go func() {
		Loop(ctx, "test", time.Hour, time.Hour, logging.New(io.Discard, slog.LevelInfo), func(_, work context.Context) (int, error) {
			ticks++
			close(started)
			<-release
			workErr = work.Err()
			return 1, nil
		})
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("Loop returned while its batch was in flight")
	default:
	}
	close(release)
	<-done
	if workErr != nil || ticks != 1 {
		t.Fatalf("batch saw work error %v after %d ticks; want a live work context and no new claim", workErr, ticks)
	}
}

func TestLoopCancelsWorkAfterGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started, done := make(chan struct{}), make(chan struct{})
	var workErr error
	go func() {
		Loop(ctx, "test", time.Millisecond, time.Hour, logging.New(io.Discard, slog.LevelInfo), func(_, work context.Context) (int, error) {
			close(started)
			<-work.Done()
			workErr = work.Err()
			return 1, nil
		})
		close(done)
	}()
	<-started
	cancel()
	<-done
	if !errors.Is(workErr, context.Canceled) {
		t.Fatalf("work context ended with %v, want cancelled once the grace passed", workErr)
	}
}

type recordingTasks struct{ ids []int64 }

func (r *recordingTasks) Task(_ context.Context, id int64) (event.Task, error) {
	r.ids = append(r.ids, id)
	return event.Task{ID: id}, nil
}

type recordingPRs struct{ reads []string }

func (r *recordingPRs) PullRequest(_ context.Context, repo string, n int, files bool) (event.PullRequest, error) {
	r.reads = append(r.reads, repo+map[bool]string{true: "+files", false: ""}[files])
	return event.PullRequest{Repository: repo, Number: n}, nil
}

func TestStateReaderReadsWhatTheRouterUses(t *testing.T) {
	cfg := profile.Defaults()
	repo := cfg.Projects[0].Repository
	cases := []struct {
		name  string
		ev    event.Event
		tasks []int64
		prs   []string
	}{
		{"task.updated configured", event.Event{Kind: event.VikunjaTaskUpdated,
			Vikunja: &event.Vikunja{Task: event.Task{ID: 5, ProjectID: cfg.Projects[0].VikunjaID}}}, []int64{5}, nil},
		{"task.updated unconfigured", event.Event{Kind: event.VikunjaTaskUpdated,
			Vikunja: &event.Vikunja{Task: event.Task{ID: 5, ProjectID: -1}}}, nil, nil},
		{"owner comment, any project", event.Event{Kind: event.VikunjaTaskCommentCreated, Actor: cfg.Policy.VikunjaOwner,
			Vikunja: &event.Vikunja{Task: event.Task{ID: 6, ProjectID: -1}}}, []int64{6}, nil},
		{"other's comment", event.Event{Kind: event.VikunjaTaskCommentCreated, Actor: "mallory",
			Vikunja: &event.Vikunja{Task: event.Task{ID: 6}}}, nil, nil},
		{"PR comment", event.Event{Kind: event.GitHubIssueComment, GitHub: &event.GitHub{Repository: repo,
			OnPullRequest: true, PullRequest: event.PullRequest{Number: 3}}}, nil, []string{repo}},
		{"issue comment", event.Event{Kind: event.GitHubIssueComment, GitHub: &event.GitHub{Repository: repo,
			PullRequest: event.PullRequest{Number: 3}}}, nil, nil},
		{"unconfigured repository", event.Event{Kind: event.GitHubReview, GitHub: &event.GitHub{Repository: "x/y",
			OnPullRequest: true, PullRequest: event.PullRequest{Number: 3}}}, nil, nil},
		{"agent PR", event.Event{Kind: event.GitHubPullRequest, GitHub: &event.GitHub{Repository: repo, OnPullRequest: true,
			PullRequest: event.PullRequest{Number: 3, HeadRef: "kelos/vikunja-42"}}}, []int64{42}, []string{repo + "+files"}},
		{"other PR", event.Event{Kind: event.GitHubPullRequest, GitHub: &event.GitHub{Repository: repo, OnPullRequest: true,
			PullRequest: event.PullRequest{Number: 3, HeadRef: "feature"}}}, nil, []string{repo + "+files"}},
		{"push", event.Event{Kind: event.GitHubPush, GitHub: &event.GitHub{Repository: repo}}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks, prs := &recordingTasks{}, &recordingPRs{}
			st, err := StateReader{Config: cfg, Tasks: tasks, PRs: prs}.Read(context.Background(), tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tasks.ids, tc.tasks) || !reflect.DeepEqual(prs.reads, tc.prs) {
				t.Fatalf("read tasks %v PRs %v, want %v %v", tasks.ids, prs.reads, tc.tasks, tc.prs)
			}
			if (st.Task != nil) != (len(tc.tasks) > 0) || (st.PullRequest != nil) != (len(tc.prs) > 0) {
				t.Fatalf("state %+v does not hold what was read", st)
			}
		})
	}
}

func TestNeedsRetry(t *testing.T) {
	if !needsRetry(route.Decision{Outcome: route.Retry}) {
		t.Fatal("top-level retry")
	}
	mixed := route.Decision{Outcome: route.Dispatch, Families: []route.FamilyDecision{
		{Outcome: route.Dispatch}, {Outcome: route.Retry}}}
	if !needsRetry(mixed) {
		t.Fatal("a family waiting for state must hold the whole item")
	}
	if needsRetry(route.Decision{Outcome: route.Dispatch, Families: []route.FamilyDecision{{Outcome: route.Dispatch}}}) {
		t.Fatal("dispatch retried")
	}
}
