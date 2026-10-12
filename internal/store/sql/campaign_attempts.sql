-- SPDX-License-Identifier: Apache-2.0

-- The attempts of AI outbound campaigns (docs/design/10-campaigns.md
-- sections 3.3, 5.3 and 5.4).

-- The write-ahead step, part one: take the contact out of the due pool. The row
-- count says whether it was still dialable; the attempt insert that follows,
-- in the same transaction, says whether its number was free.
-- name: ClaimContactForDial :one
UPDATE campaign_contacts
SET next_attempt_at = NULL, updated_at = now()
WHERE id = $1 AND status = 'ACTIVE'
RETURNING campaign_id, number_e164;

-- name: InsertCampaignAttempt :one
INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164,
                               call_id, leg_id, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: SetAttemptJob :execrows
UPDATE campaign_attempts SET originate_job_id = $2
WHERE call_id = $1 AND state = 'DIALING';

-- The lead a call recorded so far; the last call wins. Only an attempt still
-- in flight can be written to.
-- name: SetAttemptLead :execrows
UPDATE campaign_attempts SET lead_intent = $2, lead_notes = $3
WHERE call_id = $1 AND state = 'DIALING';

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
-- A recorded lead is kept (copied only when this attempt recorded one).
-- name: SettleCampaignContact :exec
UPDATE campaign_contacts
SET attempts = $2, system_failures = $3, status = $4, next_attempt_at = $5,
    last_outcome = $6,
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
SET next_attempt_at = $2, updated_at = now()
WHERE id = $1 AND status = 'ACTIVE' AND next_attempt_at IS NULL;

-- A campaign's finished attempts, newest first: what the breakers read.
-- name: RecentAttemptOutcomes :many
SELECT outcome FROM campaign_attempts
WHERE campaign_id = $1 AND state = 'FINISHED'
ORDER BY finished_at DESC, id DESC
LIMIT sqlc.arg('row_limit');
