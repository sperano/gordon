package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey is the advisory lock every migrator takes, so two pods
// starting at once apply each migration exactly once.
const migrationLockKey int64 = 0x676f72646f6e // "gordon"

// migration is one numbered schema change, applied in its own
// transaction with its version row.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations returns the embedded migrations in order; their numbers
// must run 1, 2, 3, ... without a gap.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("migration %s: name is not NNNN_<name>.sql", e.Name())
		}
		body, err := migrationFiles.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: expected version %d", m.name, i+1)
		}
	}
	return out, nil
}

// LatestVersion is the schema version this binary's migrations reach.
func LatestVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		panic(err) // the embedded set is fixed at build time and tested
	}
	return len(ms)
}

const createVersions = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    integer     PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// Migrate applies the migrations the database lacks and returns how many
// it applied. It holds an advisory lock for the whole run, so concurrent
// migrators serialize; each migration commits with its version row, so a
// migrator killed midway leaves the schema at the last whole version. A
// schema ahead of this binary (a newer release migrated it) is left alone.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return 0, fmt.Errorf("migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey) //nolint:errcheck // released with the session anyway
	if _, err := conn.Exec(ctx, createVersions); err != nil {
		return 0, err
	}
	current, err := version(ctx, conn.Conn())
	if err != nil {
		return 0, err
	}
	applied := 0
	for _, m := range ms[min(current, len(ms)):] {
		if err := apply(ctx, conn.Conn(), m); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

func apply(ctx context.Context, conn *pgx.Conn, m migration) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.version)
		return err
	})
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func version(ctx context.Context, q querier) (int, error) {
	var v int
	err := q.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&v)
	return v, err
}

// SchemaVersion is the database's applied schema version; an error when
// it was never migrated.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	return version(ctx, s.pool)
}
