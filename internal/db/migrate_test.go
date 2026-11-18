package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/db/dbtest"
)

func TestMigrationsAreEmbedded(t *testing.T) {
	migrations, err := db.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, m := range migrations {
		if m.Version != uint(i+1) {
			t.Errorf("migration %d has version %d; numbering must start at 1 with no gaps", i, m.Version)
		}
		if m.Name == "" {
			t.Errorf("migration %d has no name", m.Version)
		}
	}
}

func TestUpStatusDown(t *testing.T) {
	ctx := context.Background()
	databaseURL := dbtest.URL(t)
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	all, err := db.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	latest := all[len(all)-1].Version

	status, err := db.ReadStatus(ctx, pool)
	if err != nil {
		t.Fatalf("status of an empty schema: %v", err)
	}
	if status.Version != 0 || status.Pending() != len(all) {
		t.Fatalf("empty schema: version %d, %d pending; want 0 and %d", status.Version, status.Pending(), len(all))
	}
	if err := db.CheckMigrated(ctx, pool); !errors.Is(err, db.ErrMigrationsPending) {
		t.Fatalf("CheckMigrated on an empty schema = %v, want ErrMigrationsPending", err)
	}

	if err := db.Up(databaseURL); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := db.Up(databaseURL); err != nil {
		t.Fatalf("second up must be a no-op: %v", err)
	}
	status, err = db.ReadStatus(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != latest || status.Dirty || status.Pending() != 0 {
		t.Fatalf("after up: %+v, want version %d, clean, none pending", status, latest)
	}
	if err := db.CheckMigrated(ctx, pool); err != nil {
		t.Fatalf("CheckMigrated after up: %v", err)
	}

	if err := db.Down(databaseURL); err != nil {
		t.Fatalf("down: %v", err)
	}
	status, err = db.ReadStatus(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != latest-1 || status.Pending() != 1 {
		t.Fatalf("after down: version %d, %d pending; want %d and 1", status.Version, status.Pending(), latest-1)
	}

	// Every down must undo its up: run them all, then go back up.
	for status.Version > 0 {
		if err := db.Down(databaseURL); err != nil {
			t.Fatalf("down from %d: %v", status.Version, err)
		}
		if status, err = db.ReadStatus(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Up(databaseURL); err != nil {
		t.Fatalf("up after a full down: %v", err)
	}
}

func TestDownOnEmptySchema(t *testing.T) {
	if err := db.Down(dbtest.URL(t)); !errors.Is(err, db.ErrNothingToRollBack) {
		t.Fatalf("down on an empty schema = %v, want ErrNothingToRollBack", err)
	}
}

func TestPingQuery(t *testing.T) {
	pool := dbtest.NewDB(t)
	got, err := db.New(pool).Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("Ping = %d, want 1", got)
	}
}

func TestOpenRequiresURL(t *testing.T) {
	if _, err := db.Open(context.Background(), ""); !errors.Is(err, db.ErrNoDatabaseURL) {
		t.Fatalf("Open(\"\") = %v, want ErrNoDatabaseURL", err)
	}
}
