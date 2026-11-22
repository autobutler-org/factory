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
| `--github-api-url` | `FACTORY_GITHUB_API_URL` | `https://api.github.com` | GitHub REST API base URL; for GitHub Enterprise, `https://<host>/api/v3`. Off github.com workers commit as `factory`, not the App's bot ([commit identity](architecture/codehost.md#commit-identity)). |
| none | `FACTORY_API_TOKEN` | unset | Bearer token required on every `/api/v1` route. Environment only, so it does not show up in process listings. With `--launcher` it cannot act as a worker ([workload identity](architecture/workers.md#workload-identity)). |
| `--launcher` | `FACTORY_LAUNCHER` | `false` | Run the launcher: start worker sandboxes as Kubernetes Jobs and run server-side steps (`merge`). Needs in-cluster config. |
| `--launcher-namespace` | `FACTORY_LAUNCHER_NAMESPACE` | `factory-workers` | Namespace the worker Jobs run in. |
| `--launcher-image` | `FACTORY_LAUNCHER_IMAGE` | none, required with `--launcher` | Default worker image, for every chain whose repository names no `image`. |
| `--launcher-allowed-images` | `FACTORY_LAUNCHER_ALLOWED_IMAGES` | unset | Comma-separated image repository prefixes a repository's `image` may come from, such as `ghcr.io/autobutler-org/`. A prefix ending in `/` allows every repository under it; one without allows that one repository. Unset lets no repository choose its image. Read when planning too, so it applies with `--launcher` off. |
| `--launcher-server-url` | `FACTORY_LAUNCHER_SERVER_URL` | none, required with `--launcher` | URL workers reach this server at, such as `http://factory.factory.svc.cluster.local:80`. |
| `--launcher-max-sandboxes` | `FACTORY_LAUNCHER_MAX_SANDBOXES` | `4` | Most worker sandboxes alive at once. Keep the worker namespace's ResourceQuota in step. |
| `--launcher-pending-timeout` | `FACTORY_LAUNCHER_PENDING_TIMEOUT` | `10m` | Destroy a worker sandbox still pending after this long (an image that will not pull, a pod that cannot be scheduled), with a logged reason, so it stops holding a slot; its chain gets a new one. `0` never does. |
| `--launcher-poll-interval` | `FACTORY_LAUNCHER_POLL_INTERVAL` | `1m` | How often the launcher reads each open pull request of the items being worked from GitHub, to complete wait steps ([workers.md](architecture/workers.md#polling-the-code-host)). One GraphQL request per pull request per poll; at least `10s`, the launcher's pass. Needs a GitHub App. |
| `--launcher-capabilities` | `FACTORY_LAUNCHER_CAPABILITIES` | `claude-code` | Comma-separated capabilities the worker image registers. Items that require anything else are neither planned nor given a sandbox. |
| `--launcher-service-account` | `FACTORY_LAUNCHER_SERVICE_ACCOUNT` | `factory-worker` | ServiceAccount of worker pods. A worker registers with a projected token of this ServiceAccount; the server accepts no other. |
| `--launcher-node-selector` | `FACTORY_LAUNCHER_NODE_SELECTOR` | unset | `key=value,...` node selector for worker pods. |
| `--launcher-tolerations` | `FACTORY_LAUNCHER_TOLERATIONS` | unset | Tolerations for worker pods: `key=value:Effect` (operator Equal) or `key:Effect` (operator Exists), comma-separated. |
| `--launcher-requests` | `FACTORY_LAUNCHER_REQUESTS` | `cpu=1,memory=2Gi,ephemeral-storage=10Gi` | Worker container requests. Only `cpu`, `memory` and `ephemeral-storage`. |
| `--launcher-limits` | `FACTORY_LAUNCHER_LIMITS` | `cpu=4,memory=8Gi,ephemeral-storage=20Gi` | Worker container limits. |
| `--launcher-model-key-secret` | `FACTORY_LAUNCHER_MODEL_KEY_SECRET` | `factory-model-api-key` | Secret in the worker namespace whose key `api-key` workers get as `ANTHROPIC_API_KEY`. |
| `--launcher-runtime-class` | `FACTORY_LAUNCHER_RUNTIME_CLASS` | unset | RuntimeClass of worker pods, such as `gvisor`. |

When `FACTORY_API_TOKEN` is unset the API is unauthenticated. The server then refuses any `--addr` that is not a
loopback address, and logs a warning at startup that it is bound to loopback only. For people this is a stopgap:
one shared token and no caller identities; sign-in, roles and scoped tokens arrive with the rest of #15. Workers the
launcher starts already authenticate as their pods, verified with a TokenReview, which needs the server's
ServiceAccount bound to the ClusterRole `system:auth-delegator` (the chart does it). `--launcher-api-token-secret` is
gone: workers get no shared token. Every credential is listed in
[architecture/credentials.md](architecture/credentials.md).

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

Fixed limits: request bodies are capped at 1 MiB (a worker's gate output upload at 5 MiB), headers must arrive within 10 seconds, a request must be read
within 30 seconds and answered within 60, and shutdown waits up to 10 seconds for requests in flight.

## `factory migrate`

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--database-url` | `FACTORY_DATABASE_URL` | none, required | Postgres URL. |

## `factory worker`

Runs inside a worker sandbox; see [architecture/worker.md](architecture/worker.md). The Job sets `FACTORY_URL`,
`FACTORY_SANDBOX`, `FACTORY_TOKEN_FILE` and `FACTORY_CONTAINED`.

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--url` | `FACTORY_URL` | none, required | The factory server, for example `http://factory:8080`. |
| `--token-file` | `FACTORY_TOKEN_FILE` | unset | The pod's projected ServiceAccount token, sent once to register; the worker uses the credential the server answers with from then on. |
| none | `FACTORY_API_TOKEN` | unset | Bearer token when `--token-file` is not set: for a worker outside Kubernetes against a server without `--launcher`. |
| `--contained` | `FACTORY_CONTAINED` | `false` | The worker has a container of its own, as in a sandbox Job (which sets it): refuse to start unless a child process cannot read the worker's memory, and kill every other process of the user before a git command gets a code host token ([the session boundary](architecture/worker.md#the-session-boundary)). Linux only. Never set it on a shared machine. |
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
| `review_rounds` | `3` | Review rounds to plan; `0` skips review. |
| `require_approval` | `false` | Wait for a human approval before merge. |
| `after` | `[]` | Repositories of the same item whose chains finish first. |
| `image` | the server's `--launcher-image` | The dev container image this repository's sandboxes run in: a full reference with registry host and a tag or digest, such as `ghcr.io/autobutler-org/quark-worker:1.4.0` or `...@sha256:<digest>`. It must be under a prefix in the server's `--launcher-allowed-images`. See [operations.md](operations.md#giving-a-repository-its-own-tools). |
| `[automate] merge` | `"on"` | `"off"` leaves the pull request for a person. |

Unknown keys are an error. Two readers use the file. The worker reads `gate`, `gate_timeout` and `session_timeout`
from its checkout of the base branch, and a missing file or `gate` fails the item's `workspace` step for good. The
server reads the whole file when it plans an item, through the code host at each repository's default branch with an
installation token for that repository, and plans with `review_rounds`, `require_approval`, `after`,
`[automate] merge` and `image`:

- A missing or invalid file, or an `image` that is malformed or not allowed, moves the item to `blocked` with a
  reason that names the repository and the problem. Fix the file, then move the item back to `ready`.
- When the code host cannot be read (an outage, a token that cannot be minted), the item stays `ready`, a
  `plan_deferred` event records why, and it is tried again after `--retry-delay`. Other items are planned meanwhile.
- Without a GitHub App the server cannot read the file and plans every repository with the defaults above; it logs a
  warning at startup.

The `planned` event records, per repository, the default branch, the commit the file was read at, and the chosen
image. The config is part of the graph's `inputs_hash`. A change to the file after an item was planned does not
reach that item until it is replanned (`POST /api/v1/work/{id}/replan`), which reads it again.
