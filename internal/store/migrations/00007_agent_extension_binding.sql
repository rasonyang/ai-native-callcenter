-- SPDX-License-Identifier: Apache-2.0
-- +goose Up

-- The agent-to-extension binding is static configuration: an agent signs in at
-- the phone bound to them and nowhere else. Two agents sharing one extension
-- would make a registration ambiguous, so the database refuses it. The index is
-- partial because an agent may legitimately have no phone yet.
CREATE UNIQUE INDEX uq_agents_default_extension
    ON agents (default_extension_id)
    WHERE default_extension_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS uq_agents_default_extension;
