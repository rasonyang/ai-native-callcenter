-- SPDX-License-Identifier: Apache-2.0
-- Who is calling.
--
-- The cockpit asks one question when a call arrives — who is this number — and
-- until now the platform had no answer: an agent saw digits and whatever the
-- flow happened to collect. A contact is that answer plus somewhere to put
-- what the agent learned.
--
-- Deliberately small, and deliberately not a CRM. The phone number is unique
-- because it is the key the caller card looks up: two records for one number
-- would make that lookup a coin toss, and greeting a customer by somebody
-- else's name is worse than greeting an unknown number.
--
-- "Last contact" is read from the ledger rather than stored — a copy would be
-- wrong from the next call onwards — which is what the two indexes are for.

-- +goose Up

CREATE TABLE contacts (
    id           uuid PRIMARY KEY,
    phone_number text NOT NULL,
    name         text NOT NULL DEFAULT '',
    company      text NOT NULL DEFAULT '',
    email        text NOT NULL DEFAULT '',
    tags         text[] NOT NULL DEFAULT '{}',
    notes        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    CONSTRAINT uq_contacts_phone_number UNIQUE (phone_number)
);

CREATE INDEX idx_contacts_updated_at ON contacts (updated_at DESC);

-- A contact's last call is looked up by number on either side of a CDR: a
-- number we rang counts as much as one that rang us.
CREATE INDEX idx_cdrs_from_number ON cdrs (from_number);
CREATE INDEX idx_cdrs_to_number ON cdrs (to_number);

-- +goose Down
DROP INDEX idx_cdrs_to_number;
DROP INDEX idx_cdrs_from_number;
DROP TABLE contacts;
