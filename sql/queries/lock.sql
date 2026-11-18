-- name: TryAdvisoryLock :one
-- A session-level advisory lock: held until unlocked or the connection ends,
-- so a replica that dies mid-pass releases it.
SELECT pg_try_advisory_lock(@key::bigint)::boolean AS locked;

-- name: AdvisoryUnlock :exec
SELECT pg_advisory_unlock(@key::bigint);
