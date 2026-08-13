-- SPDX-License-Identifier: Apache-2.0
--
-- Inbound entry point: every external number answers with a bot.
--
-- The dialplan hands each call in the public context to this script. It
-- decides only three things, all of which live in luacc.dids:
--   is this number ours, should the call be recorded, and where does the
--   caller go if the bot cannot take the call at all.
--
-- Which conversation flow runs is not decided here. The application resolves
-- that from the DID when the bot leg arrives, so the flow catalogue never
-- becomes part of the switch's configuration.

local session = session
if session == nil then return end

local did = session:getVariable("destination_number")
local ani = session:getVariable("caller_id_number") or ""

local function log(level, message)
  freeswitch.consoleLog(level, "aicc_inbound: " .. message .. "\n")
end

local function quote(value)
  return "'" .. tostring(value or ""):gsub("'", "''") .. "'"
end

local dsn = freeswitch.getGlobalVariable("aicc_lua_dsn")
if dsn == nil or dsn == "" then
  log("err", "aicc_lua_dsn is not set")
  session:hangup("TEMPORARY_FAILURE")
  return
end

local dbh = freeswitch.Dbh(dsn)
if not dbh:connected() then
  log("err", "cannot reach the database")
  session:hangup("TEMPORARY_FAILURE")
  return
end

local route = nil
dbh:query("SELECT number, language, is_recording_enabled, fallback_queue_ext_number " ..
  "FROM luacc.dids WHERE number = " .. quote(did), function(row) route = row end)
dbh:release()

if route == nil then
  log("warning", "unknown number " .. tostring(did) .. " from " .. ani)
  session:hangup("UNALLOCATED_NUMBER")
  return
end

-- One call identity, minted before any leg exists, so every event the
-- application sees carries it from the first moment.
local api = freeswitch.API()
local call_id = api:executeString("create_uuid")

session:setVariable("aicc_call_id", call_id)
session:setVariable("aicc_did", route.number)
session:setVariable("aicc_language", route.language)

-- The bot leg is G.711 only: it terminates RTP in the application, which
-- speaks both laws and nothing else.
session:setVariable("absolute_codec_string", "PCMU,PCMA")
session:setVariable("hangup_after_bridge", "true")
session:setVariable("continue_on_fail", "true")

-- Business context travels to the bot in SIP headers, the same mechanism the
-- reference implementation proved in production.
session:setVariable("sip_h_X-AICC-Call-ID", call_id)
session:setVariable("sip_h_X-AICC-DID", route.number)
session:setVariable("sip_h_X-AICC-Language", route.language)
session:setVariable("sip_h_X-AICC-ANI", ani)
-- The caller's own channel, so the application can transfer this leg to a
-- queue directly when the bot hands the call to a person.
session:setVariable("sip_h_X-AICC-Channel-ID", session:getVariable("uuid"))

session:answer()

if route.is_recording_enabled == "t" or route.is_recording_enabled == true then
  -- One continuous stereo file per call: the caller on the left channel, the
  -- bot and then the agent on the right, following the call through transfers.
  local recordings = freeswitch.getGlobalVariable("aicc_recordings_dir") or
    freeswitch.getGlobalVariable("recordings_dir")
  local path = string.format("%s/%s/%s.wav", recordings, os.date("!%Y/%m/%d"), call_id)
  session:setVariable("RECORD_STEREO", "true")
  session:setVariable("recording_follow_transfer", "true")
  session:setVariable("media_bug_answer_req", "true")
  session:execute("record_session", path)
end

-- The codec pin rides the new leg itself: the caller's leg may speak
-- anything (a loopback test leg speaks L16), and the switch transcodes.
session:execute("bridge",
  "{absolute_codec_string=PCMU,PCMA}sofia/gateway/aicc_bot/" .. route.number)

-- Reaching this point means the bot never took the call: the gateway is down,
-- the application is out of capacity, or a provider is unreachable. The caller
-- must still get to a human where one is configured.
if session:ready() then
  local cause = session:getVariable("originate_disposition") or "unknown"
  log("warning", "bot leg failed for " .. route.number .. " (" .. cause .. ")")

  local fallback = route.fallback_queue_ext_number
  if fallback ~= nil and fallback ~= "" then
    session:execute("transfer", fallback .. " XML default")
  else
    session:execute("playback", "ivr/ivr-call_cannot_be_completed_as_dialed.wav")
    session:hangup("NORMAL_TEMPORARY_FAILURE")
  end
end
