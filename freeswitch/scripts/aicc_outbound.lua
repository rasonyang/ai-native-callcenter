-- SPDX-License-Identifier: Apache-2.0
--
-- Outbound recording. Starts the one stereo file a placement to the trunk is
-- filed under, so a call the application places is recorded exactly like one
-- it receives.
--
-- Neither of the other two paths covers this one. aicc_inbound.lua records the
-- DID that was dialled *in*, and aicc_queue.lua records a caller who reaches a
-- queue — but a placement goes from the agent's channel straight into the
-- trunk dialplan, so until this script existed no record_session ever ran on
-- it and the application looked for a key the switch had never written.
-- AICC_RECORDING_BACKEND was configured, CDR rows were written with
-- has_recording false, and nothing in any log said why.
--
-- Who decides: the same column as the inbound path, is_recording_enabled on
-- the DID. The DID here is the one the far end will see as the caller id —
-- click-to-dial stamps the deployment's default outbound number onto the
-- agent's leg as effective_caller_id_number, and the trunk dialplan carries it
-- into trunk_clid. A leg that carries no number of its own falls back to
-- $${pstn_gateway_caller_id}, which is a DID row like any other.
--
-- Nothing here may fail the call. This runs between the agent answering and
-- the trunk leg being placed, on a call that is already up: an unreachable
-- database or a DID with no row costs a recording, never a conversation. That
-- is why every failure below logs and returns instead of hanging up, which is
-- the opposite of what aicc_queue.lua does when its queue lookup misses.
--
-- Reached from the deployment's outbound dialplan, which is a deployment's file
-- and not this repository's — this box's is deploy/dev/freeswitch/dialplan/
-- aicc/00_pstn_gateway.xml, and it calls this script before its bridge.

local session = session
if session == nil then return end

local function log(level, message)
  freeswitch.consoleLog(level, "aicc_outbound: " .. message .. "\n")
end

-- A transfer into this path from a call already being recorded keeps that file
-- going: recording_follow_transfer rides the leg, and a second record_session
-- on the same channel would fight it for the same key.
if session:getVariable("aicc_recording_started") == "true" then
  return
end

-- The DID the far end sees. trunk_clid is set by the trunk dialplan before it
-- calls this script; effective_caller_id_number is what it was derived from.
local did = session:getVariable("trunk_clid")
if did == nil or did == "" then
  did = session:getVariable("effective_caller_id_number")
end

local enabled = false
local dsn = freeswitch.getGlobalVariable("aicc_lua_dsn")
local dbh = dsn and dsn ~= "" and freeswitch.Dbh(dsn) or nil
if dbh == nil or not dbh:connected() then
  log("warning", "cannot reach the database; placing the call unrecorded")
else
  dbh:query("SELECT is_recording_enabled FROM luacc.dids WHERE number = " ..
    "'" .. tostring(did or ""):gsub("'", "''") .. "'",
    function(row) enabled = row.is_recording_enabled == "t" or row.is_recording_enabled == true end)
  dbh:release()
end

if not enabled then
  return
end

-- UTC, not local time, and this is not a detail.
--
-- The application files the object under recording.Key, which formats the
-- call's start with startedAt.UTC(); the key it looks up is therefore the UTC
-- date. os.date("!%Y/%m/%d") is the same instant in the same zone — the "!"
-- is what makes it UTC. A dialplan's ${strftime(%Y/%m/%d)} would not do: it
-- expands in the switch's local time, and this deployment runs Asia/Shanghai,
-- so every call placed between midnight and 08:00 local would be written under
-- the following day and never found.
local call_id = session:getVariable("aicc_call_id")
if call_id == nil or call_id == "" then
  -- The application stamps aicc_call_id on the leg it originates and exports
  -- it; the channel uuid is the same fallback aicc_queue.lua uses.
  call_id = session:getVariable("uuid")
end

local recordings = freeswitch.getGlobalVariable("aicc_recordings_dir") or
  freeswitch.getGlobalVariable("recordings_dir")
if recordings == nil or recordings == "" then
  log("warning", "no recordings directory is configured; placing the call unrecorded")
  return
end

local path = string.format("%s/%s/%s.wav", recordings, os.date("!%Y/%m/%d"), call_id)

-- Stereo with the agent's channel recorded as placed and the far end opposite,
-- so both sides of the conversation survive the bridge rather than the mix.
-- recording_follow_transfer is set for symmetry with the other two paths: a
-- placement that is later transferred stays in one file.
session:setVariable("RECORD_STEREO", "true")
session:setVariable("recording_follow_transfer", "true")
session:setVariable("aicc_recording_started", "true")
session:execute("record_session", path)

log("info", string.format("recording %s as %s", tostring(call_id), path))
