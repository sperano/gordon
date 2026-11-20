package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Roles are the least-privilege database roles the receiver and the
// worker log in as; the migrator (the schema owner) grants them. An empty
// name grants nothing.
type Roles struct {
	Receiver string
	Worker   string
}

// workerTables are the tables the worker reads and inserts into; it
// never deletes, and it never changes the schema. The inbox is the
// receiver's for webhooks: the worker inserts only its own synthetic
// deliveries (workerInboxInsert: requeues and reconciliation, never a
// body digest), and it updates only the queue bookkeeping and the body
// (pruning).
var workerTables = []string{"decisions", "requests", "runs", "actions", "shadow_tasks"}

// workerUpdates are the columns the worker may update in each of
// workerTables; a table it never updates (decisions) has no entry. A
// run's name and creation time, and an action's identity (key, kind,
// target, value, its delivery), are fixed once inserted.
var workerUpdates = map[string]string{
	"requests": "outcome, executed",
	"runs": "family, project, request_key, inbox_id, identity, profile, config_version, state, " +
		"task_exists, generation, updated_at, task, task_uid, phase, observed_at",
	"actions": "state, attempts, next_attempt_at, lease_token, lease_owner, lease_until, last_error, finished_at",
	// A shadow Task's identity is fixed; its comparison is not, and its
	// built Task is only ever pruned (set NULL), which a column grant
	// cannot narrow further.
	"shadow_tasks": "task, result, diff, compared_at",
}

// gordonTables are every table the schema has: a role's grants on them
// are revoked before its mode's are applied.
var gordonTables = append([]string{"schema_migrations", "inbox"}, workerTables...)

// workerInboxInsert are the inbox columns a synthetic delivery sets.
const workerInboxInsert = "source, delivery_key, request_key, kind, scope, headers, body"

// workerInboxColumns are the inbox columns the worker may update.
const workerInboxColumns = "state, attempts, next_attempt_at, lease_token, lease_owner, lease_until, " +
	"last_error, finished_at, body"

// ErrSameRole refuses one login for both roles: it would hold the union
// of their rights, so the receiver could read bodies and the outbox.
var ErrSameRole = errors.New("receiver and worker roles must differ")

// receiverColumns are the inbox columns the receiver writes: the verified
// delivery, never its queue bookkeeping (state, attempts, lease).
const receiverColumns = "source, delivery_key, request_key, kind, scope, headers, body, body_digest"

// Grant gives each role exactly what its mode needs. The receiver may
// insert a delivery's own columns, read the new row's id (RETURNING) and
// the delivery keys, and read the schema version (readiness), nothing
// else: it cannot read a stored body, a decision or the outbox. GRANT is
// idempotent, so every migrate run re-applies it; a new table or a new
// receiver column is granted only once it is added to workerTables or
// receiverColumns. Each role first loses every right on the schema's
// tables (column rights included), so a role reused for the other mode
// keeps nothing from the one it had.
func (s *Store) Grant(ctx context.Context, r Roles) error {
	if r.Receiver != "" && r.Receiver == r.Worker {
		return ErrSameRole
	}
	var schema string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return err
	}
	usage := "GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO "
	revoke := "REVOKE ALL ON " + strings.Join(gordonTables, ", ") + " FROM "
	var stmts []string
	if r.Receiver != "" {
		role := pgx.Identifier{r.Receiver}.Sanitize()
		stmts = append(stmts,
			revoke+role,
			usage+role,
			"GRANT SELECT ON schema_migrations TO "+role,
			"GRANT INSERT ("+receiverColumns+") ON inbox TO "+role,
			"GRANT SELECT (id, delivery_key) ON inbox TO "+role,
		)
	}
	if r.Worker != "" {
		role := pgx.Identifier{r.Worker}.Sanitize()
		stmts = append(stmts, revoke+role, usage+role,
			"GRANT SELECT ON schema_migrations TO "+role,
			"GRANT SELECT, UPDATE ("+workerInboxColumns+") ON inbox TO "+role,
			"GRANT INSERT ("+workerInboxInsert+") ON inbox TO "+role)
		for _, t := range workerTables {
			stmts = append(stmts, fmt.Sprintf("GRANT SELECT, INSERT ON %s TO %s", t, role))
			if cols, ok := workerUpdates[t]; ok {
				stmts = append(stmts, fmt.Sprintf("GRANT UPDATE (%s) ON %s TO %s", cols, t, role))
			}
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
}
