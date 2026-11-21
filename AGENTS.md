# `AGENTS.md`

These instructions tell coding agents and contributors how to work in this repository. [`CLAUDE.md`](CLAUDE.md)
imports this file, so this is the one place to edit.

factory is the autobutler-org software factory. It takes work from any tracker, records it and its state in
Postgres, hands it to agent workers in our Kubernetes cluster, and shows what is happening. It is one Go binary,
`factory`, with subcommands. The plan is in the epic, [#1](https://github.com/autobutler-org/factory/issues/1).

## Layout

```text
cmd/factory/        the binary: parses arguments, dispatches subcommands, maps errors to exit codes
internal/           business logic, one package per concern; no package needs HTTP or CLI context
internal/db/        pgx pool, embedded golang-migrate migrations (migrations/), sqlc output (generated)
internal/db/dbtest/ NewDB(t): a Postgres schema per test with the real migrations applied
internal/work/      the work item service and state machine, step graphs and step leases; the only code that changes state
internal/lease/     lease protocol rules (policy, claimant check, retry or give up); no SQL
internal/plan/      the pure planner: work item to step graph, and which step may run next; no SQL
internal/api/       the HTTP API handlers and middleware (docs/api.md); no SQL, no state rules
internal/codehost/  the code host (GitHub): pull requests, checks, merge, App tokens; codehost.Fake and fakegh/ for tests (#10)
internal/sandbox/   starts, tracks, and destroys worker sandboxes (Kubernetes Jobs) and execs into them (#9)
internal/worker/    the step executor `factory worker` runs: protocol client, heartbeat, and each step kind (#11)
internal/harness/   drives agent CLIs (Claude Code, Grok) for a session; harness.Fake for tests
internal/repoconfig/ parses a repository's .factory/config.toml
internal/secretscan/ finds credential-shaped literals in the lines a branch adds
cmd/fakeagent/      a scripted stand-in for an agent CLI, for tests
internal/launcher/  the server's launcher: plans ready items, starts one sandbox per runnable chain, runs merge (#17)
sql/queries/        the SQL sqlc compiles into internal/db
compose.yaml        the development Postgres, on loopback
.factory/config.toml how the factory works on this repository: the gate is `make check`, a person approves and merges
Dockerfile          the server image (distroless, non-root); Dockerfile.worker is the agent dev container
deploy/chart/       the Helm chart; values-autobutler.example.yaml matches our cluster (docs/operations.md)
web/                the web UI, embedded in the binary (#14)
docs/               api.md, config.md, operations.md, decisions/ (numbered decision records), and architecture/ (work.md: states and rules; graph.md: the step graph and default template; workers.md: lease protocols, worker HTTP contract and the launcher; worker.md: what `factory worker` does per step kind; codehost.md: the code host and its tokens)
build/              build output (./build/factory); not source
.github/workflows/  ci.yml (build, race tests, test-db and e2e on Postgres), check.yml (make check, govulncheck, attribution,
                    chart), images.yml (builds both images; pushes them on v* tags), pr-title.yml, auto-assign-author.yml
```

`web/` does not exist yet. The issue that first needs each one creates it.

## Use Makefile targets (always)

Use the Makefile targets, not ad-hoc commands. If an action needs to be repeatable, add a target with a `## `
comment so `make help` lists it.

| Target           | What it does                                                                  |
| ---------------- | ----------------------------------------------------------------------------- |
| `make setup`     | downloads modules and builds the dev tools pinned in `go.mod`                 |
| `make build`     | builds `./build/factory` with version, commit, and date stamped in            |
| `make test`      | `go test ./...`                                                               |
| `make test/race` | the tests under the race detector, as CI runs them                            |
| `make test/e2e`  | the worker's end-to-end tests (build tag `e2e`) against Postgres              |
| `make check`     | `check/format`, `check/tidy`, `check/lint`, `check/sqlc`, `check/migrations`, then `test`; changes no files |
| `make format`    | `golangci-lint fmt` (gci, gofumpt, goimports); rewrites files                 |
| `make fix`       | applies lint autofixes, then formats                                          |
| `make tidy`      | `go mod tidy`                                                                 |
| `make check/chart` | `helm lint` and `helm template` of `deploy/chart` with default and example values |
| `make build/image`, `make build/image/worker` | the server and worker images, with the version stamped in (needs Docker) |

- The Go version is pinned once, on the `go` line of `go.mod`. The Makefile reads it into `GOTOOLCHAIN`.
- Dev tools (golangci-lint, gofumpt, gci, goimports, govulncheck, air) are `tool` directives in `go.mod` and
  run as `go tool <name>`. Do not install them globally or pin them anywhere else.
- sqlc is a `tool` directive too (`go tool sqlc`). It needs cgo, which the default macOS and Linux toolchains have.
- `.golangci.yml` enables only linters that have an autofix. Run `make format`, `make tidy`, and `make check`
  before pushing, and commit whatever the first two changed.

## Go conventions

- Respect `.golangci.yml` and the formatters it enables. Avoid `//nolint`; where one is unavoidable, name the
  linter and give a reason.
- Handlers and `main` only parse input, call a service function, and shape the output. Business logic lives in
  `internal/` packages that need no HTTP or CLI context and are tested without it.
- Each command's dispatch takes its arguments and output writers, parses flags with the standard `flag` package
  (no cobra, no viper), and returns an exit code: `0` success, `2` usage error, `1` runtime failure. Error text
  says what to do next, for example `Run 'factory --help' for usage.`
- Every package has a package doc comment that says what it does and what it does not do. When a package's
  behavior changes, its doc changes in the same pull request.
- Wrap errors with context: `fmt.Errorf("...: %w", err)`. Sentinel errors are `var Err... = errors.New(...)`
  and are matched with `errors.Is`.
- No new mutable package-level state. Prefer immutable values: `const` where possible, and values passed down
  rather than shared. The version variables in `cmd/factory` are `var` only because `-ldflags -X` needs it.
- Every subprocess has a timeout and is canceled through its context.
- Logs use `log/slog` with key-value pairs.

## Tests

Test-driven development is the default:

1. Write or update the test that pins the behavior.
2. Run it and watch it fail.
3. Write the least code that makes it pass.
4. Refactor with the tests still passing.

Tests sit beside the code they cover and use the standard `testing` package only. Use table-driven tests with
`t.Run` where a function has several input cases. Cover edge cases, error paths, and boundary values. A pure
refactor runs the existing tests before and after.

## Documentation

Architecture docs are updated in the same pull request as the code they describe, not in a follow-up. That
includes this file's layout section, `README.md`, and anything under `docs/`.

Write plainly, in American spelling.

## Pull requests and commits

- **Title:** a conventional commit in lowercase imperative mood, with the issue number as the scope:
  `feat(12): add claim lease`, not `12: Added claim lease`. The type is one of `feat`, `fix`, `docs`, `style`,
  `refactor`, `perf`, `test`, `build`, `ci`, `chore`, or `revert`. `pr-title.yml` fails any other title. The
  title becomes the commit subject on squash merge.
- **Body:** fill in the [PR template](.github/pull_request_template.md) and include `Closes #N`.
- **Branch:** `<issue number>_short-description`, for example `12_claim-lease`.
- **Commits:** signed off (`git commit -s`).
- **No AI attribution.** No AI co-author trailer, no generated-by footer, and no agent session link in a commit,
  pull request, comment, or issue. `check.yml` rejects a pull request that carries one.
- **How was this tested?** lists the commands actually run.

## Scope

- Build what the issue asks for. No flags, config keys, packages, or abstractions nobody requested.
- Advice is not a request for edits. When asked to review, explain, or plan, answer and touch no files.
- Never start, stop, or restart a server or worker yourself, and do not run `make watch`. Tests are fine to run.

## Database

- Postgres is the only store. Configure it with `FACTORY_DATABASE_URL` (or `--database-url`).
- Migrations are `internal/db/migrations/NNN_name.up.sql` and `.down.sql`, numbered from `001` with no gaps,
  embedded in the binary and applied with `factory migrate up`. Never edit a migration that has been merged;
  add a new one.
- Queries live in `sql/queries`. Run `make generate/sqlc` after changing a query or a migration and commit the
  generated code. No ORM.
- Database tests call `dbtest.NewDB(t)`. Under `make test` they skip; `make serve/postgres && make test/db` runs
  them, and CI's `test-db` job runs them against a Postgres service container.
