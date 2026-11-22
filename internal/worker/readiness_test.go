package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/upstream"
)

const (
	rdRepo = "sperano/vigie"
	rdHead = "0123456789abcdef"
)

var rdNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// readinessFakes records every readiness read.
type readinessFakes struct {
	reads     []string
	runs      []event.CheckRun
	committed time.Time
	failOn    string
	open      []upstream.ListedPR
	kelos     map[string][]route.KelosTask
}

func (f *readinessFakes) read(what string) error {
	f.reads = append(f.reads, what)
	if what == f.failOn {
		return errors.New(what + " down")
	}
	return nil
}

func (f *readinessFakes) CheckRuns(_ context.Context, repo, sha string) ([]event.CheckRun, error) {
	return f.runs, f.read("checks " + repo + "@" + sha)
}

func (f *readinessFakes) Committed(_ context.Context, repo, sha string) (time.Time, error) {
	return f.committed, f.read("commit " + repo + "@" + sha)
}

func (f *readinessFakes) ReviewThreads(_ context.Context, repo string, n int) ([]event.ReviewThread, error) {
	return []event.ReviewThread{{Author: "x"}}, f.read(fmt.Sprintf("threads %s#%d", repo, n))
}

func (f *readinessFakes) Tasks(_ context.Context, selector string) ([]route.KelosTask, error) {
	return f.kelos[selector], f.read("kelos " + selector)
}

func (f *readinessFakes) TaskReactions(_ context.Context, id int64) ([]string, error) {
	return []string{"🔔"}, f.read(fmt.Sprintf("reactions %d", id))
}

func (f *readinessFakes) OpenPullRequests(_ context.Context, repo string) ([]upstream.ListedPR, error) {
	return f.open, f.read("open " + repo)
}

func candidatePR(n int, head string) event.PullRequest {
	return event.PullRequest{Repository: rdRepo, Number: n, State: "open", Author: "sperano", HeadRef: fmt.Sprintf("kelos/vikunja-%d", 50+n),
		HeadSHA: head, HeadRepo: rdRepo, CreatedAt: rdNow.Add(-time.Hour)}
}

func readinessReader(f *readinessFakes, cfg profile.Config, prs ...event.PullRequest) StateReader {
	tasks := &fakeTasks{tasks: map[int64]event.Task{}, comments: map[int64][]event.TaskComment{}}
	gh := &fakePRs{prs: map[string]event.PullRequest{}}
	for _, pr := range prs {
		gh.prs[fmt.Sprintf("%s#%d", pr.Repository, pr.Number)] = pr
		id := int64(50 + pr.Number)
		tasks.tasks[id] = event.Task{ID: id}
	}
	return StateReader{Config: cfg, Tasks: tasks, PRs: gh, History: gh,
		Readiness: Readiness{GitHub: f, Kelos: f, Reactions: f, Open: f, Now: func() time.Time { return rdNow }}}
}

func readinessEvent(n int) event.Event {
	return event.Event{Kind: event.GitHubPullRequest, Action: event.ActionReadiness,
		GitHub: &event.GitHub{Repository: rdRepo, Synthetic: true, PullRequest: event.PullRequest{Number: n}}}
}

func TestStateReaderReadsOnePullRequestsReadiness(t *testing.T) {
	f := &readinessFakes{runs: []event.CheckRun{{ID: 1, Name: "review/pr-challenger", Status: "completed"}},
		kelos: map[string][]route.KelosTask{"vikunja.spe.quebec/workspace in (vigie)": {{Name: "a"}, {Name: "b"}},
			"vikunja.spe.quebec/task=54": {{Name: "b"}, {Name: "c"}}}}
	st, err := readinessReader(f, profile.Defaults(), candidatePR(4, rdHead)).Read(context.Background(), readinessEvent(4), replay.Ledger{})
	if err != nil || !st.ReadinessRead || len(st.Readiness) != 1 {
		t.Fatalf("state %+v, %v", st, err)
	}
	rd := st.Readiness[0]
	// vigie requires its `check` CI check, absent on a head an hour old:
	// the commit's date is read for the CHECKS MISSING notice.
	want := []string{"checks " + rdRepo + "@" + rdHead, "kelos vikunja.spe.quebec/workspace in (vigie)",
		"kelos vikunja.spe.quebec/task=54", "threads " + rdRepo + "#4", "commit " + rdRepo + "@" + rdHead, "reactions 54"}
	if !slices.Equal(f.reads, want) {
		t.Errorf("reads %v, want %v", f.reads, want)
	}
	var names []string
	for _, k := range rd.Tasks {
		names = append(names, k.Name)
	}
	if !slices.Equal(names, []string{"a", "b", "c"}) || rd.Task.ID != 54 || !rd.ReadAt.Equal(rdNow) ||
		!slices.Equal(rd.Reactions, []string{"🔔"}) || len(rd.Threads) != 1 || rd.PullRequest.Number != 4 {
		t.Errorf("readiness %+v", rd)
	}
}

func TestStateReaderReadsOnlyWhatTheGateNeeds(t *testing.T) {
	cfg := profile.Defaults()
	cfg.Policy.Readiness.Notify, cfg.Policy.ReviewBots = false, nil
	young := candidatePR(4, rdHead)
	young.CreatedAt = rdNow.Add(-time.Minute)
	f := &readinessFakes{}
	if _, err := readinessReader(f, cfg, young).Read(context.Background(), readinessEvent(4), replay.Ledger{}); err != nil {
		t.Fatal(err)
	}
	for _, skipped := range []string{"threads " + rdRepo + "#4", "commit " + rdRepo + "@" + rdHead, "reactions 54"} {
		if slices.Contains(f.reads, skipped) {
			t.Errorf("read %q: no bots, no ntfy, a young PR", skipped)
		}
	}
	f = &readinessFakes{runs: []event.CheckRun{{ID: 1, Name: "check", Status: "completed"}}}
	if _, err := readinessReader(f, cfg, candidatePR(4, rdHead)).Read(context.Background(), readinessEvent(4), replay.Ledger{}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.reads, "commit "+rdRepo+"@"+rdHead) {
		t.Error("read the commit with no CI check missing")
	}
	closed := candidatePR(4, rdHead)
	closed.State = "closed"
	f = &readinessFakes{}
	st, err := readinessReader(f, cfg, closed).Read(context.Background(), readinessEvent(4), replay.Ledger{})
	if err != nil || len(f.reads) != 0 || len(st.Readiness) != 1 || st.Readiness[0].PullRequest.State != "closed" {
		t.Errorf("a closed PR read %v (%v), state %+v", f.reads, err, st)
	}
}

func TestStateReaderReadsTheCheckRunsCandidates(t *testing.T) {
	foreign := candidatePR(6, rdHead)
	foreign.Author = "renovate[bot]"
	f := &readinessFakes{open: []upstream.ListedPR{{PullRequest: candidatePR(4, rdHead)}, {PullRequest: candidatePR(5, "fedcba98")},
		{PullRequest: foreign}}}
	ev := event.Event{Kind: event.GitHubCheckRun, Action: event.ActionCompleted,
		GitHub: &event.GitHub{Repository: rdRepo, CheckRunHeadSHA: rdHead}}
	st, err := readinessReader(f, profile.Defaults(), candidatePR(4, rdHead), candidatePR(5, "fedcba98"), foreign).
		Read(context.Background(), ev, replay.Ledger{})
	if err != nil || len(st.Readiness) != 1 || st.Readiness[0].PullRequest.Number != 4 {
		t.Fatalf("state %+v, %v; want PR 4 only", st, err)
	}
	f = &readinessFakes{}
	st, err = readinessReader(f, profile.Defaults()).Read(context.Background(), ev, replay.Ledger{})
	if err != nil || !st.ReadinessRead || st.Readiness == nil || len(st.Readiness) != 0 {
		t.Errorf("no candidate: %+v, %v; want read and empty", st, err)
	}
}

func TestStateReaderReadinessFailures(t *testing.T) {
	for _, fail := range []string{"checks " + rdRepo + "@" + rdHead, "kelos vikunja.spe.quebec/task=54",
		"threads " + rdRepo + "#4", "commit " + rdRepo + "@" + rdHead, "reactions 54"} {
		f := &readinessFakes{failOn: fail}
		st, err := readinessReader(f, profile.Defaults(), candidatePR(4, rdHead)).Read(context.Background(), readinessEvent(4), replay.Ledger{})
		if err == nil || st.ReadinessRead || st.Readiness != nil {
			t.Errorf("%s failing: state %+v, %v; want unread and the error", fail, st, err)
		}
	}
	sr := readinessReader(&readinessFakes{}, profile.Defaults(), candidatePR(4, rdHead))
	sr.Readiness.Kelos = nil
	if _, err := sr.Read(context.Background(), readinessEvent(4), replay.Ledger{}); !errors.Is(err, errNoReadiness) {
		t.Errorf("no Kelos lister: %v", err)
	}
	ev := event.Event{Kind: event.GitHubCheckRun, Action: event.ActionCompleted,
		GitHub: &event.GitHub{Repository: rdRepo, CheckRunHeadSHA: rdHead}}
	f := &readinessFakes{failOn: "open " + rdRepo}
	if st, err := readinessReader(f, profile.Defaults()).Read(context.Background(), ev, replay.Ledger{}); err == nil || st.ReadinessRead {
		t.Errorf("listing down: %+v, %v", st, err)
	}
}
