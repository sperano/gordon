package route_test

import (
	"slices"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

const (
	readyHead = "0123456789abcdef0123456789abcdef01234567"
	readyURL  = "https://github.com/sperano/vigie/pull/4"
	gordonBot = "gordon-cole-ai[bot]"
	readyTask = "vikunja/task/5"
)

var readAt = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func check(id int64, name, status, conclusion string, completed time.Time) event.CheckRun {
	return event.CheckRun{ID: id, Name: name, Status: status, Conclusion: conclusion, CompletedAt: completed}
}

// readyState is vigie's PR 4 (task 5) ready to merge: mergeable, its CI
// and both Go reviewers' checks passed a minute ago, announced as created,
// no Task on it.
func readyState() route.Readiness {
	yes, done := true, readAt.Add(-time.Minute)
	pr := event.PullRequest{Repository: repo, Number: 4, URL: readyURL, State: "open", Author: "sperano",
		HeadRef: "kelos/vikunja-5", HeadSHA: readyHead, HeadRepo: repo, Files: []string{"main.go"}, Mergeable: &yes,
		MergeableState: "clean", CreatedAt: readAt.Add(-2 * time.Hour)}
	return route.Readiness{PullRequest: pr, ReadAt: readAt,
		CheckRuns: []event.CheckRun{check(1, "check", "completed", "success", done),
			check(2, "review/pr-challenger", "completed", "success", done),
			check(3, "review/idiomatic-go", "completed", "neutral", done)},
		Tasks: []route.KelosTask{}, Task: event.Task{ID: 5, ProjectID: 103, Labels: []string{"in progress", "pr open"}},
		TaskComments: []event.TaskComment{{ID: 1, Author: "gordon", Body: notice.PROpenText(readyURL, true)}}}
}

func readinessDelivery(t *testing.T) event.Delivery {
	t.Helper()
	d := ghDelivery(t, "pull_request", map[string]any{"action": event.ActionReadiness, "sender": map[string]any{"login": "sperano"},
		"pull_request": map[string]any{"number": 4, "state": "open", "user": map[string]any{"login": "sperano"},
			"head": map[string]any{"ref": "kelos/vikunja-5", "sha": readyHead}}})
	d.Headers[event.SyntheticHeader] = "reconcile"
	return d
}

func decideReady(t *testing.T, rd route.Readiness, led replay.Ledger) route.FamilyDecision {
	t.Helper()
	dec := router().DecideAuthenticated(readinessDelivery(t), route.State{Readiness: []route.Readiness{rd}, ReadinessRead: true}, led)
	return family(t, dec)
}

func headDetail(extra string) string {
	return notice.Head{Repo: repo, Number: 4, SHA: readyHead, Extra: extra}.Detail()
}

var (
	readyNotice  = "post-notice:" + readyTask + ":ready:" + headDetail("")
	notifyKey    = "notify:" + readyTask + ":" + readyHead
	actionNeeded = "add-label:" + readyTask + ":action-needed"
	readyLabel   = "add-label:" + readyTask + ":ready to merge"
	mergeKey     = "merge-pull-request:github/sperano/vigie/pull/4:" + readyHead
)

// after maps each action key to the key it waits for.
func after(actions []action.Action) map[string]string {
	out := map[string]string{}
	for _, a := range actions {
		out[a.Key] = a.After
	}
	return out
}

func expectActions(t *testing.T, fd route.FamilyDecision, want []string, waits map[string]string) {
	t.Helper()
	if !slices.Equal(keys(fd.Actions), want) {
		t.Fatalf("actions %v, want %v", keys(fd.Actions), want)
	}
	got := after(fd.Actions)
	for k, w := range waits {
		if got[k] != w {
			t.Errorf("%s waits for %q, want %q", k, got[k], w)
		}
	}
}

func TestReadinessAnnouncesAReadyHead(t *testing.T) {
	fd := decideReady(t, readyState(), replay.Ledger{})
	expectFamily(t, fd, run.PRReadiness, route.Act, route.ReasonReady)
	expectActions(t, fd, []string{readyNotice, notifyKey, actionNeeded, readyLabel},
		map[string]string{readyNotice: "", notifyKey: readyNotice, actionNeeded: readyNotice, readyLabel: actionNeeded})
	if n := fd.Actions[1]; n.Value != readyHead || n.Detail != headDetail("") {
		t.Errorf("notify %+v", n)
	}
	if fd.Owner != "legacy" || fd.Execute {
		t.Errorf("owner %s execute %v: the zero table leaves it to legacy", fd.Owner, fd.Execute)
	}
}

func TestReadinessReadyLabelWaitsForTheNoticeWhenActionNeededIsOn(t *testing.T) {
	rd := readyState()
	rd.Task.Labels = append(rd.Task.Labels, "action-needed")
	fd := decideReady(t, rd, replay.Ledger{})
	expectActions(t, fd, []string{readyNotice, notifyKey, readyLabel}, map[string]string{readyLabel: readyNotice})
}

func TestReadinessPostsTheMissingPROpenNoticeFirst(t *testing.T) {
	rd := readyState()
	rd.TaskComments = nil
	fd := decideReady(t, rd, replay.Ledger{})
	created := "post-notice:" + readyTask + ":pr-open:sperano/vigie#4"
	expectActions(t, fd, []string{created, readyNotice, notifyKey, actionNeeded, readyLabel},
		map[string]string{readyNotice: created, notifyKey: readyNotice, readyLabel: actionNeeded})
}

func TestReadinessAutoMergeNeedsTheOwnersLabel(t *testing.T) {
	for _, c := range []struct {
		name    string
		creator string
		merge   bool
	}{{"owner's label", "eric", true}, {"lookalike label", "gordon", false}} {
		rd := readyState()
		rd.Task.Labels = append(rd.Task.Labels, "auto-merge")
		rd.Task.LabelCreators = map[string][]string{"auto-merge": {c.creator}}
		fd := decideReady(t, rd, replay.Ledger{})
		got := keys(fd.Actions)
		if slices.Contains(got, mergeKey) != c.merge {
			t.Errorf("%s: actions %v, merge %v", c.name, got, c.merge)
			continue
		}
		if !c.merge {
			continue
		}
		m := fd.Actions[len(fd.Actions)-1]
		if m.Key != mergeKey || m.Value != readyHead || m.Detail != "5" || m.After != readyNotice {
			t.Errorf("%s: merge %+v", c.name, m)
		}
	}
}

func TestReadinessNotReadyVerdicts(t *testing.T) {
	no, soon := false, readAt.Add(-10*time.Second)
	cases := []struct {
		name   string
		change func(*route.Readiness)
		led    replay.Ledger
		reason string
	}{
		{"mergeability unknown", func(rd *route.Readiness) { rd.PullRequest.Mergeable = nil }, replay.Ledger{}, route.ReasonNotMergeable},
		{"conflict", func(rd *route.Readiness) { rd.PullRequest.Mergeable = &no }, replay.Ledger{}, route.ReasonNotMergeable},
		{"CI check absent on a young PR", func(rd *route.Readiness) {
			rd.CheckRuns, rd.PullRequest.CreatedAt = rd.CheckRuns[1:], readAt.Add(-time.Minute)
		}, replay.Ledger{}, route.ReasonChecksMissing},
		{"Go reviewer absent", func(rd *route.Readiness) { rd.CheckRuns = rd.CheckRuns[:2] }, replay.Ledger{}, route.ReasonChecksMissing},
		{"unexpected check unfinished", func(rd *route.Readiness) {
			rd.CheckRuns = append(rd.CheckRuns, check(9, "lint", "in_progress", "", time.Time{}))
		}, replay.Ledger{}, route.ReasonChecksUnfinished},
		{"CI failed", func(rd *route.Readiness) { rd.CheckRuns[0].Conclusion = "failure" }, replay.Ledger{}, route.ReasonChecksUnacceptable},
		{"reviewer blocking, no round limit", func(rd *route.Readiness) { rd.CheckRuns[1].Conclusion = "failure" },
			replay.Ledger{}, route.ReasonChecksUnacceptable},
		{"last check settling", func(rd *route.Readiness) { rd.CheckRuns[2].CompletedAt = soon }, replay.Ledger{},
			route.ReasonChecksSettling},
		{"task's Kelos Task running", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "vikunja-fu-vigie-5-c9", Phase: "Running", Labels: map[string]string{route.TaskLabel: "5"}}}
		}, replay.Ledger{}, route.ReasonTasksActive},
		{"PR's review Task pending", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "review-vigie-pr-challenger-4-01234567", Labels: map[string]string{
				route.RoleLabel: "pr-review", route.PRLabel: "4"}, Annotations: map[string]string{route.RepositoryAnnotation: repo}}}
		}, replay.Ledger{}, route.ReasonTasksActive},
		{"Gordon's queued conflict run", func(*route.Readiness) {}, replay.Ledger{Runs: []route.RunRecord{{
			Name: "pr-conflict-x", Active: true, Family: run.PRConflict, Workspace: "vigie", PR: 4}}}, route.ReasonTasksActive},
		{"Gordon's task run", func(*route.Readiness) {}, replay.Ledger{Runs: []route.RunRecord{{
			Name: "vikunja-vigie-claude-opus-5", Active: true, Family: run.VikunjaImplement, TaskID: 5}}}, route.ReasonTasksActive},
	}
	for _, c := range cases {
		rd := readyState()
		c.change(&rd)
		fd := decideReady(t, rd, c.led)
		if fd.Outcome != route.Ignore || fd.Reason != c.reason || len(fd.Actions) != 0 {
			t.Errorf("%s: %s/%q %v, want ignore/%q and no write", c.name, fd.Outcome, fd.Reason, keys(fd.Actions), c.reason)
		}
	}
}

func TestReadinessIgnoresWhatDoesNotHoldIt(t *testing.T) {
	cases := []struct {
		name   string
		change func(*route.Readiness)
	}{
		{"finished Task of the task", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "vikunja-vigie-claude-opus-5", Phase: "Succeeded", Labels: map[string]string{route.TaskLabel: "5"}}}
		}},
		{"another PR's follow-up", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "pr-followup-vigie-44-c1", Labels: map[string]string{route.RoleLabel: "pr-followup",
				route.PRLabel: "44"}, Annotations: map[string]string{route.RepositoryAnnotation: repo}}}
		}},
		{"stray Cubic check on a repository Cubic does not review", func(rd *route.Readiness) {
			rd.CheckRuns = append(rd.CheckRuns, check(9, route.CubicCheck, "queued", "", time.Time{}))
		}},
		{"older failed run of a passing check", func(rd *route.Readiness) {
			rd.CheckRuns = append([]event.CheckRun{check(0, "check", "completed", "failure", readAt.Add(-time.Hour))}, rd.CheckRuns...)
		}},
	}
	for _, c := range cases {
		rd := readyState()
		c.change(&rd)
		if fd := decideReady(t, rd, replay.Ledger{}); fd.Reason != route.ReasonReady {
			t.Errorf("%s: %s/%q, want ready", c.name, fd.Outcome, fd.Reason)
		}
	}
}

func TestReadinessCubicCountsWhereItReviews(t *testing.T) {
	rd := readyState()
	rd.CheckRuns = append(rd.CheckRuns, check(9, route.CubicCheck, "queued", "", time.Time{}))
	r := router()
	r.Config.Policy.Readiness.CubicRepos = []string{repo}
	dec := r.DecideAuthenticated(readinessDelivery(t), route.State{Readiness: []route.Readiness{rd}, ReadinessRead: true}, replay.Ledger{})
	expectFamily(t, family(t, dec), run.PRReadiness, route.Ignore, route.ReasonChecksUnfinished)
}

func TestReadinessNotReadyRevokesItsLabels(t *testing.T) {
	rd := readyState()
	rd.PullRequest.Mergeable = nil
	rd.Task.Labels = append(rd.Task.Labels, "ready to merge", "action-needed")
	fd := decideReady(t, rd, replay.Ledger{})
	expectFamily(t, fd, run.PRReadiness, route.Act, route.ReasonNotMergeable)
	expectActions(t, fd, []string{"remove-label:" + readyTask + ":ready to merge", "remove-label:" + readyTask + ":action-needed"}, nil)
	rd.Task.Labels = []string{"action-needed"}
	if fd := decideReady(t, rd, replay.Ledger{}); len(fd.Actions) != 0 {
		t.Errorf("an action-needed label without a verdict label is left: %v", keys(fd.Actions))
	}
}

func TestReadinessReportsOverdueMissingChecks(t *testing.T) {
	missing := "post-notice:" + readyTask + ":checks-missing:" + headDetail(`["check"]`)
	cases := []struct {
		name   string
		change func(*route.Readiness)
		want   []string
	}{
		{"opened two hours ago", func(*route.Readiness) {}, []string{missing}},
		{"head committed ten minutes ago", func(rd *route.Readiness) { rd.HeadCommitted = readAt.Add(-10 * time.Minute) }, nil},
		{"opened ten minutes ago", func(rd *route.Readiness) { rd.PullRequest.CreatedAt = readAt.Add(-10 * time.Minute) }, nil},
		{"exactly the notice's age", func(rd *route.Readiness) { rd.PullRequest.CreatedAt = readAt.Add(-30 * time.Minute) },
			[]string{missing}},
		{"already reported", func(rd *route.Readiness) {
			rd.TaskComments = append(rd.TaskComments, event.TaskComment{Author: "gordon",
				Body: "<p>CHECKS MISSING: <a>sperano/vigie#4</a>. HEAD <code>01234567</code>.</p>"})
		}, nil},
	}
	for _, c := range cases {
		rd := readyState()
		rd.CheckRuns = rd.CheckRuns[1:]
		c.change(&rd)
		fd := decideReady(t, rd, replay.Ledger{})
		if fd.Reason != route.ReasonChecksMissing || !slices.Equal(keys(fd.Actions), append([]string{}, c.want...)) {
			t.Errorf("%s: %q %v, want %v", c.name, fd.Reason, keys(fd.Actions), c.want)
		}
	}
	r := router()
	r.Config.Policy.Readiness.MissingCheckMinutes = 0
	rd := readyState()
	rd.CheckRuns = rd.CheckRuns[1:]
	dec := r.DecideAuthenticated(readinessDelivery(t), route.State{Readiness: []route.Readiness{rd}, ReadinessRead: true}, replay.Ledger{})
	if fd := family(t, dec); len(fd.Actions) != 0 {
		t.Errorf("notice off: %v", keys(fd.Actions))
	}
}

// announced is readyState with its READY notice posted by Gordon.
func announced() route.Readiness {
	rd := readyState()
	rd.TaskComments = append(rd.TaskComments, event.TaskComment{ID: 2, Author: "gordon",
		Body: notice.ReadyText(notice.Head{Repo: repo, Number: 4, SHA: readyHead})})
	return rd
}

func TestReadinessAnnouncedHead(t *testing.T) {
	delivered := route.DeliveredReaction + "01234567"
	cases := []struct {
		name   string
		change func(*route.Readiness)
		want   []string
		reason route.Outcome
	}{
		{"labels repaired, ntfy retried", func(*route.Readiness) {}, []string{actionNeeded, readyLabel, notifyKey}, route.Act},
		{"all done", func(rd *route.Readiness) {
			rd.Task.Labels = append(rd.Task.Labels, "ready to merge")
			rd.Reactions = []string{delivered}
		}, nil, route.Duplicate},
		{"ntfy waits for the full gate", func(rd *route.Readiness) {
			rd.Task.Labels = append(rd.Task.Labels, "ready to merge")
			rd.CheckRuns[0].Status = "in_progress"
		}, nil, route.Duplicate},
		{"merge after the full gate", func(rd *route.Readiness) {
			rd.Task.Labels = append(rd.Task.Labels, "ready to merge", "auto-merge")
			rd.Task.LabelCreators = map[string][]string{"auto-merge": {"eric"}}
			rd.Reactions = []string{delivered}
		}, []string{mergeKey}, route.Act},
		{"merge waits for a re-run check", func(rd *route.Readiness) {
			rd.Task.Labels = append(rd.Task.Labels, "ready to merge", "auto-merge")
			rd.Task.LabelCreators = map[string][]string{"auto-merge": {"eric"}}
			rd.Reactions = []string{delivered}
			rd.CheckRuns = append(rd.CheckRuns, check(7, "check", "in_progress", "", time.Time{}))
		}, nil, route.Duplicate},
		{"conflict since", func(rd *route.Readiness) {
			no := false
			rd.PullRequest.Mergeable = &no
			rd.Task.Labels = append(rd.Task.Labels, "ready to merge", "action-needed")
		}, []string{"remove-label:" + readyTask + ":ready to merge", "remove-label:" + readyTask + ":action-needed"}, route.Act},
		{"a follow-up started since", func(rd *route.Readiness) {
			rd.Task.Labels = append(rd.Task.Labels, "ready to merge")
			rd.Tasks = []route.KelosTask{{Name: "pr-followup-vigie-4-c1", Labels: map[string]string{route.RoleLabel: "pr-followup",
				route.PRLabel: "4"}, Annotations: map[string]string{route.RepositoryAnnotation: repo}}}
		}, []string{"remove-label:" + readyTask + ":ready to merge"}, route.Act},
	}
	for _, c := range cases {
		rd := announced()
		c.change(&rd)
		fd := decideReady(t, rd, replay.Ledger{})
		if fd.Reason != route.ReasonAlreadyAnnounced || fd.Outcome != c.reason || !slices.Equal(keys(fd.Actions), append([]string{}, c.want...)) {
			t.Errorf("%s: %s/%q %v, want %s/already-announced %v", c.name, fd.Outcome, fd.Reason, keys(fd.Actions), c.reason, c.want)
		}
	}
}

func TestReadinessTrustsOnlyGordonsNotices(t *testing.T) {
	rd := readyState()
	rd.TaskComments = append(rd.TaskComments, event.TaskComment{Author: "eric",
		Body: notice.ReadyText(notice.Head{Repo: repo, Number: 4, SHA: readyHead})})
	if fd := decideReady(t, rd, replay.Ledger{}); fd.Reason != route.ReasonReady || !slices.Contains(keys(fd.Actions), readyNotice) {
		t.Errorf("a pasted READY notice suppressed the announcement: %q %v", fd.Reason, keys(fd.Actions))
	}
	old := readyState()
	old.TaskComments = append(old.TaskComments, event.TaskComment{Author: "gordon",
		Body: notice.ReadyText(notice.Head{Repo: repo, Number: 4, SHA: "fedcba9876543210"})})
	if fd := decideReady(t, old, replay.Ledger{}); fd.Reason != route.ReasonReady {
		t.Errorf("an older head's notice counted: %q", fd.Reason)
	}
}

func TestReadinessCandidates(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*route.Readiness)
		outcome route.Outcome
		reason  string
	}{
		{"closed", func(rd *route.Readiness) { rd.PullRequest.State = "closed" }, route.Stale, route.ReasonPullRequestClosed},
		{"draft", func(rd *route.Readiness) { rd.PullRequest.Draft = true }, route.Stale, route.ReasonDraft},
		{"foreign author", func(rd *route.Readiness) { rd.PullRequest.Author = "renovate[bot]" }, route.Ignore, route.ReasonAuthorNotAllowed},
		{"fork", func(rd *route.Readiness) { rd.PullRequest.HeadRepo = "someone/vigie" }, route.Ignore, route.ReasonForeignHead},
		{"not a task branch", func(rd *route.Readiness) { rd.PullRequest.HeadRef = "image-build/x" }, route.Ignore, route.ReasonNotATaskBranch},
		{"task not read", func(rd *route.Readiness) { rd.Task.ID = 0 }, route.Retry, route.ReasonStateUnavailable},
		{"legacy pr-open Task announcing", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "pr-open-x", Labels: map[string]string{route.RoleLabel: "pr-open"},
				Annotations: map[string]string{route.RepositoryAnnotation: repo, route.PRAnnotation: "4"}}}
		}, route.Ignore, route.ReasonAnnouncementActive},
		{"legacy waiter of the head", func(rd *route.Readiness) {
			rd.Tasks = []route.KelosTask{{Name: "pr-ready-x", Phase: "Running", Labels: map[string]string{route.RoleLabel: "pr-readiness"},
				Annotations: map[string]string{route.RepositoryAnnotation: repo, route.SHAAnnotation: readyHead}}}
		}, route.Ignore, route.ReasonAnnouncementActive},
	}
	for _, c := range cases {
		rd := readyState()
		c.change(&rd)
		fd := decideReady(t, rd, replay.Ledger{})
		if fd.Outcome != c.outcome || fd.Reason != c.reason || len(fd.Actions) != 0 {
			t.Errorf("%s: %s/%q %v, want %s/%q", c.name, fd.Outcome, fd.Reason, keys(fd.Actions), c.outcome, c.reason)
		}
	}
	finishedWaiter := readyState()
	finishedWaiter.Tasks = []route.KelosTask{{Name: "pr-ready-x", Phase: "Succeeded", Labels: map[string]string{route.RoleLabel: "pr-readiness"},
		Annotations: map[string]string{route.RepositoryAnnotation: repo, route.SHAAnnotation: readyHead}}}
	if fd := decideReady(t, finishedWaiter, replay.Ledger{}); fd.Reason != route.ReasonReady {
		t.Errorf("a finished waiter held the head: %q", fd.Reason)
	}
}

func TestReadinessNeedsItsConfigurationAndState(t *testing.T) {
	r := router()
	delete(r.Config.Policy.Readiness.CIChecks, repo)
	rd := readyState()
	dec := r.DecideAuthenticated(readinessDelivery(t), route.State{Readiness: []route.Readiness{rd}, ReadinessRead: true}, replay.Ledger{})
	expectFamily(t, family(t, dec), run.PRReadiness, route.Reject, route.ReasonReadinessUnconfigured)
	expect(t, router().DecideAuthenticated(readinessDelivery(t), route.State{}, replay.Ledger{}), route.Retry, route.ReasonStateUnavailable)
	expect(t, router().DecideAuthenticated(readinessDelivery(t), route.State{ReadinessRead: true}, replay.Ledger{}),
		route.Ignore, route.ReasonNoPullRequest)
	webhook := readinessDelivery(t)
	delete(webhook.Headers, event.SyntheticHeader)
	expect(t, router().DecideAuthenticated(webhook, route.State{}, replay.Ledger{}), route.Ignore, route.ReasonActionNotRouted)
	if got := router().Reads(mustParse(t, webhook)); got.Readiness {
		t.Errorf("a webhook's readiness action reads %+v", got)
	}
	if got := router().Reads(mustParse(t, readinessDelivery(t))); !got.Readiness || got.PullRequest {
		t.Errorf("synthetic readiness reads %+v", got)
	}
}

func checkRunDelivery(t *testing.T, action, repository string) event.Delivery {
	t.Helper()
	return ghDelivery(t, "check_run", map[string]any{"action": action, "_repo": repository,
		"check_run": map[string]any{"head_sha": readyHead}})
}

func TestReadinessOnACompletedCheckRun(t *testing.T) {
	r := router()
	other := readyState()
	other.PullRequest.Number, other.PullRequest.HeadSHA = 6, "fedcba9876543210"
	st := route.State{Readiness: []route.Readiness{readyState(), other}, ReadinessRead: true}
	dec := r.DecideAuthenticated(checkRunDelivery(t, "completed", repo), st, replay.Ledger{})
	if len(dec.Families) != 2 || dec.Outcome != route.Act {
		t.Fatalf("decision %+v", dec)
	}
	expectFamily(t, dec.Families[0], run.PRReadiness, route.Act, route.ReasonReady)
	expectFamily(t, dec.Families[1], run.PRReadiness, route.Stale, route.ReasonHeadMoved)
	expect(t, r.DecideAuthenticated(checkRunDelivery(t, "created", repo), st, replay.Ledger{}), route.Ignore, route.ReasonActionNotRouted)
	expect(t, r.DecideAuthenticated(checkRunDelivery(t, "completed", "sperano/unknown"), st, replay.Ledger{}),
		route.Ignore, route.ReasonRepositoryNotConfigured)
	if got := r.Reads(mustParse(t, checkRunDelivery(t, "completed", repo))); !got.Readiness {
		t.Errorf("completed check run reads %+v", got)
	}
	for _, d := range []event.Delivery{checkRunDelivery(t, "created", repo), checkRunDelivery(t, "completed", "sperano/unknown")} {
		if got := r.Reads(mustParse(t, d)); got.Readiness {
			t.Errorf("reads %+v for a check run the family does not decide", got)
		}
	}
}

func TestReadinessCandidateHelper(t *testing.T) {
	r := router()
	rd := readyState()
	if !r.ReadinessCandidate(rd.PullRequest) {
		t.Error("ready PR is no candidate")
	}
	rd.PullRequest.Draft = true
	if r.ReadinessCandidate(rd.PullRequest) {
		t.Error("draft is a candidate")
	}
}

func TestMissingChecksAndOverdue(t *testing.T) {
	r := router()
	r.Config.Policy.Readiness.CIChecks[repo] = []string{"test", "check", "lint", "check"}
	runs := []event.CheckRun{check(1, "lint", "queued", "", time.Time{})}
	if got := r.MissingChecks(repo, runs); !slices.Equal(got, []string{"check", "test"}) {
		t.Errorf("MissingChecks = %v", got)
	}
	cases := []struct {
		name               string
		created, committed time.Time
		want               bool
	}{
		{"both unknown", time.Time{}, time.Time{}, false},
		{"old PR, no commit", readAt.Add(-time.Hour), time.Time{}, true},
		{"old PR, fresh commit", readAt.Add(-time.Hour), readAt.Add(-time.Minute), false},
		{"commit only, old", time.Time{}, readAt.Add(-time.Hour), true},
	}
	for _, c := range cases {
		if got := r.MissingOverdue(c.created, c.committed, readAt); got != c.want {
			t.Errorf("%s: MissingOverdue = %v, want %v", c.name, got, c.want)
		}
	}
}
