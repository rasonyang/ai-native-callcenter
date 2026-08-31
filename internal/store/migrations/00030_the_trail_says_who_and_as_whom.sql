-- SPDX-License-Identifier: Apache-2.0
-- The audit trail learns to say who acted, and as whom.
--
-- actor_id has always meant one thing: the user account behind a request. It
-- still does, and this migration does not touch it — GET /audit-logs already
-- returns actorId and actorUsername, and changing what a shipped field means
-- while keeping its name and type is a break no diff tool can see (ruling 3).
--
-- What it cannot say is the two things a key made possible. A row written for
-- an API key had a null actor and the word "api-key" buried in the detail
-- jsonb, which is a fact hidden where nothing can filter on it. And a key
-- working for an agent produced a row that named the person but not that
-- somebody else's credential had asked — the difference between "Mina went
-- ready" and "the CRM put Mina ready", which is exactly the question an audit
-- is read to answer.
--
-- Four columns, purely additive:
--   subject_kind  USER or API_KEY — what authenticated
--   subject_id    the user id or the key id, in its own space
--   subject_name  the username or the key's name, snapshotted
--   agent_id      the agent identity the request acted as, if any
--
-- subject_name is a snapshot on purpose. The username is resolved by join
-- today, which is right for an account that still exists; a key can be
-- revoked and a row must still say which one it was, and joining to a table
-- for a name is how a trail turns into blanks.
--
-- Existing rows are backfilled from what they already carry: every one of
-- them was written by a person, because until now nothing else could act. A
-- row with no actor stays with no subject rather than being invented one.

-- +goose Up
ALTER TABLE audit_logs
    ADD COLUMN subject_kind varchar(16),
    ADD COLUMN subject_id   uuid,
    ADD COLUMN subject_name text NOT NULL DEFAULT '',
    ADD COLUMN agent_id     uuid;

-- The history. Every row so far is a person's, and the ones the old shared
-- key wrote carry a null actor — those keep a null subject, which is the
-- honest answer: that credential had no identity to name.
UPDATE audit_logs a
SET subject_kind = 'USER',
    subject_id   = a.actor_id,
    subject_name = COALESCE((SELECT u.username FROM users u WHERE u.id = a.actor_id), '')
WHERE a.actor_id IS NOT NULL;

ALTER TABLE audit_logs
    ADD CONSTRAINT ck_audit_logs_subject_kind
        CHECK (subject_kind IS NULL OR subject_kind IN ('USER', 'API_KEY'));

CREATE INDEX idx_audit_logs_subject ON audit_logs (subject_kind, subject_id)
    WHERE subject_id IS NOT NULL;
CREATE INDEX idx_audit_logs_agent_id ON audit_logs (agent_id) WHERE agent_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_audit_logs_agent_id;
DROP INDEX idx_audit_logs_subject;
ALTER TABLE audit_logs
    DROP CONSTRAINT ck_audit_logs_subject_kind,
    DROP COLUMN subject_kind,
    DROP COLUMN subject_id,
    DROP COLUMN subject_name,
    DROP COLUMN agent_id;
