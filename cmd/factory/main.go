// Command factory is the autobutler-org software factory. It parses arguments,
// dispatches to a subcommand from the command table (commands.go), which all
// usage text is generated from, and turns the result into an exit code: 0 on
// success, 2 for a usage error, 1 for a runtime failure. It holds no business
// logic; that lives in internal/ packages. docs/cli.md has the conventions.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
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

// run runs the command args name, with no input and no terminal: tests and
// scripts see only what a non-interactive run does.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, streams{in: strings.NewReader(""), out: stdout, err: stderr})
}

func versionCommand() command {
	return command{
		name:    "version",
		summary: "Print the version stamped into this binary.",
		about: `Print the version, commit, and build date stamped into this binary.
An unstamped build reports dev.`,
		setup: func(*flag.FlagSet) runner {
			return func(_ context.Context, _ []string, s streams) int {
				fmt.Fprintf(s.out, "factory %s (commit %s, built %s)\n", version, commit, date)
				return 0
			}
		},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := dispatch(ctx, os.Args[1:], streams{in: os.Stdin, out: os.Stdout, err: os.Stderr, interactive: isTerminal(os.Stdin) && isTerminal(os.Stdout)})
	stop()
	os.Exit(code)
}

// isTerminal reports whether f is a character device, such as a terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
