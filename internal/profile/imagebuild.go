package profile

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// ImageBuilds is the push-triggered image-build catalogue
// (Hollingsworth's imageBuilds.images with onPush) and the runner every
// build runs as. A push to one of an image's branches touching its paths
// starts one model-free runner Task per image and pushed commit.
type ImageBuilds struct {
	// Profile is the runner's execution profile
	// (executionProfiles.system.image-build).
	Profile string `json:"profile"`
	// Workspace is the runner Task's Workspace (the merge-close stub):
	// Kelos clones one, the runner never reads it.
	Workspace string `json:"workspace"`
	// Runner is the runner's worker: type, image and credentials.
	Runner ImageBuildRunner `json:"runner"`
	// Images are the catalogued images built on push, in name order.
	Images []ImageBuild `json:"images"`
}

// ImageBuildRunner is the image-build runner's worker type, image and
// credential type (imageBuilds.image; type opencode, credentials none).
type ImageBuildRunner struct {
	Type           string `json:"type"`
	Image          string `json:"image"`
	CredentialType string `json:"credential_type"`
}

// ImageBuild is one catalogued image's push trigger.
type ImageBuild struct {
	// Name is the catalogue key the runner's request names.
	Name        string `json:"name"`
	Repository  string `json:"repository"`
	Environment string `json:"environment"`
	// Branches are onPush.branches, Kelos branch globs; the first is the
	// branch a coalescing build follows.
	Branches []string `json:"branches"`
	// FilePatterns are onPush.filePatterns; nil builds whatever changed.
	FilePatterns *FilePatterns `json:"file_patterns,omitempty"`
	// Coalesce is onPush.coalesce: the runner builds the branch head,
	// not the pushed commit.
	Coalesce bool `json:"coalesce,omitempty"`
	// OwnerMerged is the image's ownerMerged: its pin pull request waits
	// for the owner's merge instead of merging itself.
	OwnerMerged bool `json:"owner_merged,omitempty"`
}

// FilePatterns are a Kelos filter's filePatterns: a push matches when a
// changed file survives Exclude and, with Include set, matches Include.
type FilePatterns struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// imageNameMax is image-builds.yaml's image name limit.
const imageNameMax = 20

// ValidateImageBuilds refuses a catalogue that could start a build
// image-builds.yaml would refuse to render, or one whose runner profile
// is not the image-build profile: an unknown, read-only or
// ServiceAccount-less profile, a profile a project names, an image name
// the chart refuses or two images of one name, an image without a
// repository, environment or branch, and a pattern Kelos would not match
// the same way. A zero catalogue builds nothing and is valid.
func (c Config) ValidateImageBuilds() error {
	ib := c.ImageBuilds
	if len(ib.Images) == 0 {
		return nil
	}
	if err := c.checkImageBuildProfile(); err != nil {
		return err
	}
	if ib.Workspace == "" || ib.Runner.Type == "" || ib.Runner.Image == "" || ib.Runner.CredentialType == "" {
		return fmt.Errorf("image_builds: workspace, runner type, image and credential_type are required")
	}
	seen := map[string]bool{}
	for _, img := range ib.Images {
		if seen[img.Name] {
			return fmt.Errorf("image_builds: image %q is listed twice", img.Name)
		}
		seen[img.Name] = true
		if err := img.validate(); err != nil {
			return fmt.Errorf("image_builds: image %q: %w", img.Name, err)
		}
	}
	return nil
}

// checkImageBuildProfile refuses a runner profile that is unknown,
// read-only or runs as no ServiceAccount, grants no writing capability,
// is the ops profile or one a project names.
func (c Config) checkImageBuildProfile() error {
	name := c.ImageBuilds.Profile
	prof, ok := c.Profiles[name]
	switch {
	case name == "":
		return fmt.Errorf("image_builds: profile is required")
	case !ok:
		return fmt.Errorf("image_builds: profile %q is not a profile", name)
	case prof.ServiceAccount == "":
		return fmt.Errorf("image_builds: profile %q names no service_account", name)
	case name == c.OpsProfile:
		return fmt.Errorf("image_builds: profile %q is the ops profile", name)
	}
	if err := checkProfile(name, prof, ModeModelFree); err != nil {
		return fmt.Errorf("image_builds: %w", err)
	}
	for _, p := range c.Projects {
		for _, mode := range slices.Sorted(maps.Keys(p.Profiles)) {
			if p.Profiles[mode] == name {
				return fmt.Errorf("image_builds: profile %q is project %s's %s profile", name, p.Name, mode)
			}
		}
		for _, mode := range slices.Sorted(maps.Keys(p.Allowed)) {
			if slices.Contains(p.Allowed[mode], name) {
				return fmt.Errorf("image_builds: profile %q is allowed to project %s's %s runs", name, p.Name, mode)
			}
		}
	}
	return nil
}

func (img ImageBuild) validate() error {
	if !validImageName(img.Name) {
		return fmt.Errorf("the name must be [a-z0-9-], at most %d characters, not ending in -reconcile", imageNameMax)
	}
	if img.Repository == "" || img.Environment == "" || len(img.Branches) == 0 {
		return fmt.Errorf("repository, environment and branches are required")
	}
	for _, b := range img.Branches {
		// route.builds matches branches with path.Match: no {a,b}.
		if _, err := path.Match(b, ""); err != nil || b == "" || strings.ContainsAny(b, "{}") {
			return fmt.Errorf("branch pattern %q is invalid or uses {a,b}", b)
		}
	}
	if img.FilePatterns == nil {
		return nil
	}
	for _, p := range append(slices.Clone(img.FilePatterns.Include), img.FilePatterns.Exclude...) {
		// Kelos's {a,b} alternation is not matched here (route.match).
		if _, err := path.Match(p, ""); err != nil || p == "" || strings.ContainsAny(p, "{}") {
			return fmt.Errorf("file pattern %q is invalid or uses {a,b}", p)
		}
	}
	return nil
}

// validImageName is image-builds.yaml's ^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$
// without the -reconcile suffix the reconcile spawners own.
func validImageName(name string) bool {
	if name == "" || len(name) > imageNameMax || strings.HasSuffix(name, "-reconcile") {
		return false
	}
	for i, r := range name {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !alnum && (r != '-' || i == 0 || i == len(name)-1) {
			return false
		}
	}
	return true
}

// Image returns the catalogued image of a name.
func (ib ImageBuilds) Image(name string) (ImageBuild, bool) {
	for _, img := range ib.Images {
		if img.Name == name {
			return img, true
		}
	}
	return ImageBuild{}, false
}

// ResolveImageBuild returns the image-build runner's profile: its
// worker, the catalogue's profile and Workspace, mode model-free. No
// project, variant, model or effort: no agent runs.
func (c Config) ResolveImageBuild() (Resolved, error) {
	ib := c.ImageBuilds
	prof, ok := c.Profiles[ib.Profile]
	if !ok {
		return Resolved{}, refuse(CodeUnknownProfile, "profile %q", ib.Profile)
	}
	if err := checkProfile(ib.Profile, prof, ModeModelFree); err != nil {
		return Resolved{}, err
	}
	return Resolved{
		ConfigVersion: c.Version, Agent: ib.Runner.Type, Image: ib.Runner.Image,
		CredentialType: ib.Runner.CredentialType, Mode: ModeModelFree, Profile: ib.Profile,
		ServiceAccount: prof.ServiceAccount, Workspace: ib.Workspace,
		Capabilities: slices.Clone(prof.Capabilities),
	}, nil
}

// OnRepository reports whether an image is built from the repository.
func (ib ImageBuilds) OnRepository(repository string) bool {
	for _, img := range ib.Images {
		if img.Repository == repository {
			return true
		}
	}
	return false
}
