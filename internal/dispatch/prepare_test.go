package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
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
	body     string
	err      error
	reads    int
	maxBytes int // the budget of the last read
}

func (c *countingComments) Comments(_ context.Context, _ int64, maxBytes int) (json.RawMessage, error) {
	c.reads++
	c.maxBytes = maxBytes
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
		"fetched":      {&countingComments{body: "[]\n"}, 0, "<vikunja-comments>\n[]\n\n</vikunja-comments>"},
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
			wantBudget := dispatch.DefaultCommentsMaxBytes
			switch {
			case tc.max < 0:
				wantBudget = 0
			case tc.max > 0:
				wantBudget = tc.max
			}
			if tc.reader.maxBytes != wantBudget {
				t.Fatalf("read with a budget of %d bytes, want %d", tc.reader.maxBytes, wantBudget)
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

// unbuildable is a dispatch config edit and the reject it makes.
type unbuildable struct {
	edit   func(*dispatch.Config)
	sample sample
	reason string
}

// withReviewPod edits the pr-review family's pod pieces.
func withReviewPod(edit func(*dispatch.Pod), reason string) unbuildable {
	return unbuildable{func(c *dispatch.Config) {
		f := c.Families[run.PRReview]
		edit(&f.Pod)
		c.Families[run.PRReview] = f
	}, prOpened(), reason}
}

// TestUnbuildableTaskRejectsTheFamily: a Task the configuration cannot
// build (or admission would refuse) is an explicit reject naming why,
// with no run and no write left, the 👀 included.
func TestUnbuildableTaskRejectsTheFamily(t *testing.T) {
	cases := map[string]unbuildable{
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
		"pod value not JSON": {func(c *dispatch.Config) {
			f := c.Families[run.PRReview]
			f.Pod.Env = append(f.Pod.Env, json.RawMessage(`{"name":"X","value":"{{.task}}`))
			c.Families[run.PRReview] = f
		}, prOpened(), "task-spec-pod-invalid"},
		"variant not configured": {func(c *dispatch.Config) { delete(c.Variants, "claude-opus") },
			taskUpdated(vikunjaTask(gordonID), "claude-opus"), "task-spec-unknown-variant"},
		"profile not configured": {func(c *dispatch.Config) { delete(c.Profiles, "review") },
			prOpened(), "task-spec-unknown-profile"},
		"secret volume not granted": withReviewPod(func(p *dispatch.Pod) {
			p.Volumes = append(p.Volumes, json.RawMessage(`{"name":"v","secret":{"secretName":"vault-root"}}`))
		}, "task-spec-secret-not-granted"),
		"projected secret not granted": withReviewPod(func(p *dispatch.Pod) {
			p.Volumes = append(p.Volumes, json.RawMessage(`{"name":"v","projected":{"sources":[{"secret":{"name":"vault-root"}}]}}`))
		}, "task-spec-secret-not-granted"),
		"csi secret not granted": withReviewPod(func(p *dispatch.Pod) {
			p.Volumes = append(p.Volumes, json.RawMessage(`{"name":"v","csi":{"driver":"d","nodePublishSecretRef":{"name":"vault-root"}}}`))
		}, "task-spec-secret-not-granted"),
		"init envFrom not granted": withReviewPod(func(p *dispatch.Pod) {
			p.ExtraInitContainers = append(p.ExtraInitContainers, json.RawMessage(`{"name":"i","envFrom":[{"secretRef":{"name":"vault-root"}}]}`))
		}, "task-spec-secret-not-granted"),
		"env entry not an object": withReviewPod(func(p *dispatch.Pod) {
			p.Env = append(p.Env, json.RawMessage(`"FOO=bar"`))
		}, "task-spec-pod-invalid"),
		"volume not an object": withReviewPod(func(p *dispatch.Pod) {
			p.Volumes = append(p.Volumes, json.RawMessage(`"vault-root"`))
		}, "task-spec-pod-invalid"),
		"init container not an object": withReviewPod(func(p *dispatch.Pod) {
			p.ExtraInitContainers = append(p.ExtraInitContainers, json.RawMessage(`"busybox"`))
		}, "task-spec-pod-invalid"),
		"in-tree volume secret not granted": withReviewPod(func(p *dispatch.Pod) {
			p.Volumes = append(p.Volumes, json.RawMessage(`{"name":"v","rbd":{"image":"i","secretRef":{"name":"vault-root"}}}`))
		}, "task-spec-secret-not-granted"),
		"cephfs secret not granted":     inTreeVolume(`cephfs`, `{"monitors":["m"],"secretRef":{"name":"vault-root"}}`),
		"iscsi secret not granted":      inTreeVolume(`iscsi`, `{"targetPortal":"t","iqn":"q","lun":0,"secretRef":{"name":"vault-root"}}`),
		"cinder secret not granted":     inTreeVolume(`cinder`, `{"volumeID":"v","secretRef":{"name":"vault-root"}}`),
		"flexVolume secret not granted": inTreeVolume(`flexVolume`, `{"driver":"d","secretRef":{"name":"vault-root"}}`),
		"scaleIO secret not granted":    inTreeVolume(`scaleIO`, `{"gateway":"g","system":"s","secretRef":{"name":"vault-root"}}`),
		"storageos secret not granted":  inTreeVolume(`storageos`, `{"volumeName":"v","secretRef":{"name":"vault-root"}}`),
		"null env": withReviewPod(func(p *dispatch.Pod) {
			p.Env = append(p.Env, json.RawMessage(`null`))
		}, "task-spec-pod-invalid"),
		"null volume": withReviewPod(func(p *dispatch.Pod) {
			p.Volumes = append(p.Volumes, json.RawMessage(`null`))
		}, "task-spec-pod-invalid"),
		"null init container": withReviewPod(func(p *dispatch.Pod) {
			p.ExtraInitContainers = append(p.ExtraInitContainers, json.RawMessage(`null`))
		}, "task-spec-pod-invalid"),
		"azure file secret not granted": withReviewPod(func(p *dispatch.Pod) {
			p.Volumes = append(p.Volumes, json.RawMessage(`{"name":"v","azureFile":{"shareName":"s","secretName":"vault-root"}}`))
		}, "task-spec-secret-not-granted"),
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

// inTreeVolume adds to the review pod an in-tree volume of that type
// whose source is src.
func inTreeVolume(kind, src string) unbuildable {
	return withReviewPod(func(p *dispatch.Pod) {
		p.Volumes = append(p.Volumes, json.RawMessage(`{"name":"v","`+kind+`":`+src+`}`))
	}, "task-spec-secret-not-granted")
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

// checkRequestIDs checks a PR follow-up Task names its request as the
// legacy Task does: a review by its review annotation, a comment by its
// comment annotation.
func checkRequestIDs(t *testing.T, rn route.Run) {
	t.Helper()
	if rn.Identity.Family != run.PRFollowUp {
		return
	}
	id := strconv.FormatInt(rn.Identity.CommentID, 10)
	want := map[string]string{"vikunja.spe.quebec/comment": id, "vikunja.spe.quebec/review": ""}
	if rn.Identity.CommentKind == string(rune(event.CommentKindReview)) {
		want = map[string]string{"vikunja.spe.quebec/comment": "", "vikunja.spe.quebec/review": id}
	}
	for k, v := range want {
		if got := rn.Task.Metadata.Annotations[k]; got != v {
			t.Errorf("run %s (%s %s): %s = %q, want %q", rn.Name, rn.Identity.CommentKind, id, k, got, v)
		}
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
					checkRequestIDs(t, rn)
					built++
				}
			}
		})
	}
	if built == 0 {
		t.Fatal("no fixture dispatched a run")
	}
}
