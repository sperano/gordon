package ghlife

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

type conflictGitHub struct {
	open    map[string][]upstream.ListedPR
	fresh   map[string]event.PullRequest
	listErr map[string]error
	readErr map[string]error
	reads   []string
}

func prKey(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func (g *conflictGitHub) OpenPullRequests(_ context.Context, repo string) ([]upstream.ListedPR, error) {
	return g.open[repo], g.listErr[repo]
}

func (g *conflictGitHub) PullRequest(_ context.Context, repo string, number int, withFiles bool) (event.PullRequest, error) {
	if withFiles {
		return event.PullRequest{}, errors.New("files read for a conflict")
	}
	k := prKey(repo, number)
	g.reads = append(g.reads, k)
	if err := g.readErr[k]; err != nil {
		return event.PullRequest{}, err
	}
	return g.fresh[k], nil
}

type conflictInbox struct {
	queued   []store.Delivery
	taken    map[string]bool
	asked    []string
	takenErr error
}

func (s *conflictInbox) Enqueue(_ context.Context, ds []store.Delivery) (int, error) {
	s.queued = append(s.queued, ds...)
	return len(ds), nil
}

func (s *conflictInbox) RunsTaken(_ context.Context, names []string) (map[string]bool, error) {
	s.asked = append(s.asked, names...)
	return s.taken, s.takenErr
}

const (
	cocovm = "sperano/cocovm"
	vigie  = "sperano/vigie"
	baseA  = "aaaaaaaa11111111"
	headA  = "bbbbbbbb22222222"
)

// conflictConfig has vigie, and cocovm with its feature Workspace on the
// same repository.
func conflictConfig() profile.Config {
	c := profile.Defaults()
	var keep []profile.Project
	for _, p := range c.Projects {
		if p.Name == "vigie" || p.Name == "cocovm" || p.Name == "cocovm-3d" {
			keep = append(keep, p)
		}
	}
	c.Projects = keep
	return c
}

func shadowing(projects ...string) rollout.Table {
	var t rollout.Table
	for _, p := range projects {
		t.Rules = append(t.Rules, rollout.Rule{Project: p, Family: run.PRConflict, Owner: rollout.Shadow,
			Evidence: []rollout.Gate{rollout.ReplayParity}})
	}
	return t
}

// conflictPR is an open agent PR of repo from head onto base.
func conflictPR(repo string, n int, head, base string, labels ...string) event.PullRequest {
	return event.PullRequest{Repository: repo, Number: n, State: event.PullRequestOpen, Author: "gordon-cole-ai[bot]",
		HeadRef: head, HeadSHA: headA, HeadRepo: repo, BaseRef: base, BaseSHA: baseA, Labels: labels,
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n), Title: "t"}
}

func dirty(pr event.PullRequest) event.PullRequest {
	no := false
	pr.Mergeable, pr.MergeableState = &no, event.MergeableDirty
	return pr
}

func openListing(prs ...event.PullRequest) []upstream.ListedPR {
	out := make([]upstream.ListedPR, len(prs))
	for i, p := range prs {
		p.BaseSHA = "" // listings carry no base SHA and no mergeability
		out[i] = upstream.ListedPR{PullRequest: p}
	}
	return out
}

func conflictReconciler(g *conflictGitHub, s *conflictInbox, owner rollout.Table) *ConflictReconciler {
	return &ConflictReconciler{GitHub: g, Store: s, Config: conflictConfig(), Ownership: owner,
		Now: func() time.Time { return time.Unix(7200*5+30, 0) }, Metrics: metrics.New(),
		Log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}
}

func TestConflictQueuedOnce(t *testing.T) {
	pr := conflictPR(vigie, 4, "kelos/vikunja-5", "main", "kelos:gpt-sol")
	g := &conflictGitHub{open: map[string][]upstream.ListedPR{vigie: openListing(pr)},
		fresh: map[string]event.PullRequest{prKey(vigie, 4): dirty(pr)}}
	s := &conflictInbox{}
	c := conflictReconciler(g, s, shadowing("vigie"))
	if err := c.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.queued) != 1 {
		t.Fatalf("queued %+v", s.queued)
	}
	episode := route.Episode(vigie, "vigie", "gpt-sol", 4, baseA, headA)
	if want := []string{"pr-conflict-" + episode}; !slices.Equal(s.asked, want) {
		t.Errorf("asked about runs %v, want %v", s.asked, want)
	}
	d := s.queued[0]
	if d.DeliveryKey != "github/delivery/gordon-conflict/sperano/vigie/vigie/"+episode+"/10" {
		t.Errorf("delivery key %q: the episode in its hour", d.DeliveryKey)
	}
	ev, err := event.Parse(event.Delivery{Source: d.Source, Headers: d.Headers, Body: d.Body})
	if err != nil {
		t.Fatal(err)
	}
	gh := ev.GitHub
	if ev.Action != event.ActionConflict || !gh.Synthetic || gh.ConflictWorkspace != "vigie" ||
		gh.PullRequest.BaseSHA != baseA || gh.PullRequest.HeadSHA != headA || gh.PullRequest.BaseRef != "main" {
		t.Errorf("queued event %+v %+v", ev, gh.PullRequest)
	}
	if n := testutil.ToFloat64(c.Metrics.ConflictReconcileQueued); n != 1 {
		t.Errorf("queued counted %v", n)
	}
	// The episode's run is taken: nothing is queued.
	s = &conflictInbox{taken: map[string]bool{"pr-conflict-" + episode: true}}
	c = conflictReconciler(g, s, shadowing("vigie"))
	if err := c.Once(context.Background()); err != nil || len(s.queued) != 0 {
		t.Errorf("taken run: %v, queued %+v", err, s.queued)
	}
}

func TestConflictCandidates(t *testing.T) {
	onMain := conflictPR(vigie, 1, "kelos/vikunja-1", "main", "kelos:follow-up")
	stacked := conflictPR(vigie, 2, "kelos/vikunja-2", "kelos/vikunja-1", "kelos:follow-up")
	stackedTwice := conflictPR(vigie, 3, "kelos/vikunja-3", "kelos/vikunja-2", "kelos:follow-up")
	elsewhere := conflictPR(vigie, 4, "kelos/vikunja-4", "release", "kelos:follow-up")
	drafted := conflictPR(vigie, 5, "kelos/vikunja-5", "main", "kelos:follow-up")
	drafted.Draft = true
	stranger := conflictPR(vigie, 6, "kelos/vikunja-6", "main", "kelos:follow-up")
	stranger.Author = "mallory"
	fork := conflictPR(vigie, 7, "kelos/vikunja-7", "main", "kelos:follow-up")
	fork.HeadRepo = "fork/vigie"
	unlabelled := conflictPR(vigie, 8, "kelos/vikunja-8", "main", "bug")
	twoVariants := conflictPR(vigie, 9, "kelos/vikunja-9", "main", "kelos:gpt-sol", "kelos:deepseek")
	// A stranger's PR is no stack base: a PR on it is not scanned.
	onStranger := conflictPR(vigie, 10, "kelos/vikunja-10", "kelos/vikunja-6", "kelos:follow-up")
	all := []event.PullRequest{onMain, stacked, stackedTwice, elsewhere, drafted, stranger, fork, unlabelled, twoVariants, onStranger}
	fresh := map[string]event.PullRequest{}
	for _, pr := range all {
		fresh[prKey(vigie, pr.Number)] = dirty(pr)
	}
	g := &conflictGitHub{open: map[string][]upstream.ListedPR{vigie: openListing(all...)}, fresh: fresh}
	s := &conflictInbox{}
	if err := conflictReconciler(g, s, shadowing("vigie")).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{prKey(vigie, 1), prKey(vigie, 2), prKey(vigie, 3)}; !slices.Equal(g.reads, want) {
		t.Errorf("read %v, want the PRs on main and stacked on it, only", g.reads)
	}
	if len(s.queued) != 3 {
		t.Errorf("queued %d", len(s.queued))
	}
}

func TestConflictFreshState(t *testing.T) {
	computing := conflictPR(vigie, 1, "kelos/vikunja-1", "main", "kelos:follow-up")
	clean := conflictPR(vigie, 2, "kelos/vikunja-2", "main", "kelos:follow-up")
	draftedSince := conflictPR(vigie, 3, "kelos/vikunja-3", "main", "kelos:follow-up")
	conflicted := conflictPR(vigie, 4, "kelos/vikunja-4", "main", "kelos:follow-up")
	failing := conflictPR(vigie, 5, "kelos/vikunja-5", "main", "kelos:follow-up")
	yes := true
	cleanFresh := clean
	cleanFresh.Mergeable, cleanFresh.MergeableState = &yes, "clean"
	draftFresh := dirty(draftedSince)
	draftFresh.Draft = true
	g := &conflictGitHub{open: map[string][]upstream.ListedPR{vigie: openListing(computing, clean, draftedSince, conflicted, failing)},
		fresh: map[string]event.PullRequest{prKey(vigie, 1): computing, prKey(vigie, 2): cleanFresh,
			prKey(vigie, 3): draftFresh, prKey(vigie, 4): dirty(conflicted)},
		readErr: map[string]error{prKey(vigie, 5): errors.New("github down")}}
	s := &conflictInbox{}
	c := conflictReconciler(g, s, shadowing("vigie"))
	err := c.Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sperano/vigie/vigie: pull request 5: github down") {
		t.Errorf("Once = %v, want the failed read", err)
	}
	if len(s.queued) != 1 || !strings.Contains(s.queued[0].DeliveryKey, "-4-") {
		t.Errorf("queued %+v, want PR 4 alone (and still queued despite PR 5)", s.queued)
	}
	if n := testutil.ToFloat64(c.Metrics.ConflictReconciles.WithLabelValues("error")); n != 1 {
		t.Errorf("errors counted %v", n)
	}
}

// TestConflictScopes: only Workspaces whose conflicts are not legacy's are
// scanned, one listing per repository, and a Workspace sharing its
// repository never scans the other's ref as a stack head.
func TestConflictScopes(t *testing.T) {
	mainPR := conflictPR(cocovm, 1, "kelos/vikunja-1", "main", "kelos:follow-up")
	feature := conflictPR(cocovm, 2, "feature/3d", "main", "kelos:follow-up")
	onFeature := conflictPR(cocovm, 3, "kelos/vikunja-3", "feature/3d", "kelos:follow-up")
	vigiePR := conflictPR(vigie, 4, "kelos/vikunja-4", "main", "kelos:follow-up")
	fresh := map[string]event.PullRequest{}
	for _, pr := range []event.PullRequest{mainPR, feature, onFeature, vigiePR} {
		fresh[prKey(pr.Repository, pr.Number)] = dirty(pr)
	}
	g := &conflictGitHub{open: map[string][]upstream.ListedPR{cocovm: openListing(mainPR, feature, onFeature), vigie: openListing(vigiePR)},
		fresh: fresh}
	s := &conflictInbox{}
	if err := conflictReconciler(g, s, shadowing("cocovm", "cocovm-3d")).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	// cocovm scans main's PRs (the feature branch's own PR included, never
	// as a stack head); cocovm-3d scans the PR on feature/3d.
	if want := []string{prKey(cocovm, 1), prKey(cocovm, 2), prKey(cocovm, 3)}; !slices.Equal(g.reads, want) {
		t.Errorf("read %v, want %v (vigie is legacy's)", g.reads, want)
	}
	var workspaces []string
	for _, d := range s.queued {
		ev, err := event.Parse(event.Delivery{Source: d.Source, Headers: d.Headers, Body: d.Body})
		if err != nil {
			t.Fatal(err)
		}
		workspaces = append(workspaces, fmt.Sprintf("%d@%s", ev.GitHub.PullRequest.Number, ev.GitHub.ConflictWorkspace))
	}
	slices.Sort(workspaces)
	if want := []string{"1@cocovm", "2@cocovm", "3@cocovm-3d"}; !slices.Equal(workspaces, want) {
		t.Errorf("queued %v, want %v", workspaces, want)
	}
}

func TestConflictPassErrors(t *testing.T) {
	vigiePR := conflictPR(vigie, 4, "kelos/vikunja-4", "main", "kelos:follow-up")
	g := &conflictGitHub{open: map[string][]upstream.ListedPR{vigie: openListing(vigiePR)},
		fresh:   map[string]event.PullRequest{prKey(vigie, 4): dirty(vigiePR)},
		listErr: map[string]error{cocovm: errors.New("list down")}}
	s := &conflictInbox{}
	err := conflictReconciler(g, s, shadowing("cocovm", "vigie")).Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sperano/cocovm: open pull requests: list down") || len(s.queued) != 1 {
		t.Errorf("list failure: %v, queued %d (vigie's still queued)", err, len(s.queued))
	}
	s = &conflictInbox{takenErr: errors.New("store down")}
	err = conflictReconciler(g, s, shadowing("vigie")).Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "conflict runs: store down") || len(s.queued) != 0 {
		t.Errorf("runs failure: %v, queued %d", err, len(s.queued))
	}
	// Nothing in conflict reads no runs and queues nothing.
	s = &conflictInbox{takenErr: errors.New("must not be asked")}
	g = &conflictGitHub{}
	if err := conflictReconciler(g, s, shadowing("vigie")).Once(context.Background()); err != nil {
		t.Errorf("empty pass: %v", err)
	}
}
