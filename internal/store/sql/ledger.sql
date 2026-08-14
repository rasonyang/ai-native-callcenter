-- SPDX-License-Identifier: Apache-2.0

-- name: InsertCDR :exec
INSERT INTO cdrs (
    call_id, started_at, answered_at, ended_at, call_type, language,
    from_number, to_number, did, flow_id, queue_id,
    agent_ids, primary_agent_id,
    ring_sec, bot_sec, queue_wait_sec, talk_sec, total_sec,
    status, hangup_cause, missed_reason, disposition,
    is_contained, has_recording, user_data, tech, legs
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11,
    $12, $13,
    $14, $15, $16, $17, $18,
    $19, $20, $21, $22,
    $23, $24, $25, $26, $27
) ON CONFLICT (call_id) DO NOTHING;

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

-- name: InsertTranscript :copyfrom
INSERT INTO transcripts (call_id, seq, occurred_at, role, kind, content)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListTranscripts :many
SELECT * FROM transcripts WHERE call_id = $1 ORDER BY seq;

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

-- name: PutSetting :exec
INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: UpdateCDRHasRecording :exec
UPDATE cdrs SET has_recording = true WHERE call_id = $1;

-- name: InsertQualityReview :one
INSERT INTO quality_reviews (id, recording_id, call_id, reviewer_id, scores, total_score, notes)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListQualityReviewsByCall :many
SELECT * FROM quality_reviews WHERE call_id = $1 ORDER BY created_at DESC;
