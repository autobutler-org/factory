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
  all generated from it, and so is shell completion, so neither can drift from behavior.

## Talking to a server

The commands that call the API (`status`, `work ...`, `workers ...`) take:

- the server's URL from `--server`, else `FACTORY_SERVER`. Neither set is a usage error that says to set one.
  A config file written by `factory login` comes with sign-in, #15.
- the bearer token from `FACTORY_API_TOKEN`, the token the server was started with. There is deliberately no flag,
  so the token stays out of process listings. #15 replaces the shared token with sign-in.
- `--output table` (the default) or `--output json`. JSON is the API's reply, indented, for scripts.

Their errors name the fix: a server that cannot be reached, a missing or refused token, an unknown item, or a route
an older server does not have.

## Commands

| Command | Does |
| --- | --- |
| `factory completion bash\|zsh\|fish` | Print a completion script generated from the command table: commands, groups, and each command's flags. Load it with `source <(factory completion bash)` (or `zsh`), or `factory completion fish \| source`. |
| `factory doctor [--worker]` | Check the setup; see below. |
| `factory migrate up`, `down`, `status` | Apply, roll back one, or report the database migrations. |
| `factory serve` | Serve the HTTP API and the web UI. |
| `factory status` | Work counts by state, workers by state, the item waiting in `ready` longest, and the five latest failures (`GET /api/v1/status`). |
| `factory version` | Print the version, commit, and build date stamped into the binary. |
| `factory work enqueue --title T [--body B] [--repos R,...] [--state design\|ready] ...` | Enqueue work by value; `--file F` (or `-` for stdin) sends a JSON item as `POST /api/v1/work` takes it. A repeat of a sourced item or an `--idempotency-key` returns the first. Enqueueing by tracker URL waits on the input adapters (#7, #18). |
| `factory work list [--state S] [--source S] [--repo R] [--claimed-by ID] [--limit N] [--cursor C]` | A table of work items, highest priority first, then oldest. The next page's cursor goes to stderr. `--mine` waits on sign-in (#15). |
| `factory work move <id> <state> [--reason R]` | Ask the state machine to move an item. When it refuses, the states the item may move to from where it is are printed, and the exit code is 1. |
| `factory work show <id>` | One item, its latest step graph, and its event timeline. |
| `factory worker` | Run the step executor in a worker sandbox. |
| `factory workers list` | The worker registry: name, kind, state, version, last seen, and what each holds. |

## `factory doctor`

Each check prints `pass`, `fail` or `skip`; a failure prints its fix. A `skip` does not apply here (the sandbox
runtime with the launcher off, a version that is not a release) and does not fail doctor. Doctor exits 1 when any
check fails, and its last line names every failing check.

Without `--worker` it checks the server named by `--server` or `FACTORY_SERVER`:

| Check | How |
| --- | --- |
| server reachable | `GET /healthz` answers. |
| token accepted | `GET /api/v1/admin/doctor` accepts `FACTORY_API_TOKEN`. |
| version compatible | The server's version and this binary's are the same release, or releases with the same major and minor version. A build that is not a release (`dev`, a bare commit) is skipped. |
| database reachable and migrated | Server side: the readiness check. Behind on migrations offers `factory migrate up`. |
| code host credential | Server side: the GitHub App authenticates and is installed somewhere (`GET /app/installations`). Fails with no App configured. |
| sandbox runtime reachable | Server side: the launcher lists its worker Jobs. Skipped with the launcher off. |
| cluster worker active | Server side: at least one cluster worker is `active`. With the launcher on, a worker starts only when an item is ready, so an idle factory fails this check. |

With `--worker`, run inside a worker image, it checks the machine instead: `git` runs, each agent CLI in
`--harnesses` is installed and logged in (its model credential; one not ready is skipped while another is, and
all fail when none is), and `--work-dir` is writable. `--work-dir` and `--harnesses` default to
`FACTORY_WORK_DIR` and `FACTORY_HARNESSES`, as `factory worker` reads them.

Not checked yet, and listed as such rather than passed: each adapter's secret (#7, #8), the stored sign-in and its
role (#15), and the telemetry exporter (#13). Each check lands with its feature.

On a terminal, a failed check whose fix is a factory command asks `Run '<command>' now? [Y/n]` and runs it on yes.
Only the database check has one today, `factory migrate up`, which reads `FACTORY_DATABASE_URL` on this machine.
A run that is not on a terminal never asks.

## Tests

`cmd/factory/testdata/` holds golden files: the help text of `factory`, every group, and every command, each read
command's table and JSON output against a fake server, and the output of each command's error paths. A test fails
when an error path's output names no next step. `make test` compares against them; after an intended change,
`make test/golden` rewrites them, and the diff is part of the pull request.
