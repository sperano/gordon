package inventory

// Stage is one later stage of the chain and the Hollingsworth paths it
// must change or read: every migration ships its Hollingsworth half.
type Stage struct {
	Number int
	Title  string
	// Repositories other than sperano/gordon the stage changes.
	Repositories  []string
	Hollingsworth []string
}

// hollingsworth is the repository that holds every Hollingsworth path.
var hollingsworth = []string{"sperano/hollingsworth"}

// Stages lists Gordon 02 to 13.
func Stages() []Stage { return append(buildStages(), cutoverStages()...) }

// buildStages are Gordon 02 to 06.
func buildStages() []Stage {
	return []Stage{
		{StageCapabilities, "provision and enforce named execution capabilities",
			[]string{"sperano/hollingsworth", "sperano/kelos"}, append(chartPath(
				"templates/_worker.tpl", "templates/readonly-serviceaccount.yaml", "templates/_test-postgres.tpl",
				"templates/workspace.yaml", "values.yaml", "values-hollingsworth.yaml",
				"pod_overrides_template_test.py", "puckdb_prod_access_template_test.py",
				"readonly_serviceaccount_template_test.py"),
				"helm/shared-postgres/values-hollingsworth.yaml", "helm/valkey/DATABASE_ALLOCATION.md", "tofu/kelos/puckdb.tf")},
		{StageIntake, "build durable intake, workers and observability", hollingsworth, chartPath(
			"scripts/webhook_enricher.py", "scripts/dispatch_signature.py", "scripts/bounded_http.py",
			"scripts/reconcile_vikunja_followups.py")},
		{StageDispatch, "dispatch ordinary Kelos Tasks from resolved profiles", hollingsworth, chartPath(
			"templates/taskspawners.yaml", "templates/commentspawners.yaml", "templates/followupspawners.yaml",
			"templates/reviewspawners.yaml", "templates/_worker.tpl", "templates/_followup.tpl",
			"templates/_dispatch.tpl", "templates/_correlation.tpl", "templates/_attachments.tpl",
			"templates/_pull-request-opener.tpl", "templates/_limit.tpl", "templates/agentconfigs.yaml")},
		{StageShadow, "deploy isolated shadow service and monitoring", []string{"sperano/hollingsworth"},
			append(chartPath("templates/webhook-proxy.yaml", "templates/webhook-enricher.yaml", "values-hollingsworth.yaml"),
				"helm/gordon/", "argocd-apps/appset.yaml", "tofu/kelos/dispatch.tf", "tofu/kelos/main.tf",
				"helm/shared-postgres/values-hollingsworth.yaml", "helm/prometheus/", "assets/avatars/gordon-8-bit.png")},
		{StageVikunja, "implement Vikunja lifecycle orchestration", hollingsworth, chartPath(
			"scripts/drag_cancel.py", "scripts/discuss_fallback.py", "scripts/unanswered_notice.py",
			"scripts/subtask_chain.py", "scripts/done_unlabel.py", "scripts/action_needed.py",
			"scripts/wait_for_blockers.py", "scripts/announce_start.py", "scripts/followup_queued_notice.py",
			"scripts/report_failures.py", "scripts/notice_kind.py", "scripts/vikunja_request.py",
			"scripts/sillage_marker.py", "scripts/run_link.py", "scripts/reconcile_vikunja_followups.py",
			"scripts/webhook_enricher.py")},
	}
}

// cutoverStages are Gordon 07 to 13.
func cutoverStages() []Stage {
	return []Stage{
		{StageVikunjaCut, "cut over Vikunja routing with rollback", []string{"sperano/hollingsworth"},
			append(chartPath("values-hollingsworth.yaml", "templates/webhook-enricher.yaml", "templates/taskspawners.yaml",
				"templates/commentspawners.yaml", "templates/vikunja-followup-reconciler.yaml",
				"templates/subtask-chain-reconcile.yaml", "templates/failure-reporter.yaml", "templates/wait-for-blockers.yaml",
				"templates/_worker.tpl", "templates/_followup.tpl", "templates/_subtask-chain.tpl", "templates/_sillage-marker.tpl"),
				"scripts/vikunja_sync.py", "helm/gordon/values-hollingsworth.yaml")},
		{StageGitHub, "implement GitHub, review and reconciliation workflows", hollingsworth, chartPath(
			"scripts/github_reactor.py", "scripts/review_skip.py", "scripts/followup_request.py", "scripts/followup_ready.py",
			"scripts/cap_review_rounds.py", "scripts/dedupe_pr_task.py", "scripts/supersede_reviews.py",
			"scripts/announce_github_start.py", "scripts/poll_review_bot.py", "scripts/reconcile_conflicts.py",
			"scripts/merge_close.py", "scripts/pr_open.py", "scripts/reconcile_merge_close.py",
			"scripts/reconcile_pr_ready.py", "scripts/pr_ready_notify.py", "scripts/auto_merge.py",
			"scripts/missing_checks.py", "scripts/review_findings.py", "scripts/limit_reaper.py")},
		{StageGitHubCut, "cut over GitHub and remaining orchestration families", []string{"sperano/hollingsworth"},
			append(chartPath("values-hollingsworth.yaml", "templates/followupspawners.yaml", "templates/reviewspawners.yaml",
				"templates/reviewpollspawners.yaml", "templates/conflictspawners.yaml", "templates/conflictreconcilers.yaml",
				"templates/conflict-reconcile.yaml", "templates/mergespawners.yaml", "templates/merge-close-reconcile.yaml",
				"templates/pr-readiness.yaml", "templates/_pr-readiness.tpl", "templates/github-reactor.yaml",
				"templates/review-poller.yaml",
				"templates/webhook-proxy.yaml", "templates/limit-reaper.yaml", "templates/wikispawners.yaml",
				"templates/wiki-comment-poller.yaml", "templates/announce-wiki-start.yaml", "templates/_followup.tpl",
				"templates/_review.tpl", "templates/_pr-task-dedup.tpl", "templates/pr-review-supersede.yaml",
				"templates/_limit.tpl", "templates/announce-github-start.yaml", "templates/_conflict.tpl",
				"templates/merge-close.yaml", "templates/_wiki-followup.tpl", "scripts/poll_wiki_comments.py",
				"scripts/pg_client.py", "scripts/announce_wiki_start.py"),
				"tofu/kelos/main.tf", "tofu/kelos/repos.tf", "helm/gordon/values-hollingsworth.yaml")},
		{StageUsage, "retain normalized Kelos usage without double counting",
			[]string{"sperano/kelos", "sperano/hollingsworth"}, append(chartPath("Chart.yaml", "values-hollingsworth.yaml"),
				"docker/kelos-agent/")},
		{StageCosts, "add Sillage API-equivalent cost history and breakdowns",
			[]string{"sperano/sillage", "sperano/hollingsworth"}, []string{"helm/sillage/", "tofu/sillage/"}},
		{StageEstimates, "publish completion estimates and evolving task totals", []string{"sperano/hollingsworth"},
			[]string{"helm/gordon/values-hollingsworth.yaml", "helm/sillage/"}},
		{StageRetire, "deploy cost reporting and retire migrated glue", []string{"sperano/hollingsworth"},
			append(chartPath("templates/", "scripts/", "values.yaml", "values-hollingsworth.yaml", "README.md"),
				"tofu/kelos/", "portal-docs/docs/")},
	}
}
