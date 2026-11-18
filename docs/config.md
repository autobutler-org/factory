# Configuration

Every flag can also be set with an environment variable named `FACTORY_` plus the flag name in upper case, with
dashes turned into underscores: `--database-url` is `FACTORY_DATABASE_URL`. A flag given on the command line
wins over the environment.

## `factory serve`

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--addr` | `FACTORY_ADDR` | `127.0.0.1:8080` | Address the HTTP API listens on. |
| `--database-url` | `FACTORY_DATABASE_URL` | none, required | Postgres URL, for example `postgres://factory:factory@127.0.0.1:5432/factory?sslmode=disable`. |
| `--lease` | `FACTORY_LEASE` | `5m` | How long a claim lasts without a heartbeat. A worker silent this long is marked gone. |
| `--stuck-after` | `FACTORY_STUCK_AFTER` | `3h` | A claim with no stage report for this long is revoked and counted as an attempt. |
| `--max-attempts` | `FACTORY_MAX_ATTEMPTS` | `3` | Failed attempts before a step (and so its item), or a locally claimed item, is `failed`. |
| `--retry-delay` | `FACTORY_RETRY_DELAY` | `1m` | How long a retried step or item waits before it is handed out again. |
| `--github-app-id` | `FACTORY_GITHUB_APP_ID` | unset | GitHub App that mints workers' repository tokens. Set it together with `--github-app-key-file`, or leave both unset. |
| `--github-app-key-file` | `FACTORY_GITHUB_APP_KEY_FILE` | unset | Path to the App's PEM private key. Read once at startup and never logged. |
| `--github-api-url` | `FACTORY_GITHUB_API_URL` | `https://api.github.com` | GitHub REST API base URL; for GitHub Enterprise, `https://<host>/api/v3`. |
| none | `FACTORY_API_TOKEN` | unset | Bearer token required on every `/api/v1` route. Environment only, so it does not show up in process listings. |
| `--launcher` | `FACTORY_LAUNCHER` | `false` | Run the launcher: start worker sandboxes as Kubernetes Jobs and run server-side steps (`merge`). Needs in-cluster config. |
| `--launcher-namespace` | `FACTORY_LAUNCHER_NAMESPACE` | `factory-workers` | Namespace the worker Jobs run in. |
| `--launcher-image` | `FACTORY_LAUNCHER_IMAGE` | none, required with `--launcher` | Worker image. |
| `--launcher-server-url` | `FACTORY_LAUNCHER_SERVER_URL` | none, required with `--launcher` | URL workers reach this server at, such as `http://factory.factory.svc.cluster.local:80`. |
| `--launcher-max-sandboxes` | `FACTORY_LAUNCHER_MAX_SANDBOXES` | `4` | Most worker sandboxes alive at once. Keep the worker namespace's ResourceQuota in step. |
| `--launcher-pending-timeout` | `FACTORY_LAUNCHER_PENDING_TIMEOUT` | `10m` | Destroy a worker sandbox still pending after this long (an image that will not pull, a pod that cannot be scheduled), with a logged reason, so it stops holding a slot; its chain gets a new one. `0` never does. |
| `--launcher-capabilities` | `FACTORY_LAUNCHER_CAPABILITIES` | `claude-code` | Comma-separated capabilities the worker image registers. Items that require anything else are neither planned nor given a sandbox. |
| `--launcher-service-account` | `FACTORY_LAUNCHER_SERVICE_ACCOUNT` | `factory-worker` | ServiceAccount of worker pods. |
| `--launcher-node-selector` | `FACTORY_LAUNCHER_NODE_SELECTOR` | unset | `key=value,...` node selector for worker pods. |
| `--launcher-tolerations` | `FACTORY_LAUNCHER_TOLERATIONS` | unset | Tolerations for worker pods: `key=value:Effect` (operator Equal) or `key:Effect` (operator Exists), comma-separated. |
| `--launcher-requests` | `FACTORY_LAUNCHER_REQUESTS` | `cpu=1,memory=2Gi,ephemeral-storage=10Gi` | Worker container requests. Only `cpu`, `memory` and `ephemeral-storage`. |
| `--launcher-limits` | `FACTORY_LAUNCHER_LIMITS` | `cpu=4,memory=8Gi,ephemeral-storage=20Gi` | Worker container limits. |
| `--launcher-api-token-secret` | `FACTORY_LAUNCHER_API_TOKEN_SECRET` | `factory-worker-api-token` | Secret in the worker namespace whose key `token` workers get as `FACTORY_API_TOKEN`. |
| `--launcher-model-key-secret` | `FACTORY_LAUNCHER_MODEL_KEY_SECRET` | `factory-model-api-key` | Secret in the worker namespace whose key `api-key` workers get as `ANTHROPIC_API_KEY`. |
| `--launcher-runtime-class` | `FACTORY_LAUNCHER_RUNTIME_CLASS` | unset | RuntimeClass of worker pods, such as `gvisor`. |

When `FACTORY_API_TOKEN` is unset the API is unauthenticated. The server then refuses any `--addr` that is not a
loopback address, and logs a warning at startup that it is bound to loopback only. This is a stopgap: one
shared token and no caller identities. Sign-in, roles and scoped tokens arrive with #15.

Without a GitHub App, `POST /api/v1/work/{id}/codehost-token` answers `503 codehost_not_configured`. The token flow
is in [architecture/codehost.md](architecture/codehost.md#tokens).

With `--launcher` off (the default) the server starts no sandboxes and runs no steps itself; it behaves as it did
before the launcher existed. With it on, the server needs a ServiceAccount allowed to manage Jobs in the worker
namespace, and server-side steps run only when a GitHub App is configured; without one the server logs a warning at
startup and `merge` steps stay `ready`. The launcher and what a worker receives are described in
[architecture/workers.md](architecture/workers.md#the-launcher); deploying it is [operations.md](operations.md).

Durations use Go's syntax: `90s`, `5m`, `3h`. The lease settings are explained in
[architecture/workers.md](architecture/workers.md).

`factory serve` refuses to start against a database that is behind its migrations and says to run
`factory migrate up`.

Fixed limits: request bodies are capped at 1 MiB, headers must arrive within 10 seconds, a request must be read
within 30 seconds and answered within 60, and shutdown waits up to 10 seconds for requests in flight.

## `factory migrate`

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--database-url` | `FACTORY_DATABASE_URL` | none, required | Postgres URL. |

## `factory worker`

Runs inside a worker sandbox; see [architecture/worker.md](architecture/worker.md). The Job sets `FACTORY_URL`,
`FACTORY_SANDBOX` and `FACTORY_TOKEN_FILE`.

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--url` | `FACTORY_URL` | none, required | The factory server, for example `http://factory:8080`. |
| `--token-file` | `FACTORY_TOKEN_FILE` | unset | File holding the bearer token, read on every request. |
| none | `FACTORY_API_TOKEN` | unset | Bearer token when `--token-file` is not set. |
| `--sandbox` | `FACTORY_SANDBOX` | the host name | The worker's name: the sandbox it runs in. The same name registers as the same worker. |
| `--sandbox-key` | `FACTORY_SANDBOX_KEY` | unset | Claim only this chain's steps (`<item id>/<owner>/<repo>`). |
| `--work-dir` | `FACTORY_WORK_DIR` | `$TMPDIR/factory-worker` | Where checkouts live, one per sandbox key. |
| `--harnesses` | `FACTORY_HARNESSES` | `claude-code,grok` | Agent CLIs to use, in order. Those not installed or not logged in are skipped; none left is an error. |
| `--git-url` | `FACTORY_GIT_URL` | `https://github.com` | Base of clone URLs: `<git-url>/<owner>/<repo>.git`. |
| `--github-api-url` | `FACTORY_GITHUB_API_URL` | `https://api.github.com` | GitHub REST API base URL. |

The model API key (`ANTHROPIC_API_KEY`, `XAI_API_KEY`) is passed to agent sessions only. Fixed limits: one git command
10 minutes, a session silent for 15 minutes is killed, one API call 30 seconds, the release on shutdown 10 seconds.

### `.factory/config.toml`

Each repository the worker changes keeps its settings on its default branch:

| Key | Default | Meaning |
| --- | --- | --- |
| `gate` | none, required | Shell command a change must pass, run in the checkout. |
| `gate_timeout` | `30m` | How long the gate may run. |
| `session_timeout` | `2h` | How long one agent session may run. |
| `review_rounds` | `3` | Review rounds to plan; `0` skips review. Parsed, not yet read by the planner. |
| `require_approval` | `false` | Wait for a human approval before merge. Parsed, not yet read by the planner. |
| `after` | `[]` | Repositories of the same item whose chains finish first. Parsed, not yet read by the planner. |
| `[automate] merge` | `"on"` | `"off"` leaves the pull request for a person. Parsed, not yet read by the planner. |

Unknown keys are an error, and a missing file or `gate` fails the item's `workspace` step for good.
