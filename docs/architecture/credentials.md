# Credentials

Every credential factory uses, who holds it, how long it lives and how it is rotated. Secret names are the chart
defaults ([operations.md](../operations.md#secrets)). The design of the worker rows is
[decision 0002](../decisions/0002-worker-credentials.md).

## The table

| Credential | Held by | Lives | Rotated by | Used for |
| --- | --- | --- | --- | --- |
| Shared API token, `FACTORY_API_TOKEN` | The server (Secret `factory/factory-api-token`); people, the CLI and the web UI | Until rotated | Updating the Secret and restarting the server | Every `/api/v1` route, except acting as a worker while the launcher is on. **Pending #15:** replaced by OIDC sign-in for people and scoped tokens for scripts. |
| Worker pod token (projected ServiceAccount token, audience `factory`) | The kubelet, in the worker pod at `/var/run/factory/token` | One hour; the kubelet replaces it before then | The kubelet | Registering the pod's worker, once. The server verifies it with a TokenReview; a second registration from the same pod is refused, so the file is worthless afterwards. Not accepted by the Kubernetes API (wrong audience). |
| Worker credential (`fwc_...`) | The worker process's memory; the server keeps its SHA-256 in `worker_credentials` | The pod's life: until a new pod of the same sandbox registers, or the worker is marked `gone` | Not rotated; every pod gets a new one | Heartbeat, claim, step reports and code host tokens, for its own worker and sandbox key only ([workload identity](workers.md#workload-identity)). |
| GitHub App private key | The server (Secret `factory/factory-github-app`), read once at startup | Until revoked in the App's settings | Generating a new key in GitHub, updating the Secret, restarting the server, then deleting the old key | Signing App JWTs. Never leaves the server. |
| App JWT | The server's memory | 9 minutes | Signed per mint | Minting installation tokens, reading the App's bot identity. |
| Installation token (`ghs_...`) for one repository | The worker's memory and the environment of each git command that talks to the remote; the server for its own reads and merges | One hour | The worker asks again 5 minutes before expiry (`codehost.RefreshingToken`) | git fetch and push, the code host API ([codehost.md](codehost.md#tokens)). Minted only for a worker credential whose live claim covers the repository. |
| Model API key, `ANTHROPIC_API_KEY` | Secret `factory-workers/factory-model-api-key`; the worker's environment; the agent session's environment | Until rotated | Updating the Secret; Jobs started after that get the new key | The agent session's calls to the model API. Not given to the gate or git. The session can read it by design. |
| Database URL | Secret `factory/factory-database`; the server and its migrate init container | Until rotated | Updating the Secret and the database role, then restarting the server | Postgres. |
| In-cluster Postgres password | Secret `factory/factory-postgres` (only with `postgres.enabled`) | Until rotated | [operations.md](../operations.md#in-cluster-postgres) | The in-cluster Postgres. |
| The server's Kubernetes ServiceAccount token | The kubelet, in the server pod (mounted only with the launcher on) | One hour; the kubelet replaces it | The kubelet | Creating and deleting worker Jobs, reading their pods and logs, exec, and TokenReviews (`system:auth-delegator`). |
| Tracker webhook secrets | **Pending** #7, #8 | | | Verifying `/webhooks/{source}`. |
| People's sign-in sessions and CLI tokens | **Pending** #15 | | | OIDC authorization code flow (UI), device flow (`factory login`). |
| Script API tokens | **Pending** #15 | | | Scoped, revocable tokens for machine clients that enqueue work. |
| Metrics bearer token | **Pending** #13 | | | Optional, for `/metrics`. |

## What a worker sandbox holds

A worker pod holds the projected ServiceAccount token file, the model API key in the worker's environment, and,
after registering, its worker credential in the worker's memory. It has no Kubernetes API token
(`automountServiceAccountToken: false`, and the projected token's audience is not the API server's) and no shared
API token. Code host tokens exist only in the worker's memory and in the git commands it runs.

An agent session and the gate run in the same container, as the same user, as children of the worker. Which of
these they can reach, and what keeps them from the rest, is in [worker.md](worker.md#what-a-session-may-do).
