# Deployment contract

What a deployment of Gordon (Hollingsworth `helm/gordon`, Gordon 05) must
provide. One image (`Dockerfile`), one binary, three modes chosen by the
first argument. Nothing here is deployed by this repository.

## Modes and identities

| Mode | Runs as | Database role | Secrets | Kubernetes rights |
|---|---|---|---|---|
| `gordon migrate` | Job or init container, before each rollout | schema owner (DDL) | owner URL | none |
| `gordon receiver` | Deployment behind the webhook ingress | receiver: `INSERT` on `inbox`, `SELECT (id, delivery_key)` on `inbox`, `SELECT` on `schema_migrations` | receiver URL, webhook keys | none (no ServiceAccount token: `automountServiceAccountToken: false`) |
| `gordon worker` | Deployment, no ingress | worker: `SELECT, INSERT, UPDATE` on `decisions`, `requests`, `runs` and `actions`; on `inbox` `SELECT` and `UPDATE` of its queue columns and `body` only (no `INSERT`: deliveries come from the receiver); no `DELETE`, no DDL; a role distinct from the receiver's (`migrate` refuses one role for both) | worker URL, Vikunja token, read-only GitHub token | none in this stage; Gordon 04 adds `create/get/list/delete` on `kelos.dev` Tasks in `kelos-agents` |

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
| `GORDON_ADMIN_ADDR` | `:9090` | `/healthz`, `/readyz`, `/metrics` (not exposed by the ingress) |
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
| `GORDON_MAX_ATTEMPTS` | `20` | inbox retries and attempted actions; then the row is dead-lettered (`failed`). A deferred action (no executor yet, or a full limit) counts no attempt and stays queued |
| `GORDON_RETRY_BASE`, `GORDON_RETRY_MAX` | `5s`, `10m` | exponential backoff |
| `GORDON_DEFER_DELAY` | `2s` | wait of a row deferred by a full limit |
| `GORDON_RECONCILE_INTERVAL` | `30s` | lease expiry, queue gauges, body pruning |
| `GORDON_BODY_RETENTION` | `168h` | stored bodies of finished rows are dropped after this; keys, decisions, requests and runs stay |
| `GORDON_SHUTDOWN_GRACE` | `25s` | in-flight items on SIGTERM; past it their leases expire and another worker recovers them |

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
  `gordon_reconcile_runs_total{result}`, `gordon_reconcile_duration_seconds`,
  `gordon_reconcile_last_success_timestamp_seconds`, `gordon_leases_expired_total{queue}`,
  `gordon_bodies_pruned_total`, plus the Go and process collectors.

Suggested alerts: `gordon_queue_oldest_age_seconds{state="pending"}` above
the retry cap, any `gordon_queue_depth{state="failed"}` or
`{queue="actions",state="ambiguous"}`, reconciliation not successful for
several intervals, `gordon_intake_requests_total{result="unavailable"}`.

## Logs

One JSON object per line on stdout. Correlation keys: `inbox_id`,
`delivery_key`, `request_key`, `source`, `kind`, `action_id`, `action_key`;
a decision's run names are in its family's `runs` field. Bodies, comments and prompts are never logged; credential-shaped
values in any field are masked and long values truncated.

## Answers to senders

`202` stored, `200` duplicate (already stored), `202` ignored (an event
type nobody routes, not stored), `400` malformed or cut short, `401`
unsigned, `403` bad signature, `404` unknown source, `405` not POST, `408`
body too slow, `411` no Content-Length, `413` over the source's cap
(GitHub 25 MiB, others 10 MiB), `431` head over 64 KiB, `503` busy or the
database unavailable (the sender retries; nothing was acknowledged).
