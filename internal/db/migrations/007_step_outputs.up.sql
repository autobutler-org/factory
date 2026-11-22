-- The combined output of a step's gate run (#70), kept so a failure can be read after its sandbox is gone. One row
-- per step: a later run of the same step replaces it. The worker cuts an output larger than 5 MiB to its head and
-- tail with a marker in between before it uploads it. Rows older than 30 days are deleted when a new one is stored.
CREATE TABLE step_outputs (
    step_id    uuid        PRIMARY KEY REFERENCES work_steps (id) ON DELETE CASCADE,
    output     bytea       NOT NULL CHECK (octet_length(output) <= 5242880),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX step_outputs_created ON step_outputs (created_at);
