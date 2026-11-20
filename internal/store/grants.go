package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Roles are the least-privilege database roles the receiver and the
// worker log in as; the migrator (the schema owner) grants them. An empty
// name grants nothing.
type Roles struct {
	Receiver string
	Worker   string
}

// workerTables are the tables the worker reads and writes; it never
// deletes, and it never changes the schema.
var workerTables = []string{"inbox", "decisions", "requests", "runs", "actions"}

// Grant gives each role exactly what its mode needs. The receiver may
// insert deliveries, read the new row's id (RETURNING) and the delivery
// keys (the ON CONFLICT arbiter), and read the
// schema version (readiness), nothing else: it cannot read a stored body,
// a decision or the outbox. GRANT is idempotent, so every migrate run
// re-applies it, new tables included.
func (s *Store) Grant(ctx context.Context, r Roles) error {
	var schema string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return err
	}
	usage := "GRANT USAGE ON SCHEMA " + pgx.Identifier{schema}.Sanitize() + " TO "
	var stmts []string
	if r.Receiver != "" {
		role := pgx.Identifier{r.Receiver}.Sanitize()
		stmts = append(stmts,
			usage+role,
			"GRANT SELECT ON schema_migrations TO "+role,
			"GRANT INSERT ON inbox TO "+role,
			"GRANT SELECT (id, delivery_key) ON inbox TO "+role,
		)
	}
	if r.Worker != "" {
		role := pgx.Identifier{r.Worker}.Sanitize()
		stmts = append(stmts, usage+role,
			"GRANT SELECT ON schema_migrations TO "+role)
		for _, t := range workerTables {
			stmts = append(stmts, fmt.Sprintf("GRANT SELECT, INSERT, UPDATE ON %s TO %s", t, role))
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
