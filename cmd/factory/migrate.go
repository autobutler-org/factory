package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/autobutler-org/factory/internal/db"
)

const migrateUsage = `usage: factory migrate <up | down | status> [--database-url URL]

Apply, roll back, or report the database migrations built into this binary.

  up       apply every pending migration
  down     roll back the most recent migration only
  status   print the applied version and each migration as applied or pending

Flags:
  --database-url URL   Postgres URL (env FACTORY_DATABASE_URL)
`

func migrateMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, migrateUsage)
		return 2
	}
	sub := args[0]
	switch sub {
	case "-h", "--help":
		fmt.Fprint(stdout, migrateUsage)
		return 0
	case "up", "down", "status":
	default:
		fmt.Fprintf(stderr, "factory migrate: unknown subcommand %q\n%s", sub, migrateUsage)
		return 2
	}

	fs := flag.NewFlagSet("factory migrate "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	databaseURL := fs.String("database-url", "", "")
	if err := parseFlags(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, migrateUsage)
			return 0
		}
		fmt.Fprintf(stderr, "factory migrate %s: %v\n%s", sub, err, migrateUsage)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "factory migrate %s: unexpected argument %q\n%s", sub, fs.Arg(0), migrateUsage)
		return 2
	}
	if *databaseURL == "" {
		fmt.Fprintf(stderr, "factory migrate %s: %v\n", sub, db.ErrNoDatabaseURL)
		return 1
	}

	var err error
	switch sub {
	case "up":
		if err = db.Up(*databaseURL); err == nil {
			fmt.Fprintln(stdout, "Migrations applied.")
		}
	case "down":
		if err = db.Down(*databaseURL); err == nil {
			fmt.Fprintln(stdout, "Rolled back the most recent migration.")
		}
	case "status":
		err = printStatus(ctx, *databaseURL, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "factory migrate %s: %v\n", sub, err)
		return 1
	}
	return 0
}

func printStatus(ctx context.Context, databaseURL string, stdout io.Writer) error {
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	status, err := db.ReadStatus(ctx, pool)
	if err != nil {
		return err
	}
	state := "clean"
	if status.Dirty {
		state = "dirty"
	}
	fmt.Fprintf(stdout, "version %d, %s, %d pending\n", status.Version, state, status.Pending())
	for _, m := range status.Migrations {
		applied := "pending"
		if m.Applied {
			applied = "applied"
		}
		fmt.Fprintf(stdout, "%03d_%s  %s\n", m.Version, m.Name, applied)
	}
	if status.Pending() > 0 {
		fmt.Fprintln(stdout, "Run `factory migrate up` to apply the pending migrations.")
	}
	return nil
}
