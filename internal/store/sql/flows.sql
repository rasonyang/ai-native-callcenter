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

-- name: DeleteFlow :exec
DELETE FROM flows WHERE id = $1;

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
