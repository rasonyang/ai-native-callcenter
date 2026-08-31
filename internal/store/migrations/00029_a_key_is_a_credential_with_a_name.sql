-- SPDX-License-Identifier: Apache-2.0
-- A key is a credential with a name, not a setting in a file.
--
-- What it replaces was one shared secret in AICC_API_KEY, sent as
-- X-AICC-Api-Key, reaching two operations while standing in for a supervisor
-- nobody could name. It could not be rotated without a restart, could not be
-- revoked for one integration without breaking the rest, and left an audit
-- trail that said "api-key" wherever it acted.
--
-- A row here is one integration: its own name, its own scopes, its own
-- revocation. Only the SHA-256 of the secret is stored, so authentication is
-- a lookup on key_hash and nothing in this database can produce the secret
-- again — a lost key is revoked and reissued, never recovered.
--
-- key_prefix is for display: it is the leading, non-secret characters, so a
-- person can tell two keys apart in a list. It is deliberately not indexed
-- and never used to find a row, because a lookup by prefix would be a lookup
-- by something an attacker can read off a screenshot.
--
-- Two states, and REVOKED is terminal. There is no disable-and-re-enable: a
-- credential somebody had reason to switch off is a credential to reissue.
-- There is no hard delete either — a key that ever authenticated is named in
-- the audit trail, and a row that can vanish makes that trail unreadable.

-- +goose Up
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY,
    name         varchar(120) NOT NULL,
    key_hash     bytea NOT NULL,
    key_prefix   varchar(16) NOT NULL,
    status       varchar(16) NOT NULL DEFAULT 'ENABLED',
    scopes       text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    last_used_at timestamptz,
    revoked_at   timestamptz,
    CONSTRAINT uq_api_keys_key_hash UNIQUE (key_hash),
    CONSTRAINT ck_api_keys_status CHECK (status IN ('ENABLED', 'REVOKED')),
    -- The two halves of one fact, kept from disagreeing: a revoked key has a
    -- time, an enabled one has none.
    CONSTRAINT ck_api_keys_revoked_at CHECK (
        (status = 'REVOKED') = (revoked_at IS NOT NULL)
    )
);

-- +goose Down
DROP TABLE api_keys;
