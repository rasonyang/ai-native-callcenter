-- SPDX-License-Identifier: Apache-2.0
-- OUT_OF_HOURS leaves the missed_reason vocabulary. Five values remain, and
-- they are exactly the five a call can be given.
--
-- The reason is derived, never stamped: CDRAssembler.missedReason reads the
-- recorded facts of the call — did the caller queue, had a phone begun
-- ringing when they went, did the queue give up on them — and returns one of
-- five strings. OUT_OF_HOURS is not among them and never was. Nothing decides
-- it because nothing in this product knows what hours a queue keeps: there is
-- no schedule on queues, no calendar, and no dialplan branch that would refuse
-- a caller for arriving late. It was written into 00005 from design 03 as a
-- value the schema might one day need.
--
-- Left in place it is worse than unused. The contract now names this
-- vocabulary as an enum, so every value here is a promise to a client that
-- some call, someday, will carry it — and a word the console must translate
-- into two languages to avoid rendering a raw key. Neither is worth paying for
-- a value the code cannot produce.
--
-- Should out-of-hours routing ever be built, the schedule is the hard part and
-- this line is the easy one; it can come back with the feature that means it.
--
-- The rewrite runs before the narrowed CHECK, as it must: a deployment holding
-- such a row would otherwise be refused the migration outright. No row can
-- hold it — nothing has ever written it — but a CHECK is a claim about a
-- table, not about the code that happened to fill it, and the honest way to
-- narrow one is to make it true first. A missed call whose only reason was
-- OUT_OF_HOURS keeps its status and loses the reason: "unserved, and the
-- ledger does not say why" is the truth once the word is gone.

-- +goose Up
UPDATE cdrs SET missed_reason = NULL WHERE missed_reason = 'OUT_OF_HOURS';

ALTER TABLE cdrs DROP CONSTRAINT cdrs_missed_reason_check;
ALTER TABLE cdrs ADD CONSTRAINT cdrs_missed_reason_check
    CHECK (missed_reason IN ('SHORT_ABANDONED', 'ABANDONED_RINGING',
                             'ABANDONED_WAITING', 'AGENTS_DID_NOT_ANSWER',
                             'NO_AVAILABLE_AGENT'));

-- +goose Down
ALTER TABLE cdrs DROP CONSTRAINT cdrs_missed_reason_check;
ALTER TABLE cdrs ADD CONSTRAINT cdrs_missed_reason_check
    CHECK (missed_reason IN ('SHORT_ABANDONED', 'ABANDONED_RINGING',
                             'ABANDONED_WAITING', 'AGENTS_DID_NOT_ANSWER',
                             'NO_AVAILABLE_AGENT', 'OUT_OF_HOURS'));
