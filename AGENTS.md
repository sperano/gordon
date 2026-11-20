# AGENTS.md

## Repository purpose

Gordon is a Go service that replaces Hollingsworth's webhook glue with one
durable, deterministic orchestrator for Kelos tasks. Read `README.md` for the
package contracts and `DEPLOYMENT.md` before changing runtime configuration,
permissions, rollout ownership, database roles, or deployment behaviour.

## Development workflow

- Use the Go version declared in `go.mod`.
- Format changed Go files with `gofmt`.
- Keep tests beside their package in sibling `_test.go` files.
- Add regression coverage for every changed branch, error path, default, and
  externally visible message. Prefer exact assertions and deterministic fakes;
  do not use real network services, clocks, sleeps, or unordered output.
- Run the full non-database checks before committing:

  ```sh
  go build ./...
  go vet ./...
  go test ./...
  ```

- Changes to `internal/store`, `internal/worker`, migrations, grants, or service
  integration also require `scripts/test-postgres.sh`. It starts a disposable
  PostgreSQL instance and accepts additional `go test` arguments.

## Design constraints

- Preserve deterministic delivery keys, request keys, task names, branches,
  routing decisions, action keys, and rendered task specifications. These are
  compatibility contracts with legacy Hollingsworth tasks and pod hooks.
- Route from fresh upstream state. Treat webhook payloads as notification data,
  not authoritative state.
- Put every external write through the durable action outbox. Give each action
  a stable idempotency key and implement recovery for ambiguous writes before
  retrying them.
- Keep lease-protected writes fenced by the lease token. A worker that loses a
  lease must not commit a decision or side effect.
- Keep routing and execution profiles in trusted configuration. Never derive
  credentials, ServiceAccounts, secret access, permission modes, or pod pieces
  from an untrusted webhook payload.
- Preserve shadow mode: it records decisions and comparisons but performs no
  external writes. A routing family has exactly one active owner per scope;
  ownership changes must retain a safe rollback to legacy handling.
- Keep provider and project concurrency bounded. Deferrals caused by limits or
  held branches are not failed attempts.
- Do not log webhook bodies, prompts, credentials, tokens, secret contents, or
  database URLs. Keep metric labels fixed-cardinality.

## Database changes

- Add a new numbered migration in `internal/store/migrations`; never rewrite an
  applied migration. Migrations must be additive and safe with old application
  pods during a rolling deployment.
- Update `internal/store/grants.go` whenever a migration adds a table or a
  column a runtime role needs. Grant only the exact operations and columns used
  by the receiver or worker.
- Cover migrations, grants, retries, lease expiry, and idempotency with the
  PostgreSQL-backed suite.

## Compatibility fixtures

- Sanitized deliveries and exact routing outcomes belong in
  `internal/replay/testdata`.
- Dispatch changes must keep Gordon's rendered tasks equal to the legacy
  fixtures in `internal/dispatch/testdata` unless the compatibility contract is
  intentionally changed in both systems.
- Notice text and markers are compatibility APIs. Preserve byte-exact legacy
  text where tests require it, and only recognize Gordon notices authored by
  the configured Gordon Vikunja user.
- When adding or moving a dispatch or lifecycle family, update
  `internal/inventory` and verify `go run ./cmd/gordon-inventory` reflects its
  owner, stages, legacy implementation, and rollback switch.

## Scope boundaries

- This repository builds the Gordon image; deployment manifests and production
  rollout switches live in Hollingsworth.
- Do not contact live Vikunja, GitHub, Kubernetes, or PostgreSQL services from
  tests. Use package fakes, `internal/kelos/kelostest`, replay fixtures, and the
  disposable PostgreSQL harness.
- Do not deploy, change ownership gates, or disable legacy handlers as part of
  an ordinary Gordon code change.
