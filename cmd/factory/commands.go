package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// command is one entry of the command table. Dispatch, `factory --help`, each
// command's usage, and shell completion are all generated from the table and
// from the flags each command defines, so help cannot drift from behavior.
type command struct {
	// name is the words after factory, such as "work list".
	name string
	// args names the positional arguments in the synopsis, such as "<id>".
	// Each word is one required argument.
	args string
	// summary is the one line in the command list.
	summary string
	// about is the text under the synopsis.
	about string
	// env is an Environment section, for variables other than the flags'.
	env string
	// fromEnv reads every flag the command line leaves out from its
	// FACTORY_<FLAG> environment variable.
	fromEnv bool
	// setup defines the command's flags on fs and returns what runs it.
	setup func(fs *flag.FlagSet) runner
}

// runner runs a command with its positional arguments, once its flags are
// parsed. It returns the exit code.
type runner func(ctx context.Context, args []string, out streams) int

// streams are a command's input and output.
type streams struct {
	in          io.Reader
	out, err    io.Writer
	interactive bool // in and out are a terminal, so a fixable problem may offer the fix
}

// commands is the command table, in the order help lists it.
func commands() []command {
	return []command{
		migrateCommand("down", "Roll back the most recent migration only."),
		migrateCommand("status", "Print the applied version and each migration as applied or pending."),
		migrateCommand("up", "Apply every pending migration."),
		serveCommand(),
		versionCommand(),
		workerCommand(),
	}
}

const usageHeader = `factory is the autobutler-org software factory.

Usage:
  factory <command> [arguments]
  factory [-h | --help]

`

// dispatch finds the command args name and runs it.
func dispatch(ctx context.Context, args []string, s streams) int {
	table := commands()
	if len(args) == 0 {
		fmt.Fprint(s.err, groupUsage(table, ""))
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(s.out, groupUsage(table, ""))
		return 0
	}
	// The longest command whose words start args wins.
	var found *command
	words := 0
	for i := range table {
		n := len(strings.Fields(table[i].name))
		if n > words && n <= len(args) && strings.Join(args[:n], " ") == table[i].name {
			found, words = &table[i], n
		}
	}
	if found == nil {
		return unknown(table, args, s)
	}
	return found.run(ctx, args[words:], s)
}

// unknown answers args that name no command: a group's usage for a group
// alone or with -h, an error otherwise.
func unknown(table []command, args []string, s streams) int {
	group := args[0]
	if !slices.ContainsFunc(table, func(c command) bool { return strings.HasPrefix(c.name, group+" ") }) {
		fmt.Fprintf(s.err, "factory: unknown command %q\nRun 'factory --help' for usage.\n", group)
		return 2
	}
	switch {
	case len(args) == 1:
		fmt.Fprint(s.err, groupUsage(table, group))
		return 2
	case args[1] == "-h" || args[1] == "--help":
		fmt.Fprint(s.out, groupUsage(table, group))
		return 0
	}
	fmt.Fprintf(s.err, "factory: unknown command %q\nRun 'factory %s --help' for its commands.\n", group+" "+args[1], group)
	return 2
}

// groupUsage lists the commands under group, or every command when group is
// empty.
func groupUsage(table []command, group string) string {
	var b strings.Builder
	if group == "" {
		b.WriteString(usageHeader)
	} else {
		fmt.Fprintf(&b, "usage: factory %s <command>\n\n", group)
	}
	b.WriteString("Commands:\n")
	width := 0
	for _, c := range table {
		width = max(width, len(c.name))
	}
	for _, c := range table {
		if group == "" || strings.HasPrefix(c.name, group+" ") {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, c.name, c.summary)
		}
	}
	if group == "" {
		b.WriteString("\nRun 'factory <command> --help' for a command's usage.\n")
	} else {
		fmt.Fprintf(&b, "\nRun 'factory %s <command> --help' for a command's usage.\n", group)
	}
	return b.String()
}

// flags returns the command's flag set and its runner.
func (c command) flags() (*flag.FlagSet, runner) {
	fs := flag.NewFlagSet("factory "+c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, c.setup(fs)
}

// run parses the command's flags, which may come before, between or after
// its positional arguments, checks the argument count, and runs it.
func (c command) run(ctx context.Context, args []string, s streams) int {
	fs, run := c.flags()
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(s.out, c.usage())
				return 0
			}
			return c.usageError(s, err.Error())
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if c.fromEnv {
		if err := applyEnv(fs); err != nil {
			return c.usageError(s, err.Error())
		}
	}
	want := strings.Fields(c.args)
	switch {
	case len(positional) > len(want):
		return c.usageError(s, fmt.Sprintf("unexpected argument %q", positional[len(want)]))
	case len(positional) < len(want):
		return c.usageError(s, "missing "+strings.Join(want[len(positional):], " "))
	}
	return run(ctx, positional, s)
}

// usageError prints a usage error with the way to the command's help, and
// returns 2.
func (c command) usageError(s streams, msg string) int { return usageError(s, c.name, msg) }

func usageError(s streams, name, msg string) int {
	fmt.Fprintf(s.err, "factory %s: %s\nRun 'factory %s --help' for usage.\n", name, msg, name)
	return 2
}

// usage is the command's help, generated from its table entry and flags.
func (c command) usage() string {
	fs, _ := c.flags()
	var b strings.Builder
	synopsis := "factory " + c.name
	hasFlags := false
	fs.VisitAll(func(*flag.Flag) { hasFlags = true })
	if hasFlags {
		synopsis += " [flags]"
	}
	if c.args != "" {
		synopsis += " " + c.args
	}
	fmt.Fprintf(&b, "usage: %s\n\n%s\n", synopsis, strings.TrimSpace(c.about))
	if hasFlags {
		header := "\nFlags:\n"
		if c.fromEnv {
			first := ""
			fs.VisitAll(func(f *flag.Flag) {
				if first == "" {
					first = f.Name
				}
			})
			header = fmt.Sprintf("\nFlags (each also read from FACTORY_<FLAG>, e.g. %s):\n", envName(first))
		}
		b.WriteString(header)
		fs.VisitAll(func(f *flag.Flag) {
			arg, text := flag.UnquoteUsage(f)
			line := "  --" + f.Name
			if isBool(f) {
				arg = ""
			}
			if arg != "" {
				line += " " + strings.ToUpper(arg)
			}
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				text += " (default " + f.DefValue + ")"
			}
			b.WriteString(line + "\n" + wrap(text, "      ", 78))
		})
	}
	if c.env != "" {
		fmt.Fprintf(&b, "\nEnvironment:\n%s\n", strings.Trim(c.env, "\n"))
	}
	return b.String()
}

func isBool(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// wrap breaks text into lines of at most width columns, each starting with
// indent.
func wrap(text, indent string, width int) string {
	var b strings.Builder
	line := indent
	for _, word := range strings.Fields(text) {
		if line != indent && len(line)+1+len(word) > width {
			b.WriteString(line + "\n")
			line = indent
		}
		if line != indent {
			line += " "
		}
		line += word
	}
	b.WriteString(line + "\n")
	return b.String()
}
