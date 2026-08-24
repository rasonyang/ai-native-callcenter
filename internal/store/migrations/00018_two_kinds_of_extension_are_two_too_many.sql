-- SPDX-License-Identifier: Apache-2.0
-- BOT and PLAIN leave the vocabulary. AGENT and QUEUE are the two an extension
-- can actually be.
--
-- PLAIN was a second way to say what AGENT already says. 00017's own CHECK
-- treated them identically — `kind IN ('AGENT', 'PLAIN') AND flow_id IS NULL
-- AND queue_id IS NULL` — and the live database says it louder: of 21
-- extensions, all labelled AGENT, only 7 are bound to anybody. "A phone nobody
-- works at" is already expressed fourteen times, by an AGENT row with no
-- binding. A second spelling for that is a question with two right answers,
-- and it would be asked every time somebody adds a desk phone.
--
-- BOT could not route anything. A caller reaches a bot through
-- `aicc_inbound.lua` bridging to sofia/gateway/aicc_bot/<did.number>, and the
-- application resolves that number to dids.flow_id when the INVITE arrives —
-- the extensions table appears nowhere on that path, and no dialplan or Lua
-- would ever have looked at a BOT row. It was a label for a routing mode that
-- does not exist. What does point at a flow is a DID, which is what the flows
-- screen already lists.
--
-- flow_id goes with it: it was BOT's target and had no other use. Should an
-- internal number that reaches a bot ever be wanted, it needs dialplan work
-- first, and this column would be the last part of it rather than the first.
--
-- The rewrite runs before the narrowed CHECK, not after: a deployment that
-- already labelled a phone BOT or PLAIN must migrate rather than be refused.
-- Both become AGENT, which is what they were doing anyway — a registerable
-- number, bound to somebody or not.

-- +goose Up
UPDATE extensions SET kind = 'AGENT' WHERE kind IN ('BOT', 'PLAIN');

ALTER TABLE extensions DROP CONSTRAINT extensions_target_matches_kind;
ALTER TABLE extensions DROP CONSTRAINT extensions_kind_check;
ALTER TABLE extensions DROP CONSTRAINT fk_extensions_flows;
ALTER TABLE extensions DROP COLUMN flow_id;

ALTER TABLE extensions ADD CONSTRAINT extensions_kind_check
    CHECK (kind IN ('AGENT', 'QUEUE'));
ALTER TABLE extensions ADD CONSTRAINT extensions_target_matches_kind
    CHECK (kind = 'QUEUE' OR queue_id IS NULL);

-- +goose Down
ALTER TABLE extensions DROP CONSTRAINT extensions_target_matches_kind;
ALTER TABLE extensions DROP CONSTRAINT extensions_kind_check;

ALTER TABLE extensions ADD COLUMN flow_id uuid;
ALTER TABLE extensions ADD CONSTRAINT fk_extensions_flows FOREIGN KEY (flow_id)
    REFERENCES flows (id) ON DELETE RESTRICT;
ALTER TABLE extensions ADD CONSTRAINT extensions_kind_check
    CHECK (kind IN ('AGENT', 'BOT', 'QUEUE', 'PLAIN'));
ALTER TABLE extensions ADD CONSTRAINT extensions_target_matches_kind CHECK (
    (kind = 'BOT'   AND queue_id IS NULL)
 OR (kind = 'QUEUE' AND flow_id  IS NULL)
 OR (kind IN ('AGENT', 'PLAIN') AND flow_id IS NULL AND queue_id IS NULL)
);
