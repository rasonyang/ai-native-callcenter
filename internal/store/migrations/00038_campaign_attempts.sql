-- SPDX-License-Identifier: Apache-2.0
-- Every call a campaign places is one row here, written before the originate is
-- sent (docs/design/10-campaigns.md sections 3.3 and 5.3), so that a call
-- always has a row and a row's outcome never disagrees with the ledger.
--
-- state is DIALING until the finish routine (section 5.4) sets the outcome,
-- once; a later, fuller CDR may replace the outcome of a FINISHED attempt. The
-- outcome is the contact's last_outcome vocabulary, byte for byte.
--
-- The partial unique index is what makes "one attempt in flight per number,
-- across every campaign" a database fact: two admitters racing for one number
-- cannot both win, and it covers "one in flight per contact" too.
--
-- call_id and leg_id are minted by the dialer. call_id is the CDR's key, so
-- the CDR and the attempt find each other with no lookup table; they are not
-- foreign keys because the CDR does not exist until the call ends.

-- +goose Up
CREATE TABLE campaign_attempts (
    id                  uuid PRIMARY KEY,
    campaign_id         uuid        NOT NULL,
    campaign_contact_id uuid        NOT NULL,
    -- Copied from the contact: the frequency check and the in-flight index
    -- read it without a join.
    number_e164         text        NOT NULL,
    call_id             uuid        NOT NULL,
    -- The customer leg's origination_uuid.
    leg_id              uuid        NOT NULL,
    -- The Job-UUID the originate's bgapi returned.
    originate_job_id    text,
    state               varchar(16) NOT NULL DEFAULT 'DIALING',
    outcome             varchar(24),
    hangup_cause        text,
    talk_sec            int,
    -- True when the outcome reached the customer (counts against maxAttempts).
    is_touched          boolean     NOT NULL DEFAULT false,
    -- What the call recorded (design section 11); NULL when it recorded none.
    lead_intent         varchar(16),
    lead_notes          text,
    started_at          timestamptz NOT NULL DEFAULT now(),
    finished_at         timestamptz,
    CONSTRAINT fk_campaign_attempts_campaigns FOREIGN KEY (campaign_id)
        REFERENCES campaigns (id) ON DELETE CASCADE,
    CONSTRAINT fk_campaign_attempts_contacts FOREIGN KEY (campaign_contact_id)
        REFERENCES campaign_contacts (id) ON DELETE CASCADE,
    CONSTRAINT uq_campaign_attempts_call_id UNIQUE (call_id),
    CONSTRAINT uq_campaign_attempts_leg_id UNIQUE (leg_id),
    CONSTRAINT ck_campaign_attempts_number_e164 CHECK (number_e164 ~ '^\+[1-9][0-9]{7,14}$'),
    CONSTRAINT ck_campaign_attempts_state CHECK (state IN ('DIALING', 'FINISHED')),
    CONSTRAINT ck_campaign_attempts_outcome CHECK (outcome IS NULL OR outcome IN
        ('ANSWERED', 'BOT_FAILED', 'OVERFLOW', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN',
         'TRANSIENT', 'ORIGINATE_FAILED', 'INVALID_NUMBER', 'DNC_SKIPPED')),
    -- An attempt in flight has no outcome and has not finished; a finished one
    -- has both.
    CONSTRAINT ck_campaign_attempts_finished CHECK (
        (state = 'DIALING' AND outcome IS NULL AND finished_at IS NULL AND NOT is_touched)
        OR (state = 'FINISHED' AND outcome IS NOT NULL AND finished_at IS NOT NULL)),
    CONSTRAINT ck_campaign_attempts_talk_sec CHECK (talk_sec IS NULL OR talk_sec >= 0),
    CONSTRAINT ck_campaign_attempts_lead_intent CHECK (lead_intent IS NULL OR lead_intent IN
        ('HIGH', 'MEDIUM', 'LOW', 'NONE', 'DO_NOT_CALL'))
);

CREATE UNIQUE INDEX uq_campaign_attempts_dialing_number_e164
    ON campaign_attempts (number_e164) WHERE state = 'DIALING';

-- A contact's attempts, latest first: "is this still its latest attempt".
CREATE INDEX idx_campaign_attempts_contact ON campaign_attempts (campaign_contact_id, started_at DESC, id DESC);
-- The breakers read a campaign's recent endings.
CREATE INDEX idx_campaign_attempts_finished ON campaign_attempts (campaign_id, finished_at DESC, id DESC)
    WHERE state = 'FINISHED';
-- The cross-campaign frequency check: who reached this number lately.
CREATE INDEX idx_campaign_attempts_touched ON campaign_attempts (number_e164, started_at DESC)
    WHERE is_touched;

-- +goose Down
DROP TABLE campaign_attempts;
