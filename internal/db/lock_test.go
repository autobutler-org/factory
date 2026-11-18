package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/db/dbtest"
)

func TestTryLockRunsOneHolderAtATime(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewDB(t)
	const key = 7

	outer, err := db.TryLock(ctx, pool, key, func(ctx context.Context) error {
		inner, err := db.TryLock(ctx, pool, key, func(context.Context) error {
			t.Error("a second holder ran while the first held the lock")
			return nil
		})
		if err != nil || inner {
			t.Errorf("second TryLock = %v, %v; want false while held", inner, err)
		}
		return nil
	})
	if err != nil || !outer {
		t.Fatalf("first TryLock = %v, %v", outer, err)
	}

	boom := errors.New("boom")
	ran, err := db.TryLock(ctx, pool, key, func(context.Context) error { return boom })
	if !ran || !errors.Is(err, boom) {
		t.Fatalf("TryLock after release = %v, %v; want it to run and return f's error", ran, err)
	}
	if ran, err := db.TryLock(ctx, pool, key, func(context.Context) error { return nil }); !ran || err != nil {
		t.Errorf("TryLock after a failed holder = %v, %v; want the lock released", ran, err)
	}
}
