package inventory

import "github.com/sperano/gordon/internal/run"

func vikunjaEntries() []Entry {
	return append(vikunjaDispatch(), vikunjaLifecycle()...)
}

func vikunjaDispatch() []Entry {
	spawners := chartPath("templates/taskspawners.yaml", "templates/_worker.tpl", "templates/_followup.tpl")
	comments := chartPath("templates/commentspawners.yaml", "templates/_worker.tpl", "templates/_followup.tpl")
	return []Entry{
		{ID: "vikunja-implement", Kind: Spawner, Legacy: spawners, Owner: Gordon, Decision: Migrate,
			Trigger:   "task.updated in Doing, one agent assignee, no in progress/no-agent/discuss/chain",
			Implement: StageVikunja, Cutover: StageVikunjaCut, Families: []run.Family{run.VikunjaImplement},
			Note: "vikunja-<project>-<variant>: 20 projects × 8 variants; ops tasks (owner-created, owner-started, ops label) get vikunja-ops-<project>-<variant> with the ops profile"},
		{ID: "vikunja-discuss", Kind: Spawner, Legacy: spawners, Owner: Gordon, Decision: Migrate,
			Trigger:   "same as vikunja-implement with the discuss label",
			Implement: StageVikunja, Cutover: StageVikunjaCut, Families: []run.Family{run.VikunjaDiscuss},
			Note: "read-only Workspace and profile; puckdb's discuss profile becomes read-only"},
		{ID: "vikunja-followup", Kind: Spawner, Legacy: comments, Owner: Gordon, Decision: Migrate,
			Trigger:   "owner /gordon comment on a task with in progress or pr open",
			Implement: StageVikunja, Cutover: StageVikunjaCut, Families: []run.Family{run.VikunjaFollowUp},
			Note: "on kelos/vikunja-<id>: Kelos's branch lock queues it behind a running Task"},
		{ID: "vikunja-followup-answer", Kind: Spawner, Legacy: comments, Owner: Gordon, Decision: Migrate,
			Trigger: "owner /gordon comment on an idle task", Implement: StageVikunja, Cutover: StageVikunjaCut,
			Families: []run.Family{run.VikunjaFollowUpAnswer}},
		{ID: "vikunja-followup-discuss", Kind: Spawner, Legacy: comments, Owner: Gordon, Decision: Migrate,
			Trigger: "owner /gordon comment on a discuss task", Implement: StageVikunja, Cutover: StageVikunjaCut,
			Families: []run.Family{run.VikunjaFollowUpDiscuss}},
		{ID: "webhook-enricher", Kind: Receiver, Owner: Gordon, Decision: Migrate,
			Legacy: chartPath("scripts/webhook_enricher.py", "scripts/bounded_http.py", "scripts/dispatch_signature.py",
				"scripts/vikunja_request.py", "templates/webhook-enricher.yaml"),
			Trigger: "every Vikunja project webhook delivery", Implement: StageIntake, Cutover: StageVikunjaCut,
			Note: "Gordon verifies X-Vikunja-Signature and re-reads the task instead of adding buckets"},
		{ID: "vikunja-followup-reconciler", Kind: Loop, Owner: Gordon, Decision: Migrate,
			Legacy:    chartPath("scripts/reconcile_vikunja_followups.py", "scripts/followup_queued_notice.py", "templates/vikunja-followup-reconciler.yaml"),
			Trigger:   "every 60 s: unanswered owner /gordon comments of the last 12 h",
			Implement: StageVikunja, Cutover: StageVikunjaCut, Note: "missed-event recovery; ⏳, queued, gave-up and branch-locked notices"},
	}
}

func vikunjaLifecycle() []Entry {
	return []Entry{
		{ID: "auto-assign", Kind: EnricherHook, Owner: Gordon, Decision: Migrate,
			Legacy:    []string{"helm/kelos/scripts/webhook_enricher.py", "scripts/vikunja_sync.py"},
			Trigger:   "task.created in an opted-in project, unassigned and without no-agent, chain or discuss",
			Implement: StageVikunja, Cutover: StageVikunjaCut, Families: []run.Family{run.VikunjaAutoAssign},
			Note: "assigns the project's configured variant; chain subtasks wait for chain handoff"},
		withFamilies(hook("drag-cancel", "task.updated: card back in To-Do without pr open", "scripts/drag_cancel.py",
			"cancels the task's queued runs, deletes its live Tasks, removes in progress, posts the stop notice"),
			run.VikunjaDragCancel),
		hook("discuss-fallback", "owner /gordon on a discuss task some spawner takes", "scripts/discuss_fallback.py",
			"one-line start comment reserved by a Lease per comment"),
		hook("unanswered-notice", "owner /gordon no spawner takes", "scripts/unanswered_notice.py",
			"router emits post-notice unanswered:<reason>"),
		hook("followup-reaction", "accepted owner /gordon comment", "scripts/webhook_enricher.py", "👀 reaction; router emits react"),
		withFamilies(hook("done-unlabel", "task.updated: done task with in progress", "scripts/done_unlabel.py", ""),
			run.VikunjaDoneCleanup),
		withFamilies(hook("action-needed-unlabel", "task.updated: done task with action-needed", "scripts/action_needed.py", ""),
			run.VikunjaDoneCleanup),
		{ID: "subtask-chain", Kind: EnricherHook, Owner: Gordon, Decision: Migrate,
			Legacy:    chartPath("scripts/subtask_chain.py", "templates/subtask-chain-reconcile.yaml", "templates/_subtask-chain.tpl"),
			Trigger:   "task.updated of a chain task, its parent or a done blocker; cron */15",
			Implement: StageVikunja, Cutover: StageVikunjaCut, Families: []run.Family{run.VikunjaChain},
			Note: "runs subtasks one at a time; this migration's own chain must not dispatch twice"},
		initContainer("wait-for-blockers", "every implementation and discuss pod", "scripts/wait_for_blockers.py",
			"Gordon waits on blockers before submitting, then claims: in progress, Doing, assignee, start notice"),
		initContainer("announce-start", "every Vikunja follow-up pod", "scripts/announce_start.py",
			"start notice with its kelos:start task=<name> marker"),
		{ID: "sillage-blocker-span", Kind: InitContainer, Owner: Gordon, Decision: Migrate,
			Legacy: chartPath("scripts/sillage_marker.py", "templates/_sillage-marker.tpl"), Trigger: "blocker wait start and end",
			Implement: StageVikunja, Cutover: StageVikunjaCut, Note: "moves with the blocker wait"},
		{ID: "run-link", Kind: InitContainer, Owner: Gordon, Decision: Migrate,
			Legacy: chartPath("scripts/run_link.py", "templates/run-link.yaml", "templates/_run-link.tpl"), Trigger: "start notices",
			Implement: StageVikunja, Cutover: StageRetire, Note: "Gordon knows the Task UID it created; the Job lookup SA retires at 13"},
		{ID: "failure-reporter", Kind: Cron, Owner: Gordon, Decision: Migrate,
			Legacy:  chartPath("scripts/report_failures.py", "templates/failure-reporter.yaml"),
			Trigger: "5-59/10: Failed Tasks with a Vikunja task", Implement: StageVikunja, Cutover: StageVikunjaCut,
			Families: []run.Family{run.VikunjaFailure, run.VikunjaDoneCleanup},
			Note:     "failed notice, agent-error and action-needed; done-task sweep backstop"},
		{ID: "fetch-attachments", Kind: InitContainer, Owner: Kelos, Decision: Retain,
			Legacy: chartPath("scripts/fetch_attachments.py", "templates/_attachments.tpl"), Trigger: "every Vikunja agent pod",
			Note: "stays in the pod as the variant's identity; Gordon only passes the task reference"},
		{ID: "claim-task", Kind: AgentTool, Owner: Hollingsworth, Decision: Retain,
			Legacy: chartPath("scripts/claim_task.py"), Trigger: "agent tool in Kelos Sessions",
			Note: "no dispatch: an agent assigns its own Session task"},
	}
}

func hook(id, trigger, script, note string) Entry {
	return Entry{ID: id, Kind: EnricherHook, Trigger: trigger, Owner: Gordon, Decision: Migrate,
		Legacy: chartPath(script), Implement: StageVikunja, Cutover: StageVikunjaCut, Note: note}
}

func withFamilies(e Entry, families ...run.Family) Entry {
	e.Families = families
	return e
}

func initContainer(id, trigger, script, note string) Entry {
	e := hook(id, trigger, script, note)
	e.Kind = InitContainer
	return e
}
