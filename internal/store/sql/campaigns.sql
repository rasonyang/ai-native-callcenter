-- SPDX-License-Identifier: Apache-2.0

-- AI outbound campaigns and their contacts (docs/design/10-campaigns.md).
-- Every status change is one UPDATE conditional on the status it leaves: the
-- rows-affected count, not a prior read, says whether the move happened.

-- name: InsertCampaign :one
INSERT INTO campaigns (id, name, did_id, status, time_zone, calling_window,
                       default_country_code, max_concurrent, ring_timeout_sec,
                       retry_policy, follow_up_queue_id)
VALUES ($1, $2, $3, 'DRAFT', $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: GetCampaign :one
SELECT sqlc.embed(c), d.number AS did_number
FROM campaigns c
JOIN dids d ON d.id = c.did_id
WHERE c.id = $1;

-- The row lock a PATCH reads under: a start, pause or cancel that races it
-- waits, so the field locks it applies are those of the status it writes to.
-- name: GetCampaignForUpdate :one
SELECT sqlc.embed(c), d.number AS did_number
FROM campaigns c
JOIN dids d ON d.id = c.did_id
WHERE c.id = $1
FOR UPDATE OF c;

-- name: UpdateCampaign :execrows
UPDATE campaigns
SET name = $2, did_id = $3, time_zone = $4, calling_window = $5,
    default_country_code = $6, max_concurrent = $7, ring_timeout_sec = $8,
    retry_policy = $9, follow_up_queue_id = $10, updated_at = now()
WHERE id = $1;

-- name: ListCampaigns :many
SELECT sqlc.embed(c), d.number AS did_number
FROM campaigns c
JOIN dids d ON d.id = c.did_id
WHERE (sqlc.arg('status')::text = '' OR c.status = sqlc.arg('status'))
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountCampaigns :one
SELECT count(*) FROM campaigns c
WHERE (sqlc.arg('status')::text = '' OR c.status = sqlc.arg('status'));

-- name: CampaignContactCounts :many
SELECT campaign_id, status, count(*)::bigint AS n
FROM campaign_contacts
WHERE campaign_id = ANY(sqlc.arg('campaign_ids')::uuid[])
GROUP BY campaign_id, status;

-- name: DeleteDraftCampaign :execrows
DELETE FROM campaigns WHERE id = $1 AND status = 'DRAFT';

-- name: StartCampaign :execrows
UPDATE campaigns
SET status = 'RUNNING', started_at = now(), updated_at = now()
WHERE id = $1 AND status = 'DRAFT';

-- Contacts imported before the campaign started waited with no due time; from
-- the start on, an ACTIVE contact is due now.
-- name: ArmCampaignContacts :execrows
UPDATE campaign_contacts
SET next_attempt_at = now(), updated_at = now()
WHERE campaign_id = $1 AND status = 'ACTIVE' AND next_attempt_at IS NULL;

-- name: PauseCampaign :execrows
UPDATE campaigns
SET status = 'PAUSED', pause_reason = 'MANUAL', updated_at = now()
WHERE id = $1 AND status = 'RUNNING';

-- name: ResumeCampaign :execrows
UPDATE campaigns
SET status = 'RUNNING', pause_reason = NULL, updated_at = now()
WHERE id = $1 AND status = 'PAUSED';

-- name: CancelCampaign :execrows
UPDATE campaigns
SET status = 'CANCELLED', pause_reason = NULL, finished_at = now(), updated_at = now()
WHERE id = $1 AND status IN ('DRAFT', 'RUNNING', 'PAUSED');

-- name: GetDIDByID :one
SELECT * FROM dids WHERE id = $1;

-- Non-terminal campaigns that dial from a DID, or from any DID answered by a
-- flow. A flow that gains a way to a person, or a DID rebound to one, must not
-- reach a campaign that is not finished.
-- name: CountLiveCampaignsOnDID :one
SELECT count(*) FROM campaigns
WHERE did_id = $1 AND status IN ('DRAFT', 'RUNNING', 'PAUSED');

-- name: CountLiveCampaignsOnFlow :one
SELECT count(*) FROM campaigns c
JOIN dids d ON d.id = c.did_id
WHERE d.flow_id = $1 AND c.status IN ('DRAFT', 'RUNNING', 'PAUSED');

-- Held shared for the length of an import, so that a cancel (an UPDATE of the
-- same row) cannot slip between the status check and the inserts.
-- name: LockCampaignStatus :one
SELECT status FROM campaigns WHERE id = $1 FOR SHARE;

-- rows is a JSON array of {id, number, raw, timeZone, slots}: one parameter
-- rather than five parallel arrays, so that the columns of a row cannot drift
-- apart.
-- name: InsertCampaignContacts :many
INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw, time_zone, slots, next_attempt_at)
SELECT (e.v->>'id')::uuid, sqlc.arg('campaign_id')::uuid, e.v->>'number', e.v->>'raw',
       nullif(e.v->>'timeZone', ''), e.v->'slots',
       CASE WHEN sqlc.arg('is_armed')::bool THEN now() END
FROM jsonb_array_elements(sqlc.arg('rows')::jsonb) WITH ORDINALITY AS e(v, n)
ORDER BY e.n
ON CONFLICT (campaign_id, number_e164) DO NOTHING
RETURNING id, number_e164;

-- name: ExistingCampaignContacts :many
SELECT id, number_e164 FROM campaign_contacts
WHERE campaign_id = $1 AND number_e164 = ANY(sqlc.arg('numbers')::text[]);

-- name: ListCampaignContacts :many
SELECT * FROM campaign_contacts
WHERE campaign_id = sqlc.arg('campaign_id')
  AND (sqlc.arg('status')::text = '' OR status = sqlc.arg('status'))
  AND (sqlc.arg('lead_intent')::text = '' OR lead_intent = sqlc.arg('lead_intent'))
ORDER BY created_at, id
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountCampaignContacts :one
SELECT count(*) FROM campaign_contacts
WHERE campaign_id = sqlc.arg('campaign_id')
  AND (sqlc.arg('status')::text = '' OR status = sqlc.arg('status'))
  AND (sqlc.arg('lead_intent')::text = '' OR lead_intent = sqlc.arg('lead_intent'));
