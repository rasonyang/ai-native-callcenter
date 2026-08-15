-- SPDX-License-Identifier: Apache-2.0

-- name: ReserveSeqBlock :one
UPDATE seq_blocks
SET value = value + sqlc.arg(block_size)::bigint
WHERE name = sqlc.arg(name)
RETURNING value;
