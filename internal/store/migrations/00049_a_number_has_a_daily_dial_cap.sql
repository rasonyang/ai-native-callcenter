-- SPDX-License-Identifier: Apache-2.0
-- The daily dial cap: at most AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY dials
-- to one number per local day (docs/design/10-campaigns.md section 5.2).
--
-- A contact whose number has reached the cap today is not admitted: its due
-- time moves to the start of its calling window on the next local day, and no
-- budget is spent. The move carries campaign_contacts.is_daily_cap_held, set
-- only where the cap moves the due time and cleared by every other write of
-- that time, as is_frequency_held is (migration 00048). A contact carries at
-- most one of the two marks, so the CHECK. Adding a number to the list of test
-- numbers re-arms exactly the contacts that carry either mark.
--
-- What counts toward the cap is read from two places. Every campaign attempt is
-- a row of campaign_attempts with the canonical number, so the count reads it
-- by (number_e164, started_at). An ad-hoc call (an agent's or an API's dial
-- out) is a cdrs row whose to_number is the string that was dialled, national
-- or E.164, so cdrs gains to_number_e164: the canonical callee, written by the
-- ledger for OUTBOUND rows that canonicalise and NULL for everything else. It
-- is not backfilled: ad-hoc dials made before this migration are not counted
-- toward the cap. Its CHECK is the canonical form the other number_e164
-- columns use.

-- +goose Up
ALTER TABLE campaign_contacts ADD COLUMN is_daily_cap_held boolean NOT NULL DEFAULT false;
ALTER TABLE campaign_contacts ADD CONSTRAINT ck_campaign_contacts_one_hold
    CHECK (NOT (is_frequency_held AND is_daily_cap_held));

ALTER TABLE cdrs ADD COLUMN to_number_e164 text;
ALTER TABLE cdrs ADD CONSTRAINT ck_cdrs_to_number_e164
    CHECK (to_number_e164 IS NULL OR to_number_e164 ~ '^\+[1-9][0-9]{7,14}$');

CREATE INDEX idx_campaign_attempts_number_started ON campaign_attempts (number_e164, started_at DESC);
CREATE INDEX idx_cdrs_to_number_e164_started_at ON cdrs (to_number_e164, started_at DESC)
    WHERE to_number_e164 IS NOT NULL;

-- +goose Down
DROP INDEX idx_cdrs_to_number_e164_started_at;
DROP INDEX idx_campaign_attempts_number_started;
ALTER TABLE cdrs DROP CONSTRAINT ck_cdrs_to_number_e164;
ALTER TABLE cdrs DROP COLUMN to_number_e164;
ALTER TABLE campaign_contacts DROP CONSTRAINT ck_campaign_contacts_one_hold;
ALTER TABLE campaign_contacts DROP COLUMN is_daily_cap_held;
