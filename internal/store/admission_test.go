package store_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/store/storetest"
)

// admissionLockKey is store's admission advisory lock ("gordon-a").
const admissionLockKey int64 = 0x676f72646f6e2d61

// admissionWait is how long a second admission waits for the held lock.
const admissionWait = 100 * time.Millisecond

// lockFree reports whether another session can take the admission lock
// (and releases it with that session).
func lockFree(t *testing.T, dbURL string) bool {
	t.Helper()
	var free bool
	storetest.QueryRow(t, dbURL, `SELECT pg_try_advisory_lock($1)`, []any{admissionLockKey}, &free)
	return free
}

func TestWithAdmissionHoldsTheLockForFn(t *testing.T) {
	s, dbURL := storetest.Open(t)
	errFn := errors.New("fn failed")
	err := s.WithAdmission(ctx, func(context.Context) error {
		if lockFree(t, dbURL) {
			t.Error("another session took the admission lock while fn held it")
		}
		return errFn
	})
	if !errors.Is(err, errFn) {
		t.Fatalf("WithAdmission = %v, want fn's error", err)
	}
	if !lockFree(t, dbURL) {
		t.Fatal("admission lock still held after fn returned")
	}
}

func TestWithAdmissionWaitHonoursContext(t *testing.T) {
	s, _ := storetest.Open(t)
	err := s.WithAdmission(ctx, func(context.Context) error {
		waiting, cancel := context.WithTimeout(ctx, admissionWait)
		defer cancel()
		ran := false
		err := s.WithAdmission(waiting, func(context.Context) error { ran = true; return nil })
		if ran || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("second admission while the lock is held: ran %v, err %v", ran, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// singleConnStore opens a second store on dbURL whose pool has one
// connection.
func singleConnStore(t *testing.T, dbURL string) *store.Store {
	t.Helper()
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "1")
	u.RawQuery = q.Encode()
	s, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// waitForLockWaiter returns once a session waits on an advisory lock.
func waitForLockWaiter(t *testing.T, dbURL string) {
	t.Helper()
	deadline := time.Now().Add(lockWaitGuard)
	for time.Now().Before(deadline) {
		if count(t, dbURL, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`) > 0 {
			return
		}
		time.Sleep(admissionPoll)
	}
	t.Fatal("no session waits on the admission lock")
}

// admissionPoll is how often the test looks for the waiting session.
const admissionPoll = 10 * time.Millisecond

// TestWithAdmissionWaiterKeepsNoPoolConnection: a submission waiting for
// the lock leaves the pool to fn, even when the pool has one connection.
func TestWithAdmissionWaiterKeepsNoPoolConnection(t *testing.T) {
	_, dbURL := storetest.Open(t)
	s := singleConnStore(t, dbURL)
	err := s.WithAdmission(ctx, func(context.Context) error {
		waiting, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- s.WithAdmission(waiting, func(context.Context) error { return nil }) }()
		waitForLockWaiter(t, dbURL)
		guarded, stop := context.WithTimeout(ctx, lockWaitGuard)
		defer stop()
		perr := s.Ping(guarded)
		cancel()
		if werr := <-done; !errors.Is(werr, context.Canceled) {
			t.Errorf("cancelled waiter = %v, want context.Canceled", werr)
		}
		return perr
	})
	if err != nil {
		t.Fatalf("a pool read while another submission waits for the lock: %v", err)
	}
}
