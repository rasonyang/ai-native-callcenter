-- SPDX-License-Identifier: Apache-2.0
-- A text model reads a finished campaign call and decides its lead
-- (docs/design/10-campaigns.md section 11.1, #661).
--
-- A campaign call the bot handled alone that recorded a HIGH or MEDIUM lead is
-- judged after it ends. The attempt row is the queue: the CDR's transaction
-- marks it PENDING with a deadline, a worker writes the verdict, and a sweep
-- turns a PENDING row past its deadline into NOT_JUDGED.
--
-- lead_intent stays the effective lead every consumer reads; the judgement only
-- ever rewrites it once, in the transaction that writes the verdict.
-- declared_lead_intent is the intent the flow recorded, copied when a
-- judgement became owed and never changed after. All four columns are NULL on
-- an attempt no judgement is owed for, which is every attempt before this
-- migration, so existing rows need no backfill.

-- +goose Up
ALTER TABLE campaign_attempts
    ADD COLUMN declared_lead_intent   varchar(16),
    ADD COLUMN lead_verdict           varchar(16),
    ADD COLUMN lead_verdict_reason    text,
    ADD COLUMN lead_judge_deadline_at timestamptz;
ALTER TABLE campaign_attempts ADD CONSTRAINT ck_campaign_attempts_declared_lead_intent
    CHECK (declared_lead_intent IS NULL OR declared_lead_intent IN ('HIGH', 'MEDIUM'));
ALTER TABLE campaign_attempts ADD CONSTRAINT ck_campaign_attempts_lead_verdict
    CHECK (lead_verdict IS NULL OR lead_verdict IN
        ('PENDING', 'CONFIRMED', 'UNDECIDED', 'REFUSED', 'MACHINE', 'NO_EVIDENCE', 'NOT_JUDGED'));
-- A judgement is owed exactly when the flow's intent and a deadline were
-- recorded: the three are set together or not at all.
ALTER TABLE campaign_attempts ADD CONSTRAINT ck_campaign_attempts_lead_judgement
    CHECK ((lead_verdict IS NULL) = (declared_lead_intent IS NULL)
       AND (lead_verdict IS NULL) = (lead_judge_deadline_at IS NULL));
-- The worker's queue: what is still owed, by deadline.
CREATE INDEX idx_campaign_attempts_lead_judge_pending
    ON campaign_attempts (lead_judge_deadline_at) WHERE lead_verdict = 'PENDING';

-- +goose Down
DROP INDEX idx_campaign_attempts_lead_judge_pending;
ALTER TABLE campaign_attempts DROP CONSTRAINT ck_campaign_attempts_lead_judgement;
ALTER TABLE campaign_attempts DROP CONSTRAINT ck_campaign_attempts_lead_verdict;
ALTER TABLE campaign_attempts DROP CONSTRAINT ck_campaign_attempts_declared_lead_intent;
ALTER TABLE campaign_attempts
    DROP COLUMN lead_judge_deadline_at,
    DROP COLUMN lead_verdict_reason,
    DROP COLUMN lead_verdict,
    DROP COLUMN declared_lead_intent;
