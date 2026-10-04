-- SPDX-License-Identifier: Apache-2.0
--
-- Is this one of the platform's own numbers? The aicc context's
-- aicc_platform_number extension asks while it hunts:
--
--   ${lua(aicc_did_route.lua <number>)}
--
-- and writes "true" when the number is an enabled row of luacc.dids, anything
-- else otherwise. Run as an API, so it decides before any extension's actions
-- are collected: a platform number is transferred to the public doorway, and
-- every other number goes on to the rules after it (a trunk, the simulated
-- PSTN, or NO_ROUTE_DESTINATION) as if this script did not exist.
--
-- It never fails a call: a missing DSN or an unreachable database answers
-- "false", and the number is left to the rules below. Disabled DIDs are
-- absent from the view, so they behave as unknown numbers.

local number = argv[1]
if number == nil or number == "" then
  stream:write("false")
  return
end

local function log(level, message)
  freeswitch.consoleLog(level, "aicc_did_route: " .. message .. "\n")
end

local function quote(value)
  return "'" .. tostring(value or ""):gsub("'", "''") .. "'"
end

local dsn = freeswitch.getGlobalVariable("aicc_lua_dsn")
if dsn == nil or dsn == "" then
  log("err", "aicc_lua_dsn is not set; leaving " .. number .. " to the next rule")
  stream:write("false")
  return
end

local dbh = freeswitch.Dbh(dsn)
if not dbh:connected() then
  log("err", "cannot reach the database; leaving " .. number .. " to the next rule")
  stream:write("false")
  return
end

local found = false
dbh:query("SELECT number FROM luacc.dids WHERE number = " .. quote(number),
  function(row) found = true end)
dbh:release()

if found then
  log("info", "platform number " .. number .. " dialled from the aicc context; delivering to public")
end
stream:write(found and "true" or "false")
