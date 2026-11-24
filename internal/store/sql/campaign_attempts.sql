-- SPDX-License-Identifier: Apache-2.0

-- The attempts of AI outbound campaigns (docs/design/10-campaigns.md
-- sections 3.3, 5.3 and 5.4).

-- The write-ahead step, part one: take the contact out of the due pool. The row
-- count says whether it was still dialable; the attempt insert that follows,
-- in the same transaction, says whether its number was free. The retry time is
-- spent: the attempt's settle writes the next one.
-- name: ClaimContactForDial :one
UPDATE campaign_contacts
SET next_attempt_at = NULL, retry_at = NULL, is_frequency_held = false, is_daily_cap_held = false, updated_at = now()
WHERE id = $1 AND status = 'ACTIVE'
RETURNING campaign_id, number_e164;

-- is_test_number is read from the list of test numbers now, and stays as it is
-- if the number leaves the list later.
-- name: InsertCampaignAttempt :one
INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164,
                               call_id, leg_id, started_at, is_test_number)
VALUES ($1, $2, $3, $4, $5, $6, $7,
        EXISTS (SELECT 1 FROM test_numbers t WHERE t.number_e164 = $4))
RETURNING *;

-- name: SetAttemptJob :execrows
UPDATE campaign_attempts SET originate_job_id = $2
WHERE call_id = $1 AND state = 'DIALING';

-- The lead a call recorded so far; the last call wins. SetLead decides whether
-- the attempt may take it (in flight, or finished early with no real CDR yet)
-- under LockAttemptByCall, so this writes by id and checks nothing itself.
-- name: SetAttemptLead :execrows
UPDATE campaign_attempts SET lead_intent = $2, lead_notes = $3
WHERE id = $1;

-- True when the ledger holds a CDR for the call that is more than a placement
-- failure: the call's final word, which a lead can no longer join. Run after
-- LockAttemptByCall, in its own statement, so it sees every CDR committed
-- before the lock was granted (InsertCDR writes the CDR before it locks).
-- name: CallHasRealCDR :one
SELECT EXISTS (
    SELECT 1 FROM cdrs
    WHERE call_id = $1 AND tech ->> 'isPlacementFailure' IS DISTINCT FROM 'true'
)::bool AS has_real_cdr;

-- name: GetAttemptByCall :one
SELECT * FROM campaign_attempts WHERE call_id = $1;

-- The finish routine's lock. A CDR and a standalone finish for one call
-- serialise here, which is what makes "counted once" true.
-- name: LockAttemptByCall :one
SELECT * FROM campaign_attempts WHERE call_id = $1 FOR UPDATE;

-- name: LockCampaignContact :one
SELECT * FROM campaign_contacts WHERE id = $1 FOR UPDATE;

-- True when no attempt of the same contact started after this one.
-- name: AttemptIsLatest :one
SELECT NOT EXISTS (
    SELECT 1
    FROM campaign_attempts later, campaign_attempts this
    WHERE this.id = $1
      AND later.campaign_contact_id = this.campaign_contact_id
      AND (later.started_at, later.id) > (this.started_at, this.id)
)::bool AS is_latest;

-- name: FinishCampaignAttempt :exec
UPDATE campaign_attempts
SET state = 'FINISHED', outcome = $2, hangup_cause = $3, talk_sec = $4,
    is_touched = $5, finished_at = $6
WHERE id = $1;

-- The contact after its latest attempt settled: counters, fate and schedule.
-- retry_at is the time the retry policy gave, before the calling window moved
-- it into next_attempt_at; both are NULL unless the contact stays ACTIVE.
-- A recorded lead is kept (copied only when this attempt recorded one); the
-- last attempt and its call are always this one, so after a retry that
-- recorded no lead they name a later call than the lead's.
-- name: SettleCampaignContact :exec
UPDATE campaign_contacts
SET attempts = $2, system_failures = $3, status = $4, next_attempt_at = $5,
    retry_at = sqlc.narg('retry_at')::timestamptz, is_frequency_held = false, is_daily_cap_held = false,
    last_outcome = $6, last_attempt_id = sqlc.arg('last_attempt_id')::uuid,
    last_call_id = sqlc.arg('last_call_id')::uuid,
    lead_intent = coalesce(sqlc.narg('lead_intent')::text, lead_intent),
    lead_notes = CASE WHEN sqlc.narg('lead_intent')::text IS NULL THEN lead_notes
                      ELSE sqlc.narg('lead_notes')::text END,
    updated_at = now()
WHERE id = $1;

-- The counters alone: an older attempt of the contact was revised, so its
-- cost moves and the contact's fate, which the latest attempt decided, does
-- not.
-- name: AdjustCampaignContactCounters :exec
UPDATE campaign_contacts
SET attempts = $2, system_failures = $3, updated_at = now()
WHERE id = $1;

-- The follow-up of a HIGH lead. One per call: a revised CDR runs the finish
-- again and must not file a second.
-- name: InsertCallbackForCall :execrows
INSERT INTO callbacks (id, call_id, queue_id, phone_number, message)
SELECT $1, sqlc.arg('call_id')::uuid, $2, $3, $4
WHERE NOT EXISTS (SELECT 1 FROM callbacks WHERE call_id = sqlc.arg('call_id')::uuid);

-- A voided attempt leaves no trace.
-- name: DeleteDialingAttempt :one
DELETE FROM campaign_attempts
WHERE call_id = $1 AND state = 'DIALING'
RETURNING campaign_contact_id;

-- A voided attempt never happened: the contact is due again with no retry time
-- of its own.
-- name: MakeContactDue :execrows
UPDATE campaign_contacts
SET next_attempt_at = $2, retry_at = NULL, is_frequency_held = false, is_daily_cap_held = false, updated_at = now()
WHERE id = $1 AND status = 'ACTIVE' AND next_attempt_at IS NULL;

-- A campaign's finished attempts, newest first: what the breakers read.
-- name: RecentAttemptOutcomes :many
SELECT outcome FROM campaign_attempts
WHERE campaign_id = $1 AND state = 'FINISHED'
ORDER BY finished_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- The campaign a call was placed for and the lead its attempt holds, within
-- the CDR's transaction, so the webhook filter and the delivered payload agree
-- with the rows just written. The verdict's deadline says how long a delivery
-- waits for a judgement still owed.
-- name: CampaignOfCall :one
SELECT campaign_id, lead_intent, declared_lead_intent, lead_verdict, lead_verdict_reason,
       lead_judge_deadline_at
FROM campaign_attempts WHERE call_id = $1;

-- The lead of each of these calls that recorded one, for the ledger's reads.
-- name: CampaignLeadsOfCalls :many
SELECT call_id, lead_intent, declared_lead_intent, lead_verdict, lead_verdict_reason
FROM campaign_attempts
WHERE call_id = ANY (sqlc.arg('call_ids')::uuid[]) AND lead_intent IS NOT NULL;

-- A judgement of the lead becomes owed (design section 11.1): the flow's
-- intent is kept apart and the deadline set, both from the database's clock.
-- Only once per attempt, and only for a lead that is judged at all; a revised
-- CDR finds declared_lead_intent set and changes nothing.
-- name: OweLeadJudgement :one
UPDATE campaign_attempts
SET declared_lead_intent = lead_intent, lead_verdict = 'PENDING',
    lead_judge_deadline_at = now() + sqlc.arg('deadline_sec')::int * interval '1 second'
WHERE id = $1 AND declared_lead_intent IS NULL AND lead_intent IN ('HIGH', 'MEDIUM')
RETURNING *;

-- The verdict and the lead it leaves, written only while the judgement is
-- still owed and its deadline has not passed: no row back means too late, and
-- the verdict is discarded, so the record says what the receiver was sent.
-- name: SettleLeadVerdict :one
UPDATE campaign_attempts
SET lead_verdict = sqlc.arg('lead_verdict'), lead_verdict_reason = sqlc.narg('lead_verdict_reason'),
    lead_intent = sqlc.arg('lead_intent')
WHERE id = $1 AND lead_verdict = 'PENDING' AND lead_judge_deadline_at > now()
RETURNING *;

-- A judgement whose deadline passed with no verdict: the lead the flow
-- declared stands, marked as not judged.
-- name: ExpireLeadJudgement :one
UPDATE campaign_attempts
SET lead_verdict = 'NOT_JUDGED', lead_verdict_reason = NULL
WHERE call_id = $1 AND lead_verdict = 'PENDING' AND lead_judge_deadline_at <= now()
RETURNING *;

-- The judgements past their deadline, oldest first, for the sweep.
-- name: ExpiredLeadJudgements :many
SELECT call_id FROM campaign_attempts
WHERE lead_verdict = 'PENDING' AND lead_judge_deadline_at <= now()
ORDER BY lead_judge_deadline_at
LIMIT sqlc.arg('row_limit');

-- The judgements still owed and still in time, soonest deadline first, for a
-- worker that starts (or looks again) and finds work nobody handed it. The
-- deadline also says when the judgement became owed: LeadJudgeDeadline before.
-- name: PendingLeadJudgements :many
SELECT call_id, lead_judge_deadline_at FROM campaign_attempts
WHERE lead_verdict = 'PENDING' AND lead_judge_deadline_at > now()
ORDER BY lead_judge_deadline_at
LIMIT sqlc.arg('row_limit');

-- A verdict moves the contact's lead only while this attempt is still the
-- contact's latest: a later call has settled the contact since, and its lead
-- (or the lead it kept) is not this call's to change.
-- name: SetContactLeadOfAttempt :execrows
UPDATE campaign_contacts
SET lead_intent = sqlc.arg('lead_intent'), updated_at = now()
WHERE id = $1 AND last_attempt_id = sqlc.arg('attempt_id')::uuid;
