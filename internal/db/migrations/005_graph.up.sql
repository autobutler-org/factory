-- The step graph (#24): each work item is planned as a graph of steps, and
-- workers lease steps instead of whole items.

CREATE TABLE work_graphs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id uuid        NOT NULL REFERENCES work_items (id),
    version      integer     NOT NULL CHECK (version >= 1),
    template     text        NOT NULL,
    inputs_hash  text        NOT NULL,
    -- current runs; superseded was replaced by a replan; handed_over was
    -- taken whole by a developer's local claim (#12).
    state        text        NOT NULL DEFAULT 'current' CHECK (state IN ('current', 'superseded', 'handed_over')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (work_item_id, version)
);

CREATE UNIQUE INDEX work_graphs_one_current ON work_graphs (work_item_id) WHERE state = 'current';

CREATE TABLE work_steps (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    graph_id          uuid        NOT NULL REFERENCES work_graphs (id),
    -- The step's place in the plan, for listing it in plan order.
    position          integer     NOT NULL,
    key               text        NOT NULL CHECK (key <> ''),
    kind              text        NOT NULL CHECK (kind <> ''),
    params            jsonb       NOT NULL DEFAULT '{}',
    -- Steps with the same sandbox_key share a working tree and never run at
    -- the same time. Empty for steps that need no sandbox.
    sandbox_key       text        NOT NULL DEFAULT '',
    condition         text        NOT NULL DEFAULT '',
    state             text        NOT NULL DEFAULT 'pending' CHECK (state IN
                          ('pending', 'ready', 'running', 'succeeded', 'failed', 'skipped', 'canceled')),
    attempts          integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    claimed_by        uuid        REFERENCES workers (id),
    lease_expires_at  timestamptz,
    -- The last stage report, or the claim; the stuck watchdog measures from here.
    stage             text        NOT NULL DEFAULT '',
    stage_reported_at timestamptz,
    -- A retried step is not handed out again before this.
    available_at      timestamptz NOT NULL DEFAULT now(),
    -- JSON null until the step finishes.
    result            jsonb       NOT NULL DEFAULT 'null',
    started_at        timestamptz,
    finished_at       timestamptz,
    UNIQUE (graph_id, key),
    CONSTRAINT work_steps_claim_only_running CHECK (claimed_by IS NULL OR state = 'running'),
    CONSTRAINT work_steps_claim_has_lease CHECK ((claimed_by IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX work_steps_graph ON work_steps (graph_id);
CREATE INDEX work_steps_ready ON work_steps (available_at) WHERE state = 'ready';
CREATE INDEX work_steps_running ON work_steps (sandbox_key) WHERE state = 'running';
CREATE INDEX work_steps_claimed ON work_steps (claimed_by) WHERE claimed_by IS NOT NULL;

-- step_id runs after depends_on_id. Both belong to the same graph, and the
-- graph is checked for cycles before it is written (internal/plan).
CREATE TABLE work_step_edges (
    step_id       uuid NOT NULL REFERENCES work_steps (id),
    depends_on_id uuid NOT NULL REFERENCES work_steps (id),
    PRIMARY KEY (step_id, depends_on_id),
    CHECK (step_id <> depends_on_id)
);
