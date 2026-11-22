# `AGENTS.md`

These instructions tell coding agents and contributors how to work in this repository. [`CLAUDE.md`](CLAUDE.md)
imports this file, so this is the one place to edit.

factory is the autobutler-org software factory. It takes work from any tracker, records it and its state in
Postgres, hands it to agent workers in our Kubernetes cluster, and shows what is happening. It is one Go binary,
`factory`, with subcommands. The plan is in the epic, [#1](https://github.com/autobutler-org/factory/issues/1).

## Layout

```text
cmd/factory/        the binary: the command table (commands.go) that dispatch and all usage text come from, each
                    subcommand, exit codes; testdata/ holds the golden help and output files (docs/cli.md)
internal/           business logic, one package per concern; no package needs HTTP or CLI context
internal/db/        pgx pool, embedded golang-migrate migrations (migrations/), sqlc output (generated)
internal/db/dbtest/ NewDB(t): a Postgres schema per test with the real migrations applied
internal/work/      the work item service and state machine, step graphs, step leases and gate outputs; the only code that changes state
internal/lease/     lease protocol rules (policy, claimant check, retry or give up); no SQL
internal/plan/      the pure planner: work item to step graph, and which step may run next; no SQL
internal/api/       the HTTP API handlers and middleware (docs/api.md); no SQL, no state rules
internal/codehost/  the code host (GitHub): pull requests, checks, merge, App tokens; codehost.Fake and fakegh/ for tests (#10)
internal/sandbox/   starts, tracks, and destroys worker sandboxes (Kubernetes Jobs), execs into them (#9), reads their logs (#50), verifies their pods' tokens (#15)
internal/doctor/    the checks `factory doctor` reports (server side, worker image, version); pass, fail or skip with a fix
internal/apiclient/ the HTTP client of the factory API that the worker and the CLI share; errors as *apiclient.Error
internal/worker/    the step executor `factory worker` runs: protocol client, heartbeat, and each step kind (#11)
internal/harness/   drives agent CLIs (Claude Code, Grok) for a session; harness.Fake for tests
internal/repoconfig/ parses a repository's .factory/config.toml
internal/secretscan/ finds credential-shaped literals in the lines a branch adds
cmd/fakeagent/      a scripted stand-in for an agent CLI, for tests
internal/launcher/  the server's launcher: polls the code host to complete wait steps (#55), plans ready items, starts one sandbox per runnable chain, runs merge (#17)
internal/server/    serves the embedded web UI from public/ (make build/web output; only stub.txt is committed)
sql/queries/        the SQL sqlc compiles into internal/db
compose.yaml        the development Postgres, on loopback
.factory/config.toml how the factory works on this repository: the gate is `make check`, a person approves and merges
Dockerfile          the server image (distroless, non-root); Dockerfile.worker is the agent dev container
deploy/chart/       the Helm chart; values-autobutler.yaml matches our cluster (docs/operations.md)
web/                the web UI (React, Vite, TypeScript; Vitest, ESLint, Prettier), built into internal/server/public (#14)
docs/               api.md, cli.md (CLI conventions and commands), config.md, operations.md, decisions/ (numbered decision records), and architecture/ (work.md: states and rules; graph.md: the step graph and default template; workers.md: lease protocols, worker HTTP contract and the launcher; worker.md: what `factory worker` does per step kind; codehost.md: the code host and its tokens; credentials.md: every credential, who holds it, how it rotates)
build/              build output (./build/factory); not source
.claude/skills/     interface design skills vendored from schmeide/template (MIT; LICENSE and UPSTREAM_COMMIT inside)
.github/workflows/  ci.yml (build, race tests, test-db and e2e on Postgres, web), check.yml (make check/go,
                    govulncheck, attribution, chart), images.yml (builds both images; pushes them on v* tags, then
                    calls deploy.yml), deploy.yml (upgrades our cluster to a tag), pr-title.yml, auto-assign-author.yml;
                    path-filters.yml says which paths each job reads, so Go-only changes skip the web job and the reverse
```

## Use Makefile targets (always)

Use the Makefile targets, not ad-hoc commands. If an action needs to be repeatable, add a target with a `## `
comment so `make help` lists it.

| Target           | What it does                                                                  |
| ---------------- | ----------------------------------------------------------------------------- |
| `make setup`     | downloads modules and builds the dev tools pinned in `go.mod`                 |
| `make build`     | `build/web`, then `build/go`: one `./build/factory` serving the UI and the API, with version, commit, and date stamped in |
| `make build/web` | type-checks and builds `web/` into `internal/server/public`, which the binary embeds |
| `make build/go`  | the Go binary alone; without a web build it serves a page that says to run `make build/web` |
| `make serve/web` | the Vite dev server, proxying `/api` to a `factory serve` you started (`FACTORY_URL`, default `http://127.0.0.1:8080`) |
| `make test/web`  | the web UI's component tests (Vitest, Testing Library)                        |
| `make test`      | `go test ./...`                                                               |
| `make test/race` | the tests under the race detector, as CI runs them                            |
| `make test/e2e`  | the worker's end-to-end tests (build tag `e2e`) against Postgres              |
| `make test/contained` | the session-boundary tests in the worker image, as uid 1000 with no capabilities (needs Docker) |
| `make test/golden` | rewrites the CLI's golden files in `cmd/factory/testdata` from current output |
| `make check`     | `check/go` (`check/format`, `check/tidy`, `check/lint`, `check/sqlc`, `check/migrations`, `test`), `check/web` (ESLint, `tsc`, Prettier), then `test/web`; changes no files |
| `make format`    | `golangci-lint fmt` (gci, gofumpt, goimports); rewrites files                 |
| `make fix`       | applies lint autofixes, then formats                                          |
| `make fix/web`   | ESLint autofixes and Prettier on `web/`; rewrites files                       |
| `make tidy`      | `go mod tidy`                                                                 |
| `make check/chart` | `helm lint` and `helm template` of `deploy/chart` with default and example values |
| `make build/image`, `make build/image/worker` | the server and worker images, with the version stamped in (needs Docker) |

- The Go version is pinned once, on the `go` line of `go.mod`. The Makefile reads it into `GOTOOLCHAIN`.
- Dev tools (golangci-lint, gofumpt, gci, goimports, govulncheck, air) are `tool` directives in `go.mod` and
  run as `go tool <name>`. Do not install them globally or pin them anywhere else.
- Node is pinned once, in `web/.nvmrc`, and matches `NODE_VERSION` in `Dockerfile.worker` and the `node` image in
  `Dockerfile`. The Makefile finds nvm's copy of that version even when nvm is not loaded. npm dependencies are in
  `web/package-lock.json`; the web targets install them with `npm ci`.
- sqlc is a `tool` directive too (`go tool sqlc`). It needs cgo, which the default macOS and Linux toolchains have.
- `.golangci.yml` enables only linters that have an autofix. Run `make format`, `make tidy`, and `make check`
  before pushing, and commit whatever the first two changed.

## Go conventions

- Respect `.golangci.yml` and the formatters it enables. Avoid `//nolint`; where one is unavoidable, name the
  linter and give a reason.
- Handlers and `main` only parse input, call a service function, and shape the output. Business logic lives in
  `internal/` packages that need no HTTP or CLI context and are tested without it.
- Each command is an entry in the command table in `cmd/factory/commands.go`: it defines its flags, with their help
  text, on a standard `flag` package FlagSet (no cobra, no viper) and returns an exit code: `0` success, `2` usage
  error, `1` runtime failure. Usage text is generated from the table; never write it by hand. Error text says what
  to do next, for example `Run 'factory --help' for usage.` [docs/cli.md](docs/cli.md) has the conventions.
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

## Web UI

The interface skills in `.claude/skills/` are vendored unchanged from
[schmeide/template](https://github.com/schmeide/template); `UPSTREAM_COMMIT` names the commit. To update them, copy the
directory again and change that file; do not edit a skill in place. An agent that does not load them as skills reads a
skill's `SKILL.md` and the files it links, and follows its procedure and report format.

- **`interface-review`** runs on every pull request that touches `web/`. Resolve what it reports, or say in the pull
  request why a finding was left.
- **`better-interface`** reviews the whole UI or one screen and ranks the findings. Run it before a redesign.
- **`better-accessibility`**, **`better-layout`**, **`better-colors`**, **`better-typography`**, **`better-ui`**, and
  **`better-writing`** are the area skills `better-interface` routes to; use one directly when fixing that area.
- **`state-machine`** and **`break`** enumerate and stress a component's states (loading, empty, error, 401, long
  text, many items). Run them on a new component with more than a couple of states, and add a test for each state
  they turn up.
- `variant`, `explain-interface`, `build-design`, and `afk` are vendored with the rest but not part of the workflow.

Patterns new UI in `web/` reuses rather than rebuilds:

- **Pages** call `useTitle` (`web/src/nav.tsx`) and give their `<h1>` `tabIndex={-1}`; `App` moves focus to it
  after a client-side navigation.
- **Polled data** comes from `usePoll` (`web/src/poll.ts`), and the page passes its result to `<Status {...poll} />`
  (`web/src/format.tsx`). Status owns the loading, updated, paused, failed-load and failed-refresh copy; only the
  error is in its live region, so the poll does not announce itself. A 404 stops the poll and is the page's to explain.
- **Color** comes only from the semantic tokens at the top of `web/src/style.css` (`--color-{bg|text|border}-…`,
  `--color-accent-text`, `--color-focus-ring`, `--color-{danger|warning|success|progress}-text`), each defined for
  light and dark, with its measured contrast in the comment above them. A new role gets a new token, measured in
  both themes; never borrow one for its value. Control edges use `--color-border-control` (3:1), not `-subtle`.
- **State** renders through `<State state=...>`, which writes the state out and colors it by `stateRole`
  (`web/src/format.tsx`): danger, warning, success, progress or neutral. A new state goes in that map and its test.
- **Type**: system fonts only; `code` and `pre` use `--font-mono`; counts and times use `tabular-nums`.
- **Words**: states and event types go through `label` (`web/src/format.tsx`), which spells them as words; headings
  set their case in CSS. An empty list says what is missing and, where there is one, how to fill it or the way out
  (a filtered board offers **Clear filters**). A list that can be long scrolls inside its own box with the next item
  peeking, so what follows it stays in view.
- **Preformatted text** that can be long (reasons, logs, JSON) goes in `<Block label=...>`, a named scrolling
  region that is a tab stop only while its text overflows, so a keyboard can scroll it.
- **Following a log** (`web/src/Logs.tsx`) polls through `usePoll` with the API's cursor, keeps a bounded window
  (`MAX_LINES`), and pauses through `ApiContext`, so the global Pause updates also pauses it. It keeps the reader's
  scroll position unless they are at the end.

## Database

- Postgres is the only store. Configure it with `FACTORY_DATABASE_URL` (or `--database-url`).
- Migrations are `internal/db/migrations/NNN_name.up.sql` and `.down.sql`, numbered from `001` with no gaps,
  embedded in the binary and applied with `factory migrate up`. Never edit a migration that has been merged;
  add a new one.
- Queries live in `sql/queries`. Run `make generate/sqlc` after changing a query or a migration and commit the
  generated code. No ORM.
- Database tests call `dbtest.NewDB(t)`. Under `make test` they skip; `make serve/postgres && make test/db` runs
  them, and CI's `test-db` job runs them against a Postgres service container.
