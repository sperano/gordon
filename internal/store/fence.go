package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// execFenced runs an UPDATE on a leased row and refuses it with
// ErrLeaseLost when no row matched the lease token.
func execFenced(ctx context.Context, q execer, sql string, args ...any) error {
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}
