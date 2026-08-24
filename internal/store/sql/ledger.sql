-- SPDX-License-Identifier: Apache-2.0

-- name: InsertCDR :exec
--
-- One row per call, written by whichever path saw the call end — and, where
-- both did, by the one that saw more of it.
--
-- The two paths do not normally overlap: a transferred call belongs to the
-- human path and a contained one to the bot's, and each declines to write the
-- other's. They overlapped in exactly one situation, and it took a live
-- restart to find: the bot's leg dies with the process, no transfer was ever
-- marked, so the bot writes the call off as ended — and then the caller lives
-- on, reaches a queue, and talks to somebody for four minutes that DO NOTHING
-- silently discarded. The row said a bot call ended at the restart, with no
-- agent, no queue and no talk time, on the record the carrier is billed from.
--
-- A later ending means more of the call is known, so that row wins. The rule
-- is monotone, which is what keeps this safe as an upsert: a row can only ever
-- be replaced by one that reaches further, never flip back.
INSERT INTO cdrs (
    call_id, started_at, answered_at, ended_at, call_type, language,
    from_number, to_number, did, flow_id, queue_id,
    agent_ids, primary_agent_id,
    ring_sec, bot_sec, queue_wait_sec, talk_sec, bill_sec, total_sec,
    status, hangup_cause, missed_reason, disposition,
    is_contained, has_recording, user_data, tech, legs
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11,
    $12, $13,
    $14, $15, $16, $17, $18, $19,
    $20, $21, $22, $23,
    $24, $25, $26, $27, $28
) ON CONFLICT (call_id) DO UPDATE SET
    started_at = EXCLUDED.started_at,
    answered_at = EXCLUDED.answered_at,
    ended_at = EXCLUDED.ended_at,
    call_type = EXCLUDED.call_type,
    language = EXCLUDED.language,
    from_number = EXCLUDED.from_number,
    to_number = EXCLUDED.to_number,
    did = EXCLUDED.did,
    flow_id = EXCLUDED.flow_id,
    queue_id = EXCLUDED.queue_id,
    agent_ids = EXCLUDED.agent_ids,
    primary_agent_id = EXCLUDED.primary_agent_id,
    ring_sec = EXCLUDED.ring_sec,
    bot_sec = EXCLUDED.bot_sec,
    queue_wait_sec = EXCLUDED.queue_wait_sec,
    talk_sec = EXCLUDED.talk_sec,
    bill_sec = EXCLUDED.bill_sec,
    total_sec = EXCLUDED.total_sec,
    status = EXCLUDED.status,
    hangup_cause = EXCLUDED.hangup_cause,
    missed_reason = EXCLUDED.missed_reason,
    disposition = EXCLUDED.disposition,
    is_contained = EXCLUDED.is_contained,
    has_recording = EXCLUDED.has_recording,
    user_data = EXCLUDED.user_data,
    tech = EXCLUDED.tech,
    legs = EXCLUDED.legs
WHERE EXCLUDED.ended_at > cdrs.ended_at;

-- name: GetCDR :one
SELECT * FROM cdrs WHERE call_id = $1;

-- name: ListCDRs :many
SELECT * FROM cdrs
WHERE (sqlc.narg('from_at')::timestamptz IS NULL OR started_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('to_at')::timestamptz IS NULL OR started_at < sqlc.narg('to_at'))
  AND (sqlc.narg('queue_id')::uuid IS NULL OR queue_id = sqlc.narg('queue_id'))
  AND (sqlc.narg('agent_id')::uuid IS NULL OR primary_agent_id = sqlc.narg('agent_id')
       OR sqlc.narg('agent_id') = ANY (agent_ids))
  AND (sqlc.arg('status')::text = '' OR status = sqlc.arg('status'))
  AND (sqlc.arg('did')::text = '' OR did = sqlc.arg('did'))
  AND (sqlc.arg('from_number')::text = '' OR from_number LIKE '%' || sqlc.arg('from_number') || '%')
ORDER BY started_at DESC
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountCDRs :one
SELECT count(*) FROM cdrs
WHERE (sqlc.narg('from_at')::timestamptz IS NULL OR started_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('to_at')::timestamptz IS NULL OR started_at < sqlc.narg('to_at'))
  AND (sqlc.narg('queue_id')::uuid IS NULL OR queue_id = sqlc.narg('queue_id'))
  AND (sqlc.narg('agent_id')::uuid IS NULL OR primary_agent_id = sqlc.narg('agent_id')
       OR sqlc.narg('agent_id') = ANY (agent_ids))
  AND (sqlc.arg('status')::text = '' OR status = sqlc.arg('status'))
  AND (sqlc.arg('did')::text = '' OR did = sqlc.arg('did'))
  AND (sqlc.arg('from_number')::text = '' OR from_number LIKE '%' || sqlc.arg('from_number') || '%');

-- InsertTranscriptLine writes one line as it is spoken. The conflict target is
-- the partial idempotency index, so a redelivered final is dropped rather than
-- duplicated; DO NOTHING is correct because a final is never revised (D3).
-- name: InsertTranscriptLine :exec
INSERT INTO transcripts (call_id, seq, occurred_at, speaker, kind, content,
                         party_id, agent_id, offset_ms, language, source, provider, utterance_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT DO NOTHING;

-- name: ListTranscripts :many
SELECT * FROM transcripts WHERE call_id = $1 ORDER BY seq;

-- ListTranscriptSince serves the backfill cursor. seq is dense per call, so
-- "everything after what I have" is a range scan on uq_transcripts_call_id_seq.
-- name: ListTranscriptSince :many
SELECT * FROM transcripts
WHERE call_id = $1 AND seq > $2
ORDER BY seq
LIMIT $3;

-- name: InsertRecording :one
INSERT INTO recordings (id, call_id, backend, bucket, object_key, size_bytes, duration_sec, format)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetRecording :one
SELECT * FROM recordings WHERE id = $1 AND deleted_at IS NULL;

-- name: ListRecordingsByCall :many
SELECT * FROM recordings WHERE call_id = $1 AND deleted_at IS NULL ORDER BY created_at;

-- name: InsertCallback :one
INSERT INTO callbacks (id, call_id, queue_id, phone_number, message)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListCallbacks :many
SELECT * FROM callbacks
WHERE (sqlc.arg('status')::text = '' OR status = sqlc.arg('status'))
ORDER BY created_at DESC
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: HandleCallback :one
UPDATE callbacks
SET status = $2, handled_by = $3, handled_at = now()
WHERE id = $1
RETURNING *;

-- name: InsertQueueEvent :exec
INSERT INTO queue_events (occurred_at, call_id, queue_id, event, agent_id, wait_ms)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (actor_id, action, target_kind, target_id, detail, ip)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: UpdateCDRHasRecording :exec
UPDATE cdrs SET has_recording = true WHERE call_id = $1;

-- name: InsertQualityReview :one
INSERT INTO quality_reviews (id, recording_id, call_id, reviewer_id, scores, total_score, notes)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListQualityReviewsByCall :many
SELECT * FROM quality_reviews WHERE call_id = $1 ORDER BY created_at DESC;

-- name: ClaimCallback :one
UPDATE callbacks
SET status = 'CLAIMED', handled_by = $2
WHERE id = $1 AND status = 'OPEN'
RETURNING *;

-- name: ReportOverview :one
SELECT
    count(*)                                                          AS total_calls,
    count(*) FILTER (WHERE status = 'ANSWERED')                       AS answered_calls,
    count(*) FILTER (WHERE missed_reason IN
        ('SHORT_ABANDONED', 'ABANDONED_RINGING', 'ABANDONED_WAITING')) AS abandoned_calls,
    count(*) FILTER (WHERE is_contained)                              AS contained_calls,
    count(*) FILTER (WHERE queue_wait_sec <= 20 AND status = 'ANSWERED'
                     AND queue_id IS NOT NULL)                        AS answered_within_sla,
    count(*) FILTER (WHERE queue_id IS NOT NULL)                      AS queue_calls,
    coalesce(avg(queue_wait_sec) FILTER (WHERE queue_id IS NOT NULL), 0)::float8 AS avg_wait_sec,
    coalesce(avg(talk_sec) FILTER (WHERE talk_sec > 0), 0)::float8    AS avg_talk_sec,
    coalesce(avg(bot_sec)  FILTER (WHERE bot_sec  > 0), 0)::float8    AS avg_bot_sec
FROM cdrs
WHERE started_at >= $1 AND started_at < $2
  AND (sqlc.narg('queue_id')::uuid IS NULL OR queue_id = sqlc.narg('queue_id'));

-- name: ReportByQueue :many
SELECT
    queue_id,
    count(*)                                                           AS total_calls,
    count(*) FILTER (WHERE status = 'ANSWERED')                        AS answered_calls,
    count(*) FILTER (WHERE missed_reason IN
        ('SHORT_ABANDONED', 'ABANDONED_RINGING', 'ABANDONED_WAITING')) AS abandoned_calls,
    count(*) FILTER (WHERE queue_wait_sec <= 20 AND status = 'ANSWERED') AS answered_within_sla,
    coalesce(avg(queue_wait_sec), 0)::float8                           AS avg_wait_sec,
    coalesce(max(queue_wait_sec), 0)::int                              AS max_wait_sec,
    coalesce(avg(talk_sec) FILTER (WHERE talk_sec > 0), 0)::float8     AS avg_talk_sec
FROM cdrs
WHERE started_at >= $1 AND started_at < $2 AND queue_id IS NOT NULL
GROUP BY queue_id
ORDER BY total_calls DESC;

-- name: ReportDaily :many
SELECT
    date_trunc('day', started_at)::date                               AS day,
    count(*)                                                          AS total_calls,
    count(*) FILTER (WHERE status = 'ANSWERED')                       AS answered_calls,
    count(*) FILTER (WHERE is_contained)                              AS contained_calls,
    count(*) FILTER (WHERE missed_reason IN
        ('SHORT_ABANDONED', 'ABANDONED_RINGING', 'ABANDONED_WAITING')) AS abandoned_calls
FROM cdrs
WHERE started_at >= $1 AND started_at < $2
GROUP BY 1
ORDER BY 1;

--
-- After-call work: the vocabulary, and what agents file against calls.
--

-- name: ListEnabledDispositions :many
SELECT * FROM dispositions WHERE is_enabled ORDER BY position, code;

-- name: GetDisposition :one
SELECT code, label FROM dispositions WHERE code = $1 AND is_enabled;

-- OpenWrapUp starts the record the agent will confirm, with the defaults the
-- platform would file on their behalf. Doing it twice for one call is not a
-- second record: the first one may already carry what the agent typed.
-- name: OpenWrapUp :exec
INSERT INTO wrap_ups (call_id, agent_id, disposition_code, disposition_label, note, is_confirmed)
VALUES ($1, $2, $3, $4, '', false)
ON CONFLICT (call_id, agent_id) DO NOTHING;

-- UpsertWrapUp writes the record whole. created_at is left as it was on an
-- existing row: it is when after-call work began, not when it was last
-- touched, and the day's numbers are grouped by it.
-- name: UpsertWrapUp :one
INSERT INTO wrap_ups (call_id, agent_id, disposition_code, disposition_label, note, is_confirmed)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (call_id, agent_id) DO UPDATE
SET disposition_code  = excluded.disposition_code,
    disposition_label = excluded.disposition_label,
    note              = excluded.note,
    is_confirmed      = excluded.is_confirmed
RETURNING *;

-- name: GetWrapUp :one
SELECT * FROM wrap_ups WHERE call_id = $1 AND agent_id = $2;

-- ListWrapUpsForCalls reads every wrap-up filed against a page of calls, so a
-- ledger listing can attach them without a join whose nullability sqlc would
-- have to guess at.
-- name: ListWrapUpsForCalls :many
SELECT * FROM wrap_ups
WHERE call_id = ANY(sqlc.arg('call_ids')::uuid[])
ORDER BY created_at DESC;

-- ReportAgentToday is one agent's own day.
--
-- Two sources, because no single one knows it all: the ledger says what the
-- agent handled and for how long they talked, and the presence history says
-- how long they were signed in and how much of it went on after-call work.
-- Intervals are clipped to the window, and an interval still open — the shift
-- they are in, the wrap-up they are typing — counts up to its end.
-- name: ReportAgentToday :one
WITH bounds AS (
    SELECT sqlc.arg('from_at')::timestamptz AS from_at, sqlc.arg('to_at')::timestamptz AS to_at
),
handled AS (
    SELECT count(*)::bigint AS calls_handled,
           COALESCE(sum(talk_sec), 0)::bigint AS talk_sec
    FROM cdrs, bounds
    WHERE primary_agent_id = sqlc.arg('agent_id')
      AND started_at >= bounds.from_at AND started_at < bounds.to_at
),
intervals AS (
    SELECT l.state, l.reason,
           GREATEST(l.entered_at, bounds.from_at) AS started_at,
           LEAST(COALESCE(l.exited_at, bounds.to_at), bounds.to_at) AS ended_at
    FROM agent_state_logs l, bounds
    WHERE l.agent_id = sqlc.arg('agent_id')
      AND l.entered_at < bounds.to_at
      AND COALESCE(l.exited_at, bounds.to_at) > bounds.from_at
),
filings AS (
    -- The day's after-call records, counted where they were opened: one per
    -- call the agent finished, and how many of them somebody confirmed.
    SELECT count(*)::bigint AS wrap_ups_opened,
           count(*) FILTER (WHERE is_confirmed)::bigint AS wrap_ups_confirmed
    FROM wrap_ups, bounds
    WHERE agent_id = sqlc.arg('agent_id')
      AND created_at >= bounds.from_at AND created_at < bounds.to_at
),
presence AS (
    SELECT
        COALESCE(sum(EXTRACT(EPOCH FROM (ended_at - started_at)))
                 FILTER (WHERE reason = 'AFTER_CALL_WORK'), 0)::bigint AS wrap_up_sec,
        count(*) FILTER (WHERE reason = 'AFTER_CALL_WORK')::bigint AS wrap_ups,
        COALESCE(sum(EXTRACT(EPOCH FROM (ended_at - started_at)))
                 FILTER (WHERE state <> 'LOGGED_OUT'), 0)::bigint AS signed_in_sec
    FROM intervals
)
SELECT handled.calls_handled, handled.talk_sec,
       presence.wrap_up_sec, presence.wrap_ups, presence.signed_in_sec,
       filings.wrap_ups_opened, filings.wrap_ups_confirmed
FROM handled, presence, filings;

-- name: QueueEventsByCall :many
-- One call's journey through the queues, oldest first. (occurred_at, id)
-- because two movements of one call can share a millisecond and a journey that
-- reorders between two reads is not a journey.
SELECT occurred_at, queue_id, event, agent_id, wait_ms
FROM queue_events
WHERE call_id = $1
ORDER BY occurred_at, id;
