package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

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
