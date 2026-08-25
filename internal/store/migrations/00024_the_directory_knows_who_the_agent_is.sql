-- SPDX-License-Identifier: Apache-2.0
-- luacc.directory gains the switch's own name for the agent at an extension.
--
-- mod_callcenter has to be told when an agent is busy on a call it did not
-- place, or its queues go on ringing a phone that is already talking — and the
-- phone is left to say no, which costs the agent a busy delay or, on its slot
-- race, a call counted against them as one they failed to answer. The field
-- the module keeps for this is external_calls_count, and callcenter_track
-- fills it in; all it needs is the agent's name.
--
-- Everything that has to say that name lives on the switch's side of the
-- boundary — the directory entry a phone registers against, the dialplan rule
-- that bridges one extension to another — and the luacc views are the only way
-- anything there can ask this database a question. The join is already here:
-- the view has had agents on it since it learned about auto-answer, so this is
-- one more column off a table it already reads.
--
-- Empty for an extension nobody works at, which is most of them. Lua treats
-- that as "no agent to track" rather than as a lookup that failed.

-- +goose Up
CREATE OR REPLACE VIEW luacc.directory AS
SELECT e.number,
       e.password,
       e.display_name,
       e.is_enabled,
       COALESCE(a.is_auto_answer, false) AS is_auto_answer,
       COALESCE(a.callcenter_name, '')   AS callcenter_agent_name
  FROM extensions e
  LEFT JOIN agents a ON a.default_extension_id = e.id
 WHERE e.is_enabled;

-- +goose Down
--
-- Dropped and recreated rather than replaced: PostgreSQL will add a column to
-- a view in place but never take one away. Dropping loses the grant that lets
-- the confined Lua role read it, so it is given back explicitly — without
-- that, rolling this migration back leaves a directory the switch cannot read
-- and every phone unable to register.
DROP VIEW luacc.directory;
CREATE VIEW luacc.directory AS
SELECT e.number,
       e.password,
       e.display_name,
       e.is_enabled,
       COALESCE(a.is_auto_answer, false) AS is_auto_answer
  FROM extensions e
  LEFT JOIN agents a ON a.default_extension_id = e.id
 WHERE e.is_enabled;
-- +goose StatementBegin
DO $$
BEGIN
    -- Only where the role exists. A deployment that never ran lua_role.sql has
    -- no grant to give back, and failing the rollback over it would be a
    -- migration refusing to undo itself for a thing it did not do.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'aicc_lua') THEN
        GRANT SELECT ON luacc.directory TO aicc_lua;
    END IF;
END
$$;
-- +goose StatementEnd
