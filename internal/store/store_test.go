package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

const (
	testTTL   = time.Minute
	noDelay   = time.Duration(0)
	longDelay = time.Hour
)

var ctx = context.Background()

func delivery(key string) store.Delivery {
	return store.Delivery{
		Source: event.SourceVikunja, DeliveryKey: key, RequestKey: "vikunja/comment/1",
		Kind: event.VikunjaTaskCommentCreated, Scope: "vikunja/project/7",
		Headers: map[string]string{"X-Test": "1"}, Body: []byte(`{"k":"` + key + `"}`),
	}
}

func accept(t *testing.T, s *store.Store, key string) int64 {
	t.Helper()
	id, dup, err := s.Accept(ctx, delivery(key))
	if err != nil || dup {
		t.Fatalf("Accept(%s) = %d, %v, %v; want new row", key, id, dup, err)
	}
	return id
}

func claimOne(t *testing.T, s *store.Store, owner string) store.Item {
	t.Helper()
	items, err := s.Claim(ctx, owner, 1, testTTL)
	if err != nil || len(items) != 1 {
		t.Fatalf("Claim = %d items, %v; want 1", len(items), err)
	}
	return items[0]
}

func TestMigrateIsIdempotentAndConcurrent(t *testing.T) {
	dbURL := storetest.Database(t)
	const migrators = 4
	applied := make([]int, migrators)
	errs := make([]error, migrators)
	var wg sync.WaitGroup
	for i := range migrators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := store.Open(ctx, dbURL)
			if err != nil {
				errs[i] = err
				return
			}
			defer s.Close()
			applied[i], errs[i] = s.Migrate(ctx)
		}()
	}
	wg.Wait()
	total := 0
	for i := range migrators {
		if errs[i] != nil {
			t.Fatalf("migrator %d: %v", i, errs[i])
		}
		total += applied[i]
	}
	if total != store.LatestVersion() {
		t.Fatalf("migrations applied %d times in all, want each once (%d)", total, store.LatestVersion())
	}
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, err := s.Migrate(ctx); n != 0 || err != nil {
		t.Fatalf("re-run Migrate = %d, %v; want 0, nil", n, err)
	}
	if v, err := s.SchemaVersion(ctx); v != store.LatestVersion() || err != nil {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, store.LatestVersion())
	}
}

func TestSchemaVersionUnmigrated(t *testing.T) {
	s, err := store.Open(ctx, storetest.Database(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SchemaVersion(ctx); err == nil {
		t.Fatal("SchemaVersion on an unmigrated database succeeded")
	}
}

func TestAcceptDeduplicatesDeliveryKey(t *testing.T) {
	s, _ := storetest.Open(t)
	first := accept(t, s, "k1")
	if id, dup, err := s.Accept(ctx, delivery("k1")); !dup || id != 0 || err != nil {
		t.Fatalf("second Accept(k1) = %d, %v, %v; want 0, true, nil", id, dup, err)
	}
	second := accept(t, s, "k2")
	if second == first {
		t.Fatalf("distinct delivery keys share row %d", first)
	}
	it := claimOne(t, s, "w")
	want := delivery("k1")
	if it.ID != first || it.DeliveryKey != "k1" || it.RequestKey != want.RequestKey || it.Kind != want.Kind ||
		it.Scope != want.Scope || it.Source != want.Source || string(it.Body) != string(want.Body) ||
		it.Headers["X-Test"] != "1" || it.Attempts != 1 {
		t.Fatalf("claimed %+v, want the stored k1 delivery on its first attempt", it)
	}
}

func TestOpenRefusesBadURLWithoutEchoingIt(t *testing.T) {
	_, err := store.Open(ctx, "postgres://u:s3cret@host:notaport/db")
	if err == nil || err.Error() != "database url invalid" {
		t.Fatalf("Open = %v, want the fixed refusal without the URL", err)
	}
}

func TestClaimLeaseContention(t *testing.T) {
	s, _ := storetest.Open(t)
	const rows, workers = 40, 4
	for i := range rows {
		accept(t, s, fmt.Sprintf("k%02d", i))
	}
	claimed := make([][]int64, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				items, err := s.Claim(ctx, fmt.Sprintf("w%d", w), 3, testTTL)
				if err != nil {
					t.Errorf("worker %d: %v", w, err)
					return
				}
				if len(items) == 0 {
					return
				}
				for _, it := range items {
					claimed[w] = append(claimed[w], it.ID)
				}
			}
		}()
	}
	wg.Wait()
	var all []int64
	for _, ids := range claimed {
		all = append(all, ids...)
	}
	slices.Sort(all)
	if len(all) != rows || len(slices.Compact(slices.Clone(all))) != rows {
		t.Fatalf("claimed %d rows (%d distinct), want each of %d exactly once", len(all), len(slices.Compact(all)), rows)
	}
}

func TestLeasedRowIsNotClaimedAgainUntilExpiry(t *testing.T) {
	s, dbURL := storetest.Open(t)
	accept(t, s, "k")
	first := claimOne(t, s, "a")
	if items, err := s.Claim(ctx, "b", 1, testTTL); len(items) != 0 || err != nil {
		t.Fatalf("Claim under a live lease = %d items, %v; want none", len(items), err)
	}
	storetest.Exec(t, dbURL, `UPDATE inbox SET lease_until = now() - interval '1 second'`)
	second := claimOne(t, s, "b")
	if second.ID != first.ID || second.Token == first.Token || second.Attempts != 2 {
		t.Fatalf("reclaim = %+v, want row %d under a new token on attempt 2", second.Lease, first.ID)
	}
	if err := s.Retry(ctx, first.Lease, noDelay, "late"); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("Retry with the expired lease = %v, want ErrLeaseLost", err)
	}
	if err := s.Fail(ctx, first.Lease, "late"); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("Fail with the expired lease = %v, want ErrLeaseLost", err)
	}
}

func TestRetryDeferAndFail(t *testing.T) {
	s, dbURL := storetest.Open(t)
	id := accept(t, s, "k")
	it := claimOne(t, s, "w")
	if err := s.Retry(ctx, it.Lease, longDelay, "vikunja unavailable"); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.Claim(ctx, "w", 1, testTTL); len(items) != 0 {
		t.Fatal("row claimed before its backoff elapsed")
	}
	var lastErr string
	var attempts int
	storetest.QueryRow(t, dbURL, `SELECT last_error, attempts FROM inbox WHERE id = $1`, []any{id}, &lastErr, &attempts)
	if lastErr != "vikunja unavailable" || attempts != 1 {
		t.Fatalf("after Retry: last_error %q attempts %d, want the reason and 1", lastErr, attempts)
	}
	storetest.Exec(t, dbURL, `UPDATE inbox SET next_attempt_at = now()`)
	it = claimOne(t, s, "w")
	if err := s.Defer(ctx, it.Lease, noDelay); err != nil {
		t.Fatal(err)
	}
	it = claimOne(t, s, "w")
	if it.Attempts != 2 {
		t.Fatalf("attempts after a deferral = %d, want 2 (the deferred claim does not count)", it.Attempts)
	}
	if err := s.Fail(ctx, it.Lease, "gave up"); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, dbURL, `UPDATE inbox SET next_attempt_at = now()`)
	if items, _ := s.Claim(ctx, "w", 1, testTTL); len(items) != 0 {
		t.Fatal("dead-lettered row claimed again")
	}
}
