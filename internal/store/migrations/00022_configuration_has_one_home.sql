-- SPDX-License-Identifier: Apache-2.0
-- The settings table goes. Configuration has one home in this product and it
-- is not the database.
--
-- Created in 00001 for "org, locale default, retentionDays, …" and never
-- written to by anything. Meanwhile every setting this product actually has —
-- forty-nine of them — is an AICC_* environment variable, with .env.example as
-- the registry that CLAUDE.md requires be kept in step with config.go. A
-- key/value table beside that is a second configuration system competing with
-- the first, and the answer to "where is this configured" stops being one
-- place.
--
-- The one entry that had a designed reader was retentionDays, and that is
-- being built in this same change — as AICC_RECORDING_RETENTION_DAYS, like
-- every other setting, rather than as the one row that would justify the
-- table.
--
-- recordings.deleted_at is untouched and stays load-bearing: every read of a
-- recording already filters on it, which is what makes a retention sweep
-- expressible at all.
--
-- Zero rows, in every deployment.

-- +goose Up
DROP TABLE settings;

-- +goose Down
CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
