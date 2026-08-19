-- SPDX-License-Identifier: Apache-2.0

-- Contacts are keyed by phone number: the cockpit's one question is "who is
-- this number", and last_call_at is answered from the ledger by that number.

-- name: InsertContact :one
INSERT INTO contacts (id, phone_number, name, company, email, tags, notes, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateContact :one
UPDATE contacts
SET phone_number = $2, name = $3, company = $4, email = $5, tags = $6, notes = $7,
    updated_by = $8, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteContact :execrows
DELETE FROM contacts WHERE id = $1;

-- name: GetContact :one
SELECT * FROM contacts WHERE id = $1;

-- name: ListContacts :many
SELECT sqlc.embed(c),
       (SELECT max(started_at) FROM cdrs
         WHERE from_number = c.phone_number OR to_number = c.phone_number)::timestamptz AS last_call_at
FROM contacts c
WHERE (sqlc.arg('phone_number')::text = '' OR c.phone_number = sqlc.arg('phone_number'))
  AND (sqlc.arg('q')::text = ''
       OR c.phone_number ILIKE '%' || sqlc.arg('q') || '%'
       OR c.name ILIKE '%' || sqlc.arg('q') || '%'
       OR c.company ILIKE '%' || sqlc.arg('q') || '%')
ORDER BY c.updated_at DESC
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: CountContacts :one
SELECT count(*) FROM contacts c
WHERE (sqlc.arg('phone_number')::text = '' OR c.phone_number = sqlc.arg('phone_number'))
  AND (sqlc.arg('q')::text = ''
       OR c.phone_number ILIKE '%' || sqlc.arg('q') || '%'
       OR c.name ILIKE '%' || sqlc.arg('q') || '%'
       OR c.company ILIKE '%' || sqlc.arg('q') || '%');
