-- SPDX-License-Identifier: Apache-2.0
-- +goose Up

-- The call ledger. One row per finished call; everything downstream — the CDR
-- explorer, reports, the wallboard's history, outbound idempotency — reads
-- this rather than reconstructing calls from events.
CREATE TABLE cdrs (
    call_id     uuid PRIMARY KEY,
    started_at  timestamptz NOT NULL,
    answered_at timestamptz,
    ended_at    timestamptz NOT NULL,

    call_type varchar(16) NOT NULL
        CHECK (call_type IN ('INBOUND', 'OUTBOUND', 'CONSULT', 'INTERNAL')),
    language  varchar(8)  NOT NULL DEFAULT '',

    from_number text NOT NULL DEFAULT '',
    to_number   text NOT NULL DEFAULT '',
    did         text NOT NULL DEFAULT '',
    flow_id     uuid,
    queue_id    uuid,

    agent_ids        uuid[] NOT NULL DEFAULT '{}',
    primary_agent_id uuid,

    ring_sec       int NOT NULL DEFAULT 0,
    bot_sec        int NOT NULL DEFAULT 0,
    queue_wait_sec int NOT NULL DEFAULT 0,
    talk_sec       int NOT NULL DEFAULT 0,
    total_sec      int NOT NULL DEFAULT 0,

    status varchar(16) NOT NULL
        CHECK (status IN ('ANSWERED', 'NO_ANSWER', 'BUSY', 'FAILED')),
    hangup_cause  text NOT NULL DEFAULT '',
    missed_reason varchar(32)
        CHECK (missed_reason IN ('SHORT_ABANDONED', 'ABANDONED_RINGING',
                                 'ABANDONED_WAITING', 'AGENTS_DID_NOT_ANSWER',
                                 'NO_AVAILABLE_AGENT', 'OUT_OF_HOURS')),
    disposition   text NOT NULL DEFAULT '',
    is_contained  boolean NOT NULL DEFAULT false,
    has_recording boolean NOT NULL DEFAULT false,

    user_data jsonb NOT NULL DEFAULT '{}',
    tech      jsonb NOT NULL DEFAULT '{}',
    legs      jsonb NOT NULL DEFAULT '[]'
);

CREATE INDEX idx_cdrs_started_at ON cdrs (started_at DESC);
CREATE INDEX idx_cdrs_queue_id_started_at ON cdrs (queue_id, started_at DESC);
CREATE INDEX idx_cdrs_primary_agent_id_started_at ON cdrs (primary_agent_id, started_at DESC);
CREATE INDEX idx_cdrs_status ON cdrs (status);

-- What was said and done on an AI leg, ordered per call. Content is jsonb
-- because the three kinds carry different shapes; the columns carry what every
-- kind shares.
CREATE TABLE transcripts (
    id          bigserial PRIMARY KEY,
    call_id     uuid NOT NULL,
    seq         int NOT NULL,
    occurred_at timestamptz NOT NULL,
    role        varchar(8) NOT NULL CHECK (role IN ('BOT', 'CALLER')),
    kind        varchar(16) NOT NULL CHECK (kind IN ('TEXT', 'TOOL_CALL', 'TOOL_RESULT')),
    content     jsonb NOT NULL,
    CONSTRAINT uq_transcripts_call_id_seq UNIQUE (call_id, seq)
);

CREATE TABLE recordings (
    id           uuid PRIMARY KEY,
    call_id      uuid NOT NULL,
    backend      varchar(8) NOT NULL CHECK (backend IN ('FS', 'S3')),
    bucket       text NOT NULL DEFAULT '',
    object_key   text NOT NULL,
    size_bytes   bigint NOT NULL DEFAULT 0,
    duration_sec int NOT NULL DEFAULT 0,
    format       varchar(8) NOT NULL DEFAULT 'WAV',
    created_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);

CREATE INDEX idx_recordings_call_id ON recordings (call_id);

CREATE TABLE quality_reviews (
    id           uuid PRIMARY KEY,
    recording_id uuid NOT NULL,
    call_id      uuid NOT NULL,
    reviewer_id  uuid NOT NULL,
    scores       jsonb NOT NULL DEFAULT '{}',
    total_score  smallint NOT NULL DEFAULT 0,
    notes        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fk_quality_reviews_recordings FOREIGN KEY (recording_id)
        REFERENCES recordings (id) ON DELETE CASCADE
);

-- A promise to call someone back: the bot took a message, or a caller
-- abandoned and asked for one. OPEN rows are the work queue.
CREATE TABLE callbacks (
    id           uuid PRIMARY KEY,
    call_id      uuid,
    queue_id     uuid,
    phone_number text NOT NULL,
    message      text NOT NULL DEFAULT '',
    status       varchar(16) NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN', 'DONE', 'DISMISSED')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    handled_by   uuid,
    handled_at   timestamptz
);

CREATE INDEX idx_callbacks_status_created_at ON callbacks (status, created_at);

-- Queue traffic facts, one row per member movement. Service level and
-- abandonment reporting reads these, never the raw switch events.
CREATE TABLE queue_events (
    id          bigserial PRIMARY KEY,
    occurred_at timestamptz NOT NULL,
    call_id     uuid,
    queue_id    uuid NOT NULL,
    event       varchar(16) NOT NULL
        CHECK (event IN ('JOINED', 'LEFT', 'OFFERED', 'BRIDGED', 'ABANDONED')),
    agent_id    uuid,
    wait_ms     int NOT NULL DEFAULT 0
);

CREATE INDEX idx_queue_events_queue_id_occurred_at ON queue_events (queue_id, occurred_at DESC);

CREATE TABLE audit_logs (
    id          bigserial PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor_id    uuid,
    action      varchar(64) NOT NULL,
    target_kind text NOT NULL DEFAULT '',
    target_id   text NOT NULL DEFAULT '',
    detail      jsonb NOT NULL DEFAULT '{}',
    ip          inet
);

CREATE INDEX idx_audit_logs_occurred_at ON audit_logs (occurred_at DESC);

-- +goose Down
DROP TABLE audit_logs;
DROP TABLE queue_events;
DROP TABLE callbacks;
DROP TABLE quality_reviews;
DROP TABLE recordings;
DROP TABLE transcripts;
DROP TABLE cdrs;
