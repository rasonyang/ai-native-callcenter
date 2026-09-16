-- SPDX-License-Identifier: Apache-2.0
-- The directory carries a fallback credential again, behind the session one.
--
-- 00031 took extensions.password out of luacc.directory, and with it the only
-- credential a phone the platform never provisioned could have used. The
-- consequence was not visible in that migration: an agent who has not signed
-- in to the SPA has no SIP session, the directory hands the switch no
-- credential at all, and the Lua handler refuses the lookup — so a handset
-- configured by hand gets 403 and there is no way to register a phone except
-- through a browser. Phase 1 is browser-phone only by plan, not by
-- prohibition, and shutting the manual path is a prohibition nobody chose.
--
-- Owner directive 2026-09-16 reopens it, with the session credential keeping
-- precedence. The view now offers both, and the handler picks exactly one:
--   a1_hash present  -> the agent is signed in, and the session credential is
--                       the one the phone was issued. Zero-config is unchanged
--                       and is still the main path.
--   a1_hash NULL     -> nobody is signed in at this extension, so the static
--                       password is what a manually configured phone
--                       authenticates with.
--   neither          -> no credential, and the handler refuses the
--                       authentication lookup exactly as it does today.
--
-- What is not reopened: sip_sessions still holds a digest and no plaintext,
-- and a session credential is never served beside the static one — FreeSWITCH
-- verifies one of them and the choice belongs here, not to the switch.
--
-- Dropped and recreated rather than replaced: PostgreSQL will add a column to
-- a view in place but never take one away, and Down has to take this one away
-- again. Dropping loses the grant that lets the confined Lua role read the
-- view, so it is given back explicitly on both sides — without it the switch
-- cannot read the directory and no phone registers at all.

-- +goose Up
DROP VIEW luacc.directory;
CREATE VIEW luacc.directory AS
SELECT e.number,
       e.password,
       e.display_name,
       e.is_enabled,
       COALESCE(a.is_auto_answer, false) AS is_auto_answer,
       COALESCE(a.callcenter_name, '')   AS callcenter_agent_name,
       ss.a1_hash
  FROM extensions e
  LEFT JOIN agents a ON a.default_extension_id = e.id
  -- Unchanged from 00031: the session must still be valid to count. An expired
  -- row is not a credential, and an extension whose session has run out falls
  -- back to the static password rather than becoming unregisterable.
  LEFT JOIN sip_sessions ss ON ss.extension = e.number AND ss.expires_at > now()
 WHERE e.is_enabled;
-- +goose StatementBegin
DO $$
BEGIN
    -- Only where the role exists. A deployment that never ran lua_role.sql has
    -- no grant to give back. The grant is on the view alone: the Lua role must
    -- never reach sip_sessions itself, and nothing here gives it that.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'aicc_lua') THEN
        GRANT SELECT ON luacc.directory TO aicc_lua;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP VIEW luacc.directory;
CREATE VIEW luacc.directory AS
SELECT e.number,
       e.display_name,
       e.is_enabled,
       COALESCE(a.is_auto_answer, false) AS is_auto_answer,
       COALESCE(a.callcenter_name, '')   AS callcenter_agent_name,
       ss.a1_hash
  FROM extensions e
  LEFT JOIN agents a ON a.default_extension_id = e.id
  LEFT JOIN sip_sessions ss ON ss.extension = e.number AND ss.expires_at > now()
 WHERE e.is_enabled;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'aicc_lua') THEN
        GRANT SELECT ON luacc.directory TO aicc_lua;
    END IF;
END
$$;
-- +goose StatementEnd
