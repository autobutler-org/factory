-- Workload identity for cluster workers (#15). A worker pod registers once with its projected ServiceAccount
-- token and gets a credential, kept here only as a hash, bound to the pod and to the sandbox key the
-- launcher gave its Job. Registering again from a new pod replaces the row; from the same pod it is refused.
CREATE TABLE worker_credentials (
    worker_id       uuid        PRIMARY KEY REFERENCES workers (id) ON DELETE CASCADE,
    pod_uid         text        NOT NULL CHECK (pod_uid <> ''),
    sandbox_key     text        NOT NULL CHECK (sandbox_key <> ''),
    credential_hash bytea       NOT NULL UNIQUE,
    created_at      timestamptz NOT NULL DEFAULT now()
);
