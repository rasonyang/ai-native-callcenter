-- SPDX-License-Identifier: Apache-2.0
--
-- Serves FreeSWITCH configuration from PostgreSQL.
--
-- Bound as an xml-handler for two sections:
--   directory     - SIP users (agents and the bot endpoint) for registration
--                   and authentication
--   configuration - callcenter.conf only; every other configuration key falls
--                   through to the static files on disk
--
-- The script reads the luacc.* views and nothing else. Those views are a
-- contract owned by the application's migrations: they flatten jsonb and
-- pre-join, so this script never parses documents or joins tables.
--
-- Connection strings come from FreeSWITCH globals, so no credential is ever
-- committed to this repository:
--   aicc_lua_dsn - read-only role, the luacc views
--   aicc_cc_dsn  - mod_callcenter's own database, rendered into callcenter.conf

local NOT_FOUND = [[<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<document type="freeswitch/xml">
  <section name="result">
    <result status="not found"/>
  </section>
</document>]]

-- escape makes a value safe to place inside an XML attribute.
local function escape(value)
  if value == nil then return "" end
  value = tostring(value)
  value = value:gsub("&", "&amp;"):gsub("<", "&lt;"):gsub(">", "&gt;")
  value = value:gsub('"', "&quot;"):gsub("'", "&apos;")
  return value
end

local function open_dbh(global_name)
  local dsn = freeswitch.getGlobalVariable(global_name)
  if dsn == nil or dsn == "" then
    freeswitch.consoleLog("err", "aicc_xml: global " .. global_name .. " is not set\n")
    return nil
  end
  local dbh = freeswitch.Dbh(dsn)
  if not dbh:connected() then
    freeswitch.consoleLog("err", "aicc_xml: cannot connect using " .. global_name .. "\n")
    return nil
  end
  return dbh
end

-- quote escapes a value for a SQL string literal. Lookup keys arrive from the
-- network, so they are never interpolated unescaped.
local function quote(value)
  return "'" .. tostring(value or ""):gsub("'", "''") .. "'"
end

--- directory ---------------------------------------------------------------

-- The digest realm a phone presents is not necessarily the domain the
-- registration is stored under: the agent softphone reaches FreeSWITCH through
-- a TLS proxy and authenticates against that hostname. Lookups therefore key
-- on the extension number alone.
--
-- The credential is a1-hash, never a password, and it is minted per session by
-- the application: luacc.directory carries the hash of whichever SIP session is
-- valid for that number right now, and NULL when there is none. An extension
-- nobody has signed in at is an extension nothing may register as.
--
-- That NULL has to be refused explicitly. sofia_reg.c:3327-3335 treats a
-- directory user carrying neither password nor a1-hash as AUTH_OK — a user
-- with no credential authenticates *anything* — unless the entry says
-- allow-empty-password is false. So two things happen below and both matter:
-- every entry emits allow-empty-password=false, and an authentication lookup
-- with no live session returns nothing at all rather than a user with no
-- credential.
--
-- Only the authentication lookup. The same section answers dial-string
-- resolution, user_call and mod_callcenter's agent tracking, and those ask
-- "where does this extension go", not "may this phone register" — refusing
-- them for want of a session would stop calls reaching a desk phone that is
-- already registered.
--
-- aicc_managed marks a channel as ours from the moment it exists. Everything a
-- dialplan can stamp arrives one step too late for the leg that triggered the
-- dialplan — the caller's CHANNEL_CREATE reaches the application first — so a
-- variable that rides the directory entry is the only thing both legs of a
-- call between two of our accounts carry from birth. user_context puts those
-- accounts in the aicc dialplan for the same reason: the account, not the
-- call, is what aicc owns.
local function directory_document(domain, row)
  -- The credential, when there is one. A non-authentication lookup for an
  -- extension with no live session gets an entry with no credential at all,
  -- which is why allow-empty-password sits beside it unconditionally.
  local a1 = ""
  if row.a1_hash ~= nil and row.a1_hash ~= "" then
    a1 = string.format('          <param name="a1-hash" value="%s"/>\n', escape(row.a1_hash))
  end

  local auto_answer = ""
  if row.is_auto_answer == "t" or row.is_auto_answer == true then
    auto_answer = '        <variable name="sip_auto_answer" value="true"/>\n'
  end

  -- Tell mod_callcenter this agent is busy the moment a call they placed
  -- starts ringing, so its queues stop offering them one until it ends.
  --
  -- The module only tracks what it dispatched: a call an agent makes leaves
  -- agents.state at Waiting, and a queue then rings a phone that is already
  -- engaged. The phone says no — 486, which costs a busy delay, or 480 on its
  -- slot race, which the module counts as a call they failed to answer.
  --
  -- Here rather than in the dial-string, and that distinction is measured.
  -- These variables reach a channel the user themselves raises — their phone's
  -- own INVITE — and not one raised towards them: a leg originated at
  -- user/1002 carries none of user_context, aicc_managed or aicc_extension.
  -- The dial-string is the opposite, and it is what mod_callcenter's own
  -- agent contact resolves through, so putting this there would count the
  -- queue's own dispatches and let a decrement that lagged by an instant skip
  -- an agent on the residue of their own previous offer.
  --
  -- On ring, because a ringing phone is already engaged and that is the window
  -- worth closing. A ring nobody answers costs nothing: mod_callcenter's own
  -- state hook puts the count back when the leg ends, answered or not.
  --
  -- Only execute_on_ring. A leg that answers 183 with SDP and never sends 180
  -- goes down mark_pre_answered and this never fires (sofia.c:7607); adding
  -- execute_on_pre_answer to cover it would run callcenter_track twice on the
  -- ordinary 180-then-183, because switch_channel_execute_on does not clear
  -- the variable and CF_RING_READY only guards a second 180. The count would
  -- balance and stop meaning anything. Agent phones send 180; 183-only is
  -- carrier and IVR behaviour.
  local track = ""
  local agent = row.callcenter_agent_name
  if agent ~= nil and agent ~= "" then
    track = string.format(
      '        <variable name="execute_on_ring" value="callcenter_track %s"/>\n',
      escape(agent))
  end

  return string.format([[<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<document type="freeswitch/xml">
  <section name="directory">
    <domain name="%s">
      <params>
        <param name="dial-string" value="{^^:sip_invite_domain=${dialed_domain}:presence_id=${dialed_user}@${dialed_domain}}${sofia_contact(*/${dialed_user}@${dialed_domain})}"/>
      </params>
      <user id="%s">
        <params>
%s          <param name="allow-empty-password" value="false"/>
        </params>
        <variables>
          <variable name="user_context" value="aicc"/>
          <variable name="effective_caller_id_name" value="%s"/>
          <variable name="effective_caller_id_number" value="%s"/>
          <variable name="aicc_managed" value="true"/>
          <variable name="aicc_extension" value="%s"/>
%s%s        </variables>
      </user>
    </domain>
  </section>
</document>]],
    escape(domain), escape(row.number), a1,
    escape(row.display_name ~= "" and row.display_name or row.number),
    escape(row.number), escape(row.number), auto_answer, track)
end

local function handle_directory(params)
  local user = params:getHeader("user")
  local domain = params:getHeader("domain") or freeswitch.getGlobalVariable("domain")
  if user == nil or user == "" then return nil end

  local dbh = open_dbh("aicc_lua_dsn")
  if dbh == nil then return nil end

  local found = nil
  dbh:query("SELECT number, a1_hash, display_name, is_auto_answer, callcenter_agent_name " ..
    "FROM luacc.directory WHERE number = "
    .. quote(user), function(row)
      found = row
    end)
  dbh:release()

  if found == nil then return nil end

  -- An authentication lookup with no live session is a lookup that found
  -- nothing. Returning the user without a credential would authenticate every
  -- REGISTER for that extension (sofia_reg.c:3327-3335); returning nothing
  -- makes the switch answer 403, which is the true answer.
  if params:getHeader("action") == "sip_auth"
      and (found.a1_hash == nil or found.a1_hash == "") then
    freeswitch.consoleLog("info",
      "aicc_xml: no active sip session for " .. tostring(user) .. "; refusing authentication\n")
    return nil
  end

  return directory_document(domain, found)
end

--- configuration: callcenter.conf -------------------------------------------

-- Our queue strategies are stored as SCREAMING_SNAKE enum values; the switch
-- spells them differently. The translation happens here, at the boundary.
local STRATEGY = {
  LONGEST_IDLE_AGENT         = "longest-idle-agent",
  ROUND_ROBIN                = "round-robin",
  TOP_DOWN                   = "top-down",
  AGENT_WITH_LEAST_TALK_TIME = "agent-with-least-talk-time",
  AGENT_WITH_FEWEST_CALLS    = "agent-with-fewest-calls",
  RANDOM                     = "random",
}

local function bool_param(value)
  if value == "t" or value == true or value == "true" then return "true" end
  return "false"
end

local function handle_callcenter()
  local dbh = open_dbh("aicc_lua_dsn")
  if dbh == nil then return nil end

  local cc_dsn = freeswitch.getGlobalVariable("aicc_cc_dsn") or ""
  local domain = freeswitch.getGlobalVariable("domain") or "default"

  local parts = {}
  table.insert(parts, [[<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<document type="freeswitch/xml">
  <section name="configuration">
    <configuration name="callcenter.conf" description="CallCenter">
      <settings>]])
  if cc_dsn ~= "" then
    table.insert(parts, string.format('        <param name="odbc-dsn" value="%s"/>', escape(cc_dsn)))
  end
  -- How long a delivered call rings an agent before the queue gives up on
  -- them. Unset it is sixty seconds, and sixty seconds is what one missed
  -- call costs the caller: they hear hold music for a full minute while a
  -- phone nobody is holding rings out, and only then does the queue try
  -- somebody else. Fifteen is long enough to reach a headset and short
  -- enough that a miss is not a minute.
  table.insert(parts, '        <param name="agent-originate-timeout" value="15"/>')
  table.insert(parts, "      </settings>\n      <queues>")

  local count = 0
  dbh:query([[SELECT name, strategy, moh_sound, max_wait_sec, max_wait_no_agent_sec,
                     announce_sound, announce_frequency_sec, is_tier_rules_applied,
                     tier_rule_wait_sec, discard_abandoned_after_sec,
                     is_abandoned_resume_allowed
              FROM luacc.queues]], function(row)
    count = count + 1
    -- No domain suffix: the switch stores what it is told, and a name that
    -- embeds this host's address goes stale the moment the address does (C1).
    local queue_name = row.name
    table.insert(parts, string.format('        <queue name="%s">', escape(queue_name)))
    table.insert(parts, string.format('          <param name="strategy" value="%s"/>',
      STRATEGY[row.strategy] or "longest-idle-agent"))
    table.insert(parts, string.format('          <param name="moh-sound" value="%s"/>', escape(row.moh_sound)))
    -- Also here, not only in <settings>. The settings block is demonstrably
    -- read — odbc-dsn arrives from it and the queues persist because of it —
    -- and the switch is demonstrably given this parameter, yet rings kept
    -- lasting the default sixty seconds (C41). Since the module reads a
    -- number of its globals as defaults for per-object values, naming it on
    -- the object costs nothing and is the one remaining thing that can be
    -- tried without guessing at source we do not have.
    table.insert(parts, '          <param name="agent-originate-timeout" value="15"/>')
    table.insert(parts, string.format('          <param name="max-wait-time" value="%s"/>', escape(row.max_wait_sec)))
    table.insert(parts, string.format('          <param name="max-wait-time-with-no-agent" value="%s"/>',
      escape(row.max_wait_no_agent_sec)))
    if row.announce_sound ~= nil and row.announce_sound ~= "" then
      table.insert(parts, string.format('          <param name="announce-sound" value="%s"/>', escape(row.announce_sound)))
      table.insert(parts, string.format('          <param name="announce-frequency" value="%s"/>',
        escape(row.announce_frequency_sec)))
    end
    table.insert(parts, string.format('          <param name="tier-rules-apply" value="%s"/>',
      bool_param(row.is_tier_rules_applied)))
    table.insert(parts, string.format('          <param name="tier-rule-wait-second" value="%s"/>',
      escape(row.tier_rule_wait_sec)))
    table.insert(parts, string.format('          <param name="discard-abandoned-after" value="%s"/>',
      escape(row.discard_abandoned_after_sec)))
    table.insert(parts, string.format('          <param name="abandoned-resume-allowed" value="%s"/>',
      bool_param(row.is_abandoned_resume_allowed)))
    table.insert(parts, "        </queue>")
  end)
  dbh:release()

  table.insert(parts, "      </queues>\n    </configuration>\n  </section>\n</document>")
  freeswitch.consoleLog("info", "aicc_xml: rendered callcenter.conf with " .. count .. " queue(s)\n")
  return table.concat(parts, "\n")
end

--- entry point --------------------------------------------------------------

local section = XML_REQUEST["section"]
local result = nil

if section == "directory" then
  result = handle_directory(params)
elseif section == "configuration" and XML_REQUEST["key_value"] == "callcenter.conf" then
  result = handle_callcenter()
end

-- Returning the not-found document lets FreeSWITCH fall back to the static
-- files for everything this script does not own.
XML_STRING = result or NOT_FOUND
