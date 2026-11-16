-- SPDX-License-Identifier: Apache-2.0

-- The attempts of AI outbound campaigns (docs/design/10-campaigns.md
-- sections 3.3, 5.3 and 5.4).

-- The write-ahead step, part one: take the contact out of the due pool. The row
-- count says whether it was still dialable; the attempt insert that follows,
-- in the same transaction, says whether its number was free.
-- name: ClaimContactForDial :one
UPDATE campaign_contacts
SET next_attempt_at = NULL, is_frequency_held = false, updated_at = now()
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
-- A recorded lead is kept (copied only when this attempt recorded one); the
-- last attempt and its call are always this one, so after a retry that
-- recorded no lead they name a later call than the lead's.
-- name: SettleCampaignContact :exec
UPDATE campaign_contacts
SET attempts = $2, system_failures = $3, status = $4, next_attempt_at = $5, is_frequency_held = false,
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

-- name: MakeContactDue :execrows
UPDATE campaign_contacts
SET next_attempt_at = $2, is_frequency_held = false, updated_at = now()
WHERE id = $1 AND status = 'ACTIVE' AND next_attempt_at IS NULL;

-- A campaign's finished attempts, newest first: what the breakers read.
-- name: RecentAttemptOutcomes :many
SELECT outcome FROM campaign_attempts
WHERE campaign_id = $1 AND state = 'FINISHED'
ORDER BY finished_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- The campaign a call was placed for, within the CDR's transaction, so the
-- webhook filter and the delivered payload agree with the row just written.
-- name: CampaignOfCall :one
SELECT campaign_id FROM campaign_attempts WHERE call_id = $1;
