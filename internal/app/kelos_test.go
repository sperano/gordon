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
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
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
	_, wm := httpGet(t, "http://"+svc.worker[ServerAdmin]+"/metrics")
	for _, want := range []string{`gordon_kelos_submissions_total{result="created"} 1`,
		`gordon_kelos_requests_total{op="create",result="ok"} 1`, `gordon_run_observations_total{state="finished"} 1`} {
		if !strings.Contains(wm, want) {
			t.Errorf("worker metrics lack %s", want)
		}
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
