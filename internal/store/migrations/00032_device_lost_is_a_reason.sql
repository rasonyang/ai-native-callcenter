-- SPDX-License-Identifier: Apache-2.0
-- DEVICE_LOST joins the not-ready vocabulary.
--
-- An agent who is READY and whose phone stops being registered is taken out of
-- routing by the platform, and the reason has to say who did it and why. None
-- of the seven existing values can: SYSTEM is the switch benching an agent who
-- ignored a delivered call, and the agent's own reasons would put a decision in
-- their mouth they never made. Losing a phone is neither.
--
-- The column is written by every presence change, so the CHECK is what decides
-- whether the state the application computed can be recorded at all. Without
-- this the transition succeeds in memory, the write is rejected, and the
-- service rolls back to READY — an agent left routable at a phone that is not
-- there, which is the exact failure the reason exists to end.
--
-- agent_state_logs.reason carries no CHECK of its own (00002) and needs none:
-- history records what presence held, and constraining it twice would mean a
-- vocabulary change had two places to be forgotten.
--
-- Down narrows, so it rewrites first. A deployment that ran this and is rolled
-- back holds rows the older vocabulary cannot express, and PostgreSQL refuses
-- the constraint outright while one survives. SYSTEM is what they become: the
-- platform took the agent out of routing, which is as much as the narrower
-- vocabulary can say, and it is true.

-- +goose Up
ALTER TABLE agent_states DROP CONSTRAINT agent_states_reason_check;
ALTER TABLE agent_states ADD CONSTRAINT agent_states_reason_check
    CHECK (reason IS NULL OR reason IN ('LOGIN', 'BREAK', 'LUNCH', 'TRAINING',
                                        'AFTER_CALL_WORK', 'SYSTEM', 'SUPERVISOR',
                                        'DEVICE_LOST'));

-- +goose Down
UPDATE agent_states SET reason = 'SYSTEM' WHERE reason = 'DEVICE_LOST';

ALTER TABLE agent_states DROP CONSTRAINT agent_states_reason_check;
ALTER TABLE agent_states ADD CONSTRAINT agent_states_reason_check
    CHECK (reason IS NULL OR reason IN ('LOGIN', 'BREAK', 'LUNCH', 'TRAINING',
                                        'AFTER_CALL_WORK', 'SYSTEM', 'SUPERVISOR'));
