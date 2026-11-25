# gordon

<img src="assets/gordon-8-bit.png" alt="Gordon Cole, 8-bit" width="160" align="right">

Kelos tasks orchestrator: one Go application that verifies GitHub and
Vikunja webhooks, decides deterministically whether and how to run, and
submits ordinary Kelos Tasks. It is Hollingsworth's only dispatcher: it
replaced the webhook glue and its project × model × mode TaskSpawner
fan-out. Kelos stays the execution
engine; Sillage keeps usage history and Lucarne run inspection.

The service runs as one image in three modes: `gordon receiver` (signed
webhook intake that stores each verified delivery before answering),
`gordon worker` (fresh-state reads, routing, the action outbox and
periodic reconciliation) and `gordon migrate` (schema and role grants).
Every dispatched run gets its Kelos Task, built from the trusted dispatch
configuration, and the outbox submits it. The outbox also performs the
Vikunja lifecycle's writes (claims, start notices, drag-cancel, done
cleanup, subtask chains, failure notices) with a write token, and the
GitHub lifecycle's (👀, follow-up drafts, start, round-cap and failure
notices, skipped checks, superseded reviews, conflict runs, the pinned
auto-merge) as the GitHub App; runs a subscription limit stopped are
restarted once it resets; agent PR heads get their readiness verdict
(ready to merge, round limit, missing checks) from fresh state, with its
labels, ntfy and auto-merge. With Sillage's API, each finished run of a
Vikunja task gets a completion notice of its estimated API-equivalent
cost and the task's total to date, kept current in place.
[DEPLOYMENT.md](DEPLOYMENT.md) is the deployment contract.

## Contracts

| Package | Contract |
|---|---|
| `internal/event` | Typed Vikunja and GitHub events. A *delivery key* names one transport delivery (GitHub's `X-GitHub-Delivery`; a Vikunja comment's id; a `task.updated`'s data digest, as the legacy `$.data` cache). A *request key* names the owner request behind it, so a reconciler resend is the same request and a new `/gordon` comment is a new one. |
| `internal/auth` | Per-source signature checks in the legacy formats (`X-Vikunja-Signature`, `X-Hub-Signature-256`, `X-Kelos-Signature-256`), body caps and the 401/403/404/413 answers. |
| `internal/run` | Run identities and deterministic Kelos Task names and branches. Follow-up, PR follow-up, review and conflict names are the legacy spawner `nameTemplate`s that pod hooks still parse; implementation and discuss names lead with the task id and carry the short variant (`vikunja-<task>-<project>-<short variant>`, discuss runs ending in `-discuss`). |
| `internal/profile` | Resolved execution profiles: variant (agent, model, image, credentials), effort, permission mode and a named capability profile (ServiceAccount, read-only Workspace, capabilities) from trusted configuration only. Discuss is always read-only; an ops run (the cluster operator) gets the configured ops profile and nothing else does; it starts only on the owner's own event, blocked or not (its pod waits for the blockers). `Defaults()` mirrors `values-hollingsworth.yaml`, its execution profiles included. |
| `internal/action` | Intended writes with durable keys, so an ambiguous write can be recovered before a retry. |
| `internal/route` | Decisions: `dispatch`, `act`, `ignore`, `reject`, `duplicate`, `stale`, `conflict` or `retry`, per family, from the delivery, the fresh upstream state and the ledger; the Vikunja lifecycle families (new-task auto-assignment, blocker waits and claims, drag-cancel, done cleanup, subtask chains) and the requeues of related tasks; the review-round caps, PR start notices and superseded reviews; the conflict runs; the PR readiness verdict and its writes. |
| `internal/dispatch` | The Task a run submits: the resolved profile (agent, model, effort, ServiceAccount, Workspace) plus per-family, per-Workspace, per-profile and per-variant templates (prompt, labels, annotations, agent configs, pod pieces) that mirror the legacy spawner `taskTemplate`s; repeated volumes and mounts collapse when equal and refuse the Task when not; a `gordonOmitIfEmpty` env entry (top-level or in an init container) is dropped when its value renders empty and it has no `valueFrom`; a Task referencing a Secret its profile does not grant is refused when built, as Kelos admission would refuse it. A new model or effort is one more entry, never one more TaskSpawner. |
| `internal/kelos` | The `kelos.dev/v1alpha2` Task resource, a create/get/list client for one namespace (in-cluster ServiceAccount token, re-read per request); `kelostest` is a fake API for tests, with `Diff` of two Tasks. |
| `internal/replay` | Sanitized deliveries in `testdata/` with their fresh state, ledger and exact expected decisions. |
| `internal/notice` | Gordon's Vikunja notices: the legacy `<!-- kelos:<kind> -->` markers, Gordon's action marker, the cost notice's revision marker, and the exact texts in `testdata/` (vigie and sillage parse them). A marker counts only on a comment by Gordon's own Vikunja user. |

## Service

| Package | Role |
|---|---|
| `internal/intake` | The receiver: method, declared length against the source cap, bounded slots per client address, body read under the request deadline, signature, delivery keys, then the stored row; 2xx only once stored. |
| `internal/store` | Postgres: the inbox, decisions, handled requests, runs and the action outbox; migrations under an advisory lock; leases fenced by a token, so a worker that lost its lease writes nothing; role grants. |
| `internal/worker` | Inbox processor (fresh state, route, build the Tasks, one fenced transaction), outbox (recover an ambiguous write before retrying it, defer one the provider cannot take yet), limits, backoff, graceful loops and the reconciler. |
| `internal/submit` | The Kelos submit executor (a Task of the run's name found first: this generation's is the lost create, another submitter's takes the run; per-variant limits and held branches keep submissions queued and tell the lifecycle why; a run cancelled meanwhile is never created), the run cancellation (drag-cancel), the review supersede, the run observer (state and `task_exists` from Kelos, its finish hooks: the run completion record, the failure notices and the cost notices). |
| `internal/upstream` | GET-only Vikunja (task with relations and label creators, comments, reactions, task listings, labels) and GitHub (pull requests, their files, reviews and comments, listings) readers, bounded per provider. |
| `internal/githubapp` | The GitHub App's installation tokens (RS256 JWT), one per repository and kind of write, cached until near expiry. |
| `internal/ghlife` | The GitHub writer and its outbox executors (reactions, the draft label and conversion, label removal, skipped checks, PR notices found again by their marker), as the App; the GitHub reconciliation (missed owner requests, review-bot findings and merge-closes), the conflict reconciliation (confirmed merge conflicts of agent PRs), the readiness backstop and reads (check runs, commit dates, review threads) and the merge pinned to the head. |
| `internal/restart` | The limit restarts: Gordon's runs a subscription limit stopped, restarted as `<name>-retry<n>` once the limit reset, at most a configured number of times. |
| `internal/sillage` | GET-only client of Sillage's cost API (`/api/v1/cost/task/<Vikunja host>/<id>`): the price table, assumptions, task total and runs by Task uid; a 404 is a task Sillage has not read yet, an answer without the contract's fields is incompatible, anything else unavailable. |
| `internal/costs` | The completion cost notices (Gordon 12): the observer's hook records each finished run of a Vikunja task once; the pass posts its notice once Sillage's estimate of the run is final (or marked not final after a deadline), keeps the task's newest notice current by editing it in place, and retries with backoff while Sillage is unavailable, never posting a zero. |
| `internal/lifecycle` | The Vikunja writer and its outbox executors (labels, buckets in every manual view, assignments, label sync, reactions, notices found again by their marker, ntfy sends bookkept in task reactions, cost notices posted or edited to their latest stored revision), the failure hook of the run observer, the held-run hooks (the `queued` label and notice of a deferred submission, the label's release once a submission dead-lettered), and the Vikunja reconciliation (missed task changes and owner requests). |
| `internal/metrics`, `internal/logging` | Prometheus metrics with fixed label sets; JSON logs with redaction. |
| `internal/config`, `internal/app`, `cmd/gordon` | Environment configuration, the three modes, health, readiness and metrics. |

## Checks

```sh
go build ./... && go vet ./... && go test ./...
```

The store, worker and service tests need Postgres: they skip without
`GORDON_TEST_DATABASE_URL` (an admin URL that may create databases and
roles). `scripts/test-postgres.sh` starts a throwaway server from
`initdb`/`pg_ctl` on `PATH` (or `PG_BIN`), UTF-8 like the shared Postgres,
and runs the checks against it. `internal/dispatch/testdata` holds a
dispatch configuration and, in `tasks/`, the golden Task of each
permission mode: the tests require Gordon's Task to equal it.

## Mascot

`assets/gordon-8-bit.png` is Hollingsworth's existing Gordon Cole mascot,
copied unchanged from `sperano/hollingsworth`
`assets/avatars/gordon-8-bit.png` at commit `4a50f33` (sha256
`033d2e67…4cb093`, pinned by `assets` tests). It is the `gordon` service
identity's avatar there; Hollingsworth's `assets/avatars/README.md` records
it as generated with the built-in imagegen tool from the approved concept
sheet, and declares no separate licence for it.
