package profile

import (
	"fmt"
	"maps"
	"slices"
)

// OpsCovers reports whether the ops mode applies to the project's tasks:
// a label is set and the project is listed.
func (p Policy) OpsCovers(project string) bool {
	return p.OpsLabel != "" && slices.Contains(p.OpsProjects, project)
}

// ValidateOps refuses an ops configuration that would widen the cluster
// operator past its contract (kelos.clusterOperator.validate): an ops
// label without an owner or a profile, a label another policy label
// already means, an unknown project, an ops profile that is read-only,
// runs as no ServiceAccount or shares its ServiceAccount, and a cluster
// write any other profile grants. An empty label is the mode off, which
// still refuses the cluster writes of other profiles.
func (c Config) ValidateOps() error {
	p := c.Policy
	if p.OpsLabel == "" {
		return c.checkClusterWrite()
	}
	if p.OpsOwner == "" {
		return fmt.Errorf("policy ops_owner is required with ops_label: only the owner's events start an ops run")
	}
	if name, ok := p.labelNamed(p.OpsLabel); ok {
		return fmt.Errorf("policy ops_label %q is already policy %s", p.OpsLabel, name)
	}
	for _, project := range p.OpsProjects {
		if _, ok := c.projectByName(project); !ok {
			return fmt.Errorf("policy ops_projects: %q is not a configured project", project)
		}
	}
	if err := c.checkOpsProfile(); err != nil {
		return err
	}
	return c.checkClusterWrite()
}

// checkOpsProfile refuses a missing, read-only or ServiceAccount-less ops
// profile, one whose ServiceAccount another profile shares, and one a
// project names for another mode.
func (c Config) checkOpsProfile() error {
	name := c.OpsProfile
	prof, ok := c.Profiles[name]
	switch {
	case name == "":
		return fmt.Errorf("ops_profile is required with policy ops_label")
	case !ok:
		return fmt.Errorf("ops_profile %q is not a profile", name)
	case prof.ReadOnly:
		return fmt.Errorf("ops_profile %q is read-only", name)
	case prof.ServiceAccount == "":
		return fmt.Errorf("ops_profile %q names no service_account", name)
	}
	for _, other := range slices.Sorted(maps.Keys(c.Profiles)) {
		if other != name && c.Profiles[other].ServiceAccount == prof.ServiceAccount {
			return fmt.Errorf("profile %q shares ops_profile %q's service_account %q", other, name, prof.ServiceAccount)
		}
	}
	for _, proj := range c.Projects {
		for _, mode := range slices.Sorted(maps.Keys(proj.Profiles)) {
			if proj.Profiles[mode] == name {
				return fmt.Errorf("project %s: %s profile %q is the ops profile", proj.Name, mode, name)
			}
		}
		for _, mode := range slices.Sorted(maps.Keys(proj.Allowed)) {
			if slices.Contains(proj.Allowed[mode], name) {
				return fmt.Errorf("project %s: %s allows the ops profile %q", proj.Name, mode, name)
			}
		}
	}
	return nil
}

// checkClusterWrite refuses ClusterWrite in any profile but the ops one.
func (c Config) checkClusterWrite() error {
	for _, name := range slices.Sorted(maps.Keys(c.Profiles)) {
		if name != c.OpsProfile && slices.Contains(c.Profiles[name].Capabilities, ClusterWrite) {
			return fmt.Errorf("profile %q grants %s but is not ops_profile", name, ClusterWrite)
		}
	}
	return nil
}

// labelNamed returns the policy field already holding the label title.
func (p Policy) labelNamed(label string) (string, bool) {
	for _, l := range []struct{ name, value string }{
		{"in_progress_label", p.InProgressLabel}, {"discuss_label", p.DiscussLabel},
		{"no_agent_label", p.NoAgentLabel}, {"chain_label", p.ChainLabel}, {"pr_open_label", p.PROpenLabel},
		{"agent_error_label", p.AgentErrorLabel}, {"action_needed_label", p.ActionNeededLabel},
		{"auto_merge_label", p.AutoMergeLabel}, {"ready_to_merge_label", p.ReadyToMergeLabel},
		{"round_limit_label", p.RoundLimitLabel}, {"draft_label", p.DraftLabel},
		{"fix_suggestions_label", p.FixSuggestionsLabel}, {"no_review_label", p.NoReviewLabel},
	} {
		if l.value == label {
			return l.name, true
		}
	}
	return "", false
}
