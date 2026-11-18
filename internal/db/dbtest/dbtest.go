// Package dbtest gives a test a Postgres schema of its own, so database tests
// run against the real migrations and never see each other's rows.
//
// It reads the server from FACTORY_DATABASE_URL. When that is unset the test
// is skipped with a message that says how to run it; when it is set but no
// Postgres answers, the test fails, because a database check that quietly did
// not run would read as a pass. It does not start Postgres: `make
// serve/postgres` does.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// URL returns a database URL whose search_path is a new, empty schema that is
// dropped when the test ends. Nothing is migrated.
func URL(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(db.DatabaseURLEnv)
	if raw == "" {
		t.Skipf("needs Postgres: %s is not set. Run `make serve/postgres`, then `make test/db`", db.DatabaseURLEnv)
	}
	base, err := url.Parse(raw)
	if err != nil || base.Host == "" {
		t.Fatalf("%s is not a postgres:// URL (%v). Run the tests through `make test/db`, which sets it", db.DatabaseURLEnv, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base.String())
	if err != nil {
		t.Fatalf("no Postgres answers at %s (%v). Run `make serve/postgres`, then run this again", base.Host, err)
	}
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	schema := "factory_test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("dbtest: create schema %s: %v", schema, err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, base.String())
		if err != nil {
			t.Errorf("dbtest: connect to drop schema %s: %v", schema, err)
			return
		}
		defer func() { _ = admin.Close(ctx) }()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("dbtest: drop schema %s: %v", schema, err)
		}
	})

	query := base.Query()
	query.Set("search_path", schema)
	base.RawQuery = query.Encode()
	return base.String()
}

// NewDB returns a pool on a schema of the test's own with every migration
// applied. The pool is closed and the schema dropped when the test ends.
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	databaseURL := URL(t)
	if err := db.Up(databaseURL); err != nil {
		t.Fatalf("dbtest: apply migrations: %v", err)
	}
	pool, err := db.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("dbtest: open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
