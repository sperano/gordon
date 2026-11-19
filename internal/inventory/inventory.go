// Package inventory records every legacy Hollingsworth dispatch family,
// lifecycle hook and reconciler, the owner each one moves to and the
// stage that implements and cuts it over. It is data with tests, so a
// family cannot be added to the router without an inventory entry, and
// the shared wiki's table is generated from it (cmd/gordon-inventory).
package inventory

import (
	"fmt"
	"strings"

	"github.com/sperano/gordon/internal/run"
)

// Kind is what a legacy entry is in the Hollingsworth chart.
type Kind string

const (
	Spawner       Kind = "taskspawner"
	EnricherHook  Kind = "enricher-hook"
	InitContainer Kind = "init-container"
	Sidecar       Kind = "sidecar"
	Loop          Kind = "deployment"
	Cron          Kind = "cronjob"
	Receiver      Kind = "receiver"
	HelmHook      Kind = "helm-hook"
	Controller    Kind = "kelos-controller"
	AgentTool     Kind = "agent-tool"
)

// Owner is who owns the responsibility after the migration.
type Owner string

const (
	// Gordon decides it in Go and writes through durable actions.
	Gordon Owner = "gordon"
	// Kelos keeps it as part of executing a Task (pod, controller).
	Kelos Owner = "kelos"
	// Hollingsworth keeps it as a chart resource outside dispatch.
	Hollingsworth Owner = "hollingsworth"
	// Nobody: the responsibility is retired.
	Nobody Owner = "retired"
)

// Decision is what happens to the legacy entry.
type Decision string

const (
	Migrate Decision = "migrate"
	Retain  Decision = "retain"
	Retire  Decision = "retire"
)

// Entry is one legacy dispatch family, hook or reconciler.
type Entry struct {
	ID       string
	Kind     Kind
	Trigger  string
	Legacy   []string
	Owner    Owner
	Decision Decision
	// Implement is the stage that builds the Gordon side (0 if none);
	// Cutover the stage that moves ownership or removes it (0 if retained).
	Implement int
	Cutover   int
	// Families are the run families the router emits for it.
	Families []run.Family
	Note     string
}

// Stage numbers of the implementation chain (Gordon 01 to 13).
const (
	StageContracts    = 1
	StageCapabilities = 2
	StageIntake       = 3
	StageDispatch     = 4
	StageShadow       = 5
	StageVikunja      = 6
	StageVikunjaCut   = 7
	StageGitHub       = 8
	StageGitHubCut    = 9
	StageUsage        = 10
	StageCosts        = 11
	StageEstimates    = 12
	StageRetire       = 13
)

// Entries returns the whole inventory, Vikunja side first.
func Entries() []Entry {
	var out []Entry
	out = append(out, vikunjaEntries()...)
	out = append(out, githubEntries()...)
	return append(out, otherEntries()...)
}

// chartPath prefixes a path relative to helm/kelos.
func chartPath(paths ...string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "helm/kelos/" + p
	}
	return out
}

// Markdown renders the inventory as the shared wiki's table.
func Markdown(entries []Entry) string {
	var b strings.Builder
	b.WriteString("| Legacy entry | Kind | Trigger | Owner | Decision | Stages | Legacy files |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s |\n", e.ID, e.Kind,
			escape(e.Trigger), e.Owner, e.Decision, stages(e), files(e.Legacy))
		if e.Note != "" {
			fmt.Fprintf(&b, "| | | ↳ %s | | | | |\n", escape(e.Note))
		}
	}
	return b.String()
}

// StagesMarkdown renders each later stage with the other repositories
// and the Hollingsworth paths it changes.
func StagesMarkdown(stages []Stage) string {
	var b strings.Builder
	b.WriteString("| Stage | Title | Repositories | Hollingsworth paths |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, s := range stages {
		repos := "–"
		if len(s.Repositories) > 0 {
			repos = strings.Join(s.Repositories, ", ")
		}
		fmt.Fprintf(&b, "| %02d | %s | %s | %s |\n", s.Number, escape(s.Title), repos, files(s.Hollingsworth))
	}
	return b.String()
}

func stages(e Entry) string {
	switch {
	case e.Implement != 0 && e.Cutover != 0:
		return fmt.Sprintf("%02d → %02d", e.Implement, e.Cutover)
	case e.Cutover != 0:
		return fmt.Sprintf("→ %02d", e.Cutover)
	default:
		return "–"
	}
}

func files(paths []string) string {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = "`" + p + "`"
	}
	return strings.Join(quoted, "<br>")
}

// escape keeps a cell one cell and its <placeholders> visible.
func escape(s string) string {
	return strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;").Replace(s)
}
