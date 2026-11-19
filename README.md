# gordon

<img src="assets/gordon-8-bit.png" alt="Gordon Cole, 8-bit" width="160" align="right">

Kelos tasks orchestrator: one Go application that will verify GitHub and
Vikunja webhooks, decide deterministically whether and how to run, and
submit ordinary Kelos Tasks, replacing Hollingsworth's webhook glue and its
project × model × mode TaskSpawner fan-out. Kelos stays the execution
engine; Sillage keeps usage history and Lucarne run inspection.

This first stage holds the routing contracts only. Nothing here serves
traffic, and no production routing changes: every family is owned by the
legacy Hollingsworth glue until a rollout rule with its gate evidence says
otherwise.

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

## Checks

```sh
go build ./... && go vet ./... && go test ./...
```

## Mascot

`assets/gordon-8-bit.png` is Hollingsworth's existing Gordon Cole mascot,
copied unchanged from `sperano/hollingsworth`
`assets/avatars/gordon-8-bit.png` at commit `4a50f33` (sha256
`033d2e67…4cb093`, pinned by `assets` tests). It is the `gordon` service
identity's avatar there; Hollingsworth's `assets/avatars/README.md` records
it as generated with the built-in imagegen tool from the approved concept
sheet, and declares no separate licence for it.
