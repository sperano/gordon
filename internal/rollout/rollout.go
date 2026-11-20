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

// required lists the gates each owner needs, in order. It is unexported
// so no caller can weaken Validate.
var required = map[Owner][]Gate{
	Legacy: nil,
	Shadow: {ReplayParity},
	Gordon: {ReplayParity, ShadowParity, RollbackTested, LegacyDisabled},
}

// coupled lists, per family, the families Gordon must own wherever it
// owns that one: a drag to the backlog stops the task's runs from the
// run ledger, which has only the runs Gordon submitted, so Gordon must
// be the one submitting them.
var coupled = map[run.Family][]run.Family{
	run.VikunjaDragCancel: {run.VikunjaImplement, run.VikunjaDiscuss},
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
// scope: only when Gordon owns it and the table is valid, so a rule
// lacking its gate evidence executes nothing. In Shadow Gordon only
// records its decisions; in Legacy the decision is informational.
func (t Table) Executes(project string, family run.Family) bool {
	return t.Owner(project, family) == Gordon && t.Validate() == nil
}

// Validate refuses a table with an unknown family or owner, two rules for
// one scope, a rule lacking its owner's gate evidence, or a family Gordon
// owns in a project without its coupled families (checkCoupled).
func (t Table) Validate() error {
	seen := map[[2]string]bool{}
	for _, r := range t.Rules {
		if !slices.Contains(run.Families, r.Family) {
			return fmt.Errorf("rule %s/%s: unknown family", r.Project, r.Family)
		}
		gates, ok := required[r.Owner]
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
	return t.checkCoupled()
}

// checkCoupled refuses a family Gordon owns in a project (by a project
// rule or an AllProjects one) whose coupled families Gordon does not own
// in that project: every project a rule names is checked, so a project
// override of a coupled family under an AllProjects rule is caught too.
func (t Table) checkCoupled() error {
	projects := []string{AllProjects}
	for _, r := range t.Rules {
		if !slices.Contains(projects, r.Project) {
			projects = append(projects, r.Project)
		}
	}
	for _, family := range run.Families {
		for _, p := range projects {
			if err := t.checkCoupledIn(p, family); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCoupledIn checks one family's coupling in one project.
func (t Table) checkCoupledIn(project string, family run.Family) error {
	if t.Owner(project, family) != Gordon {
		return nil
	}
	for _, f := range coupled[family] {
		if t.Owner(project, f) != Gordon {
			return fmt.Errorf("rule %s/%s: needs %s owned by gordon too", project, family, f)
		}
	}
	return nil
}
