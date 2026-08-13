-- SPDX-License-Identifier: Apache-2.0
-- M1 foundation: users, sessions, settings, SSE sequence blocks.
-- Naming follows docs/design/07-naming.md: tables plural snake_case, columns
-- singular snake_case, xxx_at timestamptz, is_/has_ booleans, enum columns hold
-- SCREAMING_SNAKE strings identical to their JSON values.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    username      citext NOT NULL,
    password_hash text NOT NULL,
    display_name  text NOT NULL,
    role          varchar(16) NOT NULL CHECK (role IN ('AGENT', 'SUPERVISOR', 'ADMIN')),
    status        varchar(16) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'SUSPENDED')),
    locale        varchar(8),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    CONSTRAINT uq_users_username UNIQUE (username)
);

CREATE TABLE sessions (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL,
    token_hash bytea NOT NULL,
    user_agent text,
    ip         inet,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CONSTRAINT fk_sessions_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT uq_sessions_token_hash UNIQUE (token_hash)
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Durable allocator for the global SSE sequence: the publisher reserves a block
-- of ids per round trip so PostgreSQL never sits on the per-event path.
CREATE TABLE seq_blocks (
    name  text PRIMARY KEY,
    value bigint NOT NULL
);

INSERT INTO seq_blocks (name, value) VALUES ('events', 0);

-- +goose Down
DROP TABLE seq_blocks;
DROP TABLE settings;
DROP TABLE sessions;
DROP TABLE users;
