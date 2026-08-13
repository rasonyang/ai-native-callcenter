-- SPDX-License-Identifier: Apache-2.0
--
-- Creates the read-only role FreeSWITCH uses through mod_lua.
--
-- Run once per database, as a superuser, after the application has applied its
-- migrations (the luacc views must exist). Role creation is deliberately kept
-- out of the migrations: an application should not need to be able to create
-- roles, and many managed PostgreSQL services forbid it.
--
--   psql "$AICC_DATABASE_URL" -v lua_password="'change-me'" -f deploy/sql/lua_role.sql
--
-- Then point the Lua scripts at it:
--   pgsql://hostaddr=127.0.0.1 dbname=aicc user=aicc_lua password='change-me'

\set ON_ERROR_STOP on

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'aicc_lua') THEN
        CREATE ROLE aicc_lua LOGIN;
    END IF;
END
$$;

ALTER ROLE aicc_lua PASSWORD :lua_password;

-- The switch reads the contract views and nothing else: no base tables, no
-- writes, no other schema.
GRANT CONNECT ON DATABASE :"DBNAME" TO aicc_lua;
GRANT USAGE ON SCHEMA luacc TO aicc_lua;
GRANT SELECT ON ALL TABLES IN SCHEMA luacc TO aicc_lua;
ALTER DEFAULT PRIVILEGES IN SCHEMA luacc GRANT SELECT ON TABLES TO aicc_lua;

REVOKE ALL ON SCHEMA public FROM aicc_lua;
