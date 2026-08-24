-- SPDX-License-Identifier: Apache-2.0
-- The trunks table goes. Nothing ever wrote to it and nothing could have read
-- it into effect.
--
-- It was created in 00002 against a plan for managing trunks from the product,
-- and 2026-08-20's S3 decision kept it on that basis. Following what such a
-- screen would have to do is what retired it: a gateway is defined in the
-- switch's own sofia profile XML and read when that profile loads. For a row
-- here to become a gateway there would have to be a luacc.trunks view, an
-- aicc_xml.lua that served the configuration section's sofia part rather than
-- callcenter.conf alone, and a profile rescan on every edit — a body of switch
-- work for a thing a single-host deployment has one of and configures once.
--
-- D6 had already said it: the gateway is system-level configuration. A screen
-- that edits it says the opposite, and it could not have kept its promise
-- anyway — the definition lives in a file on the host that this application
-- does not write. W11.1 showed exactly that, renaming pstn_sim to
-- pstn_gateway: the repository could only change half of it.
--
-- What replaces it is the half that was real: the trunk's live state, read
-- from the switch and shown, so an operator whose outbound calls are failing
-- can see whether the trunk is there. Reading it needs no table.
--
-- Zero rows, in every deployment: nothing has ever inserted one.

-- +goose Up
DROP TABLE trunks;

-- +goose Down
CREATE TABLE trunks (
    id           uuid PRIMARY KEY,
    name         text NOT NULL,
    direction    character varying(16) NOT NULL DEFAULT 'BIDIRECTIONAL',
    max_channels integer NOT NULL DEFAULT 0,
    config       jsonb NOT NULL DEFAULT '{}',
    is_enabled   boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_trunks_name UNIQUE (name),
    CONSTRAINT trunks_direction_check
        CHECK (direction IN ('INBOUND', 'OUTBOUND', 'BIDIRECTIONAL'))
);
