-- SPDX-License-Identifier: Apache-2.0
--
-- Unit test for the agent-side maximum in scripts/aicc_queue.lua (#637). It
-- runs with a stock interpreter, no switch needed:
--
--   lua freeswitch/tests/aicc_queue_test.lua
--
-- The script is loaded with a fake `session`, `freeswitch` and `Dbh` and a
-- fixed clock; the queue lookup returns one row and the caller leaves the
-- session after the callcenter application, so the overflow branch never runs.

local script = arg and arg[0] and arg[0]:gsub("tests/[^/]*$", "scripts/aicc_queue.lua")
  or "freeswitch/scripts/aicc_queue.lua"

local failures = 0
local function check(name, got, want)
  if got ~= want then
    failures = failures + 1
    print(string.format("FAIL %s: got %s, want %s", name, tostring(got), tostring(want)))
  else
    print("ok   " .. name)
  end
end

local NOW = 1795149000
local real_time = os.time

-- opts: now (the clock), vars (the channel's variables before the entry),
-- globals (the switch's global variables), queue (false for no queue).
-- Returns the ordered steps (api commands and applications), the variables
-- after the entry and the hangups.
local function run(opts)
  local steps, hangups = {}, {}
  local vars = { uuid = "chan-1", destination_number = "7001" }
  for k, v in pairs(opts.vars or {}) do vars[k] = v end
  local queue = { name = "support", is_recording_enabled = "f", overflow_type = "ANNOUNCE_HANGUP" }
  if opts.queue == false then queue = nil end
  _G.argv = { "7001" }
  _G.freeswitch = {
    API = function()
      return { executeString = function(_, cmd) steps[#steps + 1] = "api " .. cmd; return "+OK" end }
    end,
    consoleLog = function() end,
    getGlobalVariable = function(name)
      if name == "aicc_lua_dsn" then return "dsn" end
      return (opts.globals or {})[name]
    end,
    Dbh = function()
      return {
        connected = function() return true end,
        query = function(_, _, cb) if queue then cb(queue) end end,
        release = function() end,
      }
    end,
  }
  local isInQueue = false
  _G.session = {
    getVariable = function(_, name) return vars[name] end,
    setVariable = function(_, name, value) vars[name] = value end,
    execute = function(_, app, data)
      steps[#steps + 1] = app .. " " .. tostring(data)
      if app == "callcenter" then isInQueue = true end
    end,
    hangup = function(_, cause) hangups[#hangups + 1] = cause end,
    -- Bridged to an agent and gone, as far as the script after the queue is
    -- concerned.
    ready = function() return not isInQueue end,
    answer = function() end,
    answered = function() return true end,
  }
  os.time = function() return opts.now or NOW end
  assert(loadfile(script))()
  os.time = real_time
  return steps, vars, hangups
end

local function index_of(steps, prefix)
  for i, s in ipairs(steps) do
    if s:sub(1, #prefix) == prefix then return i end
  end
  return nil
end

local function kills(steps)
  local out = {}
  for _, s in ipairs(steps) do
    if s:sub(1, #"sched_transfer ") == "sched_transfer " then out[#out + 1] = s end
  end
  return out
end

-- The first entry fixes the deadline at the agent cap from now, deletes every
-- task of the channel (the bot's kill with them) and schedules the transfer
-- to the marking extension, before the caller is queued.
do
  local steps, vars = run({})
  local k = kills(steps)
  check("first entry: one kill", #k, 1)
  check("first entry: at the default cap", k[1], "sched_transfer +3600 aicc_max_call_duration_agent XML aicc")
  check("first entry: the deadline is kept on the channel", vars.aicc_max_call_duration_agent_at, tostring(NOW + 3600))
  local del, sched, queued = index_of(steps, "api sched_del chan-1"), index_of(steps, "sched_transfer "),
    index_of(steps, "callcenter ")
  check("first entry: the channel's tasks are deleted", del ~= nil, true)
  check("first entry: deleted before the kill is scheduled", del ~= nil and sched ~= nil and del < sched, true)
  check("first entry: scheduled before the caller is queued", sched ~= nil and queued ~= nil and sched < queued, true)
end

do
  local steps, vars = run({ globals = { aicc_max_call_duration_agent_sec = "86400" } })
  check("pushed cap: the kill", kills(steps)[1], "sched_transfer +86400 aicc_max_call_duration_agent XML aicc")
  check("pushed cap: the deadline", vars.aicc_max_call_duration_agent_at, tostring(NOW + 86400))
end

for _, bad in ipairs({ "", "abc", "0", "-60", "90.5" }) do
  local steps = run({ globals = { aicc_max_call_duration_agent_sec = bad } })
  check("unusable cap '" .. bad .. "': the default", kills(steps)[1],
    "sched_transfer +3600 aicc_max_call_duration_agent XML aicc")
end

-- A later entry (another queue, a FORWARD overflow, a bot in between) keeps
-- the first entry's deadline whatever the cap is now, and still deletes what
-- else the channel was given since, a bot's kill included.
do
  local first = NOW + 3600
  local steps, vars = run({
    now = NOW + 1000,
    vars = { aicc_max_call_duration_agent_at = tostring(first) },
    globals = { aicc_max_call_duration_agent_sec = "60" },
  })
  check("re-entry: the deadline does not move", vars.aicc_max_call_duration_agent_at, tostring(first))
  check("re-entry: the kill is what is left of it", kills(steps)[1],
    "sched_transfer +2600 aicc_max_call_duration_agent XML aicc")
  check("re-entry: the channel's tasks are deleted", index_of(steps, "api sched_del chan-1") ~= nil, true)
end

do
  local steps = run({ now = NOW + 5000, vars = { aicc_max_call_duration_agent_at = tostring(NOW + 3600) } })
  check("re-entry past the deadline: at once, with the +", kills(steps)[1],
    "sched_transfer +1 aicc_max_call_duration_agent XML aicc")
end

do
  local steps, _, hangups = run({ queue = false })
  check("no queue: nothing scheduled", #kills(steps), 0)
  check("no queue: nothing deleted", index_of(steps, "api sched_del"), nil)
  check("no queue: hung up", hangups[1], "UNALLOCATED_NUMBER")
end

if failures > 0 then
  print(failures .. " failure(s)")
  os.exit(1)
end
print("all passed")
