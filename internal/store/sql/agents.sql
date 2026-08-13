-- SPDX-License-Identifier: Apache-2.0

-- name: CreateAgent :one
INSERT INTO agents (id, user_id, callcenter_name, wrap_up_time_sec, is_auto_answer, default_extension_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAgent :one
SELECT * FROM agents WHERE id = $1;

-- name: GetAgentByUserID :one
SELECT * FROM agents WHERE user_id = $1;

-- name: UpdateAgent :one
UPDATE agents
SET callcenter_name = $2, wrap_up_time_sec = $3, is_auto_answer = $4, default_extension_id = $5
WHERE id = $1
RETURNING *;

-- name: DeleteAgent :exec
DELETE FROM agents WHERE id = $1;

-- The roster: one row per agent with everything a wallboard needs, so the
-- derived availability can be computed without a second query.
-- name: ListAgentRoster :many
SELECT a.id AS agent_id,
       a.callcenter_name,
       a.wrap_up_time_sec,
       a.is_auto_answer,
       u.id AS user_id,
       u.username,
       u.display_name,
       u.status AS user_status,
       COALESCE(s.state, 'LOGGED_OUT') AS state,
       s.reason,
       s.extension_number,
       COALESCE(s.entered_at, a.created_at) AS entered_at,
       s.wrap_up_ends_at
FROM agents a
JOIN users u ON u.id = a.user_id
LEFT JOIN agent_states s ON s.agent_id = a.id
ORDER BY u.display_name;

-- name: GetAgentState :one
SELECT * FROM agent_states WHERE agent_id = $1;

-- name: UpsertAgentState :one
INSERT INTO agent_states (agent_id, state, reason, extension_number, entered_at, wrap_up_ends_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (agent_id) DO UPDATE
SET state            = excluded.state,
    reason           = excluded.reason,
    extension_number = excluded.extension_number,
    entered_at       = excluded.entered_at,
    wrap_up_ends_at  = excluded.wrap_up_ends_at
RETURNING *;

-- name: ListAgentStates :many
SELECT * FROM agent_states;

-- name: FindAgentByExtension :one
SELECT a.*
FROM agents a
JOIN agent_states s ON s.agent_id = a.id
WHERE s.extension_number = $1 AND s.state <> 'LOGGED_OUT';

-- name: OpenAgentStateLog :one
INSERT INTO agent_state_logs (agent_id, state, reason, entered_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CloseAgentStateLog :exec
UPDATE agent_state_logs
SET exited_at = $2
WHERE agent_id = $1 AND exited_at IS NULL;
