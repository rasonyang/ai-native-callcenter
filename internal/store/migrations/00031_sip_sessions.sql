-- SPDX-License-Identifier: Apache-2.0
-- A phone's credential is minted for a session, and the directory stops
-- carrying a password at all.
--
-- Until now luacc.directory handed FreeSWITCH extensions.password in clear
-- text, and a person had to be told that password so they could type it into a
-- softphone. Two things were wrong with it. The credential outlived every
-- reason it existed — an agent who left still knew it, and a phone configured
-- once stayed registered forever — and it sat in a column, in a view and in a
-- dialogue box, three places a plaintext SIP password can leak from.
--
-- What replaces it is a session: the application mints a random password when
-- an agent signs in, hands the phone the digest of it, and keeps only the
-- digest. a1_hash is RFC 2617's A1, md5(user:realm:password), which is the
-- only form SIP digest authentication can verify against — it is not a
-- password store and nothing here treats it as one. The password itself is a
-- local variable in one function and is never written down.
--
-- One row per agent, so issuing a session replaces the previous one and the
-- browser that was holding it stops authenticating. expires_at follows the web
-- session, so the phone is signed in for exactly as long as the person is.
--
-- The view loses the password column and gains the hash of whichever session
-- is currently valid for that number, NULL when there is none. Lua reads that
-- NULL as "no phone may register as this extension right now" — which is the
-- state every extension is in until somebody signs in.

-- +goose Up
CREATE TABLE sip_sessions (
    agent_id   uuid PRIMARY KEY REFERENCES agents (id) ON DELETE CASCADE,
    extension  text        NOT NULL,
    -- The hash and nothing else. There is deliberately no plaintext column
    -- here, and adding one later would undo the whole point of this table.
    a1_hash    text        NOT NULL CHECK (a1_hash ~ '^[0-9a-f]{32}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

-- The directory looks a session up by number, and the sweeper by expiry.
CREATE INDEX idx_sip_sessions_extension ON sip_sessions (extension);
CREATE INDEX idx_sip_sessions_expires_at ON sip_sessions (expires_at);

-- Dropped and recreated rather than replaced: PostgreSQL will add a column to
-- a view in place but never take one away, and password is the column this
-- migration exists to remove. Dropping loses the grant that lets the confined
-- Lua role read it, so it is given back explicitly — without that, the switch
-- cannot read the directory and no phone can register.
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
  -- The session must still be valid to count. An expired row is not a
  -- credential, and the phone holding it is told so by the switch rather than
  -- by us noticing later.
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
       e.password,
       e.display_name,
       e.is_enabled,
       COALESCE(a.is_auto_answer, false) AS is_auto_answer,
       COALESCE(a.callcenter_name, '')   AS callcenter_agent_name
  FROM extensions e
  LEFT JOIN agents a ON a.default_extension_id = e.id
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
DROP TABLE sip_sessions;
