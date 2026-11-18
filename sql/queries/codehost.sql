-- name: HeldStepRepos :many
-- The repository of each step of the item's current graph that the worker
-- holds, and whether its lease is still live. A step whose lease ran out
-- keeps its claim until the reaper returns it.
SELECT (s.params->>'repo')::text AS repo, (s.lease_expires_at > now())::boolean AS live
FROM work_steps s
JOIN work_graphs g ON g.id = s.graph_id
WHERE g.work_item_id = @work_item_id AND g.state = 'current' AND s.claimed_by = @worker_id
ORDER BY s.position;
