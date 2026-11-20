package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

const rolePassword = "test-password"

// role creates a login role for the test and drops it afterwards.
func role(t *testing.T, dbURL string) string {
	t.Helper()
	name := "gordon_role_" + storetest.Random(t)
	storetest.Exec(t, dbURL, "CREATE ROLE "+name+" LOGIN PASSWORD '"+rolePassword+"'")
	t.Cleanup(func() {
		admin := storetest.AdminURL(t)
		conn, err := pgx.Connect(ctx, dbURL)
		if err == nil {
			conn.Exec(ctx, "DROP OWNED BY "+name) //nolint:errcheck // best effort
			conn.Close(ctx)
		}
		storetest.Exec(t, admin, "DROP ROLE IF EXISTS "+name)
	})
	return name
}

func denied(err error) bool { return err != nil && strings.Contains(err.Error(), "permission denied") }

func TestGrantReceiverInsertsOnly(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	receiver := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Receiver: receiver}); err != nil {
		t.Fatal(err)
	}
	rs, err := store.Open(ctx, storetest.WithUser(t, dbURL, receiver, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if _, dup, err := rs.Accept(ctx, delivery("k")); err != nil || dup {
		t.Fatalf("receiver Accept = %v, %v; want a new row", dup, err)
	}
	if _, dup, err := rs.Accept(ctx, delivery("k")); err != nil || !dup {
		t.Fatalf("receiver duplicate Accept = %v, %v; want dup", dup, err)
	}
	signed := delivery("k2")
	signed.BodyDigest = []byte("digest")
	if _, dup, err := rs.Accept(ctx, signed); err != nil || dup {
		t.Fatalf("receiver Accept with a digest = %v, %v; want a new row", dup, err)
	}
	signed.DeliveryKey = "k3"
	if _, dup, err := rs.Accept(ctx, signed); err != nil || !dup {
		t.Fatalf("receiver Accept of the same body under a new key = %v, %v; want dup", dup, err)
	}
	if v, err := rs.SchemaVersion(ctx); err != nil || v != store.LatestVersion() {
		t.Fatalf("receiver SchemaVersion = %d, %v", v, err)
	}
	conn, err := pgx.Connect(ctx, storetest.WithUser(t, dbURL, receiver, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO inbox (source, delivery_key, state) VALUES ('vikunja', 'k4', 'done')`); !denied(err) {
		t.Fatalf("receiver INSERT of a bookkeeping column = %v, want permission denied", err)
	}
	if _, err := rs.Claim(ctx, "receiver", 1, testTTL); !denied(err) {
		t.Fatalf("receiver Claim = %v, want permission denied", err)
	}
	if _, err := rs.QueueStats(ctx); !denied(err) {
		t.Fatalf("receiver QueueStats = %v, want permission denied", err)
	}
	if led := rs.Ledger(ctx); led.HandledRequest("x") || !denied(led.Err) {
		t.Fatalf("receiver ledger read = %v, want permission denied", led.Err)
	}
}

func TestGrantWorkerCannotChangeSchemaOrDelete(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	worker := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Worker: worker}); err != nil {
		t.Fatal(err)
	}
	accept(t, owner, "k")
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	it := claimOne(t, ws, "w")
	if _, err := ws.Complete(ctx, it.Lease, dispatch(true), false); err != nil {
		t.Fatalf("worker Complete: %v", err)
	}
	if _, err := ws.ClaimActions(ctx, "w", 1, testTTL); err != nil {
		t.Fatalf("worker ClaimActions: %v", err)
	}
	if _, err := ws.Migrate(ctx); !denied(err) {
		t.Fatalf("worker Migrate = %v, want permission denied", err)
	}
	conn, err := pgx.Connect(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DELETE FROM requests"); !denied(err) {
		t.Fatalf("worker DELETE = %v, want permission denied", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO inbox (source, delivery_key) VALUES ('vikunja', 'forged')`); !denied(err) {
		t.Fatalf("worker inbox INSERT = %v, want permission denied", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE inbox SET source = 'github'`); !denied(err) {
		t.Fatalf("worker update of a delivery column = %v, want permission denied", err)
	}
}

func TestGrantWorkerMaintainsTheInbox(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	worker := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Worker: worker}); err != nil {
		t.Fatal(err)
	}
	accept(t, owner, "retry")
	accept(t, owner, "fail")
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.Retry(ctx, claimOne(t, ws, "w").Lease, 0, "state"); err != nil {
		t.Fatalf("worker Retry: %v", err)
	}
	if err := ws.Fail(ctx, claimOne(t, ws, "w").Lease, "last"); err != nil {
		t.Fatalf("worker Fail: %v", err)
	}
	if _, _, err := ws.ExpireLeases(ctx); err != nil {
		t.Fatalf("worker ExpireLeases: %v", err)
	}
	if _, err := ws.PruneBodies(ctx, 0); err != nil {
		t.Fatalf("worker PruneBodies: %v", err)
	}
}

func TestGrantRefusesOneRoleForBoth(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	shared := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Receiver: shared, Worker: shared}); !errors.Is(err, store.ErrSameRole) {
		t.Fatalf("Grant with one role for both = %v, want ErrSameRole", err)
	}
}
