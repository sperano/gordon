package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// admissionLockKey is the advisory lock every submitter takes around its
// admission check and the create that follows ("gordon-a").
const admissionLockKey int64 = 0x676f72646f6e2d61

// admissionReleaseTimeout bounds the unlock and the close of the
// admission session, so a database that stopped answering cannot hold
// the caller.
const admissionReleaseTimeout = 5 * time.Second

// WithAdmission runs fn holding the database-wide admission lock, so the
// variant limit and branch checks of every worker replica, and the Task
// creates they admit, run one at a time. Waiting for the lock honours ctx.
//
// The lock is taken on a session of its own, outside the pool: a waiter
// holds no pool connection, so fn and the rest of the process keep the
// pool while submissions queue. The session is unlocked, then closed;
// closing it releases the lock in any case, so the result is fn's
// whatever the release does.
func (s *Store) WithAdmission(ctx context.Context, fn func(context.Context) error) error {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("admission session: %w", err)
	}
	defer func() {
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), admissionReleaseTimeout)
		defer cancel()
		_, _ = conn.Exec(release, "SELECT pg_advisory_unlock($1)", admissionLockKey) //nolint:errcheck // the close below releases it too
		_ = conn.Close(release)                                                      //nolint:errcheck // a failed close still ends the session
	}()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", admissionLockKey); err != nil {
		return fmt.Errorf("admission lock: %w", err)
	}
	return fn(ctx)
}
