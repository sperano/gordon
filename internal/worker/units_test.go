package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
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
	huge := Backoff{Base: time.Duration(math.MaxInt64/4 + 1), Max: time.Duration(math.MaxInt64 - 1)}
	if got := huge.Delay(4); got != huge.Max {
		t.Errorf("Delay doubling past MaxInt64 = %v, want the cap %v", got, huge.Max)
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
	fresh := &Limiter{Default: 1}
	fresh.Release("never-taken") // wrote to a nil map before any acquire
	if !fresh.TryAcquire("never-taken") || fresh.TryAcquire("never-taken") {
		t.Fatal("releasing a slot never taken changed the limit")
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

func TestEveryStopsCallingOnceCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	Every(ctx, time.Nanosecond, func(context.Context) {
		calls++
		cancel() // cancelled while running: no further call, though a tick is due
	})
	if calls != 1 {
		t.Fatalf("fn called %d times, want once", calls)
	}
	calls = 0
	Every(ctx, time.Hour, func(context.Context) { calls++ })
	if calls != 0 {
		t.Fatal("fn called on an already cancelled context")
	}
}

func (*recordingTasks) TaskComments(context.Context, int64) ([]event.TaskComment, error) {
	return nil, nil
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

func (r *recordingPRs) History(_ context.Context, repo string, _ int) ([]event.Review, []event.IssueComment, error) {
	r.reads = append(r.reads, repo+"+history")
	return nil, nil, nil
}

func TestStateReaderReadsWhatTheRouterUses(t *testing.T) {
	cfg := profile.Defaults()
	repo, p := cfg.Projects[0].Repository, cfg.Policy
	onPR := func(kind event.Kind, action, actor, body string) event.Event {
		return event.Event{Kind: kind, Action: action, Actor: actor, RequestKey: "github/req/1",
			GitHub: &event.GitHub{Repository: repo, OnPullRequest: true, Body: body, PullRequest: event.PullRequest{Number: 3}}}
	}
	prEvent := func(action, head, author string) event.Event {
		return event.Event{Kind: event.GitHubPullRequest, Action: action, GitHub: &event.GitHub{Repository: repo,
			OnPullRequest: true, PullRequest: event.PullRequest{Number: 3, HeadRef: head, Author: author}}}
	}
	ownerComment := func(body string) event.Event {
		return event.Event{Kind: event.VikunjaTaskCommentCreated, Actor: p.VikunjaOwner, RequestKey: "vikunja/comment/1",
			Vikunja: &event.Vikunja{Task: event.Task{ID: 6, ProjectID: -1}, Comment: &event.Comment{ID: 1, Body: body}}}
	}
	request, bot := p.Trigger+" fix it", p.ReviewBots[0].Login
	handled := replay.Ledger{Requests: []string{"github/req/1", "vikunja/comment/1"}}
	cases := []struct {
		name  string
		ev    event.Event
		led   route.Ledger
		tasks []int64
		prs   []string
	}{
		{"task.updated configured", event.Event{Kind: event.VikunjaTaskUpdated,
			Vikunja: &event.Vikunja{Task: event.Task{ID: 5, ProjectID: cfg.Projects[0].VikunjaID}}}, nil, []int64{5}, nil},
		{"task.updated unconfigured", event.Event{Kind: event.VikunjaTaskUpdated,
			Vikunja: &event.Vikunja{Task: event.Task{ID: 5, ProjectID: -1}}}, nil, nil, nil},
		{"owner request, any project", ownerComment("<p>" + request + "</p>"), nil, []int64{6}, nil},
		{"owner request already handled", ownerComment(request), handled, nil, nil},
		{"owner comment that is no request", ownerComment("<p>thanks</p>"), nil, nil, nil},
		{"other's request", event.Event{Kind: event.VikunjaTaskCommentCreated, Actor: "mallory",
			Vikunja: &event.Vikunja{Task: event.Task{ID: 6}, Comment: &event.Comment{Body: request}}}, nil, nil, nil},
		{"owner PR request", onPR(event.GitHubIssueComment, "created", p.GitHubOwner, request), nil, nil, []string{repo}},
		{"owner inline request", onPR(event.GitHubReviewComment, "created", p.GitHubOwner, request), nil, nil, []string{repo}},
		{"owner PR request already handled", onPR(event.GitHubIssueComment, "created", p.GitHubOwner, request), handled, nil, nil},
		{"edited PR request", onPR(event.GitHubIssueComment, "edited", p.GitHubOwner, request), nil, nil, nil},
		{"owner PR comment that is no request", onPR(event.GitHubIssueComment, "created", p.GitHubOwner, "lgtm"), nil, nil, nil},
		{"other's PR request", onPR(event.GitHubIssueComment, "created", "mallory", request), nil, nil, nil},
		{"review bot review", onPR(event.GitHubReview, "submitted", bot, "findings"), nil, nil, []string{repo, repo + "+history"}},
		{"review bot review, request handled", onPR(event.GitHubReview, "submitted", bot, "findings"), handled, nil,
			[]string{repo, repo + "+history"}},
		{"other's review", onPR(event.GitHubReview, "submitted", "mallory", "findings"), nil, nil, nil},
		{"issue comment", event.Event{Kind: event.GitHubIssueComment, Action: "created", Actor: p.GitHubOwner,
			GitHub: &event.GitHub{Repository: repo, Body: request, PullRequest: event.PullRequest{Number: 3}}}, nil, nil, nil},
		{"unconfigured repository", event.Event{Kind: event.GitHubReview, Action: "submitted", Actor: bot,
			GitHub: &event.GitHub{Repository: "x/y", OnPullRequest: true, PullRequest: event.PullRequest{Number: 3}}}, nil, nil, nil},
		{"agent PR opened", prEvent("opened", "kelos/vikunja-42", p.PullRequestAuthors[0]), nil, []int64{42}, []string{repo + "+files"}},
		{"agent PR closed", prEvent("closed", "kelos/vikunja-42", p.PullRequestAuthors[0]), nil, []int64{42}, []string{repo}},
		{"agent PR reopened", prEvent("reopened", "kelos/vikunja-42", p.PullRequestAuthors[0]), nil, []int64{42}, []string{repo}},
		{"stranger's task branch", prEvent("closed", "kelos/vikunja-42", "mallory"), nil, nil, nil},
		{"other PR synchronized", prEvent("synchronize", "feature", "mallory"), nil, nil, []string{repo + "+files"}},
		{"PR labeled", prEvent("labeled", "feature", p.PullRequestAuthors[0]), nil, nil, nil},
		{"push", event.Event{Kind: event.GitHubPush, GitHub: &event.GitHub{Repository: repo}}, nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks, prs := &recordingTasks{}, &recordingPRs{}
			led := tc.led
			if led == nil {
				led = replay.Ledger{}
			}
			st, err := StateReader{Config: cfg, Tasks: tasks, PRs: prs, History: prs}.Read(context.Background(), tc.ev, led)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tasks.ids, tc.tasks) || !reflect.DeepEqual(prs.reads, tc.prs) {
				t.Fatalf("read tasks %v PRs %v, want %v %v", tasks.ids, prs.reads, tc.tasks, tc.prs)
			}
			history := slices.Contains(tc.prs, repo+"+history")
			if (st.Task != nil) != (len(tc.tasks) > 0) || (st.PullRequest != nil) != (len(tc.prs) > 0) || (st.History != nil) != history {
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
