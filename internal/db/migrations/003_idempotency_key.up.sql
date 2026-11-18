-- Lets POST /api/v1/work (#18) dedupe retries of work that has no external
-- identity: the caller sends an Idempotency-Key header and a repeat returns the
-- item the first request created.
ALTER TABLE work_items ADD COLUMN idempotency_key text UNIQUE;
