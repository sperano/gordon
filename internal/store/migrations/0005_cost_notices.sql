-- Gordon 12: completion cost notices. One row per finished run generation
-- of a Vikunja task: waiting until Sillage's estimate of the run is final
-- (or the wait's deadline passed), then live while its comment's task
-- total is kept current, frozen once a newer notice of the task carries
-- the total, settled once nothing changed for the live window. body is
-- the text of revision rev, which the publish-cost action writes; digest
-- tells a changed estimate from an unchanged one.
CREATE TABLE cost_notices (
    name        text        NOT NULL,
    inbox_id    bigint      NOT NULL REFERENCES inbox (id),
    family      text        NOT NULL,
    project     text        NOT NULL DEFAULT '',
    task_id     bigint      NOT NULL,
    task_uid    text        NOT NULL DEFAULT '',
    phase       text        NOT NULL,
    finished_at timestamptz NOT NULL,
    state       text        NOT NULL DEFAULT 'waiting',
    rev         integer     NOT NULL DEFAULT 0,
    body        text        NOT NULL DEFAULT '',
    digest      text        NOT NULL DEFAULT '',
    failures    integer     NOT NULL DEFAULT 0,
    last_error  text        NOT NULL DEFAULT '',
    next_check  timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    live_until  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (name, inbox_id)
);
CREATE INDEX cost_notices_due ON cost_notices (next_check) WHERE state IN ('waiting', 'live');
CREATE INDEX cost_notices_task ON cost_notices (task_id) WHERE state = 'live';
