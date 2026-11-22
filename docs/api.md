# HTTP API

`factory serve` serves JSON over HTTP. Routes under `/api/v1` need `Authorization: Bearer <token>` when
`FACTORY_API_TOKEN` is set (see [config.md](config.md)). With the launcher on, cluster workers send their own
credential instead and reach only the worker protocol
([architecture/workers.md](architecture/workers.md#workload-identity)). Every other path outside `/healthz` and `/readyz` serves the
web UI without the token: a file of the build when the path names one, otherwise `index.html`, so the UI's own routes
(`/work/{id}`, `/workers`) load. The UI asks for the token and sends it on its API calls. Every response carries an `X-Request-ID` header; send one
to have yours used instead.

## Health

| Route | Answer |
| --- | --- |
| `GET /healthz` | `200 {"status":"ok"}` while the process is up. |
| `GET /readyz` | `200 {"status":"ready"}` when the database answers and is fully migrated; `503` with code `not_ready` and the reason otherwise. |

## Work

| Route | Body | Answer |
| --- | --- | --- |
| `POST /api/v1/work` | an item, see below | `201` with a new item, `200` with an existing one |
| `GET /api/v1/work?state=&source=&repo=&limit=&cursor=` | | `200 {"items": [...], "next_cursor": "..."}`, by priority (highest first) then oldest first; `next_cursor` is empty on the last page; `limit` is at most 200 |
| `GET /api/v1/work/{id}` | | `200` item |
| `PATCH /api/v1/work/{id}` | `{"title"?, "body"?, "priority"?}` | `200` item |
| `POST /api/v1/work/{id}/transitions` | `{"to": "ready", "reason"?: "..."}` | `200` item |
| `GET /api/v1/work/{id}/events` | | `200 {"events": [...]}`, oldest first |
| `GET /api/v1/work/{id}/outputs` | | `200 {"outputs": [...]}` |
| `PUT /api/v1/work/{id}/dependencies/{blocked_by_id}` | | `204` |
| `DELETE /api/v1/work/{id}/dependencies/{blocked_by_id}` | | `204` |
| `GET /api/v1/work/{id}/graph` | | `200 {"graph", "steps", "edges"}`, the item's latest step graph; `404` before it is planned |
| `GET /api/v1/work/{id}/sandboxes` | | `200 {"sandboxes": [...]}`, the item's worker sandboxes that still exist, oldest first; see [Sandbox logs](#sandbox-logs) |
| `GET /api/v1/work/{id}/sandboxes/{name}/logs?tail=` or `?after=` | | `200` a read of the sandbox's worker log; see [Sandbox logs](#sandbox-logs) |
| `POST /api/v1/work/{id}/replan` | | `200` with the new graph, planned with each repository's `.factory/config.toml` read again; `409 not_replannable` unless the item is `in_progress` or `in_review`; `422 invalid_request` when a config is missing or unusable; `503 config_unavailable` when the code host cannot be read |

`GET /api/v1/work` also filters on `claimed_by=<worker id>`. States and transition rules are in
[architecture/work.md](architecture/work.md); the step graph, its states and the default template are in
[architecture/graph.md](architecture/graph.md).

## Workers

`GET /api/v1/workers` lists the workers by name, each with `holds`: what it holds now, an item claimed whole
(`{"work_item_id", "title"}`) or a running step (`{"work_item_id", "title", "step_id", "step_key"}`).

`GET /api/v1/status` is the factory at a glance:
`{"counts": {<every state>: n}, "workers": {"active": n, ...}, "oldest_ready": item | null, "recent_failures": [items]}`.
`oldest_ready` is the item that has waited in `ready` longest; `recent_failures` are the five latest `failed`
items, newest first. `factory status` prints it.

`GET /api/v1/admin/doctor` answers `200 {"version": "...", "checks": [{"name", "status", "detail"?, "fix"?, "command"?}]}`
with the server's version and its own checks (database, code host credential, sandbox runtime, an active cluster
worker), each `pass`, `fail` or `skip`. A failing check is still a `200`. `factory doctor` reads it; the checks are in
[cli.md](cli.md#factory-doctor).

Cluster workers claim steps (`POST /api/v1/workers/{id}/claim`) and report on them under
`/api/v1/steps/{id}/` (`stage`, `result`, `release`); adapters complete wait steps with
`POST /api/v1/steps/{id}/complete`. Developers claim whole items under `/api/v1/work/{id}/` (`claim`, `stage`,
`result`, `release`, `outputs`). Either kind of claimant gets code host tokens, and the identity to commit as, from
`POST /api/v1/work/{id}/codehost-token`. All of it is specified in [architecture/workers.md](architecture/workers.md), with
the error codes workers handle (`not_claimant`, `lease_expired`, `worker_not_active`, `not_waiting`).

## Sandbox logs

Read-only views of the worker sandboxes the launcher ran for an item, for the web UI. They need the launcher
(`factory serve --launcher`); without it both answer `503 sandboxes_not_configured`.

**What this exposes, and to whom.** A worker's log carries the agent transcript: the model's text, the commands it
ran and their output, and the contents of files it read. The worker never logs its own tokens, but a session can
print anything it can read in the sandbox (#48). Until #15 these routes are open to every holder of the shared API
token, the same as the rest of `/api/v1`, and the server logs each read in its access log.

### List an item's sandboxes: `GET /api/v1/work/{id}/sandboxes`

```json
{"sandboxes": [
  {"name": "worker-3kq9x2m7a1bz", "key": "<item id>/autobutler-org/quark", "repo": "autobutler-org/quark",
   "image": "ghcr.io/autobutler-org/factory-worker:0.4.0", "phase": "running", "created": "2026-11-20T10:00:00Z"}
]}
```

One entry per sandbox (Kubernetes Job) whose sandbox key starts with `<id>/`, live or ended, oldest first. `name` is
what the log route takes; it is also the worker's name in `GET /api/v1/workers`. `key` is the chain's sandbox key,
the `sandbox_key` of its steps in `GET /api/v1/work/{id}/graph`, and `repo` is the key without the item ID. A chain
can have more than one sandbox over time (a new one after the last ended). `phase` is `pending`, `running`,
`succeeded` or `failed`. An ended sandbox stays listed, with a readable log, until Kubernetes deletes its Job an
hour after it ends; the launcher removes a live one once its chain has nothing to run. An unknown item, or one with
no sandboxes left, answers `200 {"sandboxes": []}`.

### Read a sandbox's log: `GET /api/v1/work/{id}/sandboxes/{name}/logs`

Query, one of:

- `tail=N` (1 to 5000, default 500 when neither is sent): the last `N` lines.
- `after=<cursor>`: the lines after the `cursor` of an earlier response. Follow a running sandbox by polling
  with the cursor of the last response.

```json
{
  "sandbox": {"name": "worker-3kq9x2m7a1bz", "key": "...", "repo": "autobutler-org/quark", "image": "...", "phase": "running", "created": "..."},
  "lines": [
    {"time": "2026-11-20T10:00:01.123456789Z", "text": "{\"time\":\"...\",\"level\":\"INFO\",\"msg\":\"claimed a step\"}"},
    {"time": "2026-11-20T10:00:02.5Z", "text": "...", "truncated": true}
  ],
  "cursor": "1763632802500000000-1",
  "more": false,
  "pending": null
}
```

- `lines` are oldest first, each the worker container's output line (stdout and stderr together) without its
  newline, with the time the container runtime received it. Worker lines are JSON (`log/slog` records and the
  agent CLI's stream-json events) but are passed through as text. A line over 16 KiB is cut there and has
  `"truncated": true`; the field is left out otherwise.
- `cursor` is opaque: send it back as `after` unchanged. Reading with it returns exactly the lines after the last one
  this response returned, none repeated and none skipped, however much time passed. With nothing new, `lines` is
  `[]` and `cursor` is the same as the one sent.
- `more` is true when a cap stopped the read before the end of the log: at most 5000 lines and 1 MiB of line text
  per response. Read again with `cursor` at once to get the rest.
- `pending` is set when the worker container has not started, so there is no log yet:
  `{"reason": "ContainerCreating", "message": "..."}`, the container's waiting reason (`ImagePullBackOff`,
  `NoPod` while the Job has not created its pod, and so on). `lines` is then `[]` and `cursor` is `""`: poll again
  with `tail`, not `after`.
- `sandbox.phase` says whether to keep polling: once it is `succeeded` or `failed` the log is complete.

`404 sandbox_not_found` means the item has no such sandbox: it ended and was removed, and its log with it, or the
name belongs to another item's sandbox. The message says so; list the item's sandboxes again. `422 invalid_request`
for a `tail` out of range, a cursor this server did not write, or both `tail` and `after`.

There is no streamed response. Following is polling, every few seconds like the rest of the UI: each read is one
bounded request well inside the server's 60 second response limit ([config.md](config.md)), and a cursor carries
nothing on the server, so polling survives a restart of either side.

### Enqueueing work

`POST /api/v1/work` takes work by value:

```json
{
  "source": "github",
  "external_id": "autobutler-org/quark#123",
  "external_url": "https://github.com/autobutler-org/quark/issues/123",
  "title": "add null check to the importer",
  "body": "...",
  "repos": ["autobutler-org/quark"],
  "state": "ready",
  "priority": 0,
  "blocked_by": ["<work item id or source:external_id>"],
  "requires": ["claude"]
}
```

Only `title` is required. `requires` lists the capabilities a worker needs to be handed the item. `state` is `design` or `ready` (default `ready`). Unknown fields are refused.
Enqueueing an existing `(source, external_id)` returns that item with `200`, refreshing title and body only while
it is in `design` or `ready`; it never changes the item's state. For work with no `source`, send an
`Idempotency-Key` header: a repeat with the same key returns the first item and creates nothing. The response is:

```json
{"item": {...}, "created": true, "changed": true, "reason": ""}
```

`changed` is false and `reason` says why when an existing item was left as it was.

```sh
curl -sS -X POST http://127.0.0.1:8080/api/v1/work \
  -H "Authorization: Bearer $FACTORY_API_TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: nightly-2026-11-18' \
  -d '{"title": "rotate the staging certificates", "repos": ["autobutler-org/infra"]}'
```

## Errors

Every error has the same shape:

```json
{"error": {"code": "illegal_transition", "message": "...", "request_id": "...", "current_state": "design", "allowed": ["ready", "blocked", "wont_fix"]}}
```

| Status | `code` | When |
| --- | --- | --- |
| 400 | `malformed_request` | The body is not JSON, has unknown fields, or has trailing data. |
| 401 | `unauthorized` | Missing or wrong bearer token, or a worker's ServiceAccount token or credential that was refused. |
| 404 | `not_found` | No such route, item, dependency, step, or graph. |
| 404 | `sandbox_not_found` | The item has no such sandbox: it ended and was removed, with its log. |
| 409 | `illegal_transition` | The state machine forbids the move. Carries `current_state` and `allowed`. |
| 409 | `unfinished_dependencies` | Work would start while an item it is blocked by is unfinished. |
| 409 | `outputs_pending` | Completing an item with no merged output or an open one. |
| 409 | `no_outputs` | A worker finished without recording an open or merged output. |
| 409 | `output_conflict` | The output already belongs to another item. |
| 409 | `not_claimant`, `lease_expired`, `worker_not_active` | Worker protocol; see [architecture/workers.md](architecture/workers.md#errors). |
| 409 | `not_waiting` | Completing a step that is not a wait step waiting for its event. |
| 409 | `not_replannable` | Replanning an item that is not `in_progress` or `in_review`. |
| 403 | `repo_not_allowed` | A code host token was asked for a repository the claim does not cover. |
| 403 | `forbidden`, `workload_identity_required` | A worker credential acting outside its own worker and chain, or the shared token acting as a worker while the launcher is on; see [architecture/workers.md](architecture/workers.md#workload-identity). |
| 409 | `already_registered` | A worker pod registering a second time. |
| 503 | `sandboxes_not_configured` | A sandbox route on a server started without `--launcher`. |
| 503 | `codehost_not_configured` | `POST /api/v1/work/{id}/codehost-token` on a server started without a GitHub App. |
| 413 | `body_too_large` | The body is over 1 MiB. |
| 422 | `invalid_request` | The body parsed but a value is invalid, including an unknown `blocked_by`. |
| 503 | `config_unavailable` | Replanning while the code host cannot be read; retry later. |
| 503 | `not_ready` | From `/readyz` only. |
| 500 | `internal` | Anything else; the server log has the details under the same request ID. |

A request whose path exists but whose method does not is answered `405` by Go's router with a plain-text body.
