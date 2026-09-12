-- SPDX-License-Identifier: Apache-2.0

-- name: UpsertSIPSession :one
-- One row per agent: issuing a session replaces the previous one, whichever
-- extension it was for, so an agent's phone credential has exactly one holder.
INSERT INTO sip_sessions (agent_id, extension, a1_hash, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (agent_id) DO UPDATE
   SET extension  = EXCLUDED.extension,
       a1_hash    = EXCLUDED.a1_hash,
       created_at = now(),
       expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: GetSIPSession :one
SELECT * FROM sip_sessions WHERE agent_id = $1;

-- name: DeleteSIPSession :one
-- Returns the extension the session was for, because revoking it means
-- flushing the registration it was holding and that is the number to flush.
DELETE FROM sip_sessions WHERE agent_id = $1 RETURNING extension;

-- name: DeleteExpiredSIPSessions :execrows
DELETE FROM sip_sessions WHERE expires_at <= now();
