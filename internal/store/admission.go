package store

import (
	"context"
	"errors"
	"fmt"
)

// admissionLockKey is the advisory lock every submitter takes around its
// admission check and the create that follows ("gordon-a").
const admissionLockKey int64 = 0x676f72646f6e2d61

// WithAdmission runs fn holding the database-wide admission lock, so the
// variant limit and branch checks of every worker replica, and the Task
// creates they admit, run one at a time. Waiting for the lock honours ctx.
func (s *Store) WithAdmission(ctx context.Context, fn func(context.Context) error) (err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", admissionLockKey); err != nil {
		conn.Release()
		return fmt.Errorf("admission lock: %w", err)
	}
	defer func() {
		_, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", admissionLockKey)
		if uerr != nil {
			// A session that may still hold the lock never goes back to
			// the pool: closing it releases the lock.
			_ = conn.Hijack().Close(context.WithoutCancel(ctx))
			err = errors.Join(err, fmt.Errorf("admission unlock: %w", uerr))
			return
		}
		conn.Release()
	}()
	return fn(ctx)
}
