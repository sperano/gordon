package inventory

import (
	"fmt"
	"strings"

	"github.com/sperano/gordon/internal/run"
)

// Switch is the ownership contract of one Vikunja family: the legacy
// components that act for it, what the Hollingsworth change that makes
// Gordon its owner (Gordon 07) must turn off for the rule's projects in
// the same change, and what Gordon needs first. Rollback is the same
// change reversed; see RollbackContract.
type Switch struct {
	Family run.Family
	// Legacy are the components acting for the family today.
	Legacy []string
	// Off is what must stop acting for the rule's projects.
	Off string
	// Needs are the worker settings and rights Gordon needs to own it.
	Needs []string
}

// Shared prerequisites.
const (
	needWriter    = "`GORDON_VIKUNJA_WRITE_TOKEN_FILE` (a token that may label, move, assign, react and comment)"
	needKelos     = "`GORDON_DISPATCH_CONFIG_FILE` and `create` on `tasks` in `kelos-agents`"
	needDelete    = "`list` and `delete` on `tasks` in `kelos-agents`"
	needReconcile = "`GORDON_VIKUNJA_RECONCILE_INTERVAL` > 0 and a read token that may list tasks (`tasks.read_all`)"
	needSillage   = "`GORDON_SILLAGE_API_URL` (Sillage's in-cluster read API, `/api/v1/cost/task/`, from sperano/sillage#57 or later) and egress to it"
	needOps       = "for a project the cluster operator covers, the routing config's ops policy (`ops_label`, `ops_owner`, `ops_projects`, `ops_profile`) and the dispatch config's `vikunja-ops` family"
	needNoWait    = "a dispatch config whose Tasks of the family carry no `wait-for-blockers`, `announce-start` or `sillage-marker` init container (Gordon waits and announces)"
)

// Switches lists the Vikunja families' contracts, in run.Families order.
func Switches() []Switch {
	return []Switch{
		{run.VikunjaImplement, chartPath("templates/taskspawners.yaml", "scripts/wait_for_blockers.py", "scripts/sillage_marker.py"),
			"the project's `vikunja-<project>-<variant>` implementation TaskSpawners and, for an ops project, its `vikunja-ops-<project>-<variant>` ones",
			[]string{needKelos, needWriter, needNoWait, needReconcile, needOps}},
		{run.VikunjaDiscuss, chartPath("templates/taskspawners.yaml", "scripts/wait_for_blockers.py", "scripts/sillage_marker.py"),
			"the project's discuss TaskSpawners",
			[]string{needKelos, needWriter, needNoWait, needReconcile}},
		{run.VikunjaFollowUp, followUpLegacy(), followUpOff, []string{needKelos, needWriter, needNoWait, needReconcile}},
		{run.VikunjaFollowUpAnswer, followUpLegacy(), followUpOff, []string{needKelos, needWriter, needNoWait, needReconcile}},
		{run.VikunjaFollowUpDiscuss, followUpLegacy(), followUpOff + "; the enricher's discuss fallback (`DISCUSS_LABEL`)",
			[]string{needKelos, needWriter, needNoWait, needReconcile}},
		{run.VikunjaDragCancel, chartPath("scripts/drag_cancel.py"),
			"the enricher's drag-cancel hook (`BACKLOG_BUCKET`); needs Gordon to own `vikunja-implement` and `vikunja-discuss` of the same projects (the ownership table refuses otherwise)",
			[]string{needWriter, needDelete}},
		{run.VikunjaDoneCleanup, chartPath("scripts/done_unlabel.py", "scripts/action_needed.py", "scripts/report_failures.py"),
			"the enricher's done-unlabel and action-needed hooks (`IN_PROGRESS_LABEL`, `ACTION_NEEDED_LABEL`) and the failure reporter's done sweeps",
			[]string{needWriter, needReconcile}},
		{run.VikunjaChain, chartPath("scripts/subtask_chain.py", "templates/subtask-chain-reconcile.yaml", "templates/_subtask-chain.tpl"),
			"the enricher's chain hook (`CHAIN_LABEL`) and the `subtask-chain-reconcile` CronJob",
			[]string{needWriter, needReconcile}},
		{run.VikunjaFailure, chartPath("scripts/report_failures.py", "templates/failure-reporter.yaml"),
			"the failure reporter for Gordon's Tasks: it must skip Tasks labelled `gordon.spe.quebec/managed` (failed notices, agent-error, answered discussions); it keeps every other Task",
			[]string{needWriter}},
		{run.VikunjaAutoAssign, []string{"helm/kelos/scripts/webhook_enricher.py", "scripts/vikunja_sync.py"},
			"no writer is disabled; retain the `task.created` webhook relay",
			[]string{needWriter}},
		{run.VikunjaCost, nil,
			"nothing: no legacy component posts cost notices (rollback is the rule removed: no new notice, waiting and live ones read and write nothing more)",
			[]string{needWriter, needSillage}},
	}
}

const followUpOff = "the project's `vikunja-fu-*` comment TaskSpawners, the enricher's 👀 reaction and unanswered notice for its requests, and the `vikunja-followup-reconciler`"

func followUpLegacy() []string {
	return chartPath("templates/commentspawners.yaml", "scripts/announce_start.py", "scripts/unanswered_notice.py",
		"scripts/webhook_enricher.py", "scripts/reconcile_vikunja_followups.py")
}

// RollbackContract is what moving a scope back from gordon to shadow or
// legacy guarantees, and what the Hollingsworth change must do.
const RollbackContract = `Rollback is one change: the scope's rule back to ` + "`shadow`" + ` (or removed) and its legacy switches back on.
Decisions Gordon recorded while it owned the scope keep their queued actions: the outbox drains them (no request is lost), each
submission still looks up its Task name before creating (a legacy spawner's Task of that name takes the run, as legacy's own
name deduplication refuses Gordon's), and each notice is found by its marker before it is posted, so neither a Task nor a
comment is made twice. New deliveries are decided by the new owner from then on. Drain first when possible:
` + "`SELECT family, state, count(*) FROM actions WHERE state NOT IN ('done', 'failed') GROUP BY 1, 2`" + ` empty for the scope's
families; ` + "`failed`" + ` actions are dead-lettered (never retried), so review them separately.`

// SwitchesMarkdown renders the contracts.
func SwitchesMarkdown(switches []Switch) string {
	var b strings.Builder
	b.WriteString("| Family | Legacy | Off when Gordon owns it | Gordon needs |\n|---|---|---|---|\n")
	for _, s := range switches {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", s.Family, files(s.Legacy), escape(s.Off),
			escape(strings.Join(s.Needs, "; ")))
	}
	return b.String()
}
