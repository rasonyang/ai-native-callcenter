-- SPDX-License-Identifier: Apache-2.0
-- A DID may exist before the flow catalogue does.
--
-- Every external number is still meant to answer with a bot; the flow
-- catalogue simply does not exist yet, so requiring one would make the numbers
-- unconfigurable until it lands. A number without a flow reaches the bot
-- gateway, fails, and falls back to its queue, which is the behaviour the
-- inbound script already implements for an unreachable bot.

-- +goose Up
ALTER TABLE dids ALTER COLUMN flow_id DROP NOT NULL;

-- +goose Down
ALTER TABLE dids ALTER COLUMN flow_id SET NOT NULL;
