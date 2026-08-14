-- SPDX-License-Identifier: Apache-2.0
-- +goose Up

-- A callback can be claimed before it is done: the agent who picks it up owns
-- it, visibly, so two agents do not ring the same person.
ALTER TABLE callbacks DROP CONSTRAINT callbacks_status_check;
ALTER TABLE callbacks ADD CONSTRAINT callbacks_status_check
    CHECK (status IN ('OPEN', 'CLAIMED', 'DONE', 'DISMISSED'));

-- +goose Down
ALTER TABLE callbacks DROP CONSTRAINT callbacks_status_check;
ALTER TABLE callbacks ADD CONSTRAINT callbacks_status_check
    CHECK (status IN ('OPEN', 'DONE', 'DISMISSED'));
