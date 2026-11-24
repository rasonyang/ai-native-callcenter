-- SPDX-License-Identifier: Apache-2.0

-- What the dialer reads and writes (docs/design/10-campaigns.md sections 4.1,
-- 5.2, 7.4 and 8.2). Every time that decides something is a parameter, never
-- now(): the dialer owns the clock.

-- name: ListRunningCampaigns :many
SELECT sqlc.embed(c), d.number AS did_number
FROM campaigns c
JOIN dids d ON d.id = c.did_id
WHERE c.status = 'RUNNING'
ORDER BY c.started_at, c.id;

-- The admission query of section 5.2, less the calling window (which depends
-- on the contact's zone and runs in Go). A number on the do-not-call list, one
-- being dialled anywhere, and one another campaign reached within the
-- frequency window are all left out here; the last is moved by
-- HeldByFrequency. A number on the list of test numbers is exempt from the
-- frequency window and from nothing else: the do-not-call and DIALING checks
-- above it apply to it as to any other.
-- name: DueCampaignContacts :many
SELECT c.* FROM campaign_contacts c
WHERE c.campaign_id = sqlc.arg('campaign_id')
  AND c.status = 'ACTIVE'
  AND c.next_attempt_at <= sqlc.arg('now')::timestamptz
  AND NOT EXISTS (SELECT 1 FROM dnc_numbers d WHERE d.number_e164 = c.number_e164)
  AND NOT EXISTS (SELECT 1 FROM campaign_attempts a
                  WHERE a.number_e164 = c.number_e164 AND a.state = 'DIALING')
  AND NOT EXISTS (SELECT 1 FROM campaign_attempts a
                  WHERE a.number_e164 = c.number_e164
                    AND a.campaign_id <> c.campaign_id
                    AND a.is_touched
                    AND a.started_at > sqlc.arg('now')::timestamptz
                        - make_interval(secs => sqlc.arg('frequency_sec')::float8)
                    AND NOT EXISTS (SELECT 1 FROM test_numbers t WHERE t.number_e164 = c.number_e164))
ORDER BY c.next_attempt_at, c.id
LIMIT sqlc.arg('row_limit');

-- Due contacts another campaign reached lately, with the time of the latest
-- touch. They wait out the frequency window in their own campaign. A number on
-- the list of test numbers is never held.
-- name: HeldByFrequency :many
SELECT c.id, c.time_zone, t.last_touch::timestamptz AS last_touch
FROM campaign_contacts c
JOIN LATERAL (
    SELECT max(a.started_at) AS last_touch
    FROM campaign_attempts a
    WHERE a.number_e164 = c.number_e164
      AND a.campaign_id <> c.campaign_id
      AND a.is_touched
      AND a.started_at > sqlc.arg('now')::timestamptz
          - make_interval(secs => sqlc.arg('frequency_sec')::float8)
) t ON t.last_touch IS NOT NULL
WHERE c.campaign_id = sqlc.arg('campaign_id')
  AND c.status = 'ACTIVE'
  AND c.next_attempt_at <= sqlc.arg('now')::timestamptz
  AND NOT EXISTS (SELECT 1 FROM dnc_numbers d WHERE d.number_e164 = c.number_e164)
  AND NOT EXISTS (SELECT 1 FROM test_numbers t WHERE t.number_e164 = c.number_e164)
  AND NOT EXISTS (SELECT 1 FROM campaign_attempts a
                  WHERE a.number_e164 = c.number_e164 AND a.state = 'DIALING')
ORDER BY c.next_attempt_at, c.id
LIMIT sqlc.arg('row_limit');

-- Moves contacts to a later due time (outside their calling window, held by the
-- frequency window, or held by the daily dial cap). hold names the mark the move
-- leaves: 'FREQUENCY' sets is_frequency_held, 'DAILY_CAP' sets is_daily_cap_held,
-- '' sets neither; the other mark is always cleared, so a contact carries at most
-- one and listing a number can tell a hold from the campaign's own retry time.
-- Never earlier, and never a contact that was claimed for a dial in the
-- meantime. retry_at, the time the retry policy gave, is left as it is: a
-- deferral says when the contact may be dialled, not what its policy asked.
-- name: DeferContacts :execrows
UPDATE campaign_contacts c
SET next_attempt_at = v.at,
    is_frequency_held = (sqlc.arg('hold')::text = 'FREQUENCY'),
    is_daily_cap_held = (sqlc.arg('hold')::text = 'DAILY_CAP'),
    updated_at = now()
FROM (SELECT unnest(sqlc.arg('ids')::uuid[]) AS id, unnest(sqlc.arg('ats')::timestamptz[]) AS at) AS v
WHERE c.id = v.id AND c.status = 'ACTIVE'
  AND c.next_attempt_at IS NOT NULL AND c.next_attempt_at < v.at;

-- How many times each number was dialled in its local day, for the daily dial
-- cap: the numbers and the start of each one's day (its contact's zone) come as
-- two parallel arrays. A dial is a campaign attempt in flight or whose outcome
-- counts (uncounted_outcomes lists the ones that do not: a DNC skip, a bad
-- number, a line's refusal), from any campaign, plus an ad-hoc outbound call
-- (a CDR with no campaign) whose canonical callee is the number. A number on
-- the list of test numbers has no row: it is never capped.
-- name: DialsTodayByNumber :many
SELECT v.number_e164::text AS number_e164, v.day_start::timestamptz AS day_start,
       ((SELECT count(*) FROM campaign_attempts a
         WHERE a.number_e164 = v.number_e164
           AND a.started_at >= v.day_start AND a.started_at <= sqlc.arg('now')::timestamptz
           AND (a.state = 'DIALING' OR a.outcome <> ALL(sqlc.arg('uncounted_outcomes')::text[])))
        + (SELECT count(*) FROM cdrs d
           WHERE d.to_number_e164 = v.number_e164
             AND d.call_type = 'OUTBOUND' AND d.campaign_id IS NULL
             AND d.started_at >= v.day_start AND d.started_at <= sqlc.arg('now')::timestamptz))::bigint AS dials
FROM (SELECT unnest(sqlc.arg('numbers')::text[]) AS number_e164,
             unnest(sqlc.arg('day_starts')::timestamptz[]) AS day_start) v
WHERE NOT EXISTS (SELECT 1 FROM test_numbers t WHERE t.number_e164 = v.number_e164);

-- A calling window or the outer hours may have changed: a page, by id, of the
-- campaign's contacts that wait for a later time and that no hold moved (a
-- contact carrying is_frequency_held or is_daily_cap_held keeps its time; the
-- admission judges the hold again when it is due). The dialer judges each one
-- again in Go, in the contact's own zone, from retry_at, the time its retry
-- policy gave it (NULL: none). A contact being dialled has no due time and is
-- not listed.
-- name: ListWaitingContacts :many
SELECT id, time_zone, next_attempt_at::timestamptz AS next_attempt_at, retry_at
FROM campaign_contacts
WHERE campaign_id = sqlc.arg('campaign_id') AND status = 'ACTIVE'
  AND next_attempt_at > sqlc.arg('now')::timestamptz
  AND NOT is_frequency_held AND NOT is_daily_cap_held
  AND id > sqlc.arg('after_id')::uuid
ORDER BY id
LIMIT sqlc.arg('row_limit');

-- Writes the due times the dialer judged for waiting contacts (ids, the due
-- time and retry time each was judged from, and its new due time, as parallel
-- arrays). A contact that moved in the meantime (claimed, settled again by a
-- revised attempt, held, finished) is left alone: its row no longer matches
-- what was judged. retry_at and the marks are not written.
-- name: RearmContacts :execrows
UPDATE campaign_contacts c
SET next_attempt_at = v.at, updated_at = now()
FROM (SELECT unnest(sqlc.arg('ids')::uuid[]) AS id,
             unnest(sqlc.arg('judged_ats')::timestamptz[]) AS judged_at,
             unnest(sqlc.arg('retry_ats')::timestamptz[]) AS retry_at,
             unnest(sqlc.arg('ats')::timestamptz[]) AS at) AS v
WHERE c.id = v.id AND c.status = 'ACTIVE'
  AND c.next_attempt_at = v.judged_at AND c.retry_at IS NOT DISTINCT FROM v.retry_at
  AND NOT c.is_frequency_held AND NOT c.is_daily_cap_held;

-- Where a campaign stands: how many contacts are still ACTIVE (including those
-- being dialled), how many are due, and when the next one will be.
-- name: CampaignPending :one
SELECT count(*)::bigint AS active_total,
       (count(*) FILTER (WHERE cc.next_attempt_at <= sqlc.arg('now')::timestamptz))::bigint AS due_now,
       min(cc.next_attempt_at) FILTER (WHERE cc.next_attempt_at > sqlc.arg('now')::timestamptz)::timestamptz AS next_future,
       (SELECT count(*) FROM campaign_contacts t WHERE t.campaign_id = sqlc.arg('campaign_id'))::bigint AS all_total
FROM campaign_contacts cc
WHERE cc.campaign_id = sqlc.arg('campaign_id') AND cc.status = 'ACTIVE';

-- The latest touch of another campaign within the frequency window on the
-- contact's number, null when none holds it (a number on the list of test
-- numbers is never held). The contact is held until that time plus the window.
-- name: ContactFrequencyTouch :one
SELECT max(a.started_at)::timestamptz AS last_touch
FROM campaign_contacts c
JOIN campaign_attempts a ON a.number_e164 = c.number_e164
WHERE c.id = sqlc.arg('contact_id')
  AND a.campaign_id <> c.campaign_id
  AND a.is_touched
  AND a.started_at > sqlc.arg('now')::timestamptz
      - make_interval(secs => sqlc.arg('frequency_sec')::float8)
  AND NOT EXISTS (SELECT 1 FROM test_numbers t WHERE t.number_e164 = c.number_e164);

-- A number was put on the list of test numbers: the contacts of RUNNING
-- campaigns that the frequency window or the daily dial cap deferred
-- (is_frequency_held and is_daily_cap_held, set only by those holds' own
-- deferral) are due again. A contact waiting on its campaign's retry policy
-- carries no mark and keeps its time, as does one being dialled. retry_at is
-- not written.
-- name: RearmHeldContacts :execrows
UPDATE campaign_contacts c
SET next_attempt_at = sqlc.arg('now')::timestamptz, is_frequency_held = false, is_daily_cap_held = false, updated_at = now()
FROM campaigns ca
WHERE ca.id = c.campaign_id AND ca.status = 'RUNNING'
  AND c.status = 'ACTIVE' AND (c.is_frequency_held OR c.is_daily_cap_held)
  AND c.next_attempt_at > sqlc.arg('now')::timestamptz
  AND EXISTS (SELECT 1 FROM test_numbers t WHERE t.number_e164 = c.number_e164)
  AND NOT EXISTS (SELECT 1 FROM campaign_attempts d
                  WHERE d.number_e164 = c.number_e164 AND d.state = 'DIALING');

-- name: EarliestPendingContact :one
SELECT * FROM campaign_contacts
WHERE campaign_id = $1 AND status = 'ACTIVE' AND next_attempt_at IS NOT NULL
ORDER BY next_attempt_at, id
LIMIT 1;

-- RUNNING to COMPLETED: only when nothing is ACTIVE any more (so nothing is
-- due or in flight) and the campaign had contacts at all.
-- name: CompleteCampaign :execrows
UPDATE campaigns
SET status = 'COMPLETED', finished_at = sqlc.arg('at')::timestamptz, updated_at = now()
WHERE campaigns.id = sqlc.arg('campaign_id') AND campaigns.status = 'RUNNING'
  AND EXISTS (SELECT 1 FROM campaign_contacts x WHERE x.campaign_id = campaigns.id)
  AND NOT EXISTS (SELECT 1 FROM campaign_contacts y
                  WHERE y.campaign_id = campaigns.id AND y.status = 'ACTIVE');

-- A breaker stopped the campaign.
-- name: PauseCampaignFor :execrows
UPDATE campaigns
SET status = 'PAUSED', pause_reason = sqlc.arg('reason'), updated_at = now()
WHERE id = sqlc.arg('campaign_id') AND status = 'RUNNING';

-- Contacts on the do-not-call list that are waiting for a dial (due now or
-- later): they are skipped, not dialled.
-- name: DueDNCContacts :many
SELECT c.id FROM campaign_contacts c
JOIN campaigns ca ON ca.id = c.campaign_id
WHERE ca.status = 'RUNNING' AND c.status = 'ACTIVE' AND c.next_attempt_at IS NOT NULL
  AND EXISTS (SELECT 1 FROM dnc_numbers d WHERE d.number_e164 = c.number_e164)
LIMIT sqlc.arg('row_limit');

-- A contact whose call is already in flight is not hung up. It becomes DNC
-- now so that, when the call ends, no retry follows.
-- name: MarkDialingContactsDNC :execrows
UPDATE campaign_contacts c
SET status = 'DNC', next_attempt_at = NULL, retry_at = NULL, is_frequency_held = false, is_daily_cap_held = false, updated_at = now()
WHERE c.status = 'ACTIVE' AND c.next_attempt_at IS NULL
  AND EXISTS (SELECT 1 FROM dnc_numbers d WHERE d.number_e164 = c.number_e164)
  AND EXISTS (SELECT 1 FROM campaign_attempts a
              WHERE a.campaign_contact_id = c.id AND a.state = 'DIALING');

-- Every attempt in flight, with the ring timeout of its campaign: what a
-- restart reconciles and the backstop watches.
-- name: ListDialingAttempts :many
SELECT sqlc.embed(a), c.ring_timeout_sec
FROM campaign_attempts a
JOIN campaigns c ON c.id = a.campaign_id
WHERE a.state = 'DIALING'
ORDER BY a.started_at, a.id;

-- A campaign's finished attempts since a time, newest first: the breakers read
-- the run that began at the campaign's last resume.
-- name: RecentAttemptOutcomesSince :many
SELECT outcome FROM campaign_attempts
WHERE campaign_id = sqlc.arg('campaign_id') AND state = 'FINISHED'
  AND finished_at >= sqlc.arg('since')::timestamptz
ORDER BY finished_at DESC, id DESC
LIMIT sqlc.arg('row_limit');

-- An attempt's number_e164 is its contact's, copied when it is written and
-- never changed (a contact's number is its identity in the campaign), so the
-- number filter reads the attempt's own column: the number dialled.
-- name: ListCampaignAttempts :many
SELECT * FROM campaign_attempts
WHERE campaign_id = sqlc.arg('campaign_id')
  AND (sqlc.narg('contact_id')::uuid IS NULL OR campaign_contact_id = sqlc.narg('contact_id')::uuid)
  AND (sqlc.arg('outcome')::text = '' OR outcome = sqlc.arg('outcome'))
  AND (sqlc.narg('is_test_number')::boolean IS NULL OR is_test_number = sqlc.narg('is_test_number')::boolean)
  AND (sqlc.arg('number_e164')::text = '' OR number_e164 = sqlc.arg('number_e164'))
ORDER BY started_at DESC, id DESC
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountCampaignAttempts :one
SELECT count(*) FROM campaign_attempts
WHERE campaign_id = sqlc.arg('campaign_id')
  AND (sqlc.narg('contact_id')::uuid IS NULL OR campaign_contact_id = sqlc.narg('contact_id')::uuid)
  AND (sqlc.arg('outcome')::text = '' OR outcome = sqlc.arg('outcome'))
  AND (sqlc.narg('is_test_number')::boolean IS NULL OR is_test_number = sqlc.narg('is_test_number')::boolean)
  AND (sqlc.arg('number_e164')::text = '' OR number_e164 = sqlc.arg('number_e164'));
