ALTER TABLE work_items
    DROP COLUMN claimed_by,
    DROP COLUMN claimed_at,
    DROP COLUMN lease_expires_at,
    DROP COLUMN stage_reported_at,
    DROP COLUMN available_at,
    DROP COLUMN requires;
DROP TABLE workers;
