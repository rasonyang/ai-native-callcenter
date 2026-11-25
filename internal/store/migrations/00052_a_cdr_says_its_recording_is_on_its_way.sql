-- SPDX-License-Identifier: Apache-2.0
-- A CDR says its recording is still on its way (#538). With a spool, the
-- recording is uploaded and booked seconds after the call ends — a client
-- that reads the row as soon as it exists saw has_recording false and could
-- not tell "recording started, object pending" from "never recorded".
--
-- recording_expected is written from the call snapshot's IsRecordingStarted:
-- the switch announced RECORD_START, so an object is owed unless the upload
-- failed. Together with has_recording it gives three states:
--   stored  = has_recording                    (the object exists)
--   pending = recording_expected AND NOT has_recording
--   none    = not recording_expected           (recording never started)
-- The API derives that ordering; the column only carries the fact.

-- +goose Up
ALTER TABLE cdrs
    ADD COLUMN recording_expected bool NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE cdrs
    DROP COLUMN recording_expected;
