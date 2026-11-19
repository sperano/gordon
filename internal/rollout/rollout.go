// Package rollout decides which dispatcher owns each (project, family) and
// whether Gordon may execute a decision. Exactly one dispatcher is active
// for a scope: the legacy Hollingsworth glue (Legacy), or Gordon. In
// Shadow, legacy stays active and Gordon only records its decisions for
// comparison. The zero table leaves everything Legacy, so no production
// routing changes until a rule with its gate evidence says so.
package rollout

import (
	"fmt"
	"slices"

	"github.com/sperano/gordon/internal/run"
)

// Owner is the active dispatcher of a scope.
type Owner string

const (
	Legacy Owner = "legacy"
	Shadow Owner = "shadow"
	Gordon Owner = "gordon"
)

// Gate is a piece of rollout evidence a rule must cite.
type Gate string

const (
	// ReplayParity: the family's replay fixtures pass against the
	// deployed Gordon version.
	ReplayParity Gate = "replay-parity"
	// ShadowParity: shadow decisions matched legacy over the pilot window
	// with every divergence explained.
	ShadowParity Gate = "shadow-parity"
	// RollbackTested: moving the scope back to Legacy was exercised with
	// no duplicate Task, comment or lost request.
	RollbackTested Gate = "rollback-tested"
	// LegacyDisabled: the Hollingsworth switch for the scope's spawners
	// or reconcilers is off in the same change, so nothing else dispatches.
	LegacyDisabled Gate = "legacy-disabled"
)

// Required lists the gates each owner needs, in order.
var Required = map[Owner][]Gate{
	Legacy: nil,
	Shadow: {ReplayParity},
	Gordon: {ReplayParity, ShadowParity, RollbackTested, LegacyDisabled},
}

// AllProjects is the project of a rule that covers every project.
const AllProjects = "*"

// Rule assigns an owner to a family, for one project or AllProjects.
type Rule struct {
	Project  string     `json:"project"`
	Family   run.Family `json:"family"`
	Owner    Owner      `json:"owner"`
	Evidence []Gate     `json:"evidence,omitempty"`
}

// Table is the authoritative ownership configuration.
type Table struct {
	Rules []Rule `json:"rules"`
}

// Owner returns the scope's owner: a project rule wins over an
// AllProjects rule; without either, Legacy.
func (t Table) Owner(project string, family run.Family) Owner {
	owner := Legacy
	for _, r := range t.Rules {
		if r.Family != family {
			continue
		}
		if r.Project == project {
			return r.Owner
		}
		if r.Project == AllProjects {
			owner = r.Owner
		}
	}
	return owner
}

// Executes reports whether Gordon performs a decision's actions in the
// scope; in Legacy and Shadow it only records them.
func (t Table) Executes(project string, family run.Family) bool {
	return t.Owner(project, family) == Gordon
}

// Validate refuses a table with an unknown family or owner, two rules for
// one scope, or a rule lacking its owner's gate evidence.
func (t Table) Validate() error {
	seen := map[[2]string]bool{}
	for _, r := range t.Rules {
		if !slices.Contains(run.Families, r.Family) {
			return fmt.Errorf("rule %s/%s: unknown family", r.Project, r.Family)
		}
		gates, ok := Required[r.Owner]
		if !ok {
			return fmt.Errorf("rule %s/%s: unknown owner %q", r.Project, r.Family, r.Owner)
		}
		if r.Project == "" {
			return fmt.Errorf("rule for %s: empty project (use %q)", r.Family, AllProjects)
		}
		scope := [2]string{r.Project, string(r.Family)}
		if seen[scope] {
			return fmt.Errorf("rule %s/%s: duplicate scope", r.Project, r.Family)
		}
		seen[scope] = true
		for _, g := range gates {
			if !slices.Contains(r.Evidence, g) {
				return fmt.Errorf("rule %s/%s: owner %s lacks gate %s", r.Project, r.Family, r.Owner, g)
			}
		}
	}
	return nil
}
