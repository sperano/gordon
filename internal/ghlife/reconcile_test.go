package ghlife

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/upstream"
)

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// ago is a time in the window unless d is under the grace or over the
// lookback.
func ago(d time.Duration) time.Time { return now.Add(-d) }

type fakeLister struct {
	open     map[string][]upstream.ListedPR
	closed   map[string][]upstream.ListedPR
	reviews  map[int][]upstream.ListedReview
	comments map[bool][]upstream.ListedComment
	fail     string
}

func (f *fakeLister) OpenPullRequests(_ context.Context, repo string) ([]upstream.ListedPR, error) {
	if repo == f.fail {
		return nil, errors.New("github down")
	}
	return f.open[repo], nil
}

func (f *fakeLister) ClosedPullRequests(_ context.Context, repo string, _ time.Time) ([]upstream.ListedPR, error) {
	return f.closed[repo], nil
}

func (f *fakeLister) Reviews(_ context.Context, _ string, n int) ([]upstream.ListedReview, error) {
	return f.reviews[n], nil
}

func (f *fakeLister) CommentsSince(_ context.Context, repo string, _ time.Time, inline bool) ([]upstream.ListedComment, error) {
	if repo != "sperano/vigie" {
		return nil, nil
	}
	return f.comments[inline], nil
}

// fakeInbox is the store's inbox: a key is stored once.
type fakeInbox struct {
	stored  []store.Delivery
	handled map[string]bool
}

func (f *fakeInbox) Enqueue(_ context.Context, ds []store.Delivery) (int, error) {
	n := 0
	for _, d := range ds {
		if !slices.ContainsFunc(f.stored, func(s store.Delivery) bool { return s.DeliveryKey == d.DeliveryKey }) {
			f.stored = append(f.stored, d)
			n++
		}
	}
	return n, nil
}

func (f *fakeInbox) KnownDeliveries(context.Context, []string) (map[string]bool, error) {
	return nil, nil
}

func (f *fakeInbox) HandledRequests(_ context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = f.handled[k]
	}
	return out, nil
}

var _ lifecycle.Inbox = (*fakeInbox)(nil)

func config() profile.Config {
	c := profile.Defaults()
	var keep []profile.Project
	for _, p := range c.Projects {
		if p.Repository == "sperano/vigie" || p.Repository == "sperano/gordon" {
			keep = append(keep, p)
		}
	}
	c.Projects = keep
	return c
}

func agentPR(n int, author, head string) upstream.ListedPR {
	return upstream.ListedPR{PullRequest: event.PullRequest{Repository: "sperano/vigie", Number: n, State: "open",
		Author: author, HeadRef: head, HeadSHA: "abc12345", HeadRepo: "sperano/vigie", BaseRef: "main"}}
}

func listed(id int64, author, body string, at time.Time) upstream.ListedReview {
	return upstream.ListedReview{Review: event.Review{ID: id, Author: author, Body: body, CommitID: "abc12345"},
		NodeID: "PRR_x", Submitted: at}
}

func reconciler(l *fakeLister, in *fakeInbox) (*Reconciler, *metrics.Metrics) {
	m := metrics.New()
	return &Reconciler{GitHub: l, Inbox: in, Config: config(), Lookback: 24 * time.Hour, Grace: 10 * time.Minute,
		Now: func() time.Time { return now }, Metrics: m, Log: logging.New(&bytes.Buffer{}, slog.LevelDebug)}, m
}

func keysOf(ds []store.Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.DeliveryKey
	}
	return out
}

func TestReconcileRecoversMissedRequests(t *testing.T) {
	merged := agentPR(9, "gordon-cole-ai[bot]", "kelos/vikunja-41")
	merged.State, merged.Merged, merged.Closed = "closed", true, ago(time.Hour)
	feature := agentPR(10, "sperano", "feature")
	feature.State, feature.Closed = "closed", ago(time.Hour)
	l := &fakeLister{
		open: map[string][]upstream.ListedPR{"sperano/vigie": {agentPR(4, "sperano", "kelos/vikunja-5"),
			agentPR(5, "mallory", "kelos/vikunja-6")}},
		closed: map[string][]upstream.ListedPR{"sperano/vigie": {merged, feature}},
		reviews: map[int][]upstream.ListedReview{
			4: {listed(1, "cubic-dev-ai[bot]", "**2 issues found**", ago(3*time.Hour)),
				listed(2, "cubic-dev-ai[bot]", "**1 issue found**", ago(2*time.Hour)),
				listed(3, "cubic-dev-ai[bot]", "", ago(time.Hour)), // a thread reply
				listed(4, "sperano", "/gordon fix the tests", ago(time.Hour)),
				listed(5, "sperano", "looks good", ago(time.Hour)),
				listed(6, "gordon-cole-ai[bot]", "**Review verdict: blocking**", ago(time.Minute))}, // within the grace
			5: {listed(7, "cubic-dev-ai[bot]", "**1 issue found**", ago(time.Hour))}, // a stranger's PR
		},
		comments: map[bool][]upstream.ListedComment{
			false: {{ID: 70, Author: "sperano", Body: "/gordon please", PR: 4, Created: ago(time.Hour)},
				{ID: 71, Author: "mallory", Body: "/gordon please", PR: 4, Created: ago(time.Hour)},
				{ID: 72, Author: "sperano", Body: "/gordon old", PR: 4, Created: ago(25 * time.Hour)},
				{ID: 73, Author: "sperano", Body: "/gordon other", PR: 99, Created: ago(time.Hour)}},
			true: {{ID: 80, ReviewID: 4, Author: "sperano", Body: "/gordon here", PR: 4, Created: ago(time.Hour)}},
		},
	}
	in := &fakeInbox{handled: map[string]bool{"github/sperano/vigie/r/4": true}}
	r, m := reconciler(l, in)
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"github/delivery/gordon-reconcile/sperano/vigie/r/2",
		"github/delivery/gordon-reconcile/sperano/vigie/c/70",
		"github/delivery/gordon-reconcile/sperano/vigie/l/80",
		"github/delivery/gordon-reconcile/sperano/vigie/closed/9",
	}
	if got := keysOf(in.stored); !slices.Equal(got, want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
	if err := r.Once(context.Background()); err != nil || len(in.stored) != len(want) {
		t.Fatalf("a second pass queued again: %v, %v", err, keysOf(in.stored))
	}
	if got := testutil.ToFloat64(m.GitHubReconcileQueued); got != float64(len(want)) {
		t.Errorf("queued metric %v", got)
	}
	if got := testutil.ToFloat64(m.GitHubReconciles.WithLabelValues(lifecycle.ReconcileOK)); got != 2 {
		t.Errorf("ok passes %v", got)
	}
}

// TestRecoveredDeliveriesRouteAsWebhooks: each synthetic delivery carries
// the webhook's request key and is decided as the webhook would be.
func TestRecoveredDeliveriesRouteAsWebhooks(t *testing.T) {
	pr := agentPR(4, "sperano", "kelos/vikunja-5")
	rv, err := reviewDelivery(pr, listed(2, "cubic-dev-ai[bot]", "**1 issue found**", ago(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := commentDelivery(pr, upstream.ListedComment{ID: 70, NodeID: "IC_70", Author: "sperano", Body: "/gordon please", PR: 4}, false)
	if err != nil {
		t.Fatal(err)
	}
	merged := pr
	merged.State, merged.Merged = "closed", true
	cl, err := closedDelivery(merged)
	if err != nil {
		t.Fatal(err)
	}
	r := route.Router{Config: config()}
	fresh := pr.PullRequest
	task := event.Task{ID: 5, ProjectID: 1}
	cases := []struct {
		d       store.Delivery
		st      route.State
		request string
		outcome route.Outcome
	}{
		{rv, route.State{PullRequest: &fresh, History: &route.PRHistory{}}, "github/sperano/vigie/r/2", route.Dispatch},
		{c, route.State{PullRequest: &fresh}, "github/sperano/vigie/c/70", route.Dispatch},
		{cl, route.State{PullRequest: &merged.PullRequest, Task: &task}, "github/sperano/vigie/pull/4/closed/abc12345", route.Act},
	}
	for _, tc := range cases {
		if tc.d.RequestKey != tc.request || tc.d.Headers[store.SyntheticHeader] != store.SyntheticReconcile {
			t.Errorf("%s: request %q headers %v", tc.d.DeliveryKey, tc.d.RequestKey, tc.d.Headers)
		}
		d := r.DecideAuthenticated(event.Delivery{Source: tc.d.Source, Headers: tc.d.Headers, Body: tc.d.Body}, tc.st, replay.Ledger{})
		if d.Outcome != tc.outcome {
			t.Errorf("%s decided %s/%s, want %s", tc.d.DeliveryKey, d.Outcome, d.Reason, tc.outcome)
		}
	}
}

func TestReconcileKeepsGoingPastAFailingRepository(t *testing.T) {
	l := &fakeLister{fail: "sperano/gordon", closed: map[string][]upstream.ListedPR{}, reviews: map[int][]upstream.ListedReview{},
		open:     map[string][]upstream.ListedPR{"sperano/vigie": {agentPR(4, "sperano", "kelos/vikunja-5")}},
		comments: map[bool][]upstream.ListedComment{false: {{ID: 70, Author: "sperano", Body: "/gordon please", PR: 4, Created: ago(time.Hour)}}}}
	in := &fakeInbox{}
	r, m := reconciler(l, in)
	err := r.Once(context.Background())
	if err == nil || err.Error() != "sperano/gordon: open pull requests: github down" {
		t.Fatalf("error %v", err)
	}
	if got := keysOf(in.stored); !slices.Equal(got, []string{"github/delivery/gordon-reconcile/sperano/vigie/c/70"}) {
		t.Errorf("the healthy repository's recovery was dropped: %v", got)
	}
	if got := testutil.ToFloat64(m.GitHubReconciles.WithLabelValues(lifecycle.ReconcileError)); got != 1 {
		t.Errorf("error passes %v", got)
	}
}
