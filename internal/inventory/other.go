package inventory

func otherEntries() []Entry {
	out := append(retiredEntries(), imageBuildEntries()...)
	out = append(out, executionEntries()...)
	return append(out, infrastructureEntries()...)
}

// retiredEntries is the legacy Wiki.js follow-up family. Wiki.js has been
// read-only legacy since 2026-10-23, yet its follow-up agents still edit
// pages and post comments there, and the family renders 152 spawners plus
// a poller. Gordon does not take it over; the owner confirms the
// retirement at Gordon 08 and Gordon 09 removes it.
func retiredEntries() []Entry {
	wiki := func(id string, kind Kind, trigger, note string, paths ...string) Entry {
		return Entry{ID: id, Kind: kind, Trigger: trigger, Legacy: chartPath(paths...), Owner: Nobody,
			Decision: Retire, Cutover: StageGitHubCut, Note: note}
	}
	return []Entry{
		wiki("wiki-comment-poller", Loop, "every 15 s: owner /gordon comments in the Wiki.js database",
			"Gordon ignores the wiki-comment source", "scripts/poll_wiki_comments.py", "scripts/pg_client.py",
			"templates/wiki-comment-poller.yaml"),
		wiki("wiki-followup", Spawner, "signed wiki-comment dispatch", "wiki-fu-<workspace>-<variant>: 19 × 8",
			"templates/wikispawners.yaml", "templates/_wiki-followup.tpl"),
		wiki("announce-wiki-start", InitContainer, "every wiki follow-up pod", "writes to read-only Wiki.js",
			"scripts/announce_wiki_start.py", "templates/announce-wiki-start.yaml"),
	}
}

// imageBuildEntries stay model-free Kelos Tasks: building and pinning
// images is execution, signed by its own dispatch key, and its push
// triggers cost one spawner per image, not a fan-out.
func imageBuildEntries() []Entry {
	paths := chartPath("templates/image-builds.yaml", "templates/_image-builds.tpl", "scripts/image_build.py")
	retain := func(id, trigger string) Entry {
		return Entry{ID: id, Kind: Spawner, Trigger: trigger, Legacy: paths, Owner: Kelos, Decision: Retain,
			Note: "Gordon ignores the image-build source"}
	}
	return []Entry{
		retain("image-build-request", "signed image-build dispatch (just image-build, agents)"),
		retain("image-build-push", "push to a configured branch of an image's repository"),
		retain("image-build-reconcile", "*/15 for coalescing images"),
	}
}

func executionEntries() []Entry {
	keep := func(id string, kind Kind, trigger, note string, paths ...string) Entry {
		return Entry{ID: id, Kind: kind, Trigger: trigger, Legacy: chartPath(paths...), Owner: Kelos,
			Decision: Retain, Note: note}
	}
	return []Entry{
		keep("kelos-branch-lock", Controller, "Tasks sharing a Workspace and spec.branch",
			"Gordon sets spec.branch; Kelos serialises", "templates/_followup.tpl"),
		keep("pr-opener", Sidecar, "agent gh pr create", "opens the one PR with the owner's PAT",
			"scripts/pr_opener.py", "templates/pr-opener.yaml"),
		keep("reviewer-scripts", Sidecar, "inside reviewer Tasks", "review_prepare, review_publish, post_check_run",
			"scripts/review_prepare.py", "scripts/review_publish.py", "scripts/post_check_run.py"),
		keep("sidecar-guard", Sidecar, "pods with native sidecars", "", "scripts/sidecar_guard.sh", "templates/sidecar-guard.yaml"),
		keep("test-postgres", Sidecar, "puckdb pods", "the postgres-test capability", "templates/_test-postgres.tpl"),
		keep("mactest", Sidecar, "solitaire pods", "client certificate mount", "templates/_mactest.tpl"),
	}
}

func infrastructureEntries() []Entry {
	keep := func(id string, kind Kind, trigger, note string, paths ...string) Entry {
		return Entry{ID: id, Kind: kind, Trigger: trigger, Legacy: chartPath(paths...), Owner: Hollingsworth,
			Decision: Retain, Note: note}
	}
	return []Entry{
		keep("dispatch-verifier", Receiver, "signed internal dispatches",
			"keeps the image-build and cluster-triage sources", "scripts/dispatch_verifier.py", "templates/dispatch-verifier.yaml"),
		keep("webhook-proxy", Receiver, "public GitHub webhook", "points /webhook/github at Gordon at 09; Kelos's GitHub source stays for image-build push",
			"templates/webhook-proxy.yaml", "templates/webhook-servers.yaml"),
		keep("cluster-triage", Spawner, "20 13 * * * and the pod-failure watcher",
			"two read-only spawners, no fan-out; retention needs the owner's approval at 09",
			"templates/cluster-triage.yaml", "templates/cluster-triage-watcher.yaml", "scripts/triage_watch.py"),
		keep("succeeded-task-cleaner", Cron, "*/10", "Gordon's ledger outlives the Tasks it deletes",
			"scripts/succeeded_task_cleaner.py", "templates/succeeded-task-cleaner.yaml"),
		keep("cheatsheet", HelmHook, "post-install/upgrade", "", "scripts/cheatsheet.py", "templates/cheatsheet.yaml"),
		keep("kelos-telemetry", Cron, "0 6 * * *", "", "templates/telemetry-cronjob.yaml"),
		keep("clone-check", Cron, "0 11 * * *", "", "scripts/clone_check.py", "templates/clone-check.yaml"),
	}
}
