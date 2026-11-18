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
| none | `FACTORY_API_TOKEN` | unset | Bearer token required on every `/api/v1` route. Environment only, so it does not show up in process listings. |

When `FACTORY_API_TOKEN` is unset the API is unauthenticated. The server then refuses any `--addr` that is not a
loopback address, and logs a warning at startup that it is bound to loopback only. This is a stopgap: one
shared token and no caller identities. Sign-in, roles and scoped tokens arrive with #15.

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
