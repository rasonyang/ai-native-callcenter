-- SPDX-License-Identifier: Apache-2.0
-- +goose Up

-- Conversation flows. The draft is what an author edits; a revision is an
-- immutable copy taken at publish time, and calls only ever run the published
-- revision — an edit in progress must never change what a live number does.
CREATE TABLE flows (
    id                    uuid PRIMARY KEY,
    slug                  text NOT NULL,
    name                  text NOT NULL,
    draft_spec            jsonb NOT NULL,
    published_revision_id uuid,
    published_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_flows_slug UNIQUE (slug)
);

CREATE TABLE flow_revisions (
    id         uuid PRIMARY KEY,
    flow_id    uuid NOT NULL,
    spec       jsonb NOT NULL,
    note       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fk_flow_revisions_flows FOREIGN KEY (flow_id)
        REFERENCES flows (id) ON DELETE CASCADE
);

CREATE INDEX idx_flow_revisions_flow_id ON flow_revisions (flow_id);

ALTER TABLE flows
    ADD CONSTRAINT fk_flows_published_revision FOREIGN KEY (published_revision_id)
        REFERENCES flow_revisions (id) ON DELETE SET NULL;

-- A number can now point at its flow properly. Numbers created before this
-- table existed may carry identifiers that never referred to anything; they
-- become unassigned rather than blocking the migration.
UPDATE dids SET flow_id = NULL
WHERE flow_id IS NOT NULL AND flow_id NOT IN (SELECT id FROM flows);

ALTER TABLE dids
    ADD CONSTRAINT fk_dids_flows FOREIGN KEY (flow_id)
        REFERENCES flows (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE dids DROP CONSTRAINT fk_dids_flows;
ALTER TABLE flows DROP CONSTRAINT fk_flows_published_revision;
DROP TABLE flow_revisions;
DROP TABLE flows;
