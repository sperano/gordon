-- Gordon 04: the Task each run submits, what Kelos reports about it, and
-- the Tasks shadow decisions would have submitted, for comparison with the
-- legacy spawners' Tasks of the same name.

-- task is the built Kelos Task (its prompt quotes the task and comments;
-- pruned after body retention once the run finished); task_uid and phase
-- are the submitted Task's uid and last observed phase.
ALTER TABLE runs ADD COLUMN task     jsonb;
ALTER TABLE runs ADD COLUMN task_uid text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN phase    text NOT NULL DEFAULT '';

CREATE TABLE shadow_tasks (
    name        text        NOT NULL,
    inbox_id    bigint      NOT NULL REFERENCES inbox (id),
    family      text        NOT NULL,
    task        jsonb,
    result      text        NOT NULL DEFAULT 'pending'
                CHECK (result IN ('pending', 'match', 'differ', 'missing')),
    diff        jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    compared_at timestamptz,
    PRIMARY KEY (name, inbox_id)
);

CREATE INDEX shadow_tasks_pending ON shadow_tasks (created_at) WHERE result = 'pending';
CREATE INDEX runs_observed ON runs (updated_at) WHERE state <> 'finished' OR task_exists;
