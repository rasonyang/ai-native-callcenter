-- SPDX-License-Identifier: Apache-2.0
-- After-call work is opened for the agent and confirmed by them.
--
-- The record used to exist only because somebody filed it, which made "no
-- record" mean two different things: a call nobody wrapped up, and a call
-- whose wrap-up was still being typed. Now the platform opens one the moment
-- after-call work begins — a default disposition, an empty note — so a
-- finished call always has a record, and `is_confirmed` is what separates one
-- an agent looked at from one that is still standing on its defaults.
--
-- Every row that exists today was written by an agent pressing the button, so
-- it is confirmed by definition. Backfilling that rather than letting the
-- default answer for them is the difference between a truthful history and a
-- completion rate that reads 0% for everything before this migration.

-- +goose Up
ALTER TABLE wrap_ups ADD COLUMN is_confirmed boolean NOT NULL DEFAULT false;
UPDATE wrap_ups SET is_confirmed = true;

CREATE INDEX idx_wrap_ups_agent_id_created_at_is_confirmed
    ON wrap_ups (agent_id, created_at DESC, is_confirmed);

-- +goose Down
DROP INDEX idx_wrap_ups_agent_id_created_at_is_confirmed;
ALTER TABLE wrap_ups DROP COLUMN is_confirmed;
