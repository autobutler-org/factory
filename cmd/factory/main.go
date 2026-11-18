// Command factory is the autobutler-org software factory. It parses arguments,
// dispatches to a subcommand, and turns the result into an exit code: 0 on
// success, 2 for a usage error, 1 for a runtime failure. It holds no business
// logic; that lives in internal/ packages.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// Stamped at build time by the Makefile with -ldflags "-X main.version=...".
// They are vars only because -X cannot set a const; nothing assigns them at
// run time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `factory is the autobutler-org software factory.

Usage:
  factory <command> [arguments]
  factory [-h | --help]

Commands:
  migrate     Apply, roll back, or report the database migrations.
  version     Print the version stamped into this binary.

Run 'factory <command> --help' for a command's usage.
`

const versionUsage = `usage: factory version

Print the version, commit, and build date stamped into this binary.
An unstamped build reports dev.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "migrate":
		return migrateMain(ctx, args[1:], stdout, stderr)
	case "version":
		return versionMain(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "factory: unknown command %q\nRun 'factory --help' for usage.\n", args[0])
	return 2
}

func versionMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, versionUsage)
		return 0
	}
	if len(args) != 0 {
		fmt.Fprintf(stderr, "factory version: takes no arguments\n%s", versionUsage)
		return 2
	}
	fmt.Fprintf(stdout, "factory %s (commit %s, built %s)\n", version, commit, date)
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
