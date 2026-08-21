-- SPDX-License-Identifier: Apache-2.0
-- What the carrier bills for, kept apart from what an agent worked.
--
-- The ledger had one notion of a call being answered and used it for both
-- questions. A call the bot picked up and no agent ever took is billed by the
-- carrier from the moment the switch sent 200 OK — every second of it — while
-- for the queue it is a call nobody answered. Reading one column for both
-- meant whichever question was asked second got the wrong answer.
--
-- bill_sec is the carrier's number: answered_at to ended_at, regardless of who
-- if anyone took the call.
--
-- The backfill is best-effort and not a recomputation. Before the change that
-- accompanies this migration, answered_at held whichever answer came to hand —
-- the agent's pickup on calls somebody took, the caller's own leg on calls the
-- bot handled alone. So historic rows for calls that reached an agent will
-- come out short by however long the bot spoke first. Rows the switch never
-- answered get 0, which is right. Trustworthy bill_sec starts from the first
-- call recorded after this migration; earlier rows are an estimate and should
-- be read as one.

-- +goose Up
ALTER TABLE cdrs ADD COLUMN bill_sec int NOT NULL DEFAULT 0 CHECK (bill_sec >= 0);

UPDATE cdrs
   SET bill_sec = GREATEST(0, EXTRACT(EPOCH FROM (ended_at - answered_at))::int)
 WHERE answered_at IS NOT NULL;

-- +goose Down
ALTER TABLE cdrs DROP COLUMN bill_sec;
