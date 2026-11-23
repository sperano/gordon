# Deployment contract

What a deployment of Gordon (Hollingsworth `helm/gordon`, Gordon 05) must
provide. One image (`Dockerfile`), one binary, three modes chosen by the
first argument. Nothing here is deployed by this repository.

## Modes and identities

| Mode | Runs as | Database role | Secrets | Kubernetes rights |
|---|---|---|---|---|
| `gordon migrate` | Job or init container, before each rollout | schema owner (DDL) | owner URL | none |
| `gordon receiver` | Deployment behind the webhook ingress | receiver: `INSERT` on `inbox`, `SELECT (id, delivery_key)` on `inbox`, `SELECT` on `schema_migrations` | receiver URL, webhook keys | none (no ServiceAccount token: `automountServiceAccountToken: false`) |
| `gordon worker` | Deployment, no ingress | worker: `SELECT, INSERT` on `decisions`, `requests`, `runs`, `actions`, `shadow_tasks`, `run_completions` and `cost_notices`, and `UPDATE` of their mutable columns only (a request's outcome and `executed`, a run's state, rerun fields, built Task and observed Task (`task_exists`, `task_uid`, `phase`, `observed_at`), an action's queue columns, a shadow Task's comparison, a cost notice's publication (`state`, `rev`, `body`, `digest`, `failures`, `last_error`, `next_check`, `published_at`, `live_until`, `updated_at`); nothing on `decisions` and `run_completions`; `internal/store/grants.go` lists the exact columns); on `inbox` `SELECT`, `UPDATE` of its queue columns and `body`, and `INSERT` of its own synthetic deliveries (requeues and reconciliation: `source, delivery_key, request_key, kind, scope, headers, body`, never `body_digest`; a row policy refuses any row but `source` `vikunja` or `github` with the `X-Gordon-Synthetic` header); no `DELETE`, no DDL; a role distinct from the receiver's (`migrate` refuses one role for both) | worker URL, Vikunja read token, read-only GitHub token, optionally the Vikunja write token and the GitHub App's keys | with `GORDON_DISPATCH_CONFIG_FILE`: `get`, `list` and (outside shadow) `create` on `kelos.dev` `tasks` in `kelos-agents`, plus `delete` once Gordon owns `vikunja-drag-cancel` or `pr-review` (superseded reviews); nothing else (no Secrets, no pods) |

`gordon migrate` grants the receiver and worker roles (`GORDON_RECEIVER_ROLE`,
`GORDON_WORKER_ROLE`) at every run, revoking first every right a role holds on Gordon's tables, so a role moved
from one mode to the other keeps nothing of the old one. A new table or receiver column is
granted only once `internal/store/grants.go` lists it: change that list in
the release that adds the migration. The roles themselves (logins and passwords) are
provisioned outside Gordon (shared Postgres, `tofu/kelos`). Migrations run
under an advisory lock, each in its own transaction with its version row:
concurrent or interrupted runs are safe, and an already migrated database
is a no-op. Migrations are additive; readiness accepts a schema newer than
the binary, so the old pods keep serving during a rollout.

The receiver never holds an API token and the worker never holds a webhook
key: the receiver verifies each signature before storing the delivery, and
the worker decides only stored (verified) rows.

## Environment

Secrets are always files (mounted Secrets), named by a `*_FILE` variable.

Every mode:

| Variable | Default | |
|---|---|---|
| `GORDON_DATABASE_URL_FILE` | required | Postgres URL of the mode's role |
| `GORDON_ADMIN_ADDR` | `:9090` | `receiver` and `worker` only (`migrate` serves nothing): `/healthz`, `/readyz`, `/metrics` (not exposed by the ingress) |
| `GORDON_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

`migrate`: `GORDON_RECEIVER_ROLE`, `GORDON_WORKER_ROLE` (optional; empty grants nothing).

`receiver`:

| Variable | Default | |
|---|---|---|
| `GORDON_LISTEN_ADDR` | `:8080` | `POST /webhook/{vikunja,github,wiki-comment,image-build}` |
| `GORDON_VIKUNJA_KEY_FILE` | — | `X-Vikunja-Signature` key (Vikunja webhook secret) |
| `GORDON_GITHUB_KEY_FILE` | — | `X-Hub-Signature-256` key (GitHub webhook secret) |
| `GORDON_WIKI_COMMENT_KEY_FILE`, `GORDON_IMAGE_BUILD_KEY_FILE` | — | `X-Kelos-Signature-256` dispatch keys |
| `GORDON_MAX_IN_FLIGHT` | `32` | requests read and stored at once; one client address gets a quarter. Each holds its whole declared body, so the receiver's memory limit must cover this times the largest body cap |
| `GORDON_TRUSTED_PROXIES` | empty | comma-separated addresses or CIDRs of the webhook ingress proxy only (its pod or Service address, never the whole pod CIDR: any pod in a trusted range can choose its `X-Forwarded-For` and so the client its slots are charged to); a request from one is charged to the nearest untrusted `X-Forwarded-For` address. Required behind a proxy: otherwise every sender shares the proxy's quarter. Keep the receiver reachable from the ingress only (a NetworkPolicy) |
| `GORDON_REQUEST_DEADLINE` | `30s` | whole request: slot wait and body |
| `GORDON_SHUTDOWN_GRACE` | `25s` | in-flight requests on SIGTERM (keep under `terminationGracePeriodSeconds`) |

At least one key file is required, and each one set must be readable and
non-empty at start; a source without a key refuses every delivery (403
signed, 401 unsigned). Key files are re-read per request, so a rotated
Secret applies without a restart. The proxy in front must append the
client address to `X-Forwarded-For` (nginx `$proxy_add_x_forwarded_for`).

`worker`:

| Variable | Default | |
|---|---|---|
| `GORDON_SHADOW` | `true` | record decisions, execute nothing (no outbox loop at all) |
| `GORDON_WORKER_ID` | hostname | lease owner name (the pod name) |
| `GORDON_VIKUNJA_URL` | required | API base, e.g. `https://vikunja.example/api/v1` |
| `GORDON_VIKUNJA_TOKEN_FILE` | required | read token: `GET /tasks/{id}` and its comments; with the reconciliation also `GET /tasks` (listing) and `GET /labels`. Re-read per request, so a rotated Secret applies without a restart |
| `GORDON_GITHUB_API_URL` | `https://api.github.com` | |
| `GORDON_GITHUB_TOKEN_FILE` | required | read-only token for pull requests and their files. Re-read per request, so a rotated Secret applies without a restart |
| `GORDON_ROUTING_CONFIG_FILE` | `profile.Defaults()` | JSON `profile.Config` (unknown fields refused) |
| `GORDON_OWNERSHIP_FILE` | empty (all legacy) | JSON `rollout.Table`; refused when invalid or ungated |
| `GORDON_WORKER_CONCURRENCY` | `4` | per queue: inbox items decided at once, and separately outbox actions performed at once (up to twice this in flight per process outside shadow mode) |
| `GORDON_PROJECT_LIMIT` | `2` | per project or repository (the delivery's Vikunja project or GitHub repository), inbox and outbox together; `0` is unbounded |
| `GORDON_UPSTREAM_LIMITS` | `vikunja=4,github=4` | per provider: reads in flight, and separately outbox actions in flight; each provider at most once. Kelos submissions take the default, 4 |
| `GORDON_LEASE_TTL` | `2m` | must exceed `GORDON_UPSTREAM_TIMEOUT`; an item's fresh-state reads, and each outbox attempt (recovery and write), are cut at half of it, the write then being ambiguous |
| `GORDON_UPSTREAM_TIMEOUT` | `10s` | one upstream request |
| `GORDON_POLL_INTERVAL` | `1s` | idle wait between claims |
| `GORDON_MAX_ATTEMPTS` | `20` | inbox retries and attempted actions; then the row is dead-lettered (`failed`). A deferred action (no executor yet, a full limit, a held branch) counts no attempt and stays queued |
| `GORDON_RETRY_BASE`, `GORDON_RETRY_MAX` | `5s`, `10m` | exponential backoff |
| `GORDON_DEFER_DELAY` | `2s` | wait of a row deferred by a full provider or project limit; a full Kelos variant limit, a held branch or a timed-out admission lock wait waits `GORDON_KELOS_BUSY_DELAY` |
| `GORDON_RECONCILE_INTERVAL` | `30s` | lease expiry, queue gauges, body pruning |
| `GORDON_BODY_RETENTION` | `168h` | stored bodies of finished rows, and the built Tasks (prompts) of finished runs and compared shadow Tasks, are dropped after this; keys, decisions, requests and runs stay |
| `GORDON_SHUTDOWN_GRACE` | `25s` | in-flight items on SIGTERM; past it their leases expire and another worker recovers them |
| `GORDON_DISPATCH_CONFIG_FILE` | empty | JSON `dispatch.Config` (unknown fields refused; below). Without it runs carry no Task, nothing touches Kelos, and `GORDON_SHADOW=false` is refused |
| `GORDON_KELOS_API_URL` | `https://kubernetes.default.svc` | Kubernetes API base |
| `GORDON_KELOS_TOKEN_FILE` | the pod's ServiceAccount token | re-read per request (projected tokens rotate) |
| `GORDON_KELOS_CA_FILE` | the pod's ServiceAccount `ca.crt` | CA bundle trusted for the API server; an empty value is the same as unset |
| `GORDON_KELOS_BUSY_DELAY` | `30s` | wait of a submission deferred by a full variant limit, a held branch or a timed-out wait for the admission lock |
| `GORDON_SHADOW_WINDOW` | `15m` | how long a shadow Task waits for the legacy Task of its name before it is recorded `missing` |
| `GORDON_VIKUNJA_WRITE_TOKEN_FILE` | empty | the token the Vikunja lifecycle writes with (labels, buckets, assignees, reactions, comments), Gordon Cole's (`policy.vikunja_service_user`): only that user's comments count as Gordon's notices. Without it Vikunja actions stay queued (deferred); outside shadow a Gordon-owned family that writes to Vikunja is refused without it, or with an empty `policy.vikunja_service_user`, `policy.vikunja_web_url` or `policy.cancel_reaction`. Re-read per request, so a rotated Secret applies without a restart |
| `GORDON_VIKUNJA_RECONCILE_INTERVAL` | `0` (off) | pace of the Vikunja reconciliation: every open task of the configured projects (and every done one still labelled `in progress` or `action-needed`) whose state the router may act on is queued as a synthetic `task.updated`, once per state; owner `/gordon` comments of the window that no delivery brought are queued too. Needs a read token that may list tasks (`tasks.read_all`) |
| `GORDON_FOLLOWUP_LOOKBACK`, `GORDON_FOLLOWUP_REDISPATCH_AFTER` | `12h`, `2m` | the window of the owner requests the reconciliation recovers: older than the second (the webhook may still be on its way), younger than the first |
| `GORDON_LUCARNE_URL`, `GORDON_LUCARNE_CLUSTER`, `GORDON_SILLAGE_URL` | empty | the start and failure notices' private links (`<lucarne>/runs/<cluster>/kelos-agents/<Task uid>`, `<sillage>/task/<Vikunja host>/<task id>`, on a pull request `<sillage>/open?url=<PR URL>`, only on a private repository); unset, no link |
| `GORDON_GITHUB_APP_DIR` | empty | directory of the GitHub App's keys, one file each: `appID`, `installationID`, `privateKey` (PEM, PKCS#1 or PKCS#8; the `github-app` Secret). Every GitHub write uses an installation token narrowed to its repository and that write's permissions, minted once per scope and cached until near expiry. Without it GitHub actions stay queued (deferred); outside shadow a Gordon-owned `pr-followup`, `pr-review`, `pr-conflict` or `pr-readiness` is refused without it, and a shadow `pr-readiness` too (it reads check runs as the App) |
| `GORDON_GITHUB_RECONCILE_INTERVAL` | `0` (off) | pace of the GitHub reconciliation: for each configured repository's open pull requests by a pull-request author, the owner's `/gordon` comments, inline comments and reviews and each review bot's latest review with a body, not handled yet, and those pull requests closed from an agent branch, are queued as synthetic deliveries, each once |
| `GORDON_GITHUB_RECONCILE_LOOKBACK`, `GORDON_GITHUB_RECONCILE_GRACE` | `24h`, `10m` | its window: submitted, created or closed within the first, and longer ago than the second (the webhook may still be on its way) |
| `GORDON_CONFLICT_RECONCILE_INTERVAL` | `0` (off) | pace of the conflict reconciliation: for each Workspace of a project whose `pr-conflict` is not legacy's, the repository's open agent PRs based on the Workspace's ref (`base_ref` of its first project, `main` by default) or stacked on another such PR are read one by one, and each GitHub reports in conflict (mergeable `false`, `dirty`) whose episode has no run left is queued, once an hour. Push and `pull_request` events start no scan: this interval is the detection delay. One read-only GitHub read per candidate PR and pass |
| `GORDON_LIMIT_RESTART_MAX` | `3` | restarts of one chain after a subscription limit; then the gave-up notice |
| `GORDON_LIMIT_RESTART_GRACE`, `GORDON_LIMIT_RESTART_FALLBACK` | `2m`, `1h` | a limit stop restarts this long after its reset; without a known reset time, the fallback after its Task's completion |
| `GORDON_SILLAGE_API_URL` | empty (off) | Sillage's read API, reached in the cluster (`http://sillage.sillage.svc`, not the public host behind forward-auth): the completion cost notices read `GET /api/v1/cost/task/<Vikunja host>/<task id>` (sperano/sillage#57 or later). Unset, the cost notice pass is off; outside shadow a Gordon-owned `vikunja-cost` is refused without it |
| `GORDON_COST_WAIT`, `GORDON_COST_DEADLINE` | `2m`, `6h` | a finished run's notice is checked every `WAIT` until Sillage's estimate of the run is final; `DEADLINE` after the run finished, it is posted with what Sillage has, marked not final. `WAIT` must be below `DEADLINE` |
| `GORDON_COST_REFRESH`, `GORDON_COST_LIVE_FOR` | `15m`, `336h` | a task's newest notice is re-read every `REFRESH` and edited in place when its figures change, until `LIVE_FOR` after its last revision |
| `GORDON_READINESS_RECONCILE_INTERVAL` | `0` (off) | pace of the readiness backstop: every open readiness candidate (an allowed author's non-draft PR from a `kelos/vikunja-<id>` branch of the repository) of each repository whose PR project's `pr-readiness` is not legacy's is queued as a synthetic `pull_request` (action `gordon-readiness`), once per head and interval. It replaces the legacy cron scan and the event waiter's polling (`eventWaitSeconds`, `pollSeconds`): `1m` keeps the legacy latency. Required, with the App and the dispatch config, by a non-legacy `pr-readiness` rule |
| `GORDON_NTFY_URL`, `GORDON_NTFY_TOPIC`, `GORDON_NTFY_TOKEN_FILE` | empty | the private ntfy publisher of a ready head (`prReadiness.notify`): an HTTPS base URL (refused otherwise), the topic and its bearer token. Outside shadow a Gordon-owned `pr-readiness` is refused without them while `policy.readiness.notify` is on |

## Dispatch configuration

`GORDON_DISPATCH_CONFIG_FILE` is the trusted half of the legacy spawners'
`taskTemplate`s; the routing configuration (`GORDON_ROUTING_CONFIG_FILE`,
`profile.Config`) already resolves each run's agent, image, model, effort,
credentials, ServiceAccount and (read-only) Workspace. Hollingsworth's
`helm/gordon` (Gordon 05) renders it from the same templates as the
spawners; `internal/dispatch/testdata/hollingsworth.json` is made from the
TaskSpawners rendered from `helm/kelos` at `bf18aa5`, with their variables
substituted, and the tests hold Gordon's Tasks equal to
the legacy ones.

| Field | |
|---|---|
| `version` | required; recorded on each Task (`gordon.spe.quebec/config-version`, after the routing version) |
| `namespace` | `kelos-agents` (the only value: every submit action names it) |
| `comments`, `comments_max_bytes` | quote `GET /tasks/{id}/comments` as `Context.comments` (default cap 256 KiB; a failed or larger read is quoted empty, as the legacy `failurePolicy: Ignore`) |
| `core_labels` | exact, unpadded Vikunja or GitHub label names mapped to positive whole-core CPU requests; after the family, Workspace, profile and variant pod pieces are composed, a selection replaces only the resulting `resources.requests.cpu`; unconfigured labels are inert, aliases for one value may coexist, and labels selecting different values reject the Task |
| `common_secrets` | Secrets every profile may reference (the admission policy's `commonSecrets`: runner credentials, the variants' Vikunja and Authentik identities, Gordon's own, the run-link token) |
| `families` | per family (`vikunja-implement`, `vikunja-discuss`, `vikunja-followup`, `vikunja-followup-answer`, `vikunja-followup-discuss`, `pr-followup`, `pr-review`): `prompt`, `labels`, `annotations`, `agent_configs` (`@project` and `@vikunja` expand to the project's and the variant's), `ttl_seconds_after_finished`, `pod_failure_policy`, `pod` |
| `workspaces`, `profiles`, `variants` | pod pieces per Workspace (keyed by the project's writing Workspace; they apply to its read-only copy too), per execution profile (with the `secrets` it grants and its reserved pod `labels`, which nothing else may set) and per variant (`agent_config_suffix`, `vikunja_agent_config`, `pod`). Every routing variant and profile needs an entry, `{}` at least: a run without one is rejected (`task-spec-unknown-variant`, `task-spec-unknown-profile`) |
| `pod` (each piece) | `labels` (profiles only), `resources`, `affinity`, `env`, `extraInitContainers`, `lastInitContainers`, `volumes`, `volumeMounts`: `resources` and `affinity` objects, each list entry an object with a `name`. Lists concatenate family, Workspace, profile, variant, then volumes repeated by `name` and volume mounts repeated by `mountPath` collapse to the first when equal and reject the Task (`task-spec-pod-invalid`) when they differ; an `env` entry, top-level or in an init container's `env`, marked `"gordonOmitIfEmpty": true` is dropped when its rendered `value` is empty and it has no `valueFrom` (a kept one loses the key), and the key is refused anywhere else; every piece's `lastInitContainers` come after every piece's `extraInitContainers` (the family's `pr-opener` after the puckdb profile's `test-postgres`, as the spawners order them) |
| `limits` | `default` and per-`variants` caps on unfinished, non-`Waiting` Tasks per variant (`vikunja.spe.quebec/variant`, legacy Tasks included); `0` is unbounded; a `variants` key not in `variants` is refused |

Prompt, label and annotation values, agent config names and pod strings
(pod `labels` aside: they are literal, and a `{{` in one is refused)
holding `{{` are Go templates over the legacy Kelos variables (`.task`,
`.comment`, `.Title`, `.Body`, `.description`, `.author`,
`.Context.comments`, `.Number`, `.URL`, `.Branch`, `.Event`, `.Action`,
`.CommentURL`, `.CommentBody`, `.Payload...`) plus `.project`,
`.project_title`, `.workspace`, `.repository`, `.variant`, `.reviewer`,
`.profile`, `.mode`, `.model`, `.effort`, `.branch` (`dispatch.Data`); a
template that does not parse refuses the config at startup, a missing
variable refuses the run. Gordon alone sets
`kelos.spe.quebec/profile`, `kelos.spe.quebec/mode`,
`vikunja.spe.quebec/discuss` (discuss), `vikunja.spe.quebec/variant`,
`gordon.spe.quebec/managed` and the `gordon.spe.quebec/inbox` annotation
(the generation's marker that tells its own Task from another
submitter's); the ServiceAccount and Workspace come from the profile. A
pod is the family's, Workspace's, profile's and variant's pieces in that
order. A run that cannot be built, or whose Task references a Secret its
profile does not grant, is a `reject` (`task-spec-<code>`) with nothing
queued.

Limits and the branch check are exact across worker replicas: each
check and the create it admits hold a Postgres advisory lock. A held branch (an unfinished Task on the same
Workspace and `spec.branch`) keeps the submission in the outbox instead of
a `Waiting` Task in Kelos. The database must be UTF-8 (built Tasks are
`jsonb` with non-ASCII text).

## Vikunja lifecycle (Gordon 06)

Gordon decides the Vikunja lifecycle the legacy hooks, init containers and
reconcilers performed, as durable actions on its families. Nothing changes
until the ownership table makes Gordon a family's owner, outside shadow,
with a write token; the routing configuration's `policy` carries the
buckets and labels (`backlog_bucket`, `agent_error_label`,
`action_needed_label`, `auto_merge_label` and its trusted
`auto_merge_label_creator`, `vikunja_service_user`, `vikunja_web_url`,
`cancel_reaction`, and `pr_readiness`, legacy `PR_READINESS_ENABLED`: the
pr-open notice promises a ready-to-merge one).

- **New-task assignment** (`vikunja-auto-assign`): a project's
  `auto_assign_variant` opts it in. A `task.created` snapshot and the fresh
  task must both be unassigned and lack `no-agent`, `chain`, and `discuss`;
  a subtask of a `chain` parent waits for chain handoff. The worker resolves
  each configured username to a project user id at startup and refuses an
  unknown or inaccessible user. The executor rechecks assignment, labels,
  and project before writing, so a retry never adds a second assignee.
- **Blockers and claims** (`vikunja-implement`, `vikunja-discuss`): a run
  waits, unsubmitted, while a task blocking it (any project) is open; a
  blocker getting done requeues the tasks it blocked. Once submitted, the
  run claims the task: `in progress` (not on a card dragged back since),
  the stale `agent-error` and `action-needed` labels off, Doing in every
  manual Kanban view, the start notice. Follow-ups announce their start
  and clear the same labels. Each claim waits for its run's submission:
  a refused submission claims nothing.
- **Drag-cancel** (`vikunja-drag-cancel`): a card in the backlog bucket
  and in no Doing bucket, its PR not open, with a live run: the runs are
  cancelled under the admission lock (a queued submission is never
  created), the task's live agent Tasks deleted (limit restarts and
  legacy spawners' included), then `in progress` removed and the stop
  notice posted.
- **Done cleanup** (`vikunja-done-cleanup`): a done task loses `in progress`
  and `action-needed`, each only while it is still done.
- **Chains** (`vikunja-chain`): `subtask_chain.py`'s rules, from the
  parent's fresh state, its fresh subtasks and comments; one subtask at a
  time, its chain label (nested), agent and auto-merge label set before
  the move to Doing that starts it. Changes of a subtask requeue its
  parents (at most two levels: a relation cycle cannot loop).
- **Failures** (`vikunja-failure`): a Gordon run whose Task Failed gets the
  failed notice (the Task's status message; the worker reads no pod log),
  `agent-error` and `action-needed`; an answered discussion gets
  `action-needed`, unless the owner asked again since.
- Every notice is posted once: before posting, Gordon looks for its action
  marker, or the legacy marker of the same notice, on a comment by its
  own user; a lost answer is recovered the same way, never posted blind.

### Ownership switches

One rule per (project, family); `go run ./cmd/gordon-inventory` prints
the full table, with a `Legacy` column naming the files acting for each
family today; below is a summary without that column. When a rule makes Gordon
the owner (Gordon 07), the same Hollingsworth change turns off, for the
rule's projects, what the `Off when Gordon owns it` column names; the
rollout table refuses `vikunja-drag-cancel` where Gordon does not own
both agent families.

| Family | Off when Gordon owns it | Gordon needs |
|---|---|---|
| `vikunja-auto-assign` | no writer; retain the `task.created` webhook relay | the write token and each configured user must have project access |
| `vikunja-implement`, `vikunja-discuss` | the project's implementation (discuss) TaskSpawners | dispatch config and Task `create`; the write token; Tasks without the `wait-for-blockers`, `announce-start` and `sillage-marker` init containers; the reconciliation |
| `vikunja-followup`, `-answer`, `-discuss` | the project's `vikunja-fu-*` comment TaskSpawners, the enricher's 👀 reaction, unanswered notice and (discuss) fallback for its requests, the `vikunja-followup-reconciler` | as above |
| `vikunja-drag-cancel` | the enricher's drag-cancel hook (`BACKLOG_BUCKET`) | the write token; `list` and `delete` on `tasks` |
| `vikunja-done-cleanup` | the enricher's done-unlabel and action-needed hooks, the failure reporter's done sweeps | the write token; the reconciliation |
| `vikunja-chain` | the enricher's chain hook (`CHAIN_LABEL`) and the `subtask-chain-reconcile` CronJob | the write token; the reconciliation |
| `vikunja-failure` | the failure reporter for Tasks labelled `gordon.spe.quebec/managed` (it keeps every other Task) | the write token |
| `vikunja-cost` | nothing: no legacy component posts cost notices | the write token; `GORDON_SILLAGE_API_URL` and egress to Sillage |

### Rollback

Rollback is one change: the scope's rule back to `shadow` (or removed)
and its legacy switches back on. Decisions Gordon recorded while it owned
the scope keep their queued actions and the outbox drains them, so no
request is lost; each submission looks up its Task name before creating
(a legacy spawner's Task of that name takes the run, as the legacy name
deduplication refuses Gordon's), and each notice is found by its marker
before it is posted, so no Task and no comment is made twice. New
deliveries are decided by the new owner from then on. Drain first when
possible: `SELECT family, state, count(*) FROM actions WHERE state NOT IN
('done', 'failed') GROUP BY 1, 2` empty for the scope's families; `failed`
actions are dead-lettered (never retried), so review them separately.

## GitHub lifecycle (Gordon 08, 08b)

Gordon decides and executes the GitHub lifecycle the reactor, the PR
follow-up, conflict and reviewer init containers, the conflict
reconcilers, the limit reaper and the pollers performed, as durable
actions on the GitHub families, PR readiness (Gordon 08c, below) included.
Its writes act as the GitHub App
(`GORDON_GITHUB_APP_DIR`); readiness reads check runs, commit dates and
review threads as the App too, and every other read keeps the read-only
token. The
routing configuration's `policy` carries the round caps
(`max_rounds_per_pr`, `suggestions_max_rounds`; `0` for none,
`values-hollingsworth.yaml`'s `50` and `4` by default).

- **PR follow-ups** (`pr-followup`): an owner request gets 👀 (GraphQL
  `addReaction`), any request the draft label then, once labelled, the
  draft conversion (a draft is left alone, label included; a repository
  without the label gets neither), and once the run is submitted the start
  notice on the pull request. A review bot's request is stopped before it
  is submitted once the pull request took `max_rounds_per_pr` bot rounds
  (bot reviews named by a start notice from a pull-request author, or by a
  run of the ledger), or, for a suggestions-only review, once
  `suggestions_max_rounds` other heads had suggestions rounds; the cap
  notice is posted once per pull request and the suggestions cap removes
  the fix-suggestions label. A capped request is never drafted. The owner's
  requests are never capped.
- **Reviews** (`pr-review`): once a review run is submitted, Gordon's older
  runs of the same pull request and reviewer are cancelled (a queued
  submission is never created) and the unfinished review Tasks of that
  pull request and reviewer created before it are deleted, a legacy
  spawner's included; a limit restart supersedes nothing. A skipped PR's
  required checks are posted completed `skipped`, once per head.
- **Conflicts** (`pr-conflict`, Gordon 08b): the conflict reconciliation
  queues each confirmed conflict as a synthetic `pull_request` delivery
  (action `gordon-conflict`, which only a synthetic delivery is routed
  with); the router re-reads the pull request and submits
  `pr-conflict-<route hash>-<PR>-<base8>-<head8>` while it is still the
  same open agent PR in conflict on the same base and head (a moved
  episode is stale, mergeability still computing a retry), on the head
  branch, in the PR's `kelos:` variant (none: ignored; two: a conflict),
  with the follow-up execution profile, and announces it on the pull
  request once submitted. An episode whose run finished is run again only
  once its Task is gone (its TTL), as the legacy Task name allowed.
- **Failed PR runs**: a Failed `pr-followup` or `pr-conflict` run of a
  family Gordon owns gets a failure notice on its pull request (its
  Task's status message and the run link only on a private repository); legacy
  reported neither. A Task the subscription limit stopped is never
  reported failed, on a pull request or a Vikunja task: the limit restarts
  take it.
- **Limit restarts** (`limit-restart`, Gordon 08b): each reconcile, outside
  shadow, Gordon's failed runs of the projects it owns the family for are
  decided once: a Task the subscription limit stopped (the agent image's
  `limit-resets-at`/`limit-type` outputs, or the agent's "hit your ...
  limit" answer) is restarted as `<name>-retry<n>` (its spec, labels,
  correlation and Gordon annotations, `kelos.spe.quebec/limit-restarts`
  and `limit-restart-of`) `GORDON_LIMIT_RESTART_GRACE` after the reset, as a
  run of the stopped run's generation (submitted through the outbox, its
  limits and held branch, followed by the observer, stopped by a drag),
  once no Task of its group (the Vikunja task, the reviewer's pull
  request, the pull request's follow-ups) is pending or running; older
  stops of the group are superseded, and after `GORDON_LIMIT_RESTART_MAX`
  restarts the Vikunja task, or the pull request of a PR follow-up or
  conflict (where legacy posted none), gets the gave-up notice. A Vikunja run's
  restart gets the reaper's restart notice and a start notice; a PR
  follow-up's or conflict's a start notice on the pull request.
- **Run completions**: the observer records every run generation it sees
  finish in `run_completions` (family, project, Task uid, phase,
  completion time and the Task's outputs, the agent's `response` left
  out), once: the usage accounting's (Gordon 10) input.
- **Recovery**: with `GORDON_GITHUB_RECONCILE_INTERVAL`, missed owner
  requests, review-bot findings and merge-closes are queued again (see
  the variable). Their request keys are the webhooks', so nothing is
  answered twice.
- Every write first reads what it changes; a notice is found again by its
  action marker, or the legacy marker of the same notice, on a comment by
  a pull-request author (the App's bot or a Workspace token's user), so a
  lost answer is recovered, never written blind.

### PR readiness (Gordon 08c)

`pr-readiness` is `reconcile_pr_ready.py` with `pr_ready_notify.py`,
`auto_merge.py`, `missing_checks.py` and `review_findings.py`: a
model-free family decided from fresh state, its writes through the
outbox. A completed `check_run` (every open candidate on its head; the
deliveries must be mirrored to the receiver) and the readiness backstop
(`GORDON_READINESS_RECONCILE_INTERVAL`) queue a decision; each reads the
pull request (files, mergeability, creation time) and, with review bots,
its reviews and conversation with the read-only token; its head's check
runs, commit date (only when a CHECKS MISSING notice may be due) and, with
review bots, review threads as the App (the read-only token has no Checks
permission); the Kelos Tasks of the
repository's Workspaces and of the task (`list` on `tasks`), the task
with its label creators, comments and, with ntfy, reactions. The
routing configuration's `policy` carries it: `ready_to_merge_label`,
`round_limit_label`, `pr_readiness` (must be `true` once Gordon owns it:
the pr-open notice's promise) and `readiness` (`ci_checks` per
repository, a repository without an entry is refused; `cubic_repos`,
`settle_seconds`, `missing_check_minutes`, `merge_method`, `notify`;
`profile.Defaults()` mirrors `values-hollingsworth.yaml`).

- **Verdict**: an open, non-draft, mergeable head whose check runs are all
  complete and settled, whose CI and applicable reviewer checks
  (`review/<reviewer>` of the reviewers the review family starts on its
  files) passed, with no active Task of its task or reviewing, following
  up or fixing a conflict of the PR (Gordon's queued runs included) and no
  bot review waiting for its follow-up, is ready. Findings a round limit
  left unaddressed (`max_rounds_per_pr`, the suggestions cap notice), a
  failed reviewer check among them, are the ROUND LIMIT REACHED verdict
  instead: never notified nor merged. A legacy pr-open Task of the PR, or
  a legacy readiness waiter of the head, still active defers the
  decision (`announcement-tasks-active`).
- **Writes**, each found again before it is made: the PR CREATED notice
  if missing (first), READY TO MERGE / ROUND LIMIT REACHED once per head
  (the deduplication key, before the labels), CHECKS MISSING once per head
  after `missing_check_minutes`, the verdict labels (the other verdict's
  off, `action-needed` then the verdict's own on; a not-ready head takes a
  verdict label off with `action-needed`), the ntfy send of a ready head
  and, on a task carrying the `auto_merge_label` its
  `auto_merge_label_creator` created, the merge (`PUT .../merge` with the
  head SHA pinned and `merge_method`, its own Contents and Pull requests
  write token). An announced head is notified and merged only once the
  full gate passes again. The notices are byte-equal to legacy and a
  legacy one of the same head counts, on a comment by Gordon's Vikunja
  user only.
- **ntfy**: under the task's lock, a `⏳<sha8>-<n>` reaction before the
  send, `🔔<sha8>` after it, `🔁<sha8>-<n>` on a failed send (retried);
  each outcome write is tried three times. One send whose outcome was
  never recorded stops every later send of the head (legacy allowed
  three), since it may have been delivered.
  Redirects are never followed (the token stays on the configured host).
- **Merge**: a merged, closed or pushed-since PR is left (legacy's pinned
  merge would have been refused and reported for a pushed head); a
  merge GitHub declines (403, 405, 409, 422) gets AUTO-MERGE REFUSED on
  the task once per head, quoting GitHub's answer, and the next scan
  tries again; a rate limit or 5xx is retried; an ambiguous merge is
  recovered by reading the PR.

## Completion cost notices (Gordon 12)

Each finished run of a Vikunja task (implementation, discuss and
follow-up runs, failed and limit-stopped ones included, each generation
once) gets a cost notice on its task where Gordon owns `vikunja-cost`:
the run's estimated API-equivalent cost, the task's total to date (with
how many runs it covers), the run's categories, the run's and task's
coverage (`complete`, `partial` shown AT LEAST, `unknown` shown UNKNOWN,
never $0, with Sillage's notes), the price table's version and date, the
assumption codes, and the Lucarne run and Sillage `#cost` links, with a
`<!-- kelos:cost task=<name> -->` marker. A review's, PR fix's or
conflict fix's cost is in the task total. There is no legacy component
to turn off.

- **When**: once Sillage reports the run (by its Task uid) `final`;
  without that by `GORDON_COST_DEADLINE`, posted marked not final.
  Sillage unanswered (transport error, 5xx, an answer that does not match
  the contract) is a retry with the worker's backoff, never a zero
  estimate (`gordon_cost_notice_checks_total{result="unavailable"}` or
  `"incompatible"`); a task Sillage has no story of yet (404) waits.
  Retries stop once the notice's live window (`GORDON_COST_LIVE_FOR`,
  from the last revision, or from the run's finish before the first)
  has passed: it settles as it is, a waiting one never posted
  (`result="expired"`).
- **Once**: the row (`cost_notices`, one per run generation) and each
  revision's text are stored in the transaction that queues its
  `publish-cost` action. The action writes the latest stored revision,
  under the task's lock, as an edit of the comment that carries the
  generation's `<!-- gordon:cost run=<name> gen=<inbox id> rev=<n> -->`
  marker (on a comment by Gordon's own user) or as a new comment. An
  ambiguous post or edit is recovered by finding that marker at the
  revision or a later one.
- **Updates**: the task's newest notice stays live: when its figures
  change (late usage, a review or fix, a price-table change) it is edited
  in place, never posted again. Runs are ordered by their finish:
  posting a newer run's notice freezes the older ones, and an older
  run's notice posted after a newer one's (its estimate turned final
  later) is posted frozen. Publications of one task's notices take the
  task's rows in turn, so concurrent passes leave one live notice. A
  notice unchanged for `GORDON_COST_LIVE_FOR` settles.
- **Rollback**: remove the rule. No new notice is recorded, and the
  project's waiting and live notices read and write nothing more
  (`result="not-owned"`); revisions already queued drain, each found by
  its marker first.

Hollingsworth (Gordon 13 deploys it):

- Pin the Gordon image built from this change and Sillage at
  sperano/sillage#57's merge or later, with its usage feed on
  (`SILLAGE_LOKI_URL`, `SILLAGE_USAGE_SINCE`; see that change's
  requirements): without it every estimate is `unknown`.
- `helm/gordon` worker env: `GORDON_SILLAGE_API_URL:
  http://sillage.sillage.svc`; `GORDON_SILLAGE_URL` stays the public
  `https://sillage.spe.quebec` for the links.
- `helm/gordon/templates/networkpolicy-egress.yaml`: the worker's egress
  to the `sillage` namespace's Sillage pods on their container port
  (`8080`; kube-router matches after the Service's DNAT).
- `sillage`'s ingress: the in-cluster port carries no credential (the
  public host's forward-auth is not on this path) and serves per-task
  cost and run data, while the `sillage` namespace has no ingress policy
  today. Add a NetworkPolicy there that admits port `8080` only from the
  Gordon worker's pods and the ingress controller, so agent pods in
  `kelos-agents` cannot read it.
- The ownership file: a `vikunja-cost` rule per project (or `*`) with
  its gate evidence; the migration (`0005_cost_notices.sql`) and its
  grants run with `gordon migrate` before the worker starts.

### GitHub ownership switches

`go run ./cmd/gordon-inventory` prints the full table (its second
switches table). When a rule makes Gordon the owner (Gordon 09), the same
Hollingsworth change turns off, for the rule's project (its repository's
PRs), what `Off when Gordon owns it` names. Rollback is the Vikunja
families' (below): the rule back to `shadow` and the switches back on;
queued actions drain, each Task-name lookup and notice marker preventing
a duplicate.

| Family | Off when Gordon owns it | Gordon needs |
|---|---|---|
| `pr-followup` | the project's `pr-followup-*` and review-poll spawners, the GitHub reactor's 👀, draft label and conversion for its repository, the review poller's repository entry | dispatch config and Task `create`; the App; Tasks without the `pr-task-dedup`, `review-round-cap` and `announce-github-start` init containers; the GitHub reconciliation |
| `pr-review` | the project's `pr-review-*` spawners and the reactor's skipped checks for its repository | dispatch config and Task `create`; the App; `list` and `delete` on `tasks`; Tasks without the `pr-task-dedup` and `pr-review-supersede` init containers |
| `pr-open` | the repository's pr-open merge spawner | the Vikunja write token |
| `merge-close` | the repository's merge-close spawner and its `merge-close-reconcile` entry | the Vikunja write token; the GitHub reconciliation |
| `pr-conflict` | the Workspace's `pr-conflict-*` spawners and its `conflict-reconcile-event-*` and `conflict-reconcile-cron-*` reconcilers | dispatch config and Task `create`; the App; Tasks without the `announce-github-start` init container; the conflict reconciliation |
| `limit-restart` | the limit reaper for Gordon's Tasks (`gordon.spe.quebec/managed`) of the rule's projects; it keeps every other Task | dispatch config, Task `create` and `list`; the Vikunja write token |
| `pr-readiness` | the repository's `pr-readiness-event-*` and `pr-readiness-cron-*` spawners | the App; Task `list`; the Vikunja write token; the readiness backstop; `pr_readiness` and `readiness.ci_checks` in the policy; `check_run` deliveries mirrored; with `readiness.notify`, the ntfy publisher |

## Probes and metrics

- Liveness `GET /healthz` (200 while the process serves).
- Readiness `GET /readyz`: 503 while draining (after SIGTERM), when the
  database does not answer, or when the schema is behind the binary.
- `GET /metrics` (Prometheus; all labels from fixed sets):
  `gordon_intake_requests_total{source,result,reason}`,
  `gordon_decisions_total{source,family,outcome,executed}`,
  `gordon_decision_latency_seconds{source}`,
  `gordon_inbox_retries_total{source,reason}`, `gordon_inbox_failures_total{source}`,
  `gordon_deferred_total{queue,limit}`,
  `gordon_queue_depth{queue,state}`, `gordon_queue_oldest_age_seconds{queue,state}`,
  `gordon_action_attempts_total{kind,result}`, `gordon_action_duration_seconds{kind}`,
  `gordon_action_latency_seconds{kind}`,
  `gordon_upstream_requests_total{provider,result}`, `gordon_upstream_request_duration_seconds{provider}`,
  `gordon_kelos_requests_total{op,result}`, `gordon_kelos_request_duration_seconds{op}`,
  `gordon_kelos_submissions_total{result}` (`created`, `recovered`, `collision`, `deferred-limit`,
  `deferred-branch`, `deferred-admission`, `refused`, `ambiguous`, `error`), `gordon_run_observations_total{state}`,
  `gordon_shadow_comparisons_total{family,result}` (`match`, `differ`, `missing`),
  `gordon_reconcile_runs_total{result}`, `gordon_reconcile_duration_seconds`,
  `gordon_reconcile_last_success_timestamp_seconds`, `gordon_leases_expired_total{queue}`,
  `gordon_bodies_pruned_total`,
  `gordon_vikunja_reconcile_runs_total{result}`, `gordon_vikunja_reconcile_queued_total`,
  `gordon_github_reconcile_runs_total{result}`, `gordon_github_reconcile_queued_total`,
  `gordon_conflict_reconcile_runs_total{result}`, `gordon_conflict_reconcile_queued_total`,
  `gordon_readiness_reconcile_runs_total{result}`, `gordon_readiness_reconcile_queued_total`,
  `gordon_limit_restart_decisions_total{result}` (`restarted`, `superseded`, `gave-up`, `name-too-long`, `none`),
  `gordon_cost_notice_checks_total{result}` (`published`, `updated`, `unchanged`, `waiting`, `settled`,
  `unavailable`, `incompatible`, `raced`, `not-owned`, `expired`),
  plus the Go and process collectors. `gordon_action_attempts_total`'s
  `kind` includes `assign`, `sync-label`, `cancel-run`, `supersede-reviews`, `notify`, `merge-pull-request` and `publish-cost`.

Suggested alerts: `gordon_queue_oldest_age_seconds{state="pending"}` above
the retry cap, any `gordon_queue_depth{state="failed"}` or
`{queue="actions",state="ambiguous"}`, reconciliation not successful for
several intervals (`gordon_vikunja_reconcile_runs_total{result="error"}`,
`gordon_github_reconcile_runs_total{result="error"}`,
`gordon_conflict_reconcile_runs_total{result="error"}` and
`gordon_readiness_reconcile_runs_total{result="error"}` too, once on), `gordon_limit_restart_decisions_total{result="gave-up"}`,
`gordon_cost_notice_checks_total{result=~"unavailable|incompatible"}` rising for an hour, `gordon_intake_requests_total{result="unavailable"}`,
`gordon_kelos_submissions_total{result="refused"}` (the Kelos API refused
a submission's request: the lookup or the admission list before the
create, through RBAC or authentication, or the create, admission included), and in shadow `gordon_shadow_comparisons_total{result!="match"}`.

## Logs

One JSON object per line on stdout. Correlation keys: `inbox_id`,
`delivery_key`, `request_key`, `source`, `kind`, `action_id`, `action_key`,
`task` (a Kelos Task name);
a decision's run names are in its family's `runs` field. Bodies, comments and prompts are never logged (a built Task, prompt included, lives in `runs.task` or `shadow_tasks.task` until `GORDON_BODY_RETENTION` after the run finished or the comparison); credential-shaped
values in any field are masked and long values truncated.

## Answers to senders

`202` stored, `200` duplicate (already stored), `202` ignored (an event
type nobody routes, not stored), `400` malformed or cut short, `401`
unsigned, `403` bad signature, `404` unknown source, `405` not POST, `408`
body too slow, `411` no Content-Length, `413` over the source's cap
(GitHub 25 MiB, others 10 MiB), `431` head over 64 KiB, `503` busy or the
database unavailable (the sender retries; nothing was acknowledged).
