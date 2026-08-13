-- SPDX-License-Identifier: Apache-2.0
--
-- Queue entry point: puts the caller into a mod_callcenter queue and decides
-- what happens when they come back out without an agent.
--
-- Reached two ways: the bot calls transfer_to_agent and the application
-- transfers the caller here, or aicc_inbound falls back here because the bot
-- could not take the call at all.
--
-- Distribution, music on hold and announcements belong to mod_callcenter. This
-- script owns only the boundary: entry, and the overflow decision on exit.

local session = session
if session == nil then return end

local ext = argv[1] or session:getVariable("destination_number")

local function log(level, message)
  freeswitch.consoleLog(level, "aicc_queue: " .. message .. "\n")
end

local function quote(value)
  return "'" .. tostring(value or ""):gsub("'", "''") .. "'"
end

local dsn = freeswitch.getGlobalVariable("aicc_lua_dsn")
local dbh = dsn and dsn ~= "" and freeswitch.Dbh(dsn) or nil
if dbh == nil or not dbh:connected() then
  log("err", "cannot reach the database")
  session:hangup("TEMPORARY_FAILURE")
  return
end

local queue = nil
dbh:query("SELECT name, is_recording_enabled, overflow_type, overflow_target, overflow_sound " ..
  "FROM luacc.queues WHERE ext_number = " .. quote(ext), function(row) queue = row end)
dbh:release()

if queue == nil then
  log("warning", "no queue serves extension " .. tostring(ext))
  session:hangup("UNALLOCATED_NUMBER")
  return
end

if not session:answered() then
  session:answer()
end

-- A caller transferred in from the bot is already being recorded, and
-- recording_follow_transfer keeps that file going. Only a call that reached
-- the queue without a bot leg needs recording started here.
if (queue.is_recording_enabled == "t" or queue.is_recording_enabled == true)
    and session:getVariable("aicc_recording_started") ~= "true" then
  local call_id = session:getVariable("aicc_call_id") or session:getVariable("uuid")
  local recordings = freeswitch.getGlobalVariable("aicc_recordings_dir") or
    freeswitch.getGlobalVariable("recordings_dir")
  session:setVariable("RECORD_STEREO", "true")
  session:setVariable("recording_follow_transfer", "true")
  session:setVariable("aicc_recording_started", "true")
  session:execute("record_session",
    string.format("%s/%s/%s.wav", recordings, os.date("!%Y/%m/%d"), call_id))
end

local domain = freeswitch.getGlobalVariable("domain") or "default"
session:setVariable("aicc_queue", queue.name)
session:execute("callcenter", queue.name .. "@" .. domain)

-- Past this point the caller left the queue without being bridged to an agent:
-- they waited too long, no agent was staffed, or the queue rejected them. A
-- caller who was bridged and then hung up is no longer here at all.
if not session:ready() then return end

local cause = session:getVariable("cc_cause") or "unknown"
local cancel_reason = session:getVariable("cc_cancel_reason") or ""
log("info", string.format("queue %s exit cause=%s reason=%s", queue.name, cause, cancel_reason))

local overflow = queue.overflow_type or "ANNOUNCE_HANGUP"

if overflow == "BOT_FLOW" then
  -- Hand the caller back to a bot, typically to take a message. The DID the
  -- bot leg dials selects the flow, exactly as on the inbound path.
  local target = queue.overflow_target
  if target ~= nil and target ~= "" then
    session:setVariable("sip_h_X-AICC-Overflow-Queue", queue.name)
    session:execute("bridge", "sofia/gateway/aicc_bot/" .. target)
    if not session:ready() then return end
  end
  session:hangup("NORMAL_CLEARING")

elseif overflow == "FORWARD" then
  local target = queue.overflow_target
  if target ~= nil and target ~= "" then
    session:execute("transfer", target .. " XML default")
    return
  end
  session:hangup("NORMAL_CLEARING")

else -- ANNOUNCE_HANGUP
  local sound = queue.overflow_sound
  if sound ~= nil and sound ~= "" then
    session:execute("playback", sound)
  end
  session:hangup("NORMAL_CLEARING")
end
