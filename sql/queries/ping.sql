-- name: Ping :one
-- Ping proves a connection can run a query; /readyz uses it.
SELECT 1::integer AS ok;
