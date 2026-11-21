package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/autobutler-org/factory/internal/db"
)

// migrateCommand is one of factory migrate up, down and status.
func migrateCommand(sub, summary string) command {
	return command{
		name:    "migrate " + sub,
		summary: summary,
		about:   summary + " The migrations are built into this binary.",
		fromEnv: true,
		setup: func(fs *flag.FlagSet) runner {
			databaseURL := fs.String("database-url", "", "Postgres `URL`")
			return func(ctx context.Context, _ []string, s streams) int {
				if *databaseURL == "" {
					fmt.Fprintf(s.err, "factory migrate %s: %v\n", sub, db.ErrNoDatabaseURL)
					return 1
				}
				var err error
				switch sub {
				case "up":
					if err = db.Up(*databaseURL); err == nil {
						fmt.Fprintln(s.out, "Migrations applied.")
					}
				case "down":
					if err = db.Down(*databaseURL); err == nil {
						fmt.Fprintln(s.out, "Rolled back the most recent migration.")
					}
				case "status":
					err = printStatus(ctx, *databaseURL, s.out)
				}
				if err != nil {
					fmt.Fprintf(s.err, "factory migrate %s: %v\n", sub, err)
					return 1
				}
				return 0
			}
		},
	}
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
