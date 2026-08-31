-- SPDX-License-Identifier: Apache-2.0

-- name: CreateAPIKey :one
INSERT INTO api_keys (id, name, key_hash, key_prefix, scopes, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- The authentication lookup. By hash, directly — the same shape
-- GetSessionByTokenHash uses, and for the same reason: the digest is the only
-- thing both sides can compute, so there is nothing to scan and nothing to
-- compare in application code.
--
-- The status condition is inlined rather than checked by the caller so that a
-- revoked key is not a row that came back and was then rejected: it does not
-- come back. That is what makes REVOKED terminal at the only place it matters.
-- name: GetAPIKeyByHash :one
SELECT * FROM api_keys WHERE key_hash = $1 AND status = 'ENABLED';

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1 AND status = 'ENABLED';

-- name: GetAPIKey :one
SELECT * FROM api_keys WHERE id = $1;

-- name: ListAPIKeys :many
SELECT * FROM api_keys ORDER BY created_at DESC;

-- A revoked key is not edited. It is a historical record: the audit trail
-- names it, and rows from months ago say what it did — with the capabilities
-- it held at the time. Letting its scopes be rewritten afterwards makes those
-- rows describe a key that never existed, which is the one table nothing may
-- falsify. The contract says so too ("A revoked key cannot be edited"), and
-- the condition lives in the WHERE rather than in a caller's if, for the same
-- reason revocation's does: a rule enforced where the row is touched cannot be
-- bypassed by a second caller who forgot it.
-- name: UpdateAPIKey :one
UPDATE api_keys
SET name   = coalesce(sqlc.narg('name'), name),
    scopes = coalesce(sqlc.narg('scopes')::text[], scopes)
WHERE id = $1 AND status = 'ENABLED'
RETURNING *;

-- Revocation is terminal, and this is where that is true rather than in a
-- comment: the WHERE clause matches only an enabled key, so revoking twice
-- returns no row and the second caller is told the key is already revoked
-- instead of quietly moving revoked_at.
-- name: RevokeAPIKey :one
UPDATE api_keys
SET status = 'REVOKED', revoked_at = now()
WHERE id = $1 AND status = 'ENABLED'
RETURNING *;
