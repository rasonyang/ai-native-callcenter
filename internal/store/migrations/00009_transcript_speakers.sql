-- SPDX-License-Identifier: Apache-2.0
-- The transcript stops being an AI-leg artefact and becomes the call's record.
--
-- 00005 built `transcripts` for one producer: the bot session, writing both
-- sides of an AI leg in one batch at hangup. Carrying the conversation through
-- a transfer means a second producer writing the human phase live, so the row
-- has to say three things it never had to say before — who spoke, on which
-- leg, and which engine heard them.
--
-- `role` is renamed to `speaker` because `role` already means two other things
-- in this contract (the user's ADMIN/SUPERVISOR/AGENT and the party's
-- ORIGINATOR/TARGET), and 07-naming exists to kill exactly that collision.
-- `CALLER` becomes `CUSTOMER` in the same move: synonyms for one concept are
-- forbidden, so the two cannot both survive, and a call now has a third
-- speaker — the human agent — that the old two-value check could not express.
--
-- The idempotency index is partial on purpose. Every row written before this
-- migration has an empty utterance_id, and a redelivered final must not
-- duplicate; a plain unique index would collide the historical rows with each
-- other.

-- +goose Up
ALTER TABLE transcripts RENAME COLUMN role TO speaker;
ALTER TABLE transcripts DROP CONSTRAINT transcripts_role_check;
UPDATE transcripts SET speaker = 'CUSTOMER' WHERE speaker = 'CALLER';
ALTER TABLE transcripts ALTER COLUMN speaker TYPE varchar(16);
ALTER TABLE transcripts ADD CONSTRAINT transcripts_speaker_check
    CHECK (speaker IN ('CUSTOMER', 'BOT', 'HUMAN_AGENT'));

ALTER TABLE transcripts ADD COLUMN party_id uuid;
ALTER TABLE transcripts ADD COLUMN agent_id uuid;
ALTER TABLE transcripts ADD COLUMN offset_ms int NOT NULL DEFAULT 0;
ALTER TABLE transcripts ADD COLUMN language varchar(8) NOT NULL DEFAULT '';
ALTER TABLE transcripts ADD COLUMN source varchar(16) NOT NULL DEFAULT 'MODEL'
    CHECK (source IN ('MODEL', 'ASR'));
ALTER TABLE transcripts ADD COLUMN provider varchar(32) NOT NULL DEFAULT '';
ALTER TABLE transcripts ADD COLUMN utterance_id text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX uq_transcripts_call_id_utterance_id
    ON transcripts (call_id, utterance_id) WHERE utterance_id <> '';

-- +goose Down
DROP INDEX uq_transcripts_call_id_utterance_id;
ALTER TABLE transcripts DROP COLUMN utterance_id;
ALTER TABLE transcripts DROP COLUMN provider;
ALTER TABLE transcripts DROP COLUMN source;
ALTER TABLE transcripts DROP COLUMN language;
ALTER TABLE transcripts DROP COLUMN offset_ms;
ALTER TABLE transcripts DROP COLUMN agent_id;
ALTER TABLE transcripts DROP COLUMN party_id;

ALTER TABLE transcripts DROP CONSTRAINT transcripts_speaker_check;
UPDATE transcripts SET speaker = 'CALLER' WHERE speaker = 'CUSTOMER';
DELETE FROM transcripts WHERE speaker = 'HUMAN_AGENT';
ALTER TABLE transcripts ALTER COLUMN speaker TYPE varchar(8);
ALTER TABLE transcripts ADD CONSTRAINT transcripts_role_check
    CHECK (speaker IN ('BOT', 'CALLER'));
ALTER TABLE transcripts RENAME COLUMN speaker TO role;
