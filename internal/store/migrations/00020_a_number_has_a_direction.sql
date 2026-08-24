-- SPDX-License-Identifier: Apache-2.0
-- A number says which way calls go through it, and which number a call this
-- platform places comes from.
--
-- Two booleans rather than an enum or an array (D7). A number that both takes
-- calls and places them is ordinary, an enum cannot say it without a third
-- value meaning "both", and an array makes every reader parse a set to answer
-- a yes/no question.
--
-- flow_id stops being a column-wide rule and becomes a conditional one (D8).
-- Requiring it of every number would forbid a legitimate row — a number used
-- only for dialling out has no bot to bind, and nobody ever calls it. What P4
-- actually rules on is that a number a caller can *reach* answers with a flow,
-- and that is what the CHECK now says. This supersedes the plan recorded in
-- m4-cleanup-findings F-01 to restore NOT NULL once a flow picker existed; the
-- picker exists (W3) and the stronger column would now be wrong.
--
-- The rewrite runs first, because this narrowing has a row to catch. 95099 is
-- enabled, inbound by default, and has no flow and no fallback queue: a call
-- to it reaches nothing today. Under the new rule it cannot claim to take
-- inbound calls, so it does not — it becomes outbound-only, which is the one
-- shape a flowless number is allowed to have. Nothing is invented about it:
-- it was not answering inbound calls before this migration either.
--
-- The default outbound number is a partial unique index (D9), so "at most one"
-- is the database's answer rather than something three call sites remember to
-- check. A number that cannot dial out cannot be the default, which the second
-- CHECK settles rather than leaving to a form.

-- +goose Up
ALTER TABLE dids
    ADD COLUMN allow_inbound       boolean NOT NULL DEFAULT true,
    ADD COLUMN allow_outbound      boolean NOT NULL DEFAULT false,
    ADD COLUMN is_default_outbound boolean NOT NULL DEFAULT false;

UPDATE dids
SET allow_inbound = false, allow_outbound = true
WHERE flow_id IS NULL;

ALTER TABLE dids ADD CONSTRAINT dids_go_somewhere
    CHECK (allow_inbound OR allow_outbound);
ALTER TABLE dids ADD CONSTRAINT dids_inbound_answers_with_a_flow
    CHECK (NOT allow_inbound OR flow_id IS NOT NULL);
ALTER TABLE dids ADD CONSTRAINT dids_default_outbound_dials_out
    CHECK (NOT is_default_outbound OR allow_outbound);

CREATE UNIQUE INDEX uq_dids_default_outbound
    ON dids (is_default_outbound) WHERE is_default_outbound;

-- +goose Down
DROP INDEX uq_dids_default_outbound;
ALTER TABLE dids DROP CONSTRAINT dids_default_outbound_dials_out;
ALTER TABLE dids DROP CONSTRAINT dids_inbound_answers_with_a_flow;
ALTER TABLE dids DROP CONSTRAINT dids_go_somewhere;
ALTER TABLE dids
    DROP COLUMN is_default_outbound,
    DROP COLUMN allow_outbound,
    DROP COLUMN allow_inbound;
