-- SPDX-License-Identifier: Apache-2.0

-- The do-not-call list. Every number is canonical E.164; the callers
-- canonicalise before they get here.

-- name: InsertDNCNumber :one
-- Idempotent: an existing number is left as it is and no row comes back, which
-- is how the caller tells "added" from "was already there".
INSERT INTO dnc_numbers (number_e164, source, call_id, created_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (number_e164) DO NOTHING
RETURNING *;

-- name: GetDNCNumber :one
SELECT * FROM dnc_numbers WHERE number_e164 = $1;

-- name: IsDNCNumber :one
SELECT EXISTS (SELECT 1 FROM dnc_numbers WHERE number_e164 = $1);

-- name: DeleteDNCNumber :execrows
DELETE FROM dnc_numbers WHERE number_e164 = $1;

-- name: ListDNCNumbers :many
SELECT * FROM dnc_numbers
WHERE (sqlc.arg('prefix')::text = '' OR starts_with(number_e164, sqlc.arg('prefix')))
ORDER BY created_at DESC, number_e164
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountDNCNumbers :one
SELECT count(*) FROM dnc_numbers
WHERE (sqlc.arg('prefix')::text = '' OR starts_with(number_e164, sqlc.arg('prefix')));
