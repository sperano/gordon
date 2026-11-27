package route

import (
	"path"
	"strings"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// Image-build push routing reasons.
const (
	// ReasonPushNotABranch ignores a push of a ref that is no branch (a
	// tag): no push spawner's branch filter matches one.
	ReasonPushNotABranch = "push-not-a-branch"
	// ReasonPushDeleted ignores a push that deleted its branch: there is
	// no commit to build.
	ReasonPushDeleted = "push-deleted"
	// ReasonPushNotBuilt ignores a push to a catalogued repository that
	// no image builds: another branch, or no matching changed file.
	ReasonPushNotBuilt = "push-not-built"
)

// push routes a verified push to the catalogued images it builds, as the
// image-build-<image>-push spawners filter it: one model-free run per
// image on the pushed repository with a branch pattern matching the
// pushed branch and, with file patterns, a changed file they match. The
// signed payload is the push itself, never stale: a commit and its files
// do not change, so no fresh state is read.
func (r Router) push(ev event.Event, led Ledger) Decision {
	gh, ib := ev.GitHub, r.Config.ImageBuilds
	if !ib.OnRepository(gh.Repository) {
		return Decision{Outcome: Ignore, Reason: ReasonRepositoryNotConfigured}
	}
	p := *gh.Push
	switch {
	case p.Branch() == "":
		return Decision{Outcome: Ignore, Reason: ReasonPushNotABranch}
	case p.Deleted || strings.Trim(p.After, "0") == "":
		return Decision{Outcome: Ignore, Reason: ReasonPushDeleted}
	}
	var out []FamilyDecision
	for _, img := range ib.Images {
		if img.Repository != gh.Repository || !builds(img, p) {
			continue
		}
		fd := r.family(run.ImageBuildPush, "")
		id := run.Identity{Family: run.ImageBuildPush, Image: img.Name, HeadSHA: p.After}
		r.submitWith(&fd, led, id, r.Config.ResolveImageBuild)
		out = append(out, fd)
	}
	if len(out) == 0 {
		return Decision{Outcome: Ignore, Reason: ReasonPushNotBuilt}
	}
	return Decision{Families: out}
}

// builds reports whether the image's push filters take the push: one of
// its branch globs matches the branch (Kelos's filepath.Match) and its
// file patterns, if any, match the changed files.
func builds(img profile.ImageBuild, p event.Push) bool {
	branch := false
	for _, b := range img.Branches {
		if ok, err := path.Match(b, p.Branch()); err == nil && ok {
			branch = true
			break
		}
	}
	if !branch || img.FilePatterns == nil {
		return branch
	}
	return filesMatch(p.Files, img.FilePatterns.Include, img.FilePatterns.Exclude)
}

// filesMatch is Kelos's source.MatchesFilePaths: no pattern matches
// anything; otherwise a file must survive the exclusions and, with
// inclusions, match one.
func filesMatch(files, include, exclude []string) bool {
	if len(include) == 0 && len(exclude) == 0 {
		return true
	}
	var remaining []string
	for _, f := range files {
		if !anyMatch(exclude, []string{f}) {
			remaining = append(remaining, f)
		}
	}
	if len(remaining) == 0 {
		return false
	}
	return len(include) == 0 || anyMatch(include, remaining)
}
