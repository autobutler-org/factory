-- name: InsertItem :one
-- Conflicts on (source, external_id) or idempotency_key insert nothing.
INSERT INTO work_items (source, external_id, external_url, title, body, repos, state, priority, idempotency_key, requires)
VALUES (sqlc.narg(source), sqlc.narg(external_id), sqlc.narg(external_url), @title, @body, @repos::text[], @state, @priority,
        sqlc.narg(idempotency_key), @requires::text[])
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetItem :one
SELECT * FROM work_items WHERE id = @id;

-- name: GetItemForUpdate :one
SELECT * FROM work_items WHERE id = @id FOR UPDATE;

-- name: GetItemBySourceForUpdate :one
SELECT * FROM work_items WHERE source = @source AND external_id = @external_id FOR UPDATE;

-- name: GetItemByIdempotencyKeyForUpdate :one
SELECT * FROM work_items WHERE idempotency_key = @idempotency_key FOR UPDATE;

-- name: GetItemBySource :one
SELECT * FROM work_items WHERE source = @source AND external_id = @external_id;

-- name: ListItems :many
-- Ordered by priority (highest first), then oldest first. The cursor is the
-- last row of the previous page.
SELECT * FROM work_items
WHERE (sqlc.narg(state)::text IS NULL OR state = sqlc.narg(state))
  AND (sqlc.narg(source)::text IS NULL OR source = sqlc.narg(source))
  AND (sqlc.narg(repo)::text IS NULL OR sqlc.narg(repo) = ANY (repos))
  AND (sqlc.narg(claimed_by)::uuid IS NULL OR claimed_by = sqlc.narg(claimed_by))
  AND (sqlc.narg(after_id)::uuid IS NULL
       OR priority < sqlc.narg(after_priority)::integer
       OR (priority = sqlc.narg(after_priority)::integer
           AND (created_at, id) > (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::uuid)))
ORDER BY priority DESC, created_at, id
LIMIT @max_rows;

-- name: UpdateItemState :one
-- A claim exists only while an item is in_progress, so leaving it drops the claim.
UPDATE work_items
SET state = @state, state_reason = @state_reason, outputs_done = @outputs_done,
    updated_at = now(), state_changed_at = now(),
    claimed_by = CASE WHEN @state::text = 'in_progress' THEN claimed_by END,
    claimed_at = CASE WHEN @state::text = 'in_progress' THEN claimed_at END,
    lease_expires_at = CASE WHEN @state::text = 'in_progress' THEN lease_expires_at END,
    stage_reported_at = CASE WHEN @state::text = 'in_progress' THEN stage_reported_at END
WHERE id = @id
RETURNING *;

-- name: UpdateItemContent :one
UPDATE work_items
SET title = @title, body = @body, external_url = coalesce(sqlc.narg(external_url), external_url), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: PatchItem :one
UPDATE work_items
SET title = coalesce(sqlc.narg(title), title),
    body = coalesce(sqlc.narg(body), body),
    priority = coalesce(sqlc.narg(priority), priority),
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetOutputsDone :one
UPDATE work_items SET outputs_done = true, updated_at = now() WHERE id = @id RETURNING *;

-- name: InsertEvent :one
INSERT INTO work_events (work_item_id, actor_kind, actor_id, type, from_state, to_state, payload)
VALUES (@work_item_id, @actor_kind, @actor_id, @type, sqlc.narg(from_state), sqlc.narg(to_state), @payload)
RETURNING *;

-- name: ListEvents :many
SELECT * FROM work_events WHERE work_item_id = @work_item_id ORDER BY id;

-- name: AddDependency :execrows
INSERT INTO work_dependencies (work_item_id, blocked_by_id) VALUES (@work_item_id, @blocked_by_id)
ON CONFLICT DO NOTHING;

-- name: RemoveDependency :execrows
DELETE FROM work_dependencies WHERE work_item_id = @work_item_id AND blocked_by_id = @blocked_by_id;

-- name: ListDependencies :many
SELECT blocked_by_id FROM work_dependencies WHERE work_item_id = @work_item_id ORDER BY created_at, blocked_by_id;

-- name: CountUnfinishedDependencies :one
-- A dependency is finished once it is completed or wont_fix.
SELECT count(*) FROM work_dependencies d
JOIN work_items b ON b.id = d.blocked_by_id
WHERE d.work_item_id = @work_item_id AND b.state NOT IN ('completed', 'wont_fix');

-- name: UpsertPullRequest :one
-- The WHERE stops an upsert from taking over another item's output.
INSERT INTO work_outputs (work_item_id, repo, kind, branch, url, sha, status)
VALUES (@work_item_id, @repo, 'pull_request', @branch, @url, @sha, @status)
ON CONFLICT (repo, kind, url) WHERE kind = 'pull_request'
DO UPDATE SET branch = coalesce(nullif(EXCLUDED.branch, ''), work_outputs.branch),
              sha = coalesce(nullif(EXCLUDED.sha, ''), work_outputs.sha),
              status = EXCLUDED.status, updated_at = now()
WHERE work_outputs.work_item_id = EXCLUDED.work_item_id
RETURNING *;

-- name: UpsertCommit :one
INSERT INTO work_outputs (work_item_id, repo, kind, branch, url, sha, status)
VALUES (@work_item_id, @repo, 'commit', @branch, @url, @sha, 'merged')
ON CONFLICT (repo, sha) WHERE kind = 'commit'
DO UPDATE SET branch = coalesce(nullif(EXCLUDED.branch, ''), work_outputs.branch),
              url = coalesce(nullif(EXCLUDED.url, ''), work_outputs.url), updated_at = now()
WHERE work_outputs.work_item_id = EXCLUDED.work_item_id
RETURNING *;

-- name: ListOutputs :many
SELECT * FROM work_outputs WHERE work_item_id = @work_item_id ORDER BY id;

-- name: CountOutputs :one
SELECT count(*) FILTER (WHERE status = 'open')   AS open,
       count(*) FILTER (WHERE status = 'merged') AS merged
FROM work_outputs WHERE work_item_id = @work_item_id;

-- name: CountItemsByState :many
SELECT state, count(*) AS items FROM work_items GROUP BY state;

-- name: OldestReadyItem :many
-- The item that has waited in ready longest; none when nothing is ready.
SELECT * FROM work_items WHERE state = 'ready' ORDER BY state_changed_at, id LIMIT 1;

-- name: RecentFailedItems :many
SELECT * FROM work_items WHERE state = 'failed' ORDER BY state_changed_at DESC, id LIMIT @max_rows;
