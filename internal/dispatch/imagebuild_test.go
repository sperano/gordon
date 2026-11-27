package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// The image-build fixtures: testdata/image-builds.json is the routing
// catalogue of every image with onPush that Hollingsworth db28a49's
// values-hollingsworth.yaml lists, the hollingsworth.json image-build-push
// family and image-build-runner profile pod are the worker those push
// spawners share, and testdata/tasks/image-build-push-<image>.json is the
// Task each image-build-<image>-push spawner renders for a push of
// headSHA (its nameTemplate and promptTemplate evaluated as Kelos does).

// imageBuildRouting is the routing defaults with the fixture catalogue.
func imageBuildRouting(t *testing.T) profile.Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "image-builds.json"))
	if err != nil {
		t.Fatal(err)
	}
	routing := profile.Defaults()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&routing.ImageBuilds); err != nil {
		t.Fatal(err)
	}
	if err := routing.ValidateImageBuilds(); err != nil {
		t.Fatalf("catalogue refused: %v", err)
	}
	return routing
}

// pushed is a push of headSHA to main of repo changing files.
func pushed(repo string, files ...string) sample {
	return sample{githubIn(repo, "push", map[string]any{"ref": "refs/heads/main", "after": headSHA,
		"sender":  map[string]string{"login": "sperano"},
		"commits": []map[string]any{{"added": []string{}, "removed": []string{}, "modified": files}}}), route.State{}}
}

// imageBuilt routes and prepares a push and returns the run of image.
func imageBuilt(t *testing.T, routing profile.Config, cfg dispatch.Config, s sample, image string) route.Run {
	t.Helper()
	dec := route.Router{Config: routing}.DecideAuthenticated(s.delivery, s.state, ledger{})
	ev, err := event.Parse(s.delivery)
	if err != nil {
		t.Fatal(err)
	}
	dispatch.Preparer{Builder: dispatch.Builder{Routing: routing, Config: cfg}}.Prepare(context.Background(), inboxID, ev, s.state, &dec)
	for _, fd := range dec.Families {
		if fd.Family == run.ImageBuildPush && len(fd.Runs) == 1 && fd.Runs[0].Identity.Image == image {
			return fd.Runs[0]
		}
	}
	t.Fatalf("no %s run in %+v", image, dec)
	return route.Run{}
}

// matchingFile is a changed file the image's include patterns match.
func matchingFile(img profile.ImageBuild) string {
	if img.FilePatterns == nil || len(img.FilePatterns.Include) == 0 {
		return "README.md"
	}
	return strings.ReplaceAll(img.FilePatterns.Include[0], "**", "Dockerfile")
}

// TestImageBuildTasksMatchPushSpawners builds every catalogued image's
// Task for a push and compares it with the push spawner's: same name,
// labels and spec, Gordon's own keys aside; no variant label, as no
// agent runs.
func TestImageBuildTasksMatchPushSpawners(t *testing.T) {
	routing, cfg := imageBuildRouting(t), fixture(t)
	for _, img := range routing.ImageBuilds.Images {
		t.Run(img.Name, func(t *testing.T) {
			rn := imageBuilt(t, routing, cfg, pushed(img.Repository, matchingFile(img)), img.Name)
			want := golden(t, "image-build-push-"+img.Name)
			var meta struct {
				Metadata kelos.Metadata `json:"metadata"`
			}
			if err := json.Unmarshal(want, &meta); err != nil {
				t.Fatal(err)
			}
			if rn.Task.Metadata.Name != meta.Metadata.Name || rn.Name != meta.Metadata.Name {
				t.Fatalf("name %q (run %q), golden %q", rn.Task.Metadata.Name, rn.Name, meta.Metadata.Name)
			}
			diff, err := kelostest.Diff(*rn.Task, want, gordonKeys...)
			if err != nil {
				t.Fatal(err)
			}
			if len(diff) > 0 {
				t.Fatalf("differs from the push spawner's Task at %v", diff)
			}
			labels := rn.Task.Metadata.Labels
			if _, ok := labels[dispatch.VariantLabel]; ok || labels[dispatch.ModeLabel] != "model-free" ||
				labels[dispatch.ManagedLabel] != "true" ||
				rn.Task.Metadata.Annotations[dispatch.VersionAnnotation] != profile.LegacyVersion+"/"+cfg.Version {
				t.Fatalf("Gordon keys %v %v", labels, rn.Task.Metadata.Annotations)
			}
		})
	}
}

// TestImageBuildOfAnImageNoLongerCatalogued: a run whose image left the
// catalogue between routing and building rejects its family.
func TestImageBuildOfAnImageNoLongerCatalogued(t *testing.T) {
	routing, cfg := imageBuildRouting(t), fixture(t)
	rn := imageBuilt(t, routing, cfg, pushed("sperano/lucarne"), "lucarne")
	routing.ImageBuilds.Images = nil
	_, err := dispatch.Builder{Routing: routing, Config: cfg}.Build(dispatch.Input{Run: rn})
	var e *dispatch.Error
	if !errors.As(err, &e) || e.Code != dispatch.CodeUnknownImage || e.Detail != `image "lucarne"` {
		t.Fatalf("Build = %v, want %s", err, dispatch.CodeUnknownImage)
	}
}

// TestImageBuildNeedsNoProjectOrVariant: a model-free run builds with
// no project and no variant entry, which an agent run would need.
func TestImageBuildNeedsNoProjectOrVariant(t *testing.T) {
	routing, cfg := imageBuildRouting(t), fixture(t)
	cfg.Variants = map[string]dispatch.VariantSpec{}
	routing.Projects = nil
	rn := imageBuilt(t, routing, cfg, pushed("sperano/vigie"), "vigie")
	if rn.Task.Spec.Worker.Model != "" || rn.Task.Spec.Worker.Effort != "" || len(rn.Task.Spec.Worker.AgentConfigRefs) != 0 {
		t.Fatalf("worker %+v, want no model, effort or agent configs", rn.Task.Spec.Worker)
	}
}
