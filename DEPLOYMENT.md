# Deployment contract

What a deployment of Gordon (Hollingsworth `helm/gordon`, Gordon 05) must
provide. One image (`Dockerfile`), one binary, three modes chosen by the
first argument. Nothing here is deployed by this repository.

## Modes and identities

| Mode | Runs as | Database role | Secrets | Kubernetes rights |
|---|---|---|---|---|
| `gordon migrate` | Job or init container, before each rollout | schema owner (DDL) | owner URL | none |
| `gordon receiver` | Deployment behind the webhook ingress | receiver: `INSERT` on `inbox`, `SELECT (id, delivery_key)` on `inbox`, `SELECT` on `schema_migrations` | receiver URL, webhook keys | none (no ServiceAccount token: `automountServiceAccountToken: false`) |
| `gordon worker` | Deployment, no ingress | worker: `SELECT, INSERT` on `decisions`, `requests`, `runs`, `actions` and `shadow_tasks`, and `UPDATE` of their mutable columns only (a request's outcome, a run's state, rerun fields and built Task, an action's queue columns, a shadow Task's comparison; nothing on `decisions`); on `inbox` `SELECT` and `UPDATE` of its queue columns and `body` only (no `INSERT`: deliveries come from the receiver); no `DELETE`, no DDL; a role distinct from the receiver's (`migrate` refuses one role for both) | worker URL, Vikunja token, read-only GitHub token | with `GORDON_DISPATCH_CONFIG_FILE`: `get`, `list` and (outside shadow) `create` on `kelos.dev` `tasks` in `kelos-agents`, nothing else (no `delete`, no Secrets) |

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
| `GORDON_VIKUNJA_TOKEN_FILE` | required | read token for `GET /tasks/{id}` |
| `GORDON_GITHUB_API_URL` | `https://api.github.com` | |
| `GORDON_GITHUB_TOKEN_FILE` | required | read-only token for pull requests and their files |
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
| `GORDON_DEFER_DELAY` | `2s` | wait of a row deferred by a full provider or project limit; a full Kelos variant limit or a held branch waits `GORDON_KELOS_BUSY_DELAY` |
| `GORDON_RECONCILE_INTERVAL` | `30s` | lease expiry, queue gauges, body pruning |
| `GORDON_BODY_RETENTION` | `168h` | stored bodies of finished rows, and the built Tasks (prompts) of finished runs and compared shadow Tasks, are dropped after this; keys, decisions, requests and runs stay |
| `GORDON_SHUTDOWN_GRACE` | `25s` | in-flight items on SIGTERM; past it their leases expire and another worker recovers them |
| `GORDON_DISPATCH_CONFIG_FILE` | empty | JSON `dispatch.Config` (unknown fields refused; below). Without it runs carry no Task, nothing touches Kelos, and `GORDON_SHADOW=false` is refused |
| `GORDON_KELOS_API_URL` | `https://kubernetes.default.svc` | Kubernetes API base |
| `GORDON_KELOS_TOKEN_FILE` | the pod's ServiceAccount token | re-read per request (projected tokens rotate) |
| `GORDON_KELOS_CA_FILE` | the pod's ServiceAccount `ca.crt` | `""` keeps the system roots |
| `GORDON_KELOS_BUSY_DELAY` | `30s` | wait of a submission deferred by a full variant limit or a held branch |
| `GORDON_SHADOW_WINDOW` | `15m` | how long a shadow Task waits for the legacy Task of its name before it is recorded `missing` |

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
| `version` | recorded on each Task (`gordon.spe.quebec/config-version`, after the routing version) |
| `namespace` | `kelos-agents` (the only value: every submit action names it) |
| `comments`, `comments_max_bytes` | quote `GET /tasks/{id}/comments` as `Context.comments` (default cap 256 KiB; a failed or larger read is quoted empty, as the legacy `failurePolicy: Ignore`) |
| `common_secrets` | Secrets every profile may reference (the admission policy's `commonSecrets`: runner credentials, the variants' Vikunja and Authentik identities, Gordon's own, the run-link token) |
| `families` | per family (`vikunja-implement`, `vikunja-discuss`, `vikunja-followup`, `vikunja-followup-answer`, `vikunja-followup-discuss`, `pr-followup`, `pr-review`): `prompt`, `labels`, `annotations`, `agent_configs` (`@project` and `@vikunja` expand to the project's and the variant's), `ttl_seconds_after_finished`, `pod_failure_policy`, `pod` |
| `workspaces`, `profiles`, `variants` | pod pieces per Workspace, per execution profile (with the `secrets` it grants and its reserved pod `labels`, which nothing else may set) and per variant (`agent_config_suffix`, `vikunja_agent_config`, `pod`). Every routing variant needs a `variants` entry, `{}` at least: a run of a variant without one is rejected (`task-spec-unknown-variant`) |
| `limits` | `default` and per-`variants` caps on unfinished, non-`Waiting` Tasks per variant (`vikunja.spe.quebec/variant`, legacy Tasks included); `0` is unbounded |

Prompt, label and annotation values, agent config names and pod strings
holding `{{` are Go templates over the legacy Kelos variables (`.task`,
`.comment`, `.Title`, `.Body`, `.description`, `.author`,
`.Context.comments`, `.Number`, `.URL`, `.Branch`, `.Event`, `.Action`,
`.CommentURL`, `.CommentBody`, `.Payload...`) plus `.project`,
`.project_title`, `.workspace`, `.repository`, `.variant`, `.reviewer`,
`.profile`, `.mode`, `.model`, `.effort`, `.branch` (`dispatch.Data`); a
missing variable refuses the run. Gordon alone sets
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
  `deferred-branch`, `refused`, `ambiguous`, `error`), `gordon_run_observations_total{state}`,
  `gordon_shadow_comparisons_total{family,result}` (`match`, `differ`, `missing`),
  `gordon_reconcile_runs_total{result}`, `gordon_reconcile_duration_seconds`,
  `gordon_reconcile_last_success_timestamp_seconds`, `gordon_leases_expired_total{queue}`,
  `gordon_bodies_pruned_total`, plus the Go and process collectors.

Suggested alerts: `gordon_queue_oldest_age_seconds{state="pending"}` above
the retry cap, any `gordon_queue_depth{state="failed"}` or
`{queue="actions",state="ambiguous"}`, reconciliation not successful for
several intervals, `gordon_intake_requests_total{result="unavailable"}`,
`gordon_kelos_submissions_total{result="refused"}` (admission refused a
Task Gordon built), and in shadow `gordon_shadow_comparisons_total{result!="match"}`.

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
