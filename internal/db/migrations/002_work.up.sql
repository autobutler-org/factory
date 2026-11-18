-- The work model (#4): work items, their append-only history, dependencies
-- between them, and the pull requests and commits they produce.

CREATE TABLE work_items (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    source           text,
    external_id      text,
    external_url     text,
    title            text        NOT NULL CHECK (title <> ''),
    body             text        NOT NULL DEFAULT '',
    repos            text[]      NOT NULL DEFAULT '{}',
    state            text        NOT NULL CHECK (state IN
                         ('design', 'ready', 'in_progress', 'in_review', 'blocked', 'failed', 'completed', 'wont_fix')),
    stage            text        NOT NULL DEFAULT '',
    state_reason     text        NOT NULL DEFAULT '',
    priority         integer     NOT NULL DEFAULT 0,
    attempts         integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- Set when the worker reports it has nothing more to produce; the item
    -- completes once every output has landed. Cleared when work resumes.
    outputs_done     boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    state_changed_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, external_id),
    CHECK ((source IS NULL) = (external_id IS NULL))
);

CREATE INDEX work_items_ready ON work_items (priority DESC, created_at, id) WHERE state = 'ready';

CREATE TABLE work_events (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    work_item_id uuid        NOT NULL REFERENCES work_items (id),
    at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    actor_kind   text        NOT NULL CHECK (actor_kind IN ('adapter', 'worker', 'user', 'system')),
    actor_id     text        NOT NULL DEFAULT '',
    type         text        NOT NULL,
    from_state   text,
    to_state     text,
    payload      jsonb       NOT NULL DEFAULT '{}'
);

CREATE INDEX work_events_item ON work_events (work_item_id, id);

-- History is append-only: the database refuses to change or remove it.
CREATE FUNCTION work_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'work_events is append-only';
END;
$$;

CREATE TRIGGER work_events_append_only
    BEFORE UPDATE OR DELETE ON work_events
    FOR EACH ROW EXECUTE FUNCTION work_events_append_only();

CREATE TABLE work_dependencies (
    work_item_id  uuid        NOT NULL REFERENCES work_items (id),
    blocked_by_id uuid        NOT NULL REFERENCES work_items (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (work_item_id, blocked_by_id),
    CHECK (work_item_id <> blocked_by_id)
);

CREATE TABLE work_outputs (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    work_item_id uuid        NOT NULL REFERENCES work_items (id),
    repo         text        NOT NULL CHECK (repo <> ''),
    kind         text        NOT NULL CHECK (kind IN ('pull_request', 'commit')),
    branch       text        NOT NULL DEFAULT '',
    url          text        NOT NULL DEFAULT '',
    sha          text        NOT NULL DEFAULT '',
    status       text        NOT NULL CHECK (status IN ('open', 'merged', 'closed')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (kind <> 'pull_request' OR url <> ''),
    CHECK (kind <> 'commit' OR (sha <> '' AND status = 'merged'))
);

CREATE UNIQUE INDEX work_outputs_pull_request ON work_outputs (repo, kind, url) WHERE kind = 'pull_request';
CREATE UNIQUE INDEX work_outputs_commit ON work_outputs (repo, sha) WHERE kind = 'commit';
CREATE INDEX work_outputs_item ON work_outputs (work_item_id);
