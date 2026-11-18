package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver golang-migrate runs on
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var (
	// ErrMigrationsPending is returned by CheckMigrated when the database is
	// behind the migrations built into this binary.
	ErrMigrationsPending = errors.New("database is behind this binary's migrations: run `factory migrate up`")
	// ErrDirty is returned when a migration failed halfway.
	ErrDirty = errors.New("database is dirty: a migration failed halfway; fix it by hand, then run `factory migrate up`")
	// ErrNothingToRollBack is returned by Down on a database with no
	// migrations applied.
	ErrNothingToRollBack = errors.New("no migrations are applied, so there is nothing to roll back")
	// ErrAhead is returned by CheckMigrated when the database has a
	// migration this binary does not know, i.e. a newer binary migrated it.
	ErrAhead = errors.New("database is ahead of this binary's migrations: run a newer factory")
)

var migrationName = regexp.MustCompile(`^(\d+)_(.+)\.up\.sql$`)

// Migration is one numbered migration built into the binary.
type Migration struct {
	Version uint
	Name    string
	Applied bool
}

// Status is where a database stands against the built-in migrations.
type Status struct {
	Version    uint
	Dirty      bool
	Migrations []Migration
}

// Pending counts the migrations not yet applied.
func (s Status) Pending() int {
	n := 0
	for _, m := range s.Migrations {
		if !m.Applied {
			n++
		}
	}
	return n
}

// Migrations lists the migrations built into the binary, oldest first.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		match := migrationName.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		version, err := strconv.ParseUint(match[1], 10, 0)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: uint(version), Name: match[2]})
	}
	slices.SortFunc(out, func(a, b Migration) int { return int(a.Version) - int(b.Version) })
	return out, nil
}

// ReadStatus reports the applied version and which built-in migrations are
// applied. A database never migrated is version 0.
func ReadStatus(ctx context.Context, pool *pgxpool.Pool) (Status, error) {
	all, err := Migrations()
	if err != nil {
		return Status{}, err
	}
	var version int64
	var dirty bool
	err = pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "42P01": // undefined_table: never migrated
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Status{}, fmt.Errorf("read migration version: %w", err)
	}
	for i := range all {
		all[i].Applied = all[i].Version <= uint(version)
	}
	return Status{Version: uint(version), Dirty: dirty, Migrations: all}, nil
}

// CheckMigrated returns nil when the database is exactly at the latest
// built-in migration, and an error that says what to run otherwise.
func CheckMigrated(ctx context.Context, pool *pgxpool.Pool) error {
	status, err := ReadStatus(ctx, pool)
	if err != nil {
		return err
	}
	if status.Dirty {
		return fmt.Errorf("version %d: %w", status.Version, ErrDirty)
	}
	latest := uint(0)
	if n := len(status.Migrations); n > 0 {
		latest = status.Migrations[n-1].Version
	}
	switch {
	case status.Version < latest:
		return fmt.Errorf("at version %d of %d: %w", status.Version, latest, ErrMigrationsPending)
	case status.Version > latest:
		return fmt.Errorf("at version %d, binary knows %d: %w", status.Version, latest, ErrAhead)
	}
	return nil
}

// Up applies every pending migration. It is a no-op on a current database.
func Up(databaseURL string) error {
	return withMigrate(databaseURL, func(m *migrate.Migrate) error {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate up: %w", err)
		}
		return nil
	})
}

// Down rolls back the most recent migration only.
func Down(databaseURL string) error {
	return withMigrate(databaseURL, func(m *migrate.Migrate) error {
		err := m.Steps(-1)
		var notExist *fs.PathError
		if errors.Is(err, migrate.ErrNilVersion) || errors.Is(err, fs.ErrNotExist) || errors.As(err, &notExist) {
			return ErrNothingToRollBack
		}
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		return nil
	})
}

func withMigrate(databaseURL string, f func(*migrate.Migrate) error) error {
	if databaseURL == "" {
		return ErrNoDatabaseURL
	}
	conn, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer conn.Close()
	driver, err := migratepgx.WithInstance(conn, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("prepare migrations (is Postgres running? `make serve/postgres` starts one locally): %w", err)
	}
	source, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("prepare migrations: %w", err)
	}
	return f(m)
}
