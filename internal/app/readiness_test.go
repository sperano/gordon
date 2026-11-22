package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/ghlife"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
	"github.com/sperano/gordon/internal/lifecycle"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

func TestReadinessKindsAreRouted(t *testing.T) {
	cfg := config.Worker{Routing: profile.Defaults(), GitHubAPIURL: "https://api.example"}
	cfg.Lifecycle.WriteToken = "w"
	cfg.Readiness = config.Readiness{NtfyURL: "https://ntfy.example", NtfyTopic: "t", NtfyToken: "n"}
	app, err := githubapp.Load(appDir(t), cfg.GitHubAPIURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[action.Kind]worker.Executor{}
	lifecycleExecutors(out, cfg, nil, nil, app)
	if got := providers(t, out[action.Notify]); !slices.Equal(got, []string{upstream.ProviderVikunja}) {
		t.Errorf("notify writes to %v", got)
	}
	if got := providers(t, out[action.MergePullRequest]); !slices.Equal(got, []string{upstream.ProviderGitHub}) {
		t.Errorf("merge writes to %v", got)
	}
	gh := out[action.MergePullRequest].(worker.ByProvider).Executors[upstream.ProviderGitHub].(*ghlife.Executor)
	if _, ok := gh.Vikunja.(*lifecycle.Executor); !ok {
		t.Errorf("a refused merge reports through %T, want the Vikunja writer", gh.Vikunja)
	}
	vk := out[action.Notify].(worker.ByProvider).Executors[upstream.ProviderVikunja].(*lifecycle.Executor)
	if vk.Ntfy == nil || vk.Ntfy.URL != "https://ntfy.example" || vk.Ntfy.Topic != "t" || vk.Ntfy.Token != "n" {
		t.Errorf("ntfy %+v", vk.Ntfy)
	}
	// Without a Vikunja writer, a refused merge has nothing to report
	// through: the field stays a nil interface, not a typed nil.
	cfg.Lifecycle.WriteToken = ""
	out = map[action.Kind]worker.Executor{}
	lifecycleExecutors(out, cfg, nil, nil, app)
	gh = out[action.MergePullRequest].(worker.ByProvider).Executors[upstream.ProviderGitHub].(*ghlife.Executor)
	if gh.Vikunja != nil {
		t.Errorf("Vikunja %v without a writer", gh.Vikunja)
	}
	if n := ntfy(config.Worker{}); n != nil {
		t.Errorf("ntfy %+v without a token", n)
	}
}

func TestReadinessReadersAndReconciler(t *testing.T) {
	cfg := config.Worker{Routing: profile.Defaults(), GitHubAPIURL: "https://api.example"}
	rd := readinessReaders(cfg, nil, nil, upstream.Vikunja{}, upstream.GitHub{})
	if rd.GitHub != nil || rd.Kelos != nil || rd.Reactions == nil || rd.Open == nil {
		t.Errorf("readers without App and Kelos %+v", rd)
	}
	app, err := githubapp.Load(appDir(t), cfg.GitHubAPIURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := kelostest.New(route.KelosNamespace)
	defer srv.Close()
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "a", Labels: map[string]string{route.TaskLabel: "5", route.RoleLabel: "pr-open"},
		Annotations: map[string]string{route.PRAnnotation: "4"}}, Status: &kelos.Status{Phase: kelos.PhaseRunning}})
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "b", Labels: map[string]string{route.TaskLabel: "6"}}})
	k := &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	rd = readinessReaders(cfg, k, app, upstream.Vikunja{}, upstream.GitHub{})
	if _, ok := rd.GitHub.(ghlife.Reads); !ok || rd.Kelos == nil {
		t.Fatalf("readers %+v", rd)
	}
	tasks, err := rd.Kelos.Tasks(context.Background(), route.TaskLabel+"=5")
	want := []route.KelosTask{{Name: "a", Phase: kelos.PhaseRunning, Labels: map[string]string{route.TaskLabel: "5",
		route.RoleLabel: "pr-open"}, Annotations: map[string]string{route.PRAnnotation: "4"}}}
	if err != nil || len(tasks) != 1 || tasks[0].Name != want[0].Name || tasks[0].Phase != want[0].Phase ||
		tasks[0].Labels[route.RoleLabel] != "pr-open" || tasks[0].Annotations[route.PRAnnotation] != "4" {
		t.Errorf("Tasks = %+v, %v; want %+v", tasks, err, want)
	}
	if r := readinessReconciler(cfg, nil, upstream.GitHub{}, nil, discardLog()); r != nil {
		t.Fatalf("readiness reconciler %+v with no interval", r)
	}
	cfg.Readiness.ReconcileInterval = time.Minute
	if r := readinessReconciler(cfg, nil, upstream.GitHub{}, nil, discardLog()); r == nil || r.Interval != time.Minute ||
		len(r.Config.Projects) == 0 {
		t.Fatalf("readiness reconciler %+v", r)
	}
}
