package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// countingComments answers with body (or err) and counts its reads.
type countingComments struct {
	body  string
	err   error
	reads int
}

func (c *countingComments) Comments(context.Context, int64) (json.RawMessage, error) {
	c.reads++
	return json.RawMessage(c.body), c.err
}

func prepareWith(t *testing.T, cfg dispatch.Config, cr dispatch.CommentReader, s sample) route.Decision {
	t.Helper()
	routing := profile.Defaults()
	dec := route.Router{Config: routing}.DecideAuthenticated(s.delivery, s.state, ledger{})
	ev, err := event.Parse(s.delivery)
	if err != nil {
		t.Fatal(err)
	}
	dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}, Comments: cr}.
		Prepare(context.Background(), inboxID, ev, s.state, &dec)
	return dec
}

func TestCommentsQuotedOrLeftEmpty(t *testing.T) {
	big := "[" + strings.Repeat(`{"id":1},`, 100) + `{"id":2}]`
	cases := map[string]struct {
		reader  *countingComments
		max     int
		comment string
	}{
		"fetched":      {&countingComments{body: " [] \n"}, 0, "<vikunja-comments>\n[]\n</vikunja-comments>"},
		"fetch failed": {&countingComments{err: errors.New("vikunja down")}, 0, "<vikunja-comments>\n\n</vikunja-comments>"},
		"over the cap": {&countingComments{body: big}, len(big) - 1, "<vikunja-comments>\n\n</vikunja-comments>"},
		"at the cap":   {&countingComments{body: big}, len(big), "<vikunja-comments>\n" + big + "\n</vikunja-comments>"},
		"comments off": {&countingComments{body: "[]"}, -1, "<vikunja-comments>\n\n</vikunja-comments>"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := fixture(t)
			switch {
			case tc.max < 0:
				cfg.Comments = false
			case tc.max > 0:
				cfg.CommentsMaxBytes = tc.max
			}
			dec := prepareWith(t, cfg, tc.reader, taskUpdated(vikunjaTask(gordonID), "claude-opus"))
			prompt := dec.Families[0].Runs[0].Task.Spec.Prompt
			if !strings.Contains(prompt, tc.comment) {
				t.Fatalf("prompt lacks %q:\n%s", tc.comment, prompt)
			}
			want := 1
			if tc.max < 0 {
				want = 0
			}
			if tc.reader.reads != want {
				t.Fatalf("%d comment reads, want %d", tc.reader.reads, want)
			}
		})
	}
}

func TestNoCommentReadForGitHubRuns(t *testing.T) {
	cr := &countingComments{body: "[]"}
	dec := prepareWith(t, fixture(t), cr, prOpened())
	if cr.reads != 0 || dec.Families[0].Runs[0].Task == nil {
		t.Fatalf("%d comment reads for a review, Task %v", cr.reads, dec.Families[0].Runs[0].Task)
	}
}

// TestUnbuildableTaskRejectsTheFamily: a Task the configuration cannot
// build (or admission would refuse) is an explicit reject naming why,
// with no run and no write left, the 👀 included.
func TestUnbuildableTaskRejectsTheFamily(t *testing.T) {
	cases := map[string]struct {
		edit   func(*dispatch.Config)
		sample sample
		reason string
	}{
		"no template": {func(c *dispatch.Config) { delete(c.Families, run.VikunjaFollowUp) },
			commentCreated(vikunjaTask(gordonID, "in progress")), "task-spec-no-task-template"},
		"missing variable": {func(c *dispatch.Config) {
			f := c.Families[run.VikunjaImplement]
			f.Prompt = "Task {{.task}} for {{.nobody}}"
			c.Families[run.VikunjaImplement] = f
		}, taskUpdated(vikunjaTask(gordonID), "claude-opus"), "task-spec-task-template-invalid"},
		"bad template": {func(c *dispatch.Config) {
			f := c.Families[run.VikunjaImplement]
			f.Labels = map[string]string{"x": "{{.task"}
			c.Families[run.VikunjaImplement] = f
		}, taskUpdated(vikunjaTask(gordonID), "claude-opus"), "task-spec-task-template-invalid"},
		"secret not granted": {func(c *dispatch.Config) {
			p := c.Profiles["puckdb"]
			p.Secrets = []string{"github-token", "kelos-dispatch-image-build", "kelos-github-readonly"}
			c.Profiles["puckdb"] = p
		}, taskUpdated(vikunjaTask(puckdbID), "claude-opus"), "task-spec-secret-not-granted"},
		"variant identity not granted": {func(c *dispatch.Config) {
			c.CommonSecrets = []string{"claude-oauth"}
		}, prComment(), "task-spec-secret-not-granted"},
		"pod template invalid": {func(c *dispatch.Config) {
			f := c.Families[run.PRReview]
			f.Pod.Env = append(f.Pod.Env, json.RawMessage(`{"name":"X","value":"{{.nope}}"}`))
			c.Families[run.PRReview] = f
		}, prOpened(), "task-spec-task-template-invalid"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := fixture(t)
			tc.edit(&cfg)
			dec := prepareWith(t, cfg, fakeComments("[]"), tc.sample)
			fd := dec.Families[0]
			if fd.Outcome != route.Reject || fd.Reason != tc.reason || len(fd.Runs) != 0 || len(fd.Actions) != 0 ||
				dec.Outcome != route.Reject {
				t.Fatalf("family %s %s runs %d actions %d, overall %s; want reject %s with nothing left",
					fd.Outcome, fd.Reason, len(fd.Runs), len(fd.Actions), dec.Outcome, tc.reason)
			}
		})
	}
}

func TestBuildRefusals(t *testing.T) {
	b := dispatch.Builder{Routing: profile.Defaults(), Config: fixture(t)}
	rn, _ := prepared(t, b.Config, taskUpdated(vikunjaTask(gordonID), "claude-opus"), run.VikunjaImplement)
	rn.Identity.Project = "nowhere"
	_, err := b.Build(dispatch.Input{Run: rn})
	var e *dispatch.Error
	if !errors.As(err, &e) || e.Code != dispatch.CodeUnknownProject {
		t.Fatalf("Build = %v, want %s", err, dispatch.CodeUnknownProject)
	}
}

func TestPodTemplatesRendered(t *testing.T) {
	cfg := fixture(t)
	f := cfg.Families[run.VikunjaImplement]
	// A literal kept byte for byte, a template rendered inside a nested value.
	f.Pod.Env = []json.RawMessage{json.RawMessage(`{"name": "LITERAL",  "value": "x"}`),
		json.RawMessage(`{"name":"NESTED","valueFrom":{"secretKeyRef":{"name":"vikunja-agent-{{.variant}}","key":"{{.task}}"}}}`)}
	cfg.Families[run.VikunjaImplement] = f
	rn, _ := prepared(t, cfg, taskUpdated(vikunjaTask(gordonID), "claude-opus"), run.VikunjaImplement)
	env := rn.Task.Spec.Worker.PodOverrides.Env
	if string(env[0]) != `{"name": "LITERAL",  "value": "x"}` ||
		string(env[1]) != `{"name":"NESTED","valueFrom":{"secretKeyRef":{"key":"1624","name":"vikunja-agent-claude-opus"}}}` {
		t.Fatalf("env %s / %s", env[0], env[1])
	}
}

// TestReplayFixturesBuild prepares every legacy replay fixture's decision:
// each dispatched run gets a Task named as the legacy spawner's Task, and
// the recorded decision is exactly the fixture's (the Task is not part
// of it).
func TestReplayFixturesBuild(t *testing.T) {
	fixtures, err := replay.Load("../replay/testdata")
	if err != nil {
		t.Fatal(err)
	}
	cfg, built := fixture(t), 0
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			dec, err := f.Run()
			if err != nil {
				t.Fatal(err)
			}
			d, err := f.Build()
			if err != nil {
				t.Fatal(err)
			}
			ev, err := event.Parse(d)
			if err != nil {
				return // refused before routing: nothing to build
			}
			r := f.Router()
			dispatch.Preparer{Builder: dispatch.Builder{Routing: r.Config, Config: cfg}, Comments: fakeComments("[]")}.
				Prepare(context.Background(), inboxID, ev, f.State, &dec)
			got, _ := json.Marshal(dec)
			want, _ := json.Marshal(f.Expect)
			if string(got) != string(want) {
				t.Fatalf("prepared decision\n%s\nwant\n%s", got, want)
			}
			for _, fd := range dec.Families {
				for _, rn := range fd.Runs {
					if rn.Task == nil || rn.Task.Metadata.Name != rn.Name || !slices.Contains(f.Legacy.Tasks, rn.Name) {
						t.Fatalf("run %s: Task %v, legacy Tasks %v", rn.Name, rn.Task, f.Legacy.Tasks)
					}
					built++
				}
			}
		})
	}
	if built == 0 {
		t.Fatal("no fixture dispatched a run")
	}
}
