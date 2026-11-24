-- name: LatestGraph :one
SELECT * FROM work_graphs WHERE work_item_id = @work_item_id ORDER BY version DESC LIMIT 1;

-- name: CurrentGraph :one
SELECT * FROM work_graphs WHERE work_item_id = @work_item_id AND state = 'current';

-- name: EndCurrentGraph :execrows
-- Marks the item's current graph superseded or handed_over.
UPDATE work_graphs SET state = @state WHERE work_item_id = @work_item_id AND state = 'current';

-- name: InsertGraph :one
INSERT INTO work_graphs (work_item_id, version, template, inputs_hash)
VALUES (@work_item_id,
        (SELECT coalesce(max(version), 0) + 1 FROM work_graphs WHERE work_item_id = @work_item_id),
        @template, @inputs_hash)
RETURNING *;

-- name: InsertStep :one
INSERT INTO work_steps (graph_id, position, key, kind, params, sandbox_key, condition)
VALUES (@graph_id, @position, @key, @kind, @params, @sandbox_key, @condition)
RETURNING id;

-- name: InsertEdge :exec
INSERT INTO work_step_edges (step_id, depends_on_id) VALUES (@step_id, @depends_on_id);

-- name: ListSteps :many
SELECT * FROM work_steps WHERE graph_id = @graph_id ORDER BY position;

-- name: ListEdges :many
SELECT e.step_id, e.depends_on_id FROM work_step_edges e
JOIN work_steps s ON s.id = e.step_id
WHERE s.graph_id = @graph_id
ORDER BY s.position, e.depends_on_id;

-- name: StepItem :one
-- The work item a step belongs to, and whether the step's graph is current.
SELECT g.work_item_id, g.state = 'current' AS current FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id
WHERE s.id = @id;

-- name: GetStepForUpdate :one
SELECT * FROM work_steps WHERE id = @id FOR UPDATE;

-- name: SetStepState :exec
-- The scheduler's move of a pending step to ready or skipped. A step that
-- becomes ready is available from now, so available_at is when a wait step
-- started waiting.
UPDATE work_steps
SET state = @state, finished_at = CASE WHEN @state::text = 'skipped' THEN now() END,
    available_at = CASE WHEN @state::text = 'ready' THEN now() ELSE available_at END
WHERE id = @id;

-- name: PickStep :one
-- The next step a worker may run: ready and available, not a wait step, in
-- the current graph of an item that is being worked by the step protocol,
-- every requirement of the item among the worker's capabilities, and no
-- running step sharing its sandbox. Optionally restricted to one sandbox or
-- one item. Locking the item (SKIP LOCKED) serializes claims per item, and
-- sandbox keys are per item, so two steps of one sandbox never both start.
SELECT s.id, w.id AS work_item_id FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id AND g.state = 'current'
JOIN work_items w ON w.id = g.work_item_id
WHERE s.state = 'ready'
  AND s.available_at <= now()
  AND NOT (s.kind = ANY (@wait_kinds::text[]))
  AND w.state IN ('in_progress', 'in_review')
  AND w.claimed_by IS NULL
  AND w.requires <@ @capabilities::text[]
  AND (sqlc.narg(sandbox_key)::text IS NULL OR s.sandbox_key = sqlc.narg(sandbox_key))
  AND (sqlc.narg(work_item_id)::uuid IS NULL OR w.id = sqlc.narg(work_item_id))
  AND (s.sandbox_key = '' OR NOT EXISTS (
      SELECT 1 FROM work_steps r WHERE r.state = 'running' AND r.sandbox_key = s.sandbox_key))
ORDER BY w.priority DESC, w.created_at, w.id, s.position
LIMIT 1
FOR UPDATE OF w SKIP LOCKED;

-- name: SetStepClaim :one
-- Claims the step only if it is still ready and its sandbox is still free.
-- PickStep's snapshot can predate a claim that committed while it waited,
-- so the caller, now holding the item's lock, checks again here.
UPDATE work_steps s
SET state = 'running', claimed_by = @worker_id, started_at = now(), stage = '', stage_reported_at = now(),
    lease_expires_at = now() + make_interval(secs => @lease_seconds::float8)
WHERE s.id = @id AND s.state = 'ready'
  AND (s.sandbox_key = '' OR NOT EXISTS (
      SELECT 1 FROM work_steps r WHERE r.state = 'running' AND r.sandbox_key = s.sandbox_key))
RETURNING *;

-- name: StepLeaseLive :one
SELECT coalesce(lease_expires_at > now(), false)::boolean FROM work_steps WHERE id = @id;

-- name: SetStepStage :one
UPDATE work_steps SET stage = @stage, stage_reported_at = now() WHERE id = @id RETURNING *;

-- name: FinishStep :one
-- Ends a step as succeeded or failed, dropping any claim.
UPDATE work_steps
SET state = @state, result = @result, finished_at = now(),
    claimed_by = NULL, lease_expires_at = NULL
WHERE id = @id
RETURNING *;

-- name: ReturnStep :one
-- Hands a step out again after delay_seconds, dropping its claim.
UPDATE work_steps
SET state = 'ready', claimed_by = NULL, lease_expires_at = NULL,
    available_at = now() + make_interval(secs => @delay_seconds::float8)
WHERE id = @id
RETURNING *;

-- name: IncrementStepAttempts :one
UPDATE work_steps SET attempts = attempts + 1 WHERE id = @id RETURNING *;

-- name: CancelSteps :execrows
-- Cancels the unfinished steps of the item's current graph.
UPDATE work_steps s
SET state = 'canceled', claimed_by = NULL, lease_expires_at = NULL, finished_at = now()
FROM work_graphs g
WHERE g.id = s.graph_id AND g.work_item_id = @work_item_id AND g.state = 'current'
  AND s.state IN ('pending', 'ready', 'running');

-- name: ExtendStepLeases :many
-- Extends only live leases: a lease that has already expired is lost.
UPDATE work_steps
SET lease_expires_at = now() + make_interval(secs => @lease_seconds::float8)
WHERE claimed_by = @worker_id AND state = 'running' AND lease_expires_at > now()
RETURNING id;

-- name: ExpiredSteps :many
-- Locks the items (SKIP LOCKED) rather than the steps: every path that
-- changes a step locks its item first, so the step cannot change under us.
SELECT s.id FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id
JOIN work_items w ON w.id = g.work_item_id
WHERE s.state = 'running' AND s.lease_expires_at <= now()
ORDER BY s.lease_expires_at
LIMIT @max_rows
FOR UPDATE OF w SKIP LOCKED;

-- name: StuckSteps :many
SELECT s.id FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id
JOIN work_items w ON w.id = g.work_item_id
WHERE s.state = 'running' AND s.stage_reported_at <= now() - make_interval(secs => @stuck_seconds::float8)
ORDER BY s.stage_reported_at
LIMIT @max_rows
FOR UPDATE OF w SKIP LOCKED;

-- name: RunnableSandboxes :many
-- The sandbox keys of chains with a step a sandboxed worker could claim now:
-- ready and available, not a wait step, in the current graph of an item
-- being worked by the step protocol whose requirements the launched workers
-- meet, and no step of the chain running. Highest-priority item first.
SELECT s.sandbox_key FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id AND g.state = 'current'
JOIN work_items w ON w.id = g.work_item_id
WHERE s.state = 'ready'
  AND s.available_at <= now()
  AND s.sandbox_key <> ''
  AND NOT (s.kind = ANY (@wait_kinds::text[]))
  AND w.state IN ('in_progress', 'in_review')
  AND w.claimed_by IS NULL
  AND w.requires <@ @capabilities::text[]
  AND NOT EXISTS (
      SELECT 1 FROM work_steps r WHERE r.state = 'running' AND r.sandbox_key = s.sandbox_key)
GROUP BY s.sandbox_key
ORDER BY max(w.priority) DESC, min(w.created_at), s.sandbox_key;

-- name: UnfinishedSandboxSteps :one
-- How many steps of a chain can still run: pending, ready or running.
SELECT count(*) FROM work_steps WHERE sandbox_key = @sandbox_key AND state IN ('pending', 'ready', 'running');

-- name: SandboxImage :one
-- The image the chain with this sandbox key runs in, as its current graph's
-- workspace step records it; '' for the launcher's default.
SELECT coalesce(s.params->>'image', '')::text FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id AND g.state = 'current'
WHERE s.sandbox_key = @sandbox_key AND s.kind = 'workspace'
LIMIT 1;

-- name: PickServerStep :one
-- The next step the server runs itself: one with no sandbox that is not a
-- wait step (merge), ready and available, in the current graph of an item
-- being worked by the step protocol. Capabilities do not apply: the server
-- needs none of the item's requirements to merge.
SELECT s.id, w.id AS work_item_id FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id AND g.state = 'current'
JOIN work_items w ON w.id = g.work_item_id
WHERE s.state = 'ready'
  AND s.available_at <= now()
  AND s.sandbox_key = ''
  AND NOT (s.kind = ANY (@wait_kinds::text[]))
  AND w.state IN ('in_progress', 'in_review')
  AND w.claimed_by IS NULL
ORDER BY w.priority DESC, w.created_at, w.id, s.position
LIMIT 1
FOR UPDATE OF w SKIP LOCKED;

-- name: PollPulls :many
-- The open pull requests of the items being worked, blocked or failed, for
-- the server's poller, highest-priority item first. A blocked or failed item
-- is polled so a person's merge completes it; it has no ready step. The newest pull request of each
-- repository carries the ready wait step of that repository's chain in the
-- item's current graph, if any; ready_since is when it started waiting.
SELECT o.work_item_id, o.repo, o.branch, o.url,
       s.id AS step_id, s.kind AS step_kind, s.key AS step_key, s.available_at AS ready_since
FROM work_outputs o
JOIN work_items w ON w.id = o.work_item_id AND w.state IN ('in_progress', 'in_review', 'blocked', 'failed')
LEFT JOIN work_graphs g ON g.work_item_id = w.id AND g.state = 'current'
LEFT JOIN work_steps s ON s.graph_id = g.id AND s.state = 'ready' AND s.kind = ANY (@wait_kinds::text[])
     AND lower(s.params->>'repo') = lower(o.repo)
     AND o.id = (SELECT max(n.id) FROM work_outputs n
                 WHERE n.work_item_id = o.work_item_id AND n.kind = 'pull_request' AND lower(n.repo) = lower(o.repo))
WHERE o.kind = 'pull_request' AND o.status = 'open'
ORDER BY w.priority DESC, w.created_at, o.id;

-- name: PutStepOutput :one
-- Keeps a gate run's output for the step, replacing an earlier run's.
INSERT INTO step_outputs (step_id, output) VALUES (@step_id, @output)
ON CONFLICT (step_id) DO UPDATE SET output = excluded.output, created_at = now()
RETURNING step_id, octet_length(output)::bigint AS bytes, created_at;

-- name: PruneStepOutputs :execrows
-- Deletes the outputs stored before @before.
DELETE FROM step_outputs WHERE created_at < @before;

-- name: GetStepOutput :one
SELECT output, created_at FROM step_outputs WHERE step_id = @step_id;

-- name: ListStepOutputs :many
-- What is kept of the outputs of a graph's steps, without the output itself.
SELECT o.step_id, octet_length(o.output)::bigint AS bytes, o.created_at FROM step_outputs o
JOIN work_steps s ON s.id = o.step_id
WHERE s.graph_id = @graph_id
ORDER BY s.position;
