package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TryLock runs f while holding the Postgres session advisory lock key, and
// reports whether it ran. When another session holds the lock, f does not
// run and TryLock returns false at once. The lock lives on one pooled
// connection; if it cannot be released, that connection is closed, which
// releases it, so a crashed or canceled holder never keeps it.
func TryLock(ctx context.Context, pool *pgxpool.Pool, key int64, f func(context.Context) error) (bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire a connection for lock %d: %w", key, err)
	}
	q := New(conn)
	locked, err := q.TryAdvisoryLock(ctx, key)
	if err != nil || !locked {
		conn.Release()
		if err != nil {
			return false, fmt.Errorf("take lock %d: %w", key, err)
		}
		return false, nil
	}
	ferr := f(ctx)
	if err := q.AdvisoryUnlock(context.WithoutCancel(ctx), key); err != nil {
		_ = conn.Hijack().Close(context.WithoutCancel(ctx))
		return true, ferr
	}
	conn.Release()
	return true, ferr
}
