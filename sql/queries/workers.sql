-- name: RegisterWorker :one
-- Registering again under the same name refreshes the row and revives it.
INSERT INTO workers (name, kind, capabilities, version)
VALUES (@name, @kind, @capabilities, @version)
ON CONFLICT (name) DO UPDATE
SET kind = EXCLUDED.kind, capabilities = EXCLUDED.capabilities, version = EXCLUDED.version,
    last_seen_at = now(), state = 'active'
RETURNING *;

-- name: GetWorker :one
SELECT * FROM workers WHERE id = @id;

-- name: ListWorkers :many
SELECT * FROM workers ORDER BY name;

-- name: TouchWorker :one
-- A heartbeat. A worker marked gone that speaks again is active again.
UPDATE workers
SET last_seen_at = now(), state = CASE WHEN state = 'gone' THEN 'active' ELSE state END
WHERE id = @id
RETURNING *;

-- name: ExtendLeases :many
-- Extends only live leases: a lease that has already expired is lost.
UPDATE work_items
SET lease_expires_at = now() + make_interval(secs => @lease_seconds::float8)
WHERE claimed_by = @worker_id AND state = 'in_progress' AND lease_expires_at > now()
RETURNING id;

-- name: PickNext :one
-- The next item a worker may take: ready, available, every requirement among
-- the worker's capabilities, and no unfinished dependency. SKIP LOCKED lets
-- concurrent claimers each take a different row.
SELECT w.id FROM work_items w
WHERE w.state = 'ready'
  AND w.available_at <= now()
  AND w.requires <@ @capabilities::text[]
  AND NOT EXISTS (
      SELECT 1 FROM work_dependencies d
      JOIN work_items b ON b.id = d.blocked_by_id
      WHERE d.work_item_id = w.id AND b.state NOT IN ('completed', 'wont_fix'))
ORDER BY w.priority DESC, w.created_at, w.id
LIMIT 1
FOR UPDATE OF w SKIP LOCKED;

-- name: SetClaim :one
UPDATE work_items
SET claimed_by = @worker_id, claimed_at = now(), stage_reported_at = now(),
    lease_expires_at = now() + make_interval(secs => @lease_seconds::float8)
WHERE id = @id
RETURNING *;

-- name: LeaseLive :one
SELECT coalesce(lease_expires_at > now(), false)::boolean FROM work_items WHERE id = @id;

-- name: SetStage :one
UPDATE work_items SET stage = @stage, stage_reported_at = now(), updated_at = now() WHERE id = @id RETURNING *;

-- name: IncrementAttempts :one
UPDATE work_items SET attempts = attempts + 1, updated_at = now() WHERE id = @id RETURNING *;

-- name: DelayAvailability :one
UPDATE work_items SET available_at = now() + make_interval(secs => @delay_seconds::float8) WHERE id = @id RETURNING *;

-- name: ExpiredLeases :many
SELECT id FROM work_items
WHERE state = 'in_progress' AND claimed_by IS NOT NULL AND lease_expires_at <= now()
ORDER BY lease_expires_at
LIMIT @max_rows
FOR UPDATE SKIP LOCKED;

-- name: StuckItems :many
SELECT id FROM work_items
WHERE state = 'in_progress' AND claimed_by IS NOT NULL
  AND stage_reported_at <= now() - make_interval(secs => @stuck_seconds::float8)
ORDER BY stage_reported_at
LIMIT @max_rows
FOR UPDATE SKIP LOCKED;

-- name: MarkSilentWorkersGone :many
UPDATE workers SET state = 'gone'
WHERE state <> 'gone' AND last_seen_at <= now() - make_interval(secs => @ttl_seconds::float8)
RETURNING id, name;
