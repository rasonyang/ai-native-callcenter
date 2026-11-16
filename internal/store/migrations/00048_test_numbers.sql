-- SPDX-License-Identifier: Apache-2.0
-- The list of test numbers: numbers the cross-campaign frequency window does
-- not hold back (docs/design/10-campaigns.md section 5.2).
--
-- A team that tests campaigns on its own phones runs the same few numbers
-- through one campaign after another, and the window keeps the second campaign
-- from dialling them for a day. Every entry gives up that protection for its
-- number. The protection exists for customers, so the list is small (the API
-- refuses a 101st entry) and nothing else about a number on it changes: the
-- do-not-call list, the calling windows and a call already in flight apply to it
-- as to any other.
--
-- The key is the canonical E.164 number (internal/phonenum), as on the
-- do-not-call list, so the dialer's check is a primary-key lookup. created_by is
-- the signed-in account that added the number; one added by an API key has none,
-- and the audit trail names the key.

-- An attempt records whether its number was on the list when the attempt was
-- placed (campaign_attempts.is_test_number), so the outcome summary can leave
-- test calls out. It is history: taking a number off the list later does not
-- change the attempts already made. Attempts that exist before this migration
-- were not to a listed number, since there was no list.

-- A contact the frequency window deferred carries campaign_contacts.is_frequency_held,
-- set only where the hold moves its due time and cleared by every other write of
-- that time. Adding a number to the list re-arms exactly the contacts that carry
-- it, so a campaign's own retry time is never mistaken for a hold.

-- +goose Up
CREATE TABLE test_numbers (
    number_e164 text         PRIMARY KEY,
    note        varchar(200),
    created_at  timestamptz  NOT NULL DEFAULT now(),
    created_by  uuid,
    CONSTRAINT ck_test_numbers_number_e164 CHECK (number_e164 ~ '^\+[1-9][0-9]{7,14}$')
);

-- The listing is newest first.
CREATE INDEX idx_test_numbers_created_at ON test_numbers (created_at DESC, number_e164);

ALTER TABLE campaign_attempts ADD COLUMN is_test_number boolean NOT NULL DEFAULT false;
ALTER TABLE campaign_contacts ADD COLUMN is_frequency_held boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE campaign_contacts DROP COLUMN is_frequency_held;
ALTER TABLE campaign_attempts DROP COLUMN is_test_number;
DROP TABLE test_numbers;
