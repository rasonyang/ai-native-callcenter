-- SPDX-License-Identifier: Apache-2.0
-- queue_events gains the index for reading one call's journey.
--
-- The table has been written since the beginning and read by nothing: one
-- INSERT in ledger.sql and no SELECT anywhere (C7). Its only index is
-- (queue_id, occurred_at DESC), which serves a per-queue view nobody built.
--
-- What was invisible for want of a reader: 273 OFFERED rows against 89 queued
-- calls, and seven calls offered ten to thirty-three times each. The CDR for
-- one of those says ABANDONED_WAITING and 220 seconds; that it was offered
-- thirty-three times in three and a half minutes exists only here. C37 — an
-- agent's phone wedged and rejecting every delivery — is open for want of a
-- reproducible case while this table holds seven instances of its shape.
--
-- Ordering is (occurred_at, id) rather than occurred_at alone: two movements
-- of the same call can share a millisecond, and a journey that reorders
-- between two reads is not a journey.
--
-- call_id is nullable — a queue movement the switch could not attribute to a
-- call still gets recorded — so this index deliberately covers only the rows
-- that can be asked for.

-- +goose Up
CREATE INDEX idx_queue_events_call_id_occurred_at
    ON queue_events (call_id, occurred_at, id)
    WHERE call_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_queue_events_call_id_occurred_at;
