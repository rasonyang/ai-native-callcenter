-- SPDX-License-Identifier: Apache-2.0
-- An extension an agent works at cannot be deleted out from under them.
--
-- The foreign key was the only thing standing between a mistyped delete and a
-- silently unbound agent, and it was ON DELETE SET NULL — which is not a guard
-- but an instruction to do the damage quietly. Deleting an extension an agent
-- had as their phone returned 204, removed the row, set their
-- default_extension_id to NULL and said nothing. In the application that agent
-- was still READY, so the queue kept offering them calls; their phone simply
-- failed its next registration. Every trail from that symptom leads to the
-- handset, not to a delete somebody made minutes earlier.
--
-- RESTRICT makes the database refuse. It holds for every path, not just the
-- one that goes through the API, which is why the guard lives here rather than
-- in a service check: the delete must fail even when it arrives by psql.
-- Unbinding the agent first is the deliberate act that was missing.
--
-- No data to rewrite: SET NULL only ever produced NULLs, and a NULL binding is
-- exactly what RESTRICT permits. Rows that point at a live extension are valid
-- under both rules, so a populated database revalidates without a rewrite.

-- +goose Up
ALTER TABLE agents DROP CONSTRAINT fk_agents_extensions;
ALTER TABLE agents ADD CONSTRAINT fk_agents_extensions
    FOREIGN KEY (default_extension_id) REFERENCES extensions (id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE agents DROP CONSTRAINT fk_agents_extensions;
ALTER TABLE agents ADD CONSTRAINT fk_agents_extensions
    FOREIGN KEY (default_extension_id) REFERENCES extensions (id) ON DELETE SET NULL;
