// Package storetest gives tests a freshly migrated database of their own
// on the Postgres server named by GORDON_TEST_DATABASE_URL (a role that
// may create databases and roles). Without it the test is skipped with
// that reason: scripts/test-postgres.sh starts a throwaway server and
// runs the whole suite against it.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/store"
)

// EnvURL names the admin connection URL of the test server.
const EnvURL = "GORDON_TEST_DATABASE_URL"

const nameBytes = 6

// AdminURL returns the test server's URL, skipping the test without one.
func AdminURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv(EnvURL)
	if u == "" {
		t.Skip(EnvURL + " is unset: this test needs a Postgres server (run scripts/test-postgres.sh)")
	}
	return u
}

// Database creates an empty database and returns its URL; it is dropped
// when the test ends.
func Database(t testing.TB) string {
	t.Helper()
	admin := AdminURL(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	name := "gordon_test_" + Random(t)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck // best effort
	})
	return WithDatabase(t, admin, name)
}

// Open returns a store on a new migrated database.
func Open(t testing.TB) (*store.Store, string) {
	t.Helper()
	dbURL := Database(t)
	s, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s, dbURL
}

// WithDatabase returns raw with its database (path) replaced.
func WithDatabase(t testing.TB, raw, name string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvURL, err)
	}
	u.Path = "/" + name
	return u.String()
}

// WithUser returns raw logging in as user with password.
func WithUser(t testing.TB, raw, user, password string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// Exec runs SQL on the database at dbURL, for test setup only.
func Exec(t testing.TB, dbURL, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// QueryRow scans one row of SQL on the database at dbURL into dest.
func QueryRow(t testing.TB, dbURL, sql string, args []any, dest ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// Random is a short random lowercase hex string for unique names.
func Random(t testing.TB) string {
	t.Helper()
	b := make([]byte, nameBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
