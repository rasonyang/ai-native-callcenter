-- SPDX-License-Identifier: Apache-2.0
-- An extension says what it serves, so `kind` finally means something.
--
-- `kind` has been stored, validated and displayed since 00002 and read by
-- nothing: the directory serves every enabled extension regardless of it, and
-- a queue is reached through queues.ext_number and the dialplan. A number was
-- labelled AGENT or BOT and the label decided nothing.
--
-- QUEUE joins the vocabulary; PLAIN stays, and is the honest name for a number
-- with no target at all rather than a value waiting to be renamed.
--
-- Two of the three targets get a column here. The third does not, and that is
-- the point worth writing down:
--
--   AGENT is already bound, from the other side — agents.default_extension_id,
--   with uq_agents_default_extension enforcing one phone per agent and
--   fk_agents_extensions RESTRICT refusing to delete a phone somebody works
--   at. Moving that binding onto this table would take both guards with it.
--   00015 put the delete guard in the database deliberately, after an incident
--   where a silently unbound agent stayed READY and the queue kept offering
--   them calls: "the delete must fail even when it arrives by psql". A service
--   check is exactly what that migration argued against, so the binding stays
--   where the database can still refuse.
--
-- RESTRICT on both new keys, for the same reason dids.flow_id has it: a flow
-- or a queue that vanishes from under a number leaves that number answering
-- nothing, and finding out at the next call is finding out too late.
--
-- The kind/target CHECK is what stops a row from claiming two things at once.
-- Every existing row is AGENT with no targets, so it satisfies the new
-- constraint as it stands and no rewrite is needed — but the constraint is
-- installed after the columns exist precisely so a populated database
-- revalidates rather than being told to.

-- +goose Up
ALTER TABLE extensions
    ADD COLUMN flow_id  uuid,
    ADD COLUMN queue_id uuid;

ALTER TABLE extensions
    ADD CONSTRAINT fk_extensions_flows FOREIGN KEY (flow_id)
        REFERENCES flows (id) ON DELETE RESTRICT,
    ADD CONSTRAINT fk_extensions_queues FOREIGN KEY (queue_id)
        REFERENCES queues (id) ON DELETE RESTRICT;

ALTER TABLE extensions DROP CONSTRAINT extensions_kind_check;
ALTER TABLE extensions ADD CONSTRAINT extensions_kind_check
    CHECK (kind IN ('AGENT', 'BOT', 'QUEUE', 'PLAIN'));

ALTER TABLE extensions ADD CONSTRAINT extensions_target_matches_kind CHECK (
    (kind = 'BOT'   AND queue_id IS NULL)
 OR (kind = 'QUEUE' AND flow_id  IS NULL)
 OR (kind IN ('AGENT', 'PLAIN') AND flow_id IS NULL AND queue_id IS NULL)
);

-- +goose Down
ALTER TABLE extensions DROP CONSTRAINT extensions_target_matches_kind;
ALTER TABLE extensions DROP CONSTRAINT extensions_kind_check;
-- A QUEUE extension cannot survive the narrowing; there is no older value it
-- could honestly become, so it goes back to being a bare number.
UPDATE extensions SET kind = 'PLAIN' WHERE kind = 'QUEUE';
ALTER TABLE extensions ADD CONSTRAINT extensions_kind_check
    CHECK (kind IN ('AGENT', 'BOT', 'PLAIN'));
ALTER TABLE extensions DROP CONSTRAINT fk_extensions_flows;
ALTER TABLE extensions DROP CONSTRAINT fk_extensions_queues;
ALTER TABLE extensions DROP COLUMN flow_id;
ALTER TABLE extensions DROP COLUMN queue_id;
