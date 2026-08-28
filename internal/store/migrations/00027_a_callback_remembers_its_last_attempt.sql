-- SPDX-License-Identifier: Apache-2.0
-- A callback remembers the last time somebody tried to keep it.
--
-- Ringing the customer is not the same as keeping the promise: they may not
-- answer, may be busy, may ask to be called later. So a dial does not close
-- the callback — it is noted on it. The agent dials from the row, the call's
-- id is written here at once, and when that call's CDR lands its status is
-- copied back. The row then reads "tried at 10:42, no answer" and stays in
-- the pool, or "tried at 10:42, answered" and waits for the agent to say
-- whether that settled it.

-- +goose Up
ALTER TABLE callbacks
    ADD COLUMN last_attempt_call_id uuid,
    ADD COLUMN last_attempt_at      timestamptz,
    ADD COLUMN last_attempt_status  varchar(16);

CREATE INDEX idx_callbacks_last_attempt_call_id
    ON callbacks (last_attempt_call_id) WHERE last_attempt_call_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_callbacks_last_attempt_call_id;
ALTER TABLE callbacks
    DROP COLUMN last_attempt_call_id,
    DROP COLUMN last_attempt_at,
    DROP COLUMN last_attempt_status;
