package inventory

import "github.com/sperano/gordon/internal/run"

// GitHub prerequisites.
const (
	needApp = "`GORDON_GITHUB_APP_DIR` (the Gordon Cole App's `appID`, `installationID` and `privateKey`; " +
		"its installation lists the repository with Pull requests, Contents, Issues and Checks: write)"
	needGitHubReconcile = "`GORDON_GITHUB_RECONCILE_INTERVAL` > 0 (missed owner requests, review-bot findings and merge-closes)"
	needNoFollowUpHooks = "a dispatch config whose Tasks of the family carry no `pr-task-dedup`, `review-round-cap` " +
		"or `announce-github-start` init container (Gordon deduplicates, caps and announces)"
	needNoReviewHooks = "a dispatch config whose Tasks of the family carry no `pr-task-dedup` " +
		"or `pr-review-supersede` init container (Gordon deduplicates and supersedes)"
	needNoConflictHooks = "a dispatch config whose Tasks of the family carry no `announce-github-start` " +
		"init container (Gordon announces)"
	needConflictReconcile = "`GORDON_CONFLICT_RECONCILE_INTERVAL` > 0 (the scan that finds conflicts: " +
		"push and pull_request events start none)"
	needList = "`list` on `tasks` in `kelos-agents`"
)

// GitHubSwitches lists the GitHub families' contracts, in run.Families
// order: what the Hollingsworth change that makes Gordon their owner
// (Gordon 09) turns off for the rule's projects, the repository each
// project's PRs are on.
func GitHubSwitches() []Switch {
	return []Switch{
		{run.PRFollowUp, chartPath("templates/followupspawners.yaml", "scripts/github_reactor.py",
			"scripts/followup_request.py", "scripts/cap_review_rounds.py", "scripts/announce_github_start.py",
			"scripts/dedupe_pr_task.py", "scripts/poll_review_bot.py", "templates/review-poller.yaml",
			"templates/reviewpollspawners.yaml"),
			"the project's `pr-followup-<workspace>-<variant>` and review-poll spawners, the GitHub reactor's 👀, draft label " +
				"and draft conversion for its repository, and the review poller's repository entry",
			[]string{needKelos, needApp, needNoFollowUpHooks, needGitHubReconcile}},
		{run.PRReview, chartPath("templates/reviewspawners.yaml", "scripts/review_skip.py", "scripts/supersede_reviews.py",
			"templates/pr-review-supersede.yaml", "scripts/dedupe_pr_task.py"),
			"the project's `pr-review-<workspace>-<reviewer>` spawners and the GitHub reactor's skipped review checks for its repository",
			[]string{needKelos, needApp, needDelete, needNoReviewHooks}},
		{run.PROpen, chartPath("templates/mergespawners.yaml", "scripts/pr_open.py"),
			"the repository's pr-open merge spawner (opened, reopened)",
			[]string{needWriter}},
		{run.MergeClose, chartPath("templates/mergespawners.yaml", "scripts/merge_close.py", "templates/merge-close.yaml",
			"scripts/reconcile_merge_close.py", "templates/merge-close-reconcile.yaml"),
			"the repository's merge-close merge spawner and its `merge-close-reconcile` CronJob entry",
			[]string{needWriter, needGitHubReconcile}},
		{run.PRConflict, chartPath("templates/conflictspawners.yaml", "templates/_conflict.tpl", "scripts/reconcile_conflicts.py",
			"templates/conflictreconcilers.yaml", "templates/conflict-reconcile.yaml", "scripts/announce_github_start.py"),
			"the Workspace's `pr-conflict-<workspace>-<variant>` spawners and its `conflict-reconcile-event-<workspace>` " +
				"and `conflict-reconcile-cron-<workspace>` reconcilers",
			[]string{needKelos, needApp, needNoConflictHooks, needConflictReconcile}},
		{run.LimitRestart, chartPath("scripts/limit_reaper.py", "templates/limit-reaper.yaml"),
			"the limit reaper for Gordon's Tasks: it must skip Tasks labelled `gordon.spe.quebec/managed` of the rule's " +
				"projects (Gordon restarts them, and its failure notices skip limit stops); it keeps every other Task",
			[]string{needKelos, needList, needWriter}},
	}
}
