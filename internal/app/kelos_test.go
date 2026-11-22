package app

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/restart"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store/storetest"
	"github.com/sperano/gordon/internal/submit"
)

const kelosToken = "kelos-sa-token"

func dispatchFixture(t *testing.T) dispatch.Config {
	t.Helper()
	b, err := os.ReadFile("../dispatch/testdata/hollingsworth.json")
	if err != nil {
		t.Fatal(err)
	}
	var c dispatch.Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestServiceSubmitsToKelos: a delivery Gordon owns goes from the
// receiver through the worker's decision and outbox to a Task in Kelos,
// and the reconciler follows the Task to its end.
func TestServiceSubmitsToKelos(t *testing.T) {
	srv := kelostest.New(route.KelosNamespace)
	defer srv.Close()
	token := writeFile(t, t.TempDir(), "token", kelosToken+"\n")
	svc := startService(t, func(c *config.Worker) {
		c.Shadow = false
		c.Ownership = rollout.Table{Rules: []rollout.Rule{{Project: rollout.AllProjects, Family: run.VikunjaImplement,
			Owner: rollout.Gordon, Evidence: []rollout.Gate{rollout.ReplayParity, rollout.ShadowParity,
				rollout.RollbackTested, rollout.LegacyDisabled}}}}
		c.Kelos = &config.Kelos{Dispatch: dispatchFixture(t), APIURL: srv.URL, TokenFile: token,
			BusyDelay: time.Second, ShadowWindow: time.Hour}
	})
	if code := svc.post(t, taskBody); code != http.StatusAccepted {
		t.Fatalf("receiver answered %d, want 202", code)
	}
	const name = "vikunja-hollingsworth-claude-opus-4101"
	eventually(t, "the Task", func() bool { _, ok := srv.Task(name); return ok })
	task, _ := srv.Task(name)
	if task.Metadata.Labels[dispatch.ProfileLabel] != "image-build-submitter" ||
		task.Spec.Worker.PodOverrides.ServiceAccountName != "image-build-submitter" ||
		!strings.Contains(task.Spec.Prompt, `<p>earlier</p>`) || !strings.Contains(task.Spec.Prompt, "in project Hollingsworth: Ship it") {
		t.Fatalf("submitted Task %+v\n%s", task.Metadata, task.Spec.Prompt)
	}
	// The submitted Task's labels, annotations and spec are the decided
	// one's (kelos.Diff leaves kelos.dev/ labels out): the dispatch tests
	// check that Task against the legacy spawners'.
	var decided kelos.Task
	storetest.QueryRow(t, svc.ownerURL, `SELECT task FROM runs WHERE name = $1`, []any{name}, &decided)
	if diff, err := kelos.Diff(decided, srv.Raw(name)); err != nil || len(diff) != 0 {
		t.Fatalf("submitted Task differs from the decided one at %v, %v", diff, err)
	}
	if srv.Authorization() != "Bearer "+kelosToken {
		t.Fatalf("Kelos Authorization %q", srv.Authorization())
	}
	if calls := svc.upstream.list(); !slices.Contains(calls, "GET /tasks/4101/comments") {
		t.Fatalf("upstream calls %v lack the comments read", calls)
	}
	eventually(t, "the active run", func() bool {
		return svc.count(t, `SELECT count(*) FROM runs WHERE state = 'active' AND task_exists`) == 1
	})
	srv.SetPhase(name, kelos.PhaseSucceeded)
	eventually(t, "the finished run", func() bool {
		return svc.count(t, `SELECT count(*) FROM runs WHERE state = 'finished' AND task_exists`) == 1
	})
	// The observation is counted after its write: scrape until it shows.
	// Each sample ends its line: "... 1" must not match "... 10".
	for _, want := range []string{"gordon_kelos_submissions_total{result=\"created\"} 1\n",
		"gordon_kelos_requests_total{op=\"create\",result=\"ok\"} 1\n", "gordon_run_observations_total{state=\"finished\"} 1\n"} {
		eventually(t, want, func() bool {
			_, wm := httpGet(t, "http://"+svc.worker[ServerAdmin]+"/metrics")
			return strings.Contains(wm, want)
		})
	}
	svc.cancel()
	for range 2 {
		if err := <-svc.done; err != nil {
			t.Fatalf("mode exited with %v", err)
		}
	}
	if strings.Contains(svc.logs.String(), kelosToken) || strings.Contains(svc.logs.String(), "<p>earlier</p>") {
		t.Fatal("a token or a comment reached the log")
	}
}

// TestPassesOutsideShadow: the observer (its completion record first, then
// the failure notices) and the limit restarts run only outside shadow.
func TestPassesOutsideShadow(t *testing.T) {
	cfg := config.Worker{Shadow: true, Routing: profile.Defaults(), Kelos: &config.Kelos{}, Restart: config.Restart{Max: 4, Grace: time.Minute,
		Fallback: time.Hour}}
	k := &kelos.Client{}
	if got := passes(cfg, nil, k, nil, discardLog()); len(got) != 1 {
		t.Fatalf("shadow passes %v, want the comparison only", got)
	}
	if got := passes(cfg, nil, nil, nil, discardLog()); got != nil {
		t.Fatalf("passes without Kelos %v", got)
	}
	cfg.Shadow = false
	got := passes(cfg, nil, k, nil, discardLog())
	if len(got) != 3 {
		t.Fatalf("passes %v", got)
	}
	obs, ok := got[1].(*submit.Observer)
	if !ok || len(obs.Finished) != 2 {
		t.Fatalf("observer %+v", got[1])
	}
	if _, ok := obs.Finished[0].(*submit.Completions); !ok {
		t.Errorf("first hook %T, want the completion record", obs.Finished[0])
	}
	if f, ok := obs.Finished[1].(*lifecycle.Failures); !ok || len(f.Projects) == 0 {
		t.Errorf("second hook %T, want the failure notices with the projects", obs.Finished[1])
	}
	r, ok := got[2].(*restart.Reaper)
	if !ok || r.MaxRestarts != 4 || r.Grace != time.Minute || r.Fallback != time.Hour || len(r.Config.Projects) == 0 {
		t.Errorf("limit restarts %+v", got[2])
	}
}
