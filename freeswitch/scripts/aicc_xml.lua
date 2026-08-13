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
local function directory_document(domain, row)
  local auto_answer = ""
  if row.is_auto_answer == "t" or row.is_auto_answer == true then
    auto_answer = '        <variable name="sip_auto_answer" value="true"/>\n'
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
          <param name="password" value="%s"/>
        </params>
        <variables>
          <variable name="user_context" value="default"/>
          <variable name="effective_caller_id_name" value="%s"/>
          <variable name="effective_caller_id_number" value="%s"/>
          <variable name="aicc_extension" value="%s"/>
%s        </variables>
      </user>
    </domain>
  </section>
</document>]],
    escape(domain), escape(row.number), escape(row.password),
    escape(row.display_name ~= "" and row.display_name or row.number),
    escape(row.number), escape(row.number), auto_answer)
end

local function handle_directory(params)
  local user = params:getHeader("user")
  local domain = params:getHeader("domain") or freeswitch.getGlobalVariable("domain")
  if user == nil or user == "" then return nil end

  local dbh = open_dbh("aicc_lua_dsn")
  if dbh == nil then return nil end

  local found = nil
  dbh:query("SELECT number, password, display_name, is_auto_answer FROM luacc.directory WHERE number = "
    .. quote(user), function(row)
      found = row
    end)
  dbh:release()

  if found == nil then return nil end
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
  table.insert(parts, "      </settings>\n      <queues>")

  local count = 0
  dbh:query([[SELECT name, strategy, moh_sound, max_wait_sec, max_wait_no_agent_sec,
                     announce_sound, announce_frequency_sec, is_tier_rules_applied,
                     tier_rule_wait_sec, discard_abandoned_after_sec,
                     is_abandoned_resume_allowed
              FROM luacc.queues]], function(row)
    count = count + 1
    local queue_name = row.name .. "@" .. domain
    table.insert(parts, string.format('        <queue name="%s">', escape(queue_name)))
    table.insert(parts, string.format('          <param name="strategy" value="%s"/>',
      STRATEGY[row.strategy] or "longest-idle-agent"))
    table.insert(parts, string.format('          <param name="moh-sound" value="%s"/>', escape(row.moh_sound)))
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
