-- SPDX-License-Identifier: Apache-2.0

-- The list of test numbers (docs/design/10-campaigns.md section 5.2). Every
-- number is canonical E.164; the callers canonicalise before they get here.

-- name: LockTestNumbers :exec
-- Serialises the adds, so the size check and the insert see one list.
SELECT pg_advisory_xact_lock(sqlc.arg('lock_key')::bigint);

-- name: InsertTestNumber :one
-- Idempotent: an existing number is left as it is and no row comes back. A new
-- number is added only while the list holds fewer than max_entries; the caller
-- holds LockTestNumbers, so the count is the list as the insert sees it.
INSERT INTO test_numbers (number_e164, note, created_by)
SELECT sqlc.arg('number_e164')::text, sqlc.narg('note')::varchar, sqlc.narg('created_by')::uuid
WHERE (SELECT count(*) FROM test_numbers) < sqlc.arg('max_entries')::bigint
ON CONFLICT (number_e164) DO NOTHING
RETURNING *;

-- name: GetTestNumber :one
SELECT * FROM test_numbers WHERE number_e164 = $1;

-- name: DeleteTestNumber :execrows
DELETE FROM test_numbers WHERE number_e164 = $1;

-- name: ListTestNumbers :many
SELECT * FROM test_numbers
WHERE (sqlc.arg('prefix')::text = '' OR starts_with(number_e164, sqlc.arg('prefix')))
ORDER BY created_at DESC, number_e164
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountTestNumbers :one
SELECT count(*) FROM test_numbers
WHERE (sqlc.arg('prefix')::text = '' OR starts_with(number_e164, sqlc.arg('prefix')));
