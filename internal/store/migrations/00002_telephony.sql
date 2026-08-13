-- SPDX-License-Identifier: Apache-2.0
-- M2 telephony: extensions, agents, queues, DIDs, trunks, and the luacc views
-- that FreeSWITCH reads through mod_lua.
--
-- The luacc views are a contract with the switch, not an implementation
-- detail: any later migration that reshapes these base tables must re-assert
-- the view shapes in the same file and is reviewed as a schema plus Lua pair
-- (docs/design/03-data.md §4).
--
-- Enum columns store SCREAMING_SNAKE strings identical to their JSON values;
-- mod_callcenter's own vocabulary (longest-idle-agent, "On Break") is produced
-- at the boundary and never stored here.

-- +goose Up

CREATE TABLE extensions (
    id           uuid PRIMARY KEY,
    number       text NOT NULL,
    kind         varchar(16) NOT NULL CHECK (kind IN ('AGENT', 'BOT', 'PLAIN')),
    -- Write-only through the API; read by the Lua directory handler.
    password     text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    is_enabled   bool NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_extensions_number UNIQUE (number)
);

CREATE TABLE agents (
    id                   uuid PRIMARY KEY,
    user_id              uuid NOT NULL,
    -- mod_callcenter identifies agents by a stable name of our choosing.
    callcenter_name      text NOT NULL,
    wrap_up_time_sec     int NOT NULL DEFAULT 30 CHECK (wrap_up_time_sec >= 0),
    is_auto_answer       bool NOT NULL DEFAULT false,
    default_extension_id uuid,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_agents_user_id UNIQUE (user_id),
    CONSTRAINT uq_agents_callcenter_name UNIQUE (callcenter_name),
    CONSTRAINT fk_agents_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_agents_extensions FOREIGN KEY (default_extension_id)
        REFERENCES extensions (id) ON DELETE SET NULL
);

-- The live presence row. Written synchronously in-request: acknowledging a
-- state change we could not record would lie to the agent.
CREATE TABLE agent_states (
    agent_id         uuid PRIMARY KEY,
    state            varchar(16) NOT NULL DEFAULT 'LOGGED_OUT'
        CHECK (state IN ('LOGGED_OUT', 'NOT_READY', 'READY')),
    reason           varchar(24)
        CHECK (reason IS NULL OR reason IN ('LOGIN', 'BREAK', 'LUNCH', 'TRAINING',
                                            'AFTER_CALL_WORK', 'SYSTEM', 'SUPERVISOR')),
    extension_number text,
    entered_at       timestamptz NOT NULL DEFAULT now(),
    wrap_up_ends_at  timestamptz,
    CONSTRAINT fk_agent_states_agents FOREIGN KEY (agent_id) REFERENCES agents (id) ON DELETE CASCADE
);

-- History behind occupancy and adherence reporting.
CREATE TABLE agent_state_logs (
    id         bigserial PRIMARY KEY,
    agent_id   uuid NOT NULL,
    state      varchar(16) NOT NULL,
    reason     varchar(24),
    entered_at timestamptz NOT NULL,
    exited_at  timestamptz,
    CONSTRAINT fk_agent_state_logs_agents FOREIGN KEY (agent_id) REFERENCES agents (id) ON DELETE CASCADE
);

CREATE INDEX idx_agent_state_logs_agent_id_entered_at ON agent_state_logs (agent_id, entered_at DESC);

CREATE TABLE queues (
    id                          uuid PRIMARY KEY,
    name                        text NOT NULL,
    ext_number                  text NOT NULL,
    display_name                text NOT NULL DEFAULT '',
    strategy                    varchar(32) NOT NULL DEFAULT 'LONGEST_IDLE_AGENT'
        CHECK (strategy IN ('LONGEST_IDLE_AGENT', 'ROUND_ROBIN', 'TOP_DOWN',
                            'AGENT_WITH_LEAST_TALK_TIME', 'AGENT_WITH_FEWEST_CALLS', 'RANDOM')),
    moh_sound                   text NOT NULL DEFAULT '$${hold_music}',
    max_wait_sec                int NOT NULL DEFAULT 0 CHECK (max_wait_sec >= 0),
    max_wait_no_agent_sec       int NOT NULL DEFAULT 0 CHECK (max_wait_no_agent_sec >= 0),
    announce_sound              text,
    announce_frequency_sec      int NOT NULL DEFAULT 0 CHECK (announce_frequency_sec >= 0),
    tier_rules                  jsonb NOT NULL DEFAULT '{"isApplied": false, "waitSec": 300}'::jsonb,
    discard_abandoned_after_sec int NOT NULL DEFAULT 60 CHECK (discard_abandoned_after_sec >= 0),
    is_abandoned_resume_allowed bool NOT NULL DEFAULT false,
    rona_delay_sec              int NOT NULL DEFAULT 10 CHECK (rona_delay_sec >= 0),
    sla_threshold_sec           int NOT NULL DEFAULT 20 CHECK (sla_threshold_sec >= 0),
    is_recording_enabled        bool NOT NULL DEFAULT true,
    -- [{"weekday": 1, "open": "09:00", "close": "18:00"}]; empty means always open.
    hours                       jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- {"type": "BOT_FLOW" | "ANNOUNCE_HANGUP" | "FORWARD", ...}
    overflow                    jsonb NOT NULL DEFAULT '{"type": "ANNOUNCE_HANGUP"}'::jsonb,
    is_enabled                  bool NOT NULL DEFAULT true,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_queues_name UNIQUE (name),
    CONSTRAINT uq_queues_ext_number UNIQUE (ext_number)
);

-- Staffing. Level and position are mod_callcenter's tier ordering.
CREATE TABLE queue_agents (
    queue_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    level    int NOT NULL DEFAULT 1 CHECK (level >= 1),
    position int NOT NULL DEFAULT 1 CHECK (position >= 1),
    PRIMARY KEY (queue_id, agent_id),
    CONSTRAINT fk_queue_agents_queues FOREIGN KEY (queue_id) REFERENCES queues (id) ON DELETE CASCADE,
    CONSTRAINT fk_queue_agents_agents FOREIGN KEY (agent_id) REFERENCES agents (id) ON DELETE CASCADE
);

CREATE INDEX idx_queue_agents_agent_id ON queue_agents (agent_id);

-- Every external number answers with a bot flow: this is an AI-native call
-- center, so the model talks first and hands off to a queue by calling
-- transfer_to_agent. A DID therefore has no alternative target, only a
-- fallback for the case where the bot cannot run at all (provider outage, bot
-- capacity exhausted), where the caller must still reach a human.
CREATE TABLE dids (
    id                   uuid PRIMARY KEY,
    number               text NOT NULL,
    -- BCP 47 language subtag, lowercase, consumed verbatim by the frontend.
    language             varchar(8) NOT NULL DEFAULT 'en',
    flow_id              uuid NOT NULL,
    fallback_queue_id    uuid,
    is_recording_enabled bool NOT NULL DEFAULT true,
    description          text NOT NULL DEFAULT '',
    is_enabled           bool NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_dids_number UNIQUE (number),
    CONSTRAINT fk_dids_queues FOREIGN KEY (fallback_queue_id)
        REFERENCES queues (id) ON DELETE SET NULL
);

CREATE TABLE trunks (
    id           uuid PRIMARY KEY,
    name         text NOT NULL,
    direction    varchar(16) NOT NULL DEFAULT 'BIDIRECTIONAL'
        CHECK (direction IN ('INBOUND', 'OUTBOUND', 'BIDIRECTIONAL')),
    max_channels int NOT NULL DEFAULT 0 CHECK (max_channels >= 0),
    -- camelCase keys: {"proxy": "...", "isRegister": false, "username": "..."}
    config       jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_enabled   bool NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_trunks_name UNIQUE (name)
);

-- The FreeSWITCH read contract. Lua sees only these views, never base tables.
CREATE SCHEMA luacc;

-- Directory lookups key on the extension number alone: a phone registering
-- through the WSS proxy presents that host as its digest realm, which is not
-- the profile domain the registration is stored under.
CREATE VIEW luacc.directory AS
SELECT e.number,
       e.password,
       e.display_name,
       e.is_enabled,
       COALESCE(a.is_auto_answer, false) AS is_auto_answer
FROM extensions e
LEFT JOIN agents a ON a.default_extension_id = e.id
WHERE e.is_enabled;

-- The dialplan only needs to know that the number is ours, whether to record,
-- and where to send the caller if the bot cannot take the call. Which flow
-- runs is resolved by the application from the DID, so the flow catalogue
-- never becomes part of the switch contract.
CREATE VIEW luacc.dids AS
SELECT d.number,
       d.language,
       d.is_recording_enabled,
       q.ext_number AS fallback_queue_ext_number,
       d.is_enabled
FROM dids d
LEFT JOIN queues q ON q.id = d.fallback_queue_id
WHERE d.is_enabled;

-- jsonb is flattened here on purpose: mod_lua ships no JSON parser, so the
-- switch-side scripts must never have to decode a document.
CREATE VIEW luacc.queues AS
SELECT name,
       ext_number,
       strategy,
       moh_sound,
       max_wait_sec,
       max_wait_no_agent_sec,
       announce_sound,
       announce_frequency_sec,
       COALESCE((tier_rules ->> 'isApplied')::bool, false) AS is_tier_rules_applied,
       COALESCE((tier_rules ->> 'waitSec')::int, 300) AS tier_rule_wait_sec,
       discard_abandoned_after_sec,
       is_abandoned_resume_allowed,
       is_recording_enabled,
       COALESCE(overflow ->> 'type', 'ANNOUNCE_HANGUP') AS overflow_type,
       overflow ->> 'target' AS overflow_target,
       overflow ->> 'sound' AS overflow_sound,
       is_enabled
FROM queues
WHERE is_enabled;

-- +goose Down
DROP VIEW luacc.queues;
DROP VIEW luacc.dids;
DROP VIEW luacc.directory;
DROP SCHEMA luacc;
DROP TABLE trunks;
DROP TABLE dids;
DROP TABLE queue_agents;
DROP TABLE queues;
DROP TABLE agent_state_logs;
DROP TABLE agent_states;
DROP TABLE agents;
DROP TABLE extensions;
