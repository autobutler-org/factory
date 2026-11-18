# HTTP API

`factory serve` serves JSON over HTTP. Routes under `/api/v1` need `Authorization: Bearer <token>` when
`FACTORY_API_TOKEN` is set (see [config.md](config.md)). Every response carries an `X-Request-ID` header; send one
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

States and transition rules are in [architecture/work.md](architecture/work.md).

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
  "blocked_by": ["<work item id or source:external_id>"]
}
```

Only `title` is required. `state` is `design` or `ready` (default `ready`). Unknown fields are refused.
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
| 401 | `unauthorized` | Missing or wrong bearer token. |
| 404 | `not_found` | No such route, item or dependency. |
| 409 | `illegal_transition` | The state machine forbids the move. Carries `current_state` and `allowed`. |
| 409 | `unfinished_dependencies` | Work would start while an item it is blocked by is unfinished. |
| 409 | `outputs_pending` | Completing an item with no merged output or an open one. |
| 409 | `no_outputs` | A worker finished without recording an open or merged output. |
| 409 | `output_conflict` | The output already belongs to another item. |
| 413 | `body_too_large` | The body is over 1 MiB. |
| 422 | `invalid_request` | The body parsed but a value is invalid, including an unknown `blocked_by`. |
| 503 | `not_ready` | From `/readyz` only. |
| 500 | `internal` | Anything else; the server log has the details under the same request ID. |

A request whose path exists but whose method does not is answered `405` by Go's router with a plain-text body.
