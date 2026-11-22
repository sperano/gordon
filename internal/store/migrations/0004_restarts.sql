-- Gordon 08b: limit restarts and run completions.

-- limit_restart is what the limit restart pass did with a run its Task
-- failed on a subscription limit (restarted, superseded, gave-up,
-- name-too-long) or that failed otherwise (none); '' while undecided. A new
-- generation of the run starts undecided again.
ALTER TABLE runs ADD COLUMN limit_restart text NOT NULL DEFAULT '';
CREATE INDEX runs_failed_undecided ON runs (project)
    WHERE state = 'finished' AND task_exists AND phase = 'Failed' AND limit_restart = '';

-- run_completions records each run generation the observer saw finish,
-- with its Task's outputs other than the agent's answer: the usage
-- accounting's (Gordon 10) input, written once per generation.
CREATE TABLE run_completions (
    name         text        NOT NULL,
    inbox_id     bigint      NOT NULL REFERENCES inbox (id),
    family       text        NOT NULL,
    project      text        NOT NULL DEFAULT '',
    task_uid     text        NOT NULL DEFAULT '',
    phase        text        NOT NULL,
    completed_at timestamptz,
    results      jsonb       NOT NULL DEFAULT '{}',
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (name, inbox_id)
);
