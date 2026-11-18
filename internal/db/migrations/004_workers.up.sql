-- The worker registry and claim leases (#6).

CREATE TABLE workers (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL UNIQUE CHECK (name <> ''),
    kind          text        NOT NULL CHECK (kind IN ('cluster', 'local')),
    capabilities  jsonb       NOT NULL DEFAULT '{}',
    version       text        NOT NULL DEFAULT '',
    registered_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    state         text        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'draining', 'gone'))
);

ALTER TABLE work_items
    ADD COLUMN claimed_by        uuid REFERENCES workers (id),
    ADD COLUMN claimed_at        timestamptz,
    ADD COLUMN lease_expires_at  timestamptz,
    -- The last stage report, or the claim; the stuck watchdog measures from here.
    ADD COLUMN stage_reported_at timestamptz,
    -- A retried item is not handed out again before this.
    ADD COLUMN available_at      timestamptz NOT NULL DEFAULT now(),
    -- Capabilities (harnesses or labels) a worker needs to be handed this item.
    ADD COLUMN requires          text[]      NOT NULL DEFAULT '{}',
    ADD CONSTRAINT work_items_claim_only_in_progress CHECK (claimed_by IS NULL OR state = 'in_progress'),
    ADD CONSTRAINT work_items_claim_has_lease CHECK ((claimed_by IS NULL) = (lease_expires_at IS NULL));

CREATE INDEX work_items_claimed ON work_items (claimed_by) WHERE claimed_by IS NOT NULL;
