package profile

import (
	"fmt"
	"slices"
)

// readOnlyWorkspaceSuffix names the clone-only copy of a Workspace.
const readOnlyWorkspaceSuffix = "-readonly"

// Error codes of a refused resolution.
const (
	CodeUnknownProject     = "unknown-project"
	CodeUnknownVariant     = "unknown-variant"
	CodeUnsupportedEffort  = "unsupported-effort"
	CodeUnknownProfile     = "unknown-profile"
	CodeProfileNotAllowed  = "profile-not-allowed"
	CodeReadOnlyViolation  = "read-only-violation"
	CodeNoProfileForMode   = "no-profile-for-mode"
	CodeUnknownMode        = "unknown-mode"
	CodeIncompleteVariant  = "incomplete-variant"
	CodeConflictingProfile = "conflicting-profile"
	CodeUnknownCapability  = "unknown-capability"
)

// Error is a refused resolution; Code is stable for decisions and metrics.
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

func refuse(code, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Request asks for the profile of one run. Every field comes from routing
// on trusted configuration; Effort and Profile are optional overrides that
// must still pass the configuration's allowlists.
type Request struct {
	Project string
	Variant string
	Mode    Mode
	Effort  string
	Profile string
}

// Resolved is the execution profile a run records.
type Resolved struct {
	ConfigVersion    string       `json:"config_version"`
	Variant          string       `json:"variant"`
	Agent            string       `json:"agent"`
	Model            string       `json:"model"`
	Effort           string       `json:"effort,omitempty"`
	Image            string       `json:"image"`
	CredentialType   string       `json:"credential_type"`
	CredentialSecret string       `json:"credential_secret,omitempty"`
	Mode             Mode         `json:"mode"`
	Profile          string       `json:"profile"`
	ServiceAccount   string       `json:"service_account"`
	Workspace        string       `json:"workspace"`
	ReadOnly         bool         `json:"read_only"`
	Capabilities     []Capability `json:"capabilities"`
}

var modes = []Mode{ModeImplement, ModeDiscuss, ModeFollowUp, ModeAnswer, ModeReview}

// Resolve returns the run's profile, or an *Error.
func (c Config) Resolve(r Request) (Resolved, error) {
	if !slices.Contains(modes, r.Mode) {
		return Resolved{}, refuse(CodeUnknownMode, "mode %q", r.Mode)
	}
	proj, ok := c.projectByName(r.Project)
	if !ok {
		return Resolved{}, refuse(CodeUnknownProject, "project %q", r.Project)
	}
	out, err := c.agent(r)
	if err != nil {
		return Resolved{}, err
	}
	name, prof, err := c.pickProfile(proj, r)
	if err != nil {
		return Resolved{}, err
	}
	out.ConfigVersion, out.Mode, out.Profile = c.Version, r.Mode, name
	out.ServiceAccount, out.ReadOnly = prof.ServiceAccount, prof.ReadOnly
	out.Capabilities = slices.Clone(prof.Capabilities)
	out.Workspace = proj.Workspace
	if prof.ReadOnly {
		out.Workspace += readOnlyWorkspaceSuffix
	}
	return out, nil
}

// agent resolves the variant's runtime and the effort.
func (c Config) agent(r Request) (Resolved, error) {
	v, ok := c.Variants[r.Variant]
	if !ok {
		return Resolved{}, refuse(CodeUnknownVariant, "variant %q", r.Variant)
	}
	run, ok := c.Runners[v.Runner]
	if !ok {
		return Resolved{}, refuse(CodeIncompleteVariant, "variant %q: runner %q", r.Variant, v.Runner)
	}
	effort := v.Effort
	if r.Effort != "" {
		effort = r.Effort
	}
	if !slices.Contains(run.Efforts, effort) {
		return Resolved{}, refuse(CodeUnsupportedEffort, "effort %q on %s", effort, run.Type)
	}
	return Resolved{
		Variant: r.Variant, Agent: run.Type, Model: v.Model, Effort: effort,
		Image: run.Image, CredentialType: run.CredentialType, CredentialSecret: run.CredentialSecret,
	}, nil
}

// pickProfile returns the mode's profile: the override when the project
// allows it, else the default. Follow-ups and answers default to the
// implementation profile and reviews to the review profile, as legacy does.
func (c Config) pickProfile(p Project, r Request) (string, Profile, error) {
	name := c.defaultProfile(p, r.Mode)
	if r.Profile != "" && r.Profile != name {
		if !slices.Contains(p.Allowed[r.Mode], r.Profile) {
			return "", Profile{}, refuse(CodeProfileNotAllowed, "profile %q for %s in %s", r.Profile, r.Mode, p.Name)
		}
		name = r.Profile
	}
	if name == "" {
		return "", Profile{}, refuse(CodeNoProfileForMode, "%s in %s", r.Mode, p.Name)
	}
	prof, ok := c.Profiles[name]
	if !ok {
		return "", Profile{}, refuse(CodeUnknownProfile, "profile %q", name)
	}
	return name, prof, checkProfile(name, prof, r.Mode)
}

func (c Config) defaultProfile(p Project, m Mode) string {
	if name, ok := p.Profiles[m]; ok {
		return name
	}
	switch m {
	case ModeFollowUp, ModeAnswer:
		return p.Profiles[ModeImplement]
	case ModeReview:
		return c.ReviewProfile
	default:
		return ""
	}
}

// checkProfile enforces that a discuss run is read-only whatever else it
// asks for, that every capability is classified, that a read-only profile
// carries no writing capability, and that a writing mode gets a profile
// that is not read-only and grants at least one writing capability.
func checkProfile(name string, p Profile, m Mode) error {
	grantsWrite := false
	for _, capability := range p.Capabilities {
		writing, known := capability.Writes()
		if !known {
			return refuse(CodeUnknownCapability, "profile %q grants unclassified %s", name, capability)
		}
		if p.ReadOnly && writing {
			return refuse(CodeConflictingProfile, "profile %q is read-only but grants %s", name, capability)
		}
		grantsWrite = grantsWrite || writing
	}
	if m == ModeDiscuss && !p.ReadOnly {
		return refuse(CodeReadOnlyViolation, "discuss needs a read-only profile, %q writes", name)
	}
	if m != ModeDiscuss && p.ReadOnly {
		return refuse(CodeReadOnlyViolation, "%s needs a writing profile, %q is read-only", m, name)
	}
	if m != ModeDiscuss && !grantsWrite {
		return refuse(CodeReadOnlyViolation, "%s needs a writing profile, %q grants no writing capability", m, name)
	}
	return nil
}

func (c Config) projectByName(name string) (Project, bool) {
	for _, p := range c.Projects {
		if p.Name == name {
			return p, true
		}
	}
	return Project{}, false
}
