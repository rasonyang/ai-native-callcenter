-- SPDX-License-Identifier: Apache-2.0

-- name: ListFlows :many
SELECT * FROM flows ORDER BY name;

-- name: GetFlow :one
SELECT * FROM flows WHERE id = $1;

-- name: CreateFlow :one
INSERT INTO flows (id, slug, name, draft_spec)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateFlowDraft :one
UPDATE flows
SET name = $2, draft_spec = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: CreateFlowRevision :one
INSERT INTO flow_revisions (id, flow_id, spec, note)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: SetPublishedRevision :exec
UPDATE flows
SET published_revision_id = $2, published_at = now(), updated_at = now()
WHERE id = $1;

-- name: GetPublishedSpec :one
SELECT r.spec
FROM flows f
JOIN flow_revisions r ON r.id = f.published_revision_id
WHERE f.id = $1;

-- name: GetFlowBySlug :one
SELECT * FROM flows WHERE slug = $1;

-- The list and detail reads never select draft_spec alongside the summary:
-- a roster of flows should not ship every spec, and the one place the draft
-- is wanted asks for it by itself.
--
-- has_unpublished_changes is decided by PostgreSQL, not by Go: jsonb equality
-- is semantic (key order and whitespace are not differences), and IS DISTINCT
-- FROM makes "never published" — a NULL revision — the true case it is.

-- name: ListFlowSummaries :many
SELECT f.id, f.slug, f.name, f.published_revision_id, f.published_at,
       f.created_at, f.updated_at,
       (r.spec IS DISTINCT FROM f.draft_spec)::boolean AS has_unpublished_changes
FROM flows f
LEFT JOIN flow_revisions r ON r.id = f.published_revision_id
ORDER BY f.name;

-- name: GetFlowSummary :one
SELECT f.id, f.slug, f.name, f.published_revision_id, f.published_at,
       f.created_at, f.updated_at,
       (r.spec IS DISTINCT FROM f.draft_spec)::boolean AS has_unpublished_changes
FROM flows f
LEFT JOIN flow_revisions r ON r.id = f.published_revision_id
WHERE f.id = $1;

-- name: GetFlowDraft :one
SELECT draft_spec FROM flows WHERE id = $1;

-- name: ListFlowRevisions :many
SELECT r.id, r.note, r.created_at,
       (r.id = f.published_revision_id)::boolean AS is_published
FROM flow_revisions r
JOIN flows f ON f.id = r.flow_id
WHERE r.flow_id = $1
ORDER BY r.created_at DESC, r.id DESC;
