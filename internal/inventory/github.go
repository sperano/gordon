package inventory

import "github.com/sperano/gordon/internal/run"

func githubEntries() []Entry {
	return append(githubDispatch(), githubReconcilers()...)
}

func gh(id string, kind Kind, trigger string, families []run.Family, note string, paths ...string) Entry {
	return Entry{ID: id, Kind: kind, Trigger: trigger, Legacy: chartPath(paths...), Owner: Gordon,
		Decision: Migrate, Implement: StageGitHub, Cutover: StageGitHubCut, Families: families, Note: note}
}

func githubDispatch() []Entry {
	return []Entry{
		gh("pr-followup", Spawner, "owner /gordon comment, inline comment or review; review-bot findings",
			[]run.Family{run.PRFollowUp}, "pr-followup-<workspace>-<variant>; variant from the PR's kelos:<variant> label",
			"templates/followupspawners.yaml", "templates/_followup.tpl"),
		gh("github-reactor", Receiver, "mirrored GitHub deliveries: owner requests, bot findings",
			[]run.Family{run.PRFollowUp}, "👀 reaction, kelos:follow-up label and draft conversion",
			"scripts/github_reactor.py", "scripts/followup_request.py", "templates/github-reactor.yaml"),
		gh("review-skip", Receiver, "pull_request by renovate/dependabot, or kelos:no-review on image-build/ or onboard/",
			[]run.Family{run.PRReview}, "skipped review/pr-challenger check run", "scripts/review_skip.py"),
		gh("pr-review", Spawner, "pull_request opened, synchronize, ready_for_review (not draft)",
			[]run.Family{run.PRReview}, "pr-review-<workspace>-<reviewer>; stack reviewers by file pattern",
			"templates/reviewspawners.yaml", "templates/_review.tpl"),
		gh("pr-task-dedup", InitContainer, "first init container of review and PR follow-up pods", nil,
			"replaced by the run ledger; the skip marker stays until cutover", "scripts/dedupe_pr_task.py", "templates/_pr-task-dedup.tpl"),
		gh("pr-review-supersede", InitContainer, "every reviewer pod", nil,
			"Gordon cancels older in-flight reviews of the PR itself", "scripts/supersede_reviews.py", "templates/pr-review-supersede.yaml"),
		gh("review-round-cap", InitContainer, "review-triggered PR follow-up pods", nil,
			"50 bot rounds per PR, 4 fix-suggestion rounds; decided before submission",
			"scripts/cap_review_rounds.py", "templates/_limit.tpl"),
		gh("announce-github-start", InitContainer, "PR follow-up and conflict pods", nil,
			"start notice with its JOB UID row and kelos:start marker",
			"scripts/announce_github_start.py", "templates/announce-github-start.yaml"),
		gh("pr-conflict", Spawner, "signed pr-conflict dispatch of a dirty agent PR", nil,
			"pr-conflict-<id>: one episode per base/head pair", "templates/conflictspawners.yaml", "templates/_conflict.tpl"),
		gh("pr-open", Spawner, "pull_request opened/reopened on kelos/vikunja-<id>", []run.Family{run.PROpen},
			"model-free Task today; Gordon writes directly", "templates/mergespawners.yaml", "scripts/pr_open.py"),
		gh("merge-close", Spawner, "pull_request closed on kelos/vikunja-<id>", []run.Family{run.MergeClose},
			"model-free Task today; Gordon writes directly", "templates/mergespawners.yaml", "scripts/merge_close.py",
			"templates/merge-close.yaml"),
		gh("limit-reaper", Cron, "*/10: Tasks failed on a subscription limit", nil,
			"restart as <name>-retry<n> after the reset, at most 3", "scripts/limit_reaper.py", "templates/limit-reaper.yaml"),
	}
}

func githubReconcilers() []Entry {
	return []Entry{
		gh("review-poller", Cron, "*/5: open PRs with an unanswered review-bot review", []run.Family{run.PRFollowUp},
			"missed-webhook recovery for bot findings", "scripts/poll_review_bot.py", "templates/review-poller.yaml",
			"templates/reviewpollspawners.yaml"),
		gh("conflict-reconcile", Cron, "push to a base, PR events, hourly backstop", nil,
			"model-free mergeability scan that dispatches pr-conflict", "scripts/reconcile_conflicts.py",
			"templates/conflictreconcilers.yaml", "templates/conflict-reconcile.yaml"),
		gh("merge-close-reconcile", Cron, "*/15: agent PRs closed in the last 24 h", []run.Family{run.MergeClose},
			"missed-webhook recovery for merge-close", "scripts/reconcile_merge_close.py", "templates/merge-close-reconcile.yaml"),
		gh("pr-readiness", Spawner, "check_run completed; */5 backstop", nil,
			"exact-head readiness, ready/round-limit notices, ntfy, auto-merge, missing checks",
			"scripts/reconcile_pr_ready.py", "scripts/pr_ready_notify.py", "scripts/auto_merge.py",
			"scripts/missing_checks.py", "templates/pr-readiness.yaml"),
	}
}
