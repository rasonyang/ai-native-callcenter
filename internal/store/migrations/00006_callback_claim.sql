-- SPDX-License-Identifier: Apache-2.0
-- +goose Up

-- A callback can be claimed before it is done: the agent who picks it up owns
-- it, visibly, so two agents do not ring the same person.
ALTER TABLE callbacks DROP CONSTRAINT callbacks_status_check;
ALTER TABLE callbacks ADD CONSTRAINT callbacks_status_check
    CHECK (status IN ('OPEN', 'CLAIMED', 'DONE', 'DISMISSED'));

-- +goose Down
-- The older vocabulary has no claim: a claimed callback is still an unkept
-- promise, so it goes back to the OPEN work queue with its claimant cleared
-- (before 00006 only a DONE or DISMISSED row named who handled it). The rows
-- are rewritten first, or the narrower CHECK is refused as violated.
ALTER TABLE callbacks DROP CONSTRAINT callbacks_status_check;
UPDATE callbacks SET status = 'OPEN', handled_by = NULL WHERE status = 'CLAIMED';
ALTER TABLE callbacks ADD CONSTRAINT callbacks_status_check
    CHECK (status IN ('OPEN', 'DONE', 'DISMISSED'));
