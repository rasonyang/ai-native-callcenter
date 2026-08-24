-- SPDX-License-Identifier: Apache-2.0
-- `kind` and `queue_id` leave the extensions table. What remains is what an
-- extension has always actually been: a number a phone can register at, and
-- the agent it belongs to.
--
-- 00018 left one kind standing. A column with one legal value classifies
-- nothing, and every form that offers it asks a question with one answer.
--
-- queue_id never had a use, and following it shows why the whole idea was
-- wrong. This table feeds luacc.directory, which is SIP *registration*: a row
-- here is an account a phone can register as, with a password. A queue is not
-- something anything registers as — it is dialled, and aicc_queue.lua reaches
-- it through luacc.queues WHERE ext_number, which never consults this table.
-- Giving a queue a row here would not have made it reachable; it would have
-- made its number registerable, which is an exposure bought for nothing.
--
-- A queue's number stays where it already lives and already has its own unique
-- constraint: queues.ext_number. What changes above this migration is that it
-- is allocated from a pool rather than typed, the same way an agent's is.
--
-- No data to move. No extension has ever pointed at a queue — 00017 added the
-- column and nothing wrote to it — and every row is the one remaining kind.

-- +goose Up
ALTER TABLE extensions DROP CONSTRAINT extensions_target_matches_kind;
ALTER TABLE extensions DROP CONSTRAINT extensions_kind_check;
ALTER TABLE extensions DROP CONSTRAINT fk_extensions_queues;
ALTER TABLE extensions DROP COLUMN queue_id;
ALTER TABLE extensions DROP COLUMN kind;

-- +goose Down
ALTER TABLE extensions ADD COLUMN kind character varying(16) NOT NULL DEFAULT 'AGENT';
ALTER TABLE extensions ADD COLUMN queue_id uuid;
ALTER TABLE extensions ADD CONSTRAINT fk_extensions_queues FOREIGN KEY (queue_id)
    REFERENCES queues (id) ON DELETE RESTRICT;
ALTER TABLE extensions ADD CONSTRAINT extensions_kind_check
    CHECK (kind IN ('AGENT', 'QUEUE'));
ALTER TABLE extensions ADD CONSTRAINT extensions_target_matches_kind
    CHECK (kind = 'QUEUE' OR queue_id IS NULL);
