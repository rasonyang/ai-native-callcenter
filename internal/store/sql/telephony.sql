-- SPDX-License-Identifier: Apache-2.0

-- name: CreateExtension :one
INSERT INTO extensions (id, number, kind, password, display_name, is_enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetExtension :one
SELECT * FROM extensions WHERE id = $1;

-- name: GetExtensionByNumber :one
SELECT * FROM extensions WHERE number = $1;

-- name: ListExtensions :many
SELECT * FROM extensions ORDER BY number;

-- name: UpdateExtension :one
UPDATE extensions
SET kind = $2, display_name = $3, is_enabled = $4, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateExtensionPassword :exec
UPDATE extensions SET password = $2, updated_at = now() WHERE id = $1;

-- name: DeleteExtension :exec
DELETE FROM extensions WHERE id = $1;

-- name: CreateQueue :one
INSERT INTO queues (
    id, name, ext_number, display_name, strategy, moh_sound,
    max_wait_sec, max_wait_no_agent_sec, announce_sound, announce_frequency_sec,
    tier_rules, discard_abandoned_after_sec, is_abandoned_resume_allowed,
    rona_delay_sec, sla_threshold_sec, is_recording_enabled, hours, overflow, is_enabled
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19
)
RETURNING *;

-- name: GetQueue :one
SELECT * FROM queues WHERE id = $1;

-- name: GetQueueByName :one
SELECT * FROM queues WHERE name = $1;

-- name: ListQueues :many
SELECT * FROM queues ORDER BY name;

-- name: UpdateQueue :one
UPDATE queues
SET display_name = $2, strategy = $3, moh_sound = $4,
    max_wait_sec = $5, max_wait_no_agent_sec = $6,
    announce_sound = $7, announce_frequency_sec = $8, tier_rules = $9,
    discard_abandoned_after_sec = $10, is_abandoned_resume_allowed = $11,
    rona_delay_sec = $12, sla_threshold_sec = $13, is_recording_enabled = $14,
    hours = $15, overflow = $16, is_enabled = $17, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteQueue :exec
DELETE FROM queues WHERE id = $1;

-- name: SetQueueAgent :exec
INSERT INTO queue_agents (queue_id, agent_id, level, position)
VALUES ($1, $2, $3, $4)
ON CONFLICT (queue_id, agent_id)
DO UPDATE SET level = excluded.level, position = excluded.position;

-- name: RemoveQueueAgent :exec
DELETE FROM queue_agents WHERE queue_id = $1 AND agent_id = $2;

-- name: ListQueueAgents :many
SELECT qa.queue_id, qa.agent_id, qa.level, qa.position,
       a.callcenter_name, u.display_name
FROM queue_agents qa
JOIN agents a ON a.id = qa.agent_id
JOIN users u ON u.id = a.user_id
WHERE qa.queue_id = $1
ORDER BY qa.level, qa.position;

-- name: ListQueuesForAgent :many
SELECT q.*
FROM queues q
JOIN queue_agents qa ON qa.queue_id = q.id
WHERE qa.agent_id = $1
ORDER BY q.name;

-- name: CreateDID :one
INSERT INTO dids (id, number, language, target_kind, flow_id, queue_id, description, is_enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetDIDByNumber :one
SELECT * FROM dids WHERE number = $1;

-- name: ListDIDs :many
SELECT * FROM dids ORDER BY number;

-- name: UpdateDID :one
UPDATE dids
SET language = $2, target_kind = $3, flow_id = $4, queue_id = $5,
    description = $6, is_enabled = $7
WHERE id = $1
RETURNING *;

-- name: DeleteDID :exec
DELETE FROM dids WHERE id = $1;

-- name: ListTrunks :many
SELECT * FROM trunks ORDER BY name;

-- name: CreateTrunk :one
INSERT INTO trunks (id, name, direction, max_channels, config, is_enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateTrunk :one
UPDATE trunks
SET direction = $2, max_channels = $3, config = $4, is_enabled = $5, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteTrunk :exec
DELETE FROM trunks WHERE id = $1;
