# The `factory` command line

`factory` is the server, the worker, and the tool people use from a terminal. Every subcommand follows the
conventions below. The settings of `serve`, `worker` and `migrate` are in [config.md](config.md).

## Conventions

- `factory`, `factory -h`, `factory <group>` (such as `factory migrate`) and `factory <command> -h` print usage. With
  no arguments, or a group alone, the usage goes to stderr and the exit code is 2; asked for with `-h`, it goes to
  stdout and the exit code is 0.
- Exit codes: `0` success, `2` usage error (an unknown command or flag, a missing or extra argument), `1` runtime
  error.
- An error says what to do next: a usage error ends with `Run 'factory <command> --help' for usage.`, and a runtime
  error names the fix, for example `database is behind this binary's migrations: run 'factory migrate up'`.
- Flags may come before, between, or after a command's arguments. They are spelled `--name`; Go's `flag` package
  also accepts `-name`.
- One command table in `cmd/factory/commands.go` holds every command's name, arguments, summary and description,
  and each command defines its flags with their help text. `factory --help`, each command's usage, and dispatch are
  all generated from it, so help cannot drift from behavior.

## Commands

| Command | Does |
| --- | --- |
| `factory migrate up`, `down`, `status` | Apply, roll back one, or report the database migrations. |
| `factory serve` | Serve the HTTP API and the web UI. |
| `factory version` | Print the version, commit, and build date stamped into the binary. |
| `factory worker` | Run the step executor in a worker sandbox. |

## Tests

`cmd/factory/testdata/` holds golden files: the help text of `factory`, every group, and every command, and the
output of each command's error paths. `make test` compares against them; after an intended change,
`make test/golden` rewrites them, and the diff is part of the pull request.
