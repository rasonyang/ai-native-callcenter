-- SPDX-License-Identifier: Apache-2.0
--
-- Builds the variable block for a leg raised *towards* one of our extensions,
-- so mod_callcenter learns the agent there is busy the moment it starts
-- ringing.
--
-- It exists because a directory entry cannot say this. Those variables reach a
-- channel the user themselves raises — their phone's own INVITE — and not one
-- raised at them: a leg originated at user/1002 carries none of user_context,
-- aicc_managed or aicc_extension (measured). So the agent making an internal
-- call is covered by their directory entry, and the agent being called is not
-- covered by anything until this runs.
--
-- The dial-string would reach that leg, and is the wrong place for the same
-- reason it is the right shape: mod_callcenter's own agent contact resolves
-- through it, so the queue's dispatches would be counted too. A decrement that
-- lagged an instant behind the next selection would then skip an agent on the
-- residue of their own previous offer.
--
-- Sets aicc_bleg_vars to a {…} prefix, or to nothing when the extension
-- belongs to no agent — which is most of them. An unset variable expands to
-- empty, so the bridge string is unchanged in that case.

local session = session
if session == nil then return end

local ext = argv[1] or session:getVariable("destination_number")

local function log(level, message)
  freeswitch.consoleLog(level, "aicc_track: " .. message .. "\n")
end

local function quote(value)
  return "'" .. tostring(value or ""):gsub("'", "''") .. "'"
end

session:setVariable("aicc_bleg_vars", "")

local dsn = freeswitch.getGlobalVariable("aicc_lua_dsn")
local dbh = dsn and dsn ~= "" and freeswitch.Dbh(dsn) or nil
if dbh == nil or not dbh:connected() then
  -- Not fatal. A call between two extensions is a call; failing to tell the
  -- queues about it is worth a line in the log and nothing more.
  log("warning", "cannot reach the database; the agent at " .. tostring(ext) ..
    " will not be marked busy")
  return
end

local agent = nil
dbh:query("SELECT callcenter_agent_name FROM luacc.directory WHERE number = " .. quote(ext),
  function(row) agent = row.callcenter_agent_name end)
dbh:release()

if agent == nil or agent == "" then
  return
end

-- Single-quoted because the value has a space in it and the block is split on
-- commas outside quotes; a bare space ends the block and the bridge fails
-- before it dials. The name itself cannot carry a quote — it is derived from a
-- username — but it is escaped rather than trusted.
session:setVariable("aicc_bleg_vars",
  string.format("{execute_on_ring='callcenter_track %s'}", agent:gsub("'", "\\'")))
