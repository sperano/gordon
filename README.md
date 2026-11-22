# gordon

<img src="assets/gordon-8-bit.png" alt="Gordon Cole, 8-bit" width="160" align="right">

Kelos tasks orchestrator: one Go application that will verify GitHub and
Vikunja webhooks, decide deterministically whether and how to run, and
submit ordinary Kelos Tasks, replacing Hollingsworth's webhook glue and its
project × model × mode TaskSpawner fan-out. Kelos stays the execution
engine; Sillage keeps usage history and Lucarne run inspection.

The service runs as one image in three modes: `gordon receiver` (signed
webhook intake that stores each verified delivery before answering),
`gordon worker` (fresh-state reads, routing, the action outbox and
periodic reconciliation) and `gordon migrate` (schema and role grants).
The worker starts in shadow mode, recording decisions and executing none,
and no production routing changes: every family is owned by the legacy
Hollingsworth glue until a rollout rule with its gate evidence says
otherwise. With a dispatch configuration, every dispatched run gets the
Kelos Task it would submit, built from trusted templates; outside shadow
mode the outbox submits it, and in shadow it is compared with the legacy
spawner's Task of the same name. Outside shadow the outbox also performs
the Vikunja lifecycle's writes (claims, start notices, drag-cancel, done
cleanup, subtask chains, failure notices) with a write token, and the
GitHub lifecycle's (👀, follow-up drafts, start, round-cap and failure
notices, skipped checks, superseded reviews, conflict runs) as the GitHub
App; runs a subscription limit stopped are restarted once it resets.
[DEPLOYMENT.md](DEPLOYMENT.md) is the deployment contract, its ownership
switches and rollback included.

## Contracts

| Package | Contract |
|---|---|
| `internal/event` | Typed Vikunja and GitHub events. A *delivery key* names one transport delivery (GitHub's `X-GitHub-Delivery`; a Vikunja comment's id; a `task.updated`'s data digest, as the legacy `$.data` cache). A *request key* names the owner request behind it, so a reconciler resend is the same request and a new `/gordon` comment is a new one. |
| `internal/auth` | Per-source signature checks in the legacy formats (`X-Vikunja-Signature`, `X-Hub-Signature-256`, `X-Kelos-Signature-256`), body caps and the 401/403/404/413 answers. |
| `internal/run` | Run identities and deterministic Kelos Task names and branches, identical to the legacy spawner `nameTemplate`s that pod hooks still parse. |
| `internal/profile` | Resolved execution profiles: variant (agent, model, image, credentials), effort, permission mode and a named capability profile (ServiceAccount, read-only Workspace, capabilities) from trusted configuration only. Discuss is always read-only. `Defaults()` mirrors `values-hollingsworth.yaml`, its execution profiles included. |
| `internal/action` | Intended writes with durable keys, so an ambiguous write can be recovered before a retry. |
| `internal/rollout` | One active dispatcher per (project, family): `legacy`, `shadow` (decisions recorded, nothing executed) or `gordon`, with the gate evidence each owner requires. |
| `internal/route` | Decisions: `dispatch`, `act`, `ignore`, `reject`, `duplicate`, `stale`, `conflict` or `retry`, per family, from the delivery, the fresh upstream state and the ledger; the Vikunja lifecycle families (blocker waits and claims, drag-cancel, done cleanup, subtask chains) and the requeues of related tasks; the review-round caps, PR start notices and superseded reviews; the conflict runs. |
| `internal/inventory` | Every legacy dispatch family, hook and reconciler with its future owner, decision and stages, the Hollingsworth files each later stage changes, each Vikunja and GitHub family's ownership switch, and the rollback contract. `go run ./cmd/gordon-inventory` prints it as Markdown. |
| `internal/dispatch` | The Task a run submits: the resolved profile (agent, model, effort, ServiceAccount, Workspace) plus per-family, per-Workspace, per-profile and per-variant templates (prompt, labels, annotations, agent configs, pod pieces) that mirror the legacy spawner `taskTemplate`s; a Task referencing a Secret its profile does not grant is refused when built, as Kelos admission would refuse it. A new model or effort is one more entry, never one more TaskSpawner. |
| `internal/kelos` | The `kelos.dev/v1alpha2` Task resource, a create/get/list client for one namespace (in-cluster ServiceAccount token, re-read per request) and `Diff` of two Tasks; `kelostest` is a fake API for tests. |
| `internal/replay` | Sanitized legacy deliveries in `testdata/` with their exact expected decisions, the Task names the legacy spawners created and, for the lifecycle, the writes the legacy hooks made. |
| `internal/notice` | Gordon's Vikunja notices: the legacy `<!-- kelos:<kind> -->` markers, Gordon's action marker, and the texts, byte-equal to the legacy scripts'. A marker counts only on a comment by Gordon's own Vikunja user. |

## Service

| Package | Role |
|---|---|
| `internal/intake` | The receiver: method, declared length against the source cap, bounded slots per client address, body read under the request deadline, signature, delivery keys, then the stored row; 2xx only once stored. |
| `internal/store` | Postgres: the inbox, decisions, handled requests, runs and the action outbox; migrations under an advisory lock; leases fenced by a token, so a worker that lost its lease writes nothing; role grants. |
| `internal/worker` | Inbox processor (fresh state, route, build the Tasks, one fenced transaction), outbox (recover an ambiguous write before retrying it, defer one the provider cannot take yet), limits, backoff, graceful loops and the reconciler. |
| `internal/submit` | The Kelos submit executor (a Task of the run's name found first: this generation's is the lost create, another submitter's takes the run; per-variant limits and held branches keep submissions queued; a run cancelled meanwhile is never created), the run cancellation (drag-cancel), the review supersede, the run observer (state and `task_exists` from Kelos, its finish hooks: the run completion record and the failure notices) and the shadow comparison. |
| `internal/upstream` | GET-only Vikunja (task with relations, comments, task listings, labels) and GitHub (pull requests, their files, reviews and comments, listings) readers, bounded per provider. |
| `internal/githubapp` | The GitHub App's installation tokens (RS256 JWT), one per repository and kind of write, cached until near expiry. |
| `internal/ghlife` | The GitHub writer and its outbox executors (reactions, the draft label and conversion, label removal, skipped checks, PR notices found again by their marker), as the App; the GitHub reconciliation (missed owner requests, review-bot findings and merge-closes) and the conflict reconciliation (confirmed merge conflicts of agent PRs). |
| `internal/restart` | The limit restarts: Gordon's runs a subscription limit stopped, restarted as `<name>-retry<n>` once the limit reset, at most a configured number of times. |
| `internal/lifecycle` | The Vikunja writer and its outbox executors (labels, buckets in every manual view, assignments, label sync, reactions, notices found again by their marker), the failure hook of the run observer, and the Vikunja reconciliation (missed task changes and owner requests). |
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
dispatch configuration and the legacy Tasks rendered from Hollingsworth's
chart: the tests require Gordon's Task for each permission mode to equal
the legacy spawner's.

## Mascot

`assets/gordon-8-bit.png` is Hollingsworth's existing Gordon Cole mascot,
copied unchanged from `sperano/hollingsworth`
`assets/avatars/gordon-8-bit.png` at commit `4a50f33` (sha256
`033d2e67…4cb093`, pinned by `assets` tests). It is the `gordon` service
identity's avatar there; Hollingsworth's `assets/avatars/README.md` records
it as generated with the built-in imagegen tool from the approved concept
sheet, and declares no separate licence for it.
