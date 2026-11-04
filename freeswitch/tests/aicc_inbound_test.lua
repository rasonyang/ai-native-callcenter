-- SPDX-License-Identifier: Apache-2.0
--
-- Unit test for the bot-unavailable branch of scripts/aicc_inbound.lua.
-- It runs with a stock interpreter, no switch needed:
--
--   lua freeswitch/tests/aicc_inbound_test.lua
--
-- The script is loaded with a fake `session`, `freeswitch` and `Dbh`; the
-- route query returns one row and the session is still ready after the bridge,
-- so the code after the bridge is what runs.

local script = arg and arg[0] and arg[0]:gsub("tests/[^/]*$", "scripts/aicc_inbound.lua")
  or "freeswitch/scripts/aicc_inbound.lua"

local failures = 0
local function check(name, got, want)
  if got ~= want then
    failures = failures + 1
    print(string.format("FAIL %s: got %s, want %s", name, tostring(got), tostring(want)))
  else
    print("ok   " .. name)
  end
end

local SOUNDS = "/usr/local/freeswitch/sounds"

-- opts: language, fallback, finished, sounds_dir (use false for unset).
local function run(opts)
  local executed, hangups, logs = {}, {}, {}
  local vars = { destination_number = "95001", caller_id_number = "13800000000" }
  if opts.finished then vars.aicc_bot_finished = opts.finished end
  local sounds = opts.sounds_dir
  if sounds == nil then sounds = SOUNDS end
  if sounds == false then sounds = nil end
  local route = {
    number = "95001",
    language = opts.language,
    is_recording_enabled = "false",
    fallback_queue_ext_number = opts.fallback,
  }
  _G.freeswitch = {
    API = function() return { executeString = function() return "uuid-1" end } end,
    consoleLog = function(level, message) logs[#logs + 1] = level .. " " .. message end,
    getGlobalVariable = function(name)
      if name == "aicc_lua_dsn" then return "dsn" end
      if name == "sounds_dir" then return sounds end
      return nil
    end,
    Dbh = function()
      return {
        connected = function() return true end,
        query = function(_, _, cb) cb(route) end,
        release = function() end,
      }
    end,
  }
  _G.session = {
    getVariable = function(_, name) return vars[name] end,
    setVariable = function(_, name, value) vars[name] = value end,
    execute = function(_, app, arg1) executed[#executed + 1] = { app = app, arg = arg1 } end,
    hangup = function(_, cause) hangups[#hangups + 1] = cause end,
    ready = function() return true end,
    answer = function() end,
    answered = function() return true end,
  }
  assert(loadfile(script))()
  return executed, hangups, logs
end

-- The script also executes the bridge (and other apps); only playback matters.
local function playbacks(executed)
  local out = {}
  for _, e in ipairs(executed) do
    if e.app == "playback" then out[#out + 1] = e.arg end
  end
  return out
end

local function expectPrompt(name, language, want)
  local executed, hangups = run({ language = language })
  local p = playbacks(executed)
  check(name .. ": one playback", #p, 1)
  check(name .. ": path", p[1], SOUNDS .. "/aicc/" .. want .. "/bot-unavailable.wav")
  check(name .. ": hangs up after playing", hangups[#hangups], "NORMAL_TEMPORARY_FAILURE")
  local last = executed[#executed]
  check(name .. ": playback is the last application", last and last.app, "playback")
end

expectPrompt("zh", "zh", "zh")
expectPrompt("en", "en", "en")
expectPrompt("fr", "fr", "en")
expectPrompt("zh-hans", "zh-hans", "en")
expectPrompt("empty", "", "en")
expectPrompt("nil", nil, "en")
expectPrompt("traversal", "../../etc/passwd", "en")

do
  local executed, hangups = run({ language = "zh", fallback = "7001" })
  check("fallback: no playback", #playbacks(executed), 0)
  local last = executed[#executed]
  check("fallback: transfers", last and last.app .. " " .. tostring(last.arg), "transfer 7001 XML aicc")
  check("fallback: no hangup", #hangups, 0)
end

do
  local executed, hangups = run({ language = "zh", finished = "GOODBYE" })
  check("finished: no playback", #playbacks(executed), 0)
  check("finished: NORMAL_CLEARING", hangups[1], "NORMAL_CLEARING")
end

do
  local executed, hangups, logs = run({ language = "zh", sounds_dir = false })
  check("no sounds_dir: no playback", #playbacks(executed), 0)
  check("no sounds_dir: hangs up", hangups[#hangups], "NORMAL_TEMPORARY_FAILURE")
  local logged = false
  for _, l in ipairs(logs) do if l:find("sounds_dir", 1, true) then logged = true end end
  check("no sounds_dir: logged", logged, true)
end

do
  local executed, hangups = run({ language = "zh", sounds_dir = "" })
  check("empty sounds_dir: no playback", #playbacks(executed), 0)
  check("empty sounds_dir: hangs up", hangups[#hangups], "NORMAL_TEMPORARY_FAILURE")
end

if failures > 0 then
  print(failures .. " failure(s)")
  os.exit(1)
end
