-- SPDX-License-Identifier: Apache-2.0
-- AI outbound campaigns and the contacts they dial (docs/design/10-campaigns.md
-- sections 3.1 and 3.2).
--
-- This migration is the model only. The dialer's attempts table, the CDR's
-- campaign_id and the DID line limit arrive with the slices that use them; the
-- contact columns the dialer and the lead recorder will write (attempts,
-- system_failures, next_attempt_at, last_outcome, lead_intent, lead_notes)
-- are here now so that those slices add no migration to a table that already
-- holds a customer's list.
--
-- Every enum is a CHECK, byte-identical to the contract's SCREAMING_SNAKE
-- values (docs/design/07-naming.md). A campaign names a DID, and the DID names
-- the flow and the caller id, so the DID cannot be deleted while a campaign
-- refers to it. The follow-up queue is optional and only a destination, so a
-- queue that goes away leaves the campaign without one.

-- +goose Up
CREATE TABLE campaigns (
    id                   uuid PRIMARY KEY,
    name                 text        NOT NULL,
    did_id               uuid        NOT NULL,
    status               varchar(16) NOT NULL DEFAULT 'DRAFT',
    -- Only a PAUSED campaign has a reason; MANUAL is an operator, the other
    -- two are breakers (design 7.4) written by the dialer.
    pause_reason         varchar(24),
    -- IANA name: the default for contacts that carry none.
    time_zone            text        NOT NULL,
    -- {"days": ["MON", ...], "ranges": [{"start": "09:00", "end": "17:00"}]}
    calling_window       jsonb       NOT NULL,
    -- Digits, no "+": completes a national number on import. NULL falls back
    -- to the deployment's AICC_DEFAULT_COUNTRY_CODE.
    default_country_code varchar(3),
    max_concurrent       int         NOT NULL,
    ring_timeout_sec     int         NOT NULL,
    retry_policy         jsonb       NOT NULL,
    follow_up_queue_id   uuid,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    started_at           timestamptz,
    finished_at          timestamptz,
    CONSTRAINT fk_campaigns_dids FOREIGN KEY (did_id) REFERENCES dids (id) ON DELETE RESTRICT,
    CONSTRAINT fk_campaigns_queues FOREIGN KEY (follow_up_queue_id)
        REFERENCES queues (id) ON DELETE SET NULL,
    CONSTRAINT ck_campaigns_name CHECK (btrim(name) <> ''),
    CONSTRAINT ck_campaigns_status CHECK (status IN
        ('DRAFT', 'RUNNING', 'PAUSED', 'COMPLETED', 'CANCELLED')),
    CONSTRAINT ck_campaigns_pause_reason CHECK (
        pause_reason IS NULL
        OR (status = 'PAUSED' AND pause_reason IN ('MANUAL', 'TRUNK_FAILURE', 'PROVIDER_FAILURE'))),
    CONSTRAINT ck_campaigns_calling_window CHECK (jsonb_typeof(calling_window) = 'object'),
    CONSTRAINT ck_campaigns_default_country_code CHECK (
        default_country_code IS NULL OR default_country_code ~ '^[1-9][0-9]{0,2}$'),
    CONSTRAINT ck_campaigns_max_concurrent CHECK (max_concurrent >= 1),
    CONSTRAINT ck_campaigns_ring_timeout_sec CHECK (ring_timeout_sec >= 5),
    CONSTRAINT ck_campaigns_retry_policy CHECK (jsonb_typeof(retry_policy) = 'object'),
    -- A campaign has finished exactly when it is in an absorbing state.
    CONSTRAINT ck_campaigns_finished_at CHECK (
        (status IN ('COMPLETED', 'CANCELLED')) = (finished_at IS NOT NULL))
);

CREATE INDEX idx_campaigns_created_at ON campaigns (created_at DESC, id);
CREATE INDEX idx_campaigns_status ON campaigns (status);
CREATE INDEX idx_campaigns_did_id ON campaigns (did_id);

CREATE TABLE campaign_contacts (
    id              uuid PRIMARY KEY,
    campaign_id     uuid        NOT NULL,
    -- Canonical E.164 (internal/phonenum): the dedup key within a campaign.
    number_e164     text        NOT NULL,
    -- As imported, for the operator.
    number_raw      text        NOT NULL,
    -- Overrides the campaign's time zone when set.
    time_zone       text,
    -- Handed to the flow as userData.
    slots           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    status          varchar(16) NOT NULL DEFAULT 'ACTIVE',
    -- Attempts that reached the customer (against maxAttempts).
    attempts        int         NOT NULL DEFAULT 0,
    -- Attempts that never reached the customer (against maxSystemRetries).
    system_failures int         NOT NULL DEFAULT 0,
    -- When it may be dialled next; NULL unless ACTIVE.
    next_attempt_at timestamptz,
    last_outcome    varchar(24),
    lead_intent     varchar(16),
    lead_notes      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fk_campaign_contacts_campaigns FOREIGN KEY (campaign_id)
        REFERENCES campaigns (id) ON DELETE CASCADE,
    CONSTRAINT uq_campaign_contacts_number UNIQUE (campaign_id, number_e164),
    CONSTRAINT ck_campaign_contacts_number_e164 CHECK (number_e164 ~ '^\+[1-9][0-9]{7,14}$'),
    CONSTRAINT ck_campaign_contacts_slots CHECK (jsonb_typeof(slots) = 'object'),
    CONSTRAINT ck_campaign_contacts_status CHECK (status IN
        ('ACTIVE', 'DONE', 'EXHAUSTED', 'INVALID_NUMBER', 'DNC')),
    CONSTRAINT ck_campaign_contacts_attempts CHECK (attempts >= 0),
    CONSTRAINT ck_campaign_contacts_system_failures CHECK (system_failures >= 0),
    CONSTRAINT ck_campaign_contacts_next_attempt_at CHECK (
        next_attempt_at IS NULL OR status = 'ACTIVE'),
    CONSTRAINT ck_campaign_contacts_last_outcome CHECK (last_outcome IS NULL OR last_outcome IN
        ('ANSWERED', 'BOT_FAILED', 'OVERFLOW', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN',
         'TRANSIENT', 'ORIGINATE_FAILED', 'INVALID_NUMBER', 'DNC_SKIPPED')),
    CONSTRAINT ck_campaign_contacts_lead_intent CHECK (lead_intent IS NULL OR lead_intent IN
        ('HIGH', 'MEDIUM', 'LOW', 'NONE', 'DO_NOT_CALL'))
);

-- The dialer's admission scan and the listing by status.
CREATE INDEX idx_campaign_contacts_due ON campaign_contacts (campaign_id, status, next_attempt_at);
CREATE INDEX idx_campaign_contacts_lead ON campaign_contacts (campaign_id, lead_intent)
    WHERE lead_intent IS NOT NULL;

-- +goose Down
DROP TABLE campaign_contacts;
DROP TABLE campaigns;
