package route_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

const pushSHA = "0123456789abcdef0123456789abcdef01234567"

// imageRouter is router() with a catalogue: lucarne on any change to
// main, kelos-agent and portal-docs on their paths of hollingsworth's
// main, release on release/* branches of nhl.
func imageRouter() route.Router {
	r := router()
	r.Config.ImageBuilds = profile.ImageBuilds{Profile: profile.ProfileImageBuildRunner, Workspace: "merge-close-stub",
		Runner: profile.ImageBuildRunner{Type: "opencode", Image: "builder:1", CredentialType: "none"},
		Images: []profile.ImageBuild{
			{Name: "kelos-agent", Repository: "sperano/hollingsworth", Environment: "hollingsworth", Branches: []string{"main"},
				FilePatterns: &profile.FilePatterns{Include: []string{"docker/kelos-agent/**", "helm/kelos/Chart.yaml"}}},
			{Name: "lucarne", Repository: "sperano/lucarne", Environment: "hollingsworth", Branches: []string{"main"}},
			{Name: "nhl", Repository: "sperano/nhl", Environment: "hollingsworth", Branches: []string{"release/*"},
				FilePatterns: &profile.FilePatterns{Exclude: []string{"**/*.md"}}},
			{Name: "portal-docs", Repository: "sperano/hollingsworth", Environment: "hollingsworth", Branches: []string{"main"},
				FilePatterns: &profile.FilePatterns{Include: []string{"portal-docs/**"}, Exclude: []string{"portal-docs/drafts/**"}}},
		}}
	return r
}

// pushDelivery is a signed push of ref in repo after sha changing files.
func pushDelivery(t *testing.T, repo, ref, sha string, deleted bool, files ...string) event.Delivery {
	t.Helper()
	return signed(t, event.SourceGitHub, map[string]string{event.GitHubEventHeader: "push", event.GitHubDeliveryHeader: "push-1"},
		map[string]any{"ref": ref, "after": sha, "deleted": deleted, "sender": map[string]string{"login": "sperano"},
			"repository": map[string]string{"full_name": repo},
			"commits":    []map[string]any{{"added": files, "removed": []string{}, "modified": []string{}}}})
}

func runNames(d route.Decision) []string {
	var out []string
	for _, fd := range d.Families {
		for _, rn := range fd.Runs {
			out = append(out, rn.Name)
		}
	}
	return out
}

func TestPushBuildsTheImagesItMatches(t *testing.T) {
	cases := map[string]struct {
		repo, ref string
		files     []string
		want      []string
	}{
		"any change":       {"sperano/lucarne", "refs/heads/main", nil, []string{"image-build-lucarne-push-0123456789ab"}},
		"one include":      {"sperano/hollingsworth", "refs/heads/main", []string{"helm/kelos/Chart.yaml"}, []string{"image-build-kelos-agent-push-0123456789ab"}},
		"two images":       {"sperano/hollingsworth", "refs/heads/main", []string{"docker/kelos-agent/Dockerfile", "portal-docs/index.md"}, []string{"image-build-kelos-agent-push-0123456789ab", "image-build-portal-docs-push-0123456789ab"}},
		"globbed branch":   {"sperano/nhl", "refs/heads/release/1.2", []string{"src/main.rs", "README.md"}, []string{"image-build-nhl-push-0123456789ab"}},
		"exclude only":     {"sperano/hollingsworth", "refs/heads/main", []string{"portal-docs/drafts/a.md", "portal-docs/b.md"}, []string{"image-build-portal-docs-push-0123456789ab"}},
		"glob spans dirs":  {"sperano/hollingsworth", "refs/heads/main", []string{"docker/kelos-agent/a/b/c.sh"}, []string{"image-build-kelos-agent-push-0123456789ab"}},
		"branch is a path": {"sperano/nhl", "refs/heads/release/a/b", []string{"x"}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := imageRouter().Decide(pushDelivery(t, tc.repo, tc.ref, pushSHA, false, tc.files...), route.State{}, replay.Ledger{})
			if got := runNames(d); !slices.Equal(got, tc.want) {
				t.Fatalf("runs %v, want %v (%s/%s)", got, tc.want, d.Outcome, d.Reason)
			}
			if tc.want == nil {
				expect(t, d, route.Ignore, route.ReasonPushNotBuilt)
				return
			}
			expect(t, d, route.Dispatch, "")
			for _, fd := range d.Families {
				rn := fd.Runs[0]
				submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, rn.Name), "")
				if fd.Family != run.ImageBuildPush || fd.Project != "" || rn.Branch != "" || rn.Identity.HeadSHA != pushSHA ||
					rn.Profile.Mode != profile.ModeModelFree || rn.Profile.ServiceAccount != "image-build-runner" ||
					!slices.Equal(keys(fd.Actions), []string{submit.Key}) {
					t.Fatalf("family %+v", fd)
				}
			}
		})
	}
}

func TestPushIgnored(t *testing.T) {
	zero := strings.Repeat("0", len(pushSHA))
	cases := map[string]struct {
		router  route.Router
		repo    string
		ref     string
		sha     string
		deleted bool
		files   []string
		reason  string
	}{
		"no catalogue":       {router(), "sperano/lucarne", "refs/heads/main", pushSHA, false, nil, route.ReasonRepositoryNotConfigured},
		"other repository":   {imageRouter(), "sperano/vigie", "refs/heads/main", pushSHA, false, nil, route.ReasonRepositoryNotConfigured},
		"tag":                {imageRouter(), "sperano/lucarne", "refs/tags/v1", pushSHA, false, nil, route.ReasonPushNotABranch},
		"deleted":            {imageRouter(), "sperano/lucarne", "refs/heads/main", zero, true, nil, route.ReasonPushDeleted},
		"zero after":         {imageRouter(), "sperano/lucarne", "refs/heads/main", zero, false, nil, route.ReasonPushDeleted},
		"other branch":       {imageRouter(), "sperano/lucarne", "refs/heads/feature", pushSHA, false, nil, route.ReasonPushNotBuilt},
		"no matching path":   {imageRouter(), "sperano/hollingsworth", "refs/heads/main", pushSHA, false, []string{"README.md"}, route.ReasonPushNotBuilt},
		"no files":           {imageRouter(), "sperano/hollingsworth", "refs/heads/main", pushSHA, false, nil, route.ReasonPushNotBuilt},
		"every file exclude": {imageRouter(), "sperano/nhl", "refs/heads/release/1", pushSHA, false, []string{"README.md", "docs/a.md"}, route.ReasonPushNotBuilt},
		"excluded included":  {imageRouter(), "sperano/hollingsworth", "refs/heads/main", pushSHA, false, []string{"portal-docs/drafts/x.md"}, route.ReasonPushNotBuilt},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := tc.router.Decide(pushDelivery(t, tc.repo, tc.ref, tc.sha, tc.deleted, tc.files...), route.State{}, replay.Ledger{})
			expect(t, d, route.Ignore, tc.reason)
			if len(d.Families) != 0 || d.DeliveryKey != "github/delivery/push-1" {
				t.Fatalf("decision %+v", d)
			}
		})
	}
}

func TestPushDeduplicates(t *testing.T) {
	r := imageRouter()
	d := pushDelivery(t, "sperano/lucarne", "refs/heads/main", pushSHA, false)
	expect(t, r.Decide(d, route.State{}, replay.Ledger{Deliveries: []string{"github/delivery/push-1"}}), route.Duplicate, route.ReasonRedelivery)
	name := "image-build-lucarne-push-0123456789ab"
	for reason, rec := range map[string]route.RunRecord{
		route.ReasonRunActive: {Name: name, Active: true, Exists: true},
		route.ReasonRunExists: {Name: name, Exists: true},
	} {
		got := r.Decide(d, route.State{}, replay.Ledger{Runs: []route.RunRecord{rec}})
		expectFamily(t, family(t, got), run.ImageBuildPush, route.Duplicate, reason)
	}
	gone := r.Decide(d, route.State{}, replay.Ledger{Runs: []route.RunRecord{{Name: name}}})
	expectFamily(t, family(t, gone), run.ImageBuildPush, route.Dispatch, "")
}

func TestPushRefusedProfileRejects(t *testing.T) {
	r := imageRouter()
	r.Config.ImageBuilds.Profile = "ghost"
	d := r.Decide(pushDelivery(t, "sperano/lucarne", "refs/heads/main", pushSHA, false), route.State{}, replay.Ledger{})
	expectFamily(t, family(t, d), run.ImageBuildPush, route.Reject, profile.CodeUnknownProfile)
	expect(t, d, route.Reject, "")
}

func TestPushReadsNothing(t *testing.T) {
	ev, err := event.Parse(pushDelivery(t, "sperano/lucarne", "refs/heads/main", pushSHA, false))
	if err != nil {
		t.Fatal(err)
	}
	if got := imageRouter().Reads(ev); got != (route.Reads{}) {
		t.Fatalf("reads %+v, want none", got)
	}
}
