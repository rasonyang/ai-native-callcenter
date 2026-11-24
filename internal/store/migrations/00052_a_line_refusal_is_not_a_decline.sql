-- SPDX-License-Identifier: Apache-2.0
-- A line that refuses a call is not a callee who declined it
-- (docs/design/10-campaigns.md section 7.2).
--
-- The switch reports a carrier's own refusal (402 Payment Required, 401 or 407
-- for the line's authentication, 403 Forbidden, 608 Rejected) with the same
-- hangup cause as a callee's 603 Decline, CALL_REJECTED. The final SIP status
-- tells them apart, so an attempt now keeps it (sip_status, NULL when the
-- switch reported none) and the refusal has an outcome of its own,
-- LINE_REJECTED, a system failure paid for as TRANSIENT.
--
-- Going up nothing is rewritten in the attempts or the contacts: both outcome
-- CHECKs only widen, and sip_status is NULL on every attempt made before (no
-- backfill; the CDR's tech still holds the status). A campaign whose retry
-- policy retries TRANSIENT is given LINE_REJECTED beside it, because the two
-- are retried alike and a policy without it would end a contact EXHAUSTED on
-- its first refusal.

-- +goose Up
ALTER TABLE campaign_attempts ADD COLUMN sip_status int;
ALTER TABLE campaign_attempts ADD CONSTRAINT ck_campaign_attempts_sip_status
    CHECK (sip_status IS NULL OR sip_status BETWEEN 100 AND 699);

ALTER TABLE campaign_attempts DROP CONSTRAINT ck_campaign_attempts_outcome;
ALTER TABLE campaign_attempts ADD CONSTRAINT ck_campaign_attempts_outcome CHECK (outcome IS NULL OR outcome IN
    ('ANSWERED', 'BOT_FAILED', 'OVERFLOW', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN',
     'TRANSIENT', 'LINE_REJECTED', 'ORIGINATE_FAILED', 'INVALID_NUMBER', 'DNC_SKIPPED'));

ALTER TABLE campaign_contacts DROP CONSTRAINT ck_campaign_contacts_last_outcome;
ALTER TABLE campaign_contacts ADD CONSTRAINT ck_campaign_contacts_last_outcome CHECK (last_outcome IS NULL OR last_outcome IN
    ('ANSWERED', 'BOT_FAILED', 'OVERFLOW', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN',
     'TRANSIENT', 'LINE_REJECTED', 'ORIGINATE_FAILED', 'INVALID_NUMBER', 'DNC_SKIPPED'));

-- LINE_REJECTED goes right after TRANSIENT, where the default policy lists it.
UPDATE campaigns c
SET retry_policy = jsonb_set(c.retry_policy, '{retryOutcomes}', (
    SELECT jsonb_agg(e.o ORDER BY e.pos)
    FROM (
        SELECT o, ord::numeric AS pos
        FROM jsonb_array_elements(c.retry_policy -> 'retryOutcomes') WITH ORDINALITY AS a(o, ord)
        UNION ALL
        SELECT '"LINE_REJECTED"'::jsonb, ord + 0.5
        FROM jsonb_array_elements(c.retry_policy -> 'retryOutcomes') WITH ORDINALITY AS a(o, ord)
        WHERE o = '"TRANSIENT"'::jsonb
    ) e))
WHERE jsonb_typeof(c.retry_policy -> 'retryOutcomes') = 'array'
  AND c.retry_policy -> 'retryOutcomes' @> '["TRANSIENT"]'::jsonb
  AND NOT c.retry_policy -> 'retryOutcomes' @> '["LINE_REJECTED"]'::jsonb;

-- +goose Down
-- Narrowing: a row holding the new outcome is rewritten to TRANSIENT, the old
-- outcome with the same budget (system_failures) and no touch, so the
-- contact's counters and the attempt's is_touched still agree with it, before
-- the constraints return. A retry policy loses the value it cannot name.
UPDATE campaigns
SET retry_policy = jsonb_set(retry_policy, '{retryOutcomes}', (retry_policy -> 'retryOutcomes') - 'LINE_REJECTED')
WHERE jsonb_typeof(retry_policy -> 'retryOutcomes') = 'array'
  AND retry_policy -> 'retryOutcomes' @> '["LINE_REJECTED"]'::jsonb;

ALTER TABLE campaign_contacts DROP CONSTRAINT ck_campaign_contacts_last_outcome;
UPDATE campaign_contacts SET last_outcome = 'TRANSIENT' WHERE last_outcome = 'LINE_REJECTED';
ALTER TABLE campaign_contacts ADD CONSTRAINT ck_campaign_contacts_last_outcome CHECK (last_outcome IS NULL OR last_outcome IN
    ('ANSWERED', 'BOT_FAILED', 'OVERFLOW', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN',
     'TRANSIENT', 'ORIGINATE_FAILED', 'INVALID_NUMBER', 'DNC_SKIPPED'));

ALTER TABLE campaign_attempts DROP CONSTRAINT ck_campaign_attempts_outcome;
UPDATE campaign_attempts SET outcome = 'TRANSIENT' WHERE outcome = 'LINE_REJECTED';
ALTER TABLE campaign_attempts ADD CONSTRAINT ck_campaign_attempts_outcome CHECK (outcome IS NULL OR outcome IN
    ('ANSWERED', 'BOT_FAILED', 'OVERFLOW', 'NO_ANSWER', 'BUSY', 'REJECTED', 'UNKNOWN',
     'TRANSIENT', 'ORIGINATE_FAILED', 'INVALID_NUMBER', 'DNC_SKIPPED'));

ALTER TABLE campaign_attempts DROP CONSTRAINT ck_campaign_attempts_sip_status;
ALTER TABLE campaign_attempts DROP COLUMN sip_status;
