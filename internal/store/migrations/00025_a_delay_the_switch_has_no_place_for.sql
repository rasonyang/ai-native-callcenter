-- SPDX-License-Identifier: Apache-2.0
-- queues.rona_delay_sec goes, because there is nowhere for it to arrive.
--
-- It described how long a queue waits before offering a call again after an
-- agent let it ring out. mod_callcenter has no such setting: the columns of
-- `callcenter_config queue list` carry no RONA delay, because the module keeps
-- that delay on the *agent* as no_answer_delay_time. That is not an oversight
-- on its part — an agent tiered into two queues can only have one delay, so a
-- per-queue value has no answer when the two disagree.
--
-- So the column was readable, editable and published in the contract while
-- aicc_xml.lua never delivered it anywhere and nothing ever consulted it: a
-- setting an operator could change, with no effect they could observe. The
-- delay itself is not lost — no_answer_delay_time=60 rides every agent's
-- registration into the switch (W2, 2026-08-23) and was measured working on
-- 2026-08-25.
--
-- Removing it is a breaking change to the contract: ronaDelaySec was a
-- required property of Queue. It is declared as one rather than kept as a
-- field that lies.

-- +goose Up
ALTER TABLE queues DROP COLUMN rona_delay_sec;

-- +goose Down
-- The default is the one 00002 shipped. Rows created while the column was gone
-- come back with it, which is right: nothing consumed the value, so no row can
-- be said to have had a different one.
ALTER TABLE queues ADD COLUMN rona_delay_sec int NOT NULL DEFAULT 10
    CHECK (rona_delay_sec >= 0);
