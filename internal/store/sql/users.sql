-- SPDX-License-Identifier: Apache-2.0

-- name: CreateUser :one
INSERT INTO users (id, username, password_hash, display_name, role, status, locale)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY username;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- Everything the accounts screen shows in one read: the account, and the ACD
-- identity and phone it has when it has them. An administrator matches neither
-- join, which is the point — a user is not always an agent.

-- name: ListAccounts :many
SELECT u.*, a.id AS agent_id, a.callcenter_name, e.number AS extension_number
FROM users u
LEFT JOIN agents a ON a.user_id = u.id
LEFT JOIN extensions e ON e.id = a.default_extension_id
ORDER BY u.username;

-- name: GetAccount :one
SELECT u.*, a.id AS agent_id, a.callcenter_name, e.number AS extension_number
FROM users u
LEFT JOIN agents a ON a.user_id = u.id
LEFT JOIN extensions e ON e.id = a.default_extension_id
WHERE u.id = $1;

-- name: UpdateUser :one
UPDATE users
SET username = $2, display_name = $3, role = $4, status = $5, locale = $6,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- Guards the last way in. Counted inside the same transaction as the change,
-- so two administrators cannot each demote the other by acting at once.

-- name: CountOtherActiveAdmins :one
SELECT count(*) FROM users
WHERE role = 'ADMIN' AND status = 'ACTIVE' AND id <> $1;
