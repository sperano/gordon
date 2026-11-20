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
otherwise. No action has an executor yet (Kelos submission is the next
stage), so even an executed decision's actions stay queued.
[DEPLOYMENT.md](DEPLOYMENT.md) is the deployment contract.

## Contracts

| Package | Contract |
|---|---|
| `internal/event` | Typed Vikunja and GitHub events. A *delivery key* names one transport delivery (GitHub's `X-GitHub-Delivery`; a Vikunja comment's id; a `task.updated`'s data digest, as the legacy `$.data` cache). A *request key* names the owner request behind it, so a reconciler resend is the same request and a new `/gordon` comment is a new one. |
| `internal/auth` | Per-source signature checks in the legacy formats (`X-Vikunja-Signature`, `X-Hub-Signature-256`, `X-Kelos-Signature-256`), body caps and the 401/403/404/413 answers. |
| `internal/run` | Run identities and deterministic Kelos Task names and branches, identical to the legacy spawner `nameTemplate`s that pod hooks still parse. |
| `internal/profile` | Resolved execution profiles: variant (agent, model, image, credentials), effort, permission mode and a named capability profile (ServiceAccount, read-only Workspace, capabilities) from trusted configuration only. Discuss is always read-only. `Defaults()` mirrors `values-hollingsworth.yaml`. |
| `internal/action` | Intended writes with durable keys, so an ambiguous write can be recovered before a retry. |
| `internal/rollout` | One active dispatcher per (project, family): `legacy`, `shadow` (decisions recorded, nothing executed) or `gordon`, with the gate evidence each owner requires. |
| `internal/route` | Decisions: `dispatch`, `act`, `ignore`, `reject`, `duplicate`, `stale`, `conflict` or `retry`, per family, from the delivery, the fresh upstream state and the ledger. |
| `internal/inventory` | Every legacy dispatch family, hook and reconciler with its future owner, decision and stages, and the Hollingsworth files each later stage changes. `go run ./cmd/gordon-inventory` prints it as Markdown. |
| `internal/replay` | Sanitized legacy deliveries in `testdata/` with their exact expected decisions and the Task names the legacy spawners created. |

## Service

| Package | Role |
|---|---|
| `internal/intake` | The receiver: method, declared length against the source cap, bounded slots per client address, body read under the request deadline, signature, delivery keys, then the stored row; 2xx only once stored. |
| `internal/store` | Postgres: the inbox, decisions, handled requests, runs and the action outbox; migrations under an advisory lock; leases fenced by a token, so a worker that lost its lease writes nothing; role grants. |
| `internal/worker` | Inbox processor (fresh state, route, one fenced transaction), outbox (recover an ambiguous write before retrying it), limits, backoff, graceful loops and the reconciler. |
| `internal/upstream` | GET-only Vikunja and GitHub readers, bounded per provider. |
| `internal/metrics`, `internal/logging` | Prometheus metrics with fixed label sets; JSON logs with redaction. |
| `internal/config`, `internal/app`, `cmd/gordon` | Environment configuration, the three modes, health, readiness and metrics. |

## Checks

```sh
go build ./... && go vet ./... && go test ./...
```

The store, worker and service tests need Postgres: they skip without
`GORDON_TEST_DATABASE_URL` (an admin URL that may create databases and
roles). `scripts/test-postgres.sh` starts a throwaway server from
`initdb`/`pg_ctl` on `PATH` (or `PG_BIN`) and runs the checks against it.

## Mascot

`assets/gordon-8-bit.png` is Hollingsworth's existing Gordon Cole mascot,
copied unchanged from `sperano/hollingsworth`
`assets/avatars/gordon-8-bit.png` at commit `4a50f33` (sha256
`033d2e67…4cb093`, pinned by `assets` tests). It is the `gordon` service
identity's avatar there; Hollingsworth's `assets/avatars/README.md` records
it as generated with the built-in imagegen tool from the approved concept
sheet, and declares no separate licence for it.
