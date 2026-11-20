package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/route"
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

// insufficientPrivilege is the SQLSTATE of a refused privilege, the same
// whatever the server's message language.
const insufficientPrivilege = "42501"

func denied(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == insufficientPrivilege
}

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
	// The worker queues its own synthetic deliveries, never a body digest
	// (the receiver's duplicate check of signed GitHub bodies): a row the
	// insert policy admits, so only the column grant refuses it.
	if _, err := conn.Exec(ctx, `INSERT INTO inbox (source, delivery_key, request_key, kind, scope, headers, body, body_digest)
		VALUES ('vikunja', 'forged', '', 'vikunja/task.updated', '', '{"`+store.SyntheticHeader+`": "requeue"}', '\x7b7d', '\x00')`); !denied(err) {
		t.Fatalf("worker inbox INSERT of a body digest = %v, want permission denied", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO inbox (source, delivery_key, request_key, kind, scope, headers, body)
		VALUES ('vikunja', 'synthetic', '', 'vikunja/task.updated', '', '{"X-Gordon-Synthetic": "requeue"}', '\x7b7d')`); err != nil {
		t.Fatalf("worker synthetic inbox INSERT = %v, want allowed", err)
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

func TestGrantReusedWorkerRoleAsReceiverKeepsNoWorkerRight(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	reused := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Worker: reused}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Grant(ctx, store.Roles{Receiver: reused}); err != nil {
		t.Fatal(err)
	}
	accept(t, owner, "k")
	rs, err := store.Open(ctx, storetest.WithUser(t, dbURL, reused, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if _, dup, err := rs.Accept(ctx, delivery("new")); err != nil || dup {
		t.Fatalf("receiver Accept = %v, %v; want a new row", dup, err)
	}
	if _, err := rs.Claim(ctx, "receiver", 1, testTTL); !denied(err) {
		t.Fatalf("former worker's inbox Claim = %v, want permission denied", err)
	}
	if _, err := rs.ClaimActions(ctx, "receiver", 1, testTTL); !denied(err) {
		t.Fatalf("former worker's ClaimActions = %v, want permission denied", err)
	}
	if led := rs.Ledger(ctx); led.HandledRequest("x") || !denied(led.Err) {
		t.Fatalf("former worker's ledger read = %v, want permission denied", led.Err)
	}
}

func TestGrantReusedReceiverRoleAsWorkerCannotInsertDeliveries(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	reused := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Receiver: reused}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Grant(ctx, store.Roles{Worker: reused}); err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, reused, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, _, err := ws.Accept(ctx, delivery("forged")); !denied(err) {
		t.Fatalf("former receiver's Accept = %v, want permission denied", err)
	}
}

func TestGrantWorkerUpdatesOnlyMutableColumns(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	worker := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Worker: worker}); err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	accept(t, owner, "k1")
	if _, err := ws.Complete(ctx, claimOne(t, ws, "w").Lease, submitOnly(), false); err != nil {
		t.Fatalf("worker Complete: %v", err)
	}
	storetest.Exec(t, dbURL, `UPDATE runs SET state = 'finished'`)
	acceptRequest(t, owner, "k2", "vikunja/comment/2")
	if got, err := ws.Complete(ctx, claimOne(t, ws, "w").Lease, submitOnly(), false); err != nil || got.Outcome != route.Dispatch {
		t.Fatalf("worker rerun of a finished run = %s, %v; want dispatch", got.Outcome, err)
	}
	claimed, err := ws.ClaimActions(ctx, "w", 1, testTTL)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("worker ClaimActions = %d, %v; want one", len(claimed), err)
	}
	if err := ws.ActionDone(ctx, claimed[0].Lease); err != nil {
		t.Fatalf("worker ActionDone: %v", err)
	}
	conn, err := pgx.Connect(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`UPDATE decisions SET outcome = 'ignored'`,
		`UPDATE actions SET target = 'github/x'`,
		`UPDATE runs SET name = 'other'`,
		`UPDATE requests SET inbox_id = inbox_id`,
		`UPDATE shadow_tasks SET name = 'other'`,
		`DELETE FROM shadow_tasks`,
		`DELETE FROM runs`,
	} {
		if _, err := conn.Exec(ctx, q); !denied(err) {
			t.Errorf("worker %s = %v, want permission denied", q, err)
		}
	}
}

func TestGrantWorkerDispatchRights(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	worker := role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Worker: worker}); err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	id := acceptRequest(t, owner, "k1", "vikunja/comment/1")
	if _, err := ws.Complete(ctx, claimOne(t, ws, "w").Lease, withTask("p"), false); err != nil {
		t.Fatalf("worker Complete with a Task: %v", err)
	}
	if _, err := ws.RunToSubmit(ctx, runName); err != nil {
		t.Fatalf("worker RunToSubmit: %v", err)
	}
	if err := ws.RunSubmitted(ctx, runName, id, "u", "Running"); err != nil {
		t.Fatalf("worker RunSubmitted: %v", err)
	}
	if _, err := ws.ObservedRuns(ctx, 1); err != nil {
		t.Fatalf("worker ObservedRuns: %v", err)
	}
	if err := ws.ObserveRun(ctx, runName, id, false, ""); err != nil {
		t.Fatalf("worker ObserveRun: %v", err)
	}
	id2 := acceptRequest(t, owner, "k2", "vikunja/comment/2")
	if _, err := ws.Complete(ctx, claimOne(t, ws, "w").Lease, withTask("p"), true); err != nil {
		t.Fatalf("worker shadow Complete: %v", err)
	}
	pending, err := ws.PendingShadow(ctx, 1)
	if err != nil || len(pending) != 1 || pending[0].InboxID != id2 {
		t.Fatalf("worker PendingShadow = %+v, %v; want the shadow Task of k2", pending, err)
	}
	if err := ws.ShadowCompared(ctx, pending[0].Name, pending[0].InboxID, store.ShadowMatch, nil); err != nil {
		t.Fatalf("worker ShadowCompared: %v", err)
	}
	var result string
	storetest.QueryRow(t, dbURL, `SELECT result FROM shadow_tasks WHERE inbox_id = $1`, []any{id2}, &result)
	if result != store.ShadowMatch {
		t.Fatalf("shadow result %q after the worker's compare, want %q", result, store.ShadowMatch)
	}
	if _, err := ws.PruneTasks(ctx, 0); err != nil {
		t.Fatalf("worker PruneTasks: %v", err)
	}
}

// TestGrantWorkerInsertsOnlySyntheticVikunjaDeliveries: the row policy
// refuses a worker insert of a GitHub delivery or of a Vikunja delivery
// without the synthetic header; the receiver and the worker's own
// synthetic deliveries are unaffected.
func TestGrantWorkerInsertsOnlySyntheticVikunjaDeliveries(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	receiver, worker := role(t, dbURL), role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Receiver: receiver, Worker: worker}); err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	syn, err := store.TaskUpdate(7, 3, event.Synthetic{Reconcile: "x"})
	if err != nil {
		t.Fatal(err)
	}
	github := syn
	github.Source, github.DeliveryKey = event.SourceGitHub, "forged-github"
	unmarked := syn
	unmarked.DeliveryKey, unmarked.Headers = "forged-unmarked", map[string]string{"X-Test": "1"}
	for name, d := range map[string]store.Delivery{"github": github, "no synthetic header": unmarked} {
		if _, err := ws.Enqueue(ctx, []store.Delivery{d}); !denied(err) {
			t.Errorf("worker insert of a %s delivery = %v, want permission denied", name, err)
		}
	}
	if n, err := ws.Enqueue(ctx, []store.Delivery{syn}); err != nil || n != 1 {
		t.Fatalf("worker synthetic Enqueue = %d, %v; want 1 stored", n, err)
	}
	rs, err := store.Open(ctx, storetest.WithUser(t, dbURL, receiver, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if _, dup, err := rs.Accept(ctx, delivery("k")); err != nil || dup {
		t.Fatalf("receiver Accept under the row policies = %v, %v; want a new row", dup, err)
	}
	if _, err := ws.Claim(ctx, "w", 2, testTTL); err != nil {
		t.Fatalf("worker Claim under the row policies: %v", err)
	}
}

// forgedDeliveries are a GitHub delivery and a Vikunja one without the
// synthetic header, both of which the worker insert policy refuses.
func forgedDeliveries(t *testing.T) map[string]store.Delivery {
	t.Helper()
	syn, err := store.TaskUpdate(7, 3, event.Synthetic{Reconcile: "x"})
	if err != nil {
		t.Fatal(err)
	}
	github := syn
	github.Source, github.DeliveryKey = event.SourceGitHub, "forged-github"
	unmarked := syn
	unmarked.DeliveryKey, unmarked.Headers = "forged-unmarked", map[string]string{"X-Test": "1"}
	return map[string]store.Delivery{"github": github, "no synthetic header": unmarked}
}

// assertWorkerRestricted fails unless every forged delivery the worker
// role inserts is refused.
func assertWorkerRestricted(t *testing.T, dbURL, worker string) {
	t.Helper()
	ws, err := store.Open(ctx, storetest.WithUser(t, dbURL, worker, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	for name, d := range forgedDeliveries(t) {
		if _, err := ws.Enqueue(ctx, []store.Delivery{d}); !denied(err) {
			t.Errorf("worker %s insert of a %s delivery = %v, want permission denied", worker, name, err)
		}
	}
}

// TestGrantWithoutAWorkerKeepsTheInsertPolicy: a later Grant of only a
// receiver (a migrate run without GORDON_WORKER_ROLE) or of nothing
// leaves the worker's insert check in place.
func TestGrantWithoutAWorkerKeepsTheInsertPolicy(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	worker, receiver := role(t, dbURL), role(t, dbURL)
	for _, r := range []store.Roles{{Worker: worker}, {Receiver: receiver}, {}} {
		if err := owner.Grant(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	assertWorkerRestricted(t, dbURL, worker)
}

// TestGrantNewWorkerKeepsTheFormerWorkerRestricted: a former worker keeps
// its INSERT grant, so a Grant of another worker role restricts both.
func TestGrantNewWorkerKeepsTheFormerWorkerRestricted(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	former, worker := role(t, dbURL), role(t, dbURL)
	for _, w := range []string{former, worker} {
		if err := owner.Grant(ctx, store.Roles{Worker: w}); err != nil {
			t.Fatal(err)
		}
	}
	assertWorkerRestricted(t, dbURL, former)
	assertWorkerRestricted(t, dbURL, worker)
}

// TestGrantWorkerRoleReusedAsReceiverAcceptsWebhooks: a worker role
// granted the receiver mode leaves the insert policy, so it can store a
// GitHub webhook, while the new worker stays restricted.
func TestGrantWorkerRoleReusedAsReceiverAcceptsWebhooks(t *testing.T) {
	owner, dbURL := storetest.Open(t)
	reused, worker := role(t, dbURL), role(t, dbURL)
	if err := owner.Grant(ctx, store.Roles{Worker: reused}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Grant(ctx, store.Roles{Receiver: reused, Worker: worker}); err != nil {
		t.Fatal(err)
	}
	rs, err := store.Open(ctx, storetest.WithUser(t, dbURL, reused, rolePassword))
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	webhook := delivery("k")
	webhook.Source = event.SourceGitHub
	if _, dup, err := rs.Accept(ctx, webhook); err != nil || dup {
		t.Fatalf("former worker's receiver Accept of a GitHub webhook = %v, %v; want a new row", dup, err)
	}
	assertWorkerRestricted(t, dbURL, worker)
}
