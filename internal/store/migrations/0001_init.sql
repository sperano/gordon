-- Durable intake, decisions, the ledger of handled work and the action
-- outbox. Bodies are verified webhook bytes; the receiver never stores an
-- unverified delivery.

CREATE TABLE inbox (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source          text        NOT NULL,
    delivery_key    text        NOT NULL UNIQUE,
    request_key     text        NOT NULL DEFAULT '',
    kind            text        NOT NULL DEFAULT '',
    scope           text        NOT NULL DEFAULT '',
    headers         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    body            bytea,
    received_at     timestamptz NOT NULL DEFAULT now(),
    state           text        NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'done', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_token     uuid,
    lease_owner     text,
    lease_until     timestamptz,
    last_error      text        NOT NULL DEFAULT '',
    finished_at     timestamptz
);

CREATE INDEX inbox_ready ON inbox (next_attempt_at, id) WHERE state = 'pending';

CREATE TABLE decisions (
    inbox_id    bigint      PRIMARY KEY REFERENCES inbox (id),
    outcome     text        NOT NULL,
    reason      text        NOT NULL DEFAULT '',
    executed    boolean     NOT NULL,
    decision    jsonb       NOT NULL,
    decided_at  timestamptz NOT NULL DEFAULT now()
);

-- Handled owner requests and state changes: kept after Kelos deletes the
-- Tasks they started, so a resend stays a duplicate.
CREATE TABLE requests (
    request_key text        PRIMARY KEY,
    inbox_id    bigint      NOT NULL REFERENCES inbox (id),
    outcome     text        NOT NULL,
    executed    boolean     NOT NULL,
    handled_at  timestamptz NOT NULL DEFAULT now()
);

-- Runs Gordon submitted (executed decisions only), kept after cleanup.
CREATE TABLE runs (
    name           text        PRIMARY KEY,
    family         text        NOT NULL,
    project        text        NOT NULL DEFAULT '',
    request_key    text        NOT NULL DEFAULT '',
    inbox_id       bigint      NOT NULL REFERENCES inbox (id),
    identity       jsonb       NOT NULL,
    profile        jsonb       NOT NULL,
    config_version text        NOT NULL DEFAULT '',
    state          text        NOT NULL DEFAULT 'submitting'
                   CHECK (state IN ('submitting', 'active', 'finished')),
    task_exists    boolean     NOT NULL DEFAULT false,
    generation     integer     NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE actions (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    inbox_id        bigint      NOT NULL REFERENCES inbox (id),
    key             text        NOT NULL,
    kind            text        NOT NULL,
    target          text        NOT NULL,
    value           text        NOT NULL DEFAULT '',
    detail          text        NOT NULL DEFAULT '',
    family          text        NOT NULL,
    project         text        NOT NULL DEFAULT '',
    state           text        NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'running', 'ambiguous', 'done', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_token     uuid,
    lease_owner     text,
    lease_until     timestamptz,
    last_error      text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    UNIQUE (inbox_id, key)
);

CREATE INDEX actions_ready ON actions (next_attempt_at, id)
    WHERE state IN ('pending', 'ambiguous', 'running');
