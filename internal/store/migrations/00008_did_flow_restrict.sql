-- SPDX-License-Identifier: Apache-2.0
-- A flow cannot be deleted out from under the number that answers with it.
--
-- 00004 attached dids.flow_id to flows with ON DELETE SET NULL. That is a
-- silent path into a state phase1-decisions P4 forbids: a number without a
-- flow fails its bot leg and falls through to its fallback queue, which is
-- exactly the direct-to-queue route that decision rules out. Removing a flow
-- should be refused while a number still points at it, not quietly unhook
-- every number that did.
--
-- The column stays nullable, because a number is created before its flow is
-- attached: the admin screen writes the number, and `aicc flowadd -did` binds
-- the conversation to it afterwards. NULL is the gap between those two steps,
-- not a routing mode.

-- +goose Up
ALTER TABLE dids DROP CONSTRAINT fk_dids_flows;
ALTER TABLE dids
    ADD CONSTRAINT fk_dids_flows FOREIGN KEY (flow_id)
        REFERENCES flows (id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE dids DROP CONSTRAINT fk_dids_flows;
ALTER TABLE dids
    ADD CONSTRAINT fk_dids_flows FOREIGN KEY (flow_id)
        REFERENCES flows (id) ON DELETE SET NULL;
