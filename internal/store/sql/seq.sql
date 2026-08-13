-- SPDX-License-Identifier: Apache-2.0

-- name: ReserveSeqBlock :one
UPDATE seq_blocks
SET value = value + sqlc.arg(block_size)::bigint
WHERE name = sqlc.arg(name)
RETURNING value;

-- name: GetSetting :one
SELECT * FROM settings WHERE key = $1;

-- name: UpsertSetting :one
INSERT INTO settings (key, value, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = now()
RETURNING *;
