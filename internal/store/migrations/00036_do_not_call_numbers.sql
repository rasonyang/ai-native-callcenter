-- SPDX-License-Identifier: Apache-2.0
-- The do-not-call list: numbers this platform must never place a call to.
--
-- Global, not per campaign (docs/design/10-campaigns.md section 3.4). The key
-- is the canonical E.164 number (internal/phonenum), so "+86 138-0013-8000",
-- "0086 138 0013 8000" and, with the deployment's default country code,
-- "13800138000" are one entry, and the check on a dial is a primary-key
-- lookup.
--
-- source says how the number got here: MANUAL (somebody typed it), IMPORT (a
-- bulk load) or CONVERSATION (the customer asked on a call; call_id is that
-- call). created_by is the signed-in account that added it; a number added by
-- an API key has none, and the audit trail names the key.

-- +goose Up
CREATE TABLE dnc_numbers (
    number_e164 text        PRIMARY KEY,
    source      varchar(16) NOT NULL,
    call_id     uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    CONSTRAINT ck_dnc_numbers_number_e164 CHECK (number_e164 ~ '^\+[1-9][0-9]{7,14}$'),
    CONSTRAINT ck_dnc_numbers_source CHECK (source IN ('MANUAL', 'IMPORT', 'CONVERSATION')),
    CONSTRAINT ck_dnc_numbers_call_id CHECK (call_id IS NULL OR source = 'CONVERSATION')
);

-- The listing is newest first.
CREATE INDEX idx_dnc_numbers_created_at ON dnc_numbers (created_at DESC, number_e164);

-- +goose Down
DROP TABLE dnc_numbers;
