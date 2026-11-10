-- SPDX-License-Identifier: Apache-2.0
--
-- Unit test for scripts/aicc_spool.lua. It runs with a stock interpreter, no
-- switch needed:
--
--   lua freeswitch/tests/aicc_spool_test.lua
--
-- The script is loaded with a fake `freeswitch`, `stream` and `argv`, against
-- a real temporary directory.

local script = arg and arg[0] and arg[0]:gsub("tests/[^/]*$", "scripts/aicc_spool.lua")
  or "freeswitch/scripts/aicc_spool.lua"

local failures = 0
local function check(name, got, want)
  if got ~= want then
    failures = failures + 1
    print(string.format("FAIL %s: got %s, want %s", name, tostring(got), tostring(want)))
  else
    print("ok   " .. name)
  end
end

local function sh(cmd) assert(os.execute(cmd)) end
local function exists(path) local f = io.open(path, "rb"); if f then f:close() end return f ~= nil end
local function isdir(path) return os.execute("test -d '" .. path .. "'") == true or os.execute("test -d '" .. path .. "'") == 0 end

local tmp = os.tmpname()
os.remove(tmp)
local root = tmp .. "-spool"
sh("mkdir -p '" .. root .. "'")

local function run(vars, ...)
  local out = {}
  _G.freeswitch = {
    consoleLog = function() end,
    getGlobalVariable = function(name) return vars[name] end,
  }
  _G.stream = { write = function(_, s) out[#out + 1] = s end }
  _G.argv = { [0] = "aicc_spool.lua", ... }
  assert(loadfile(script))()
  return (table.concat(out):gsub("%s+$", ""))
end

local id = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
local id2 = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5c"
local key = "2026/11/10/" .. id .. ".wav"
local key2 = "2026/11/10/" .. id2 .. ".wav"
local vars = { aicc_recordings_dir = root }
local function put(k)
  sh("mkdir -p '" .. root .. "/" .. k:match("^(.*)/") .. "'")
  local f = assert(io.open(root .. "/" .. k, "wb")); f:write("x"); f:close()
end

-- release removes the file and the empty dated parents, never the root
put(key)
check("release of a nested key", run(vars, "release", key), "+OK")
check("file removed", exists(root .. "/" .. key), false)
check("empty day dir removed", isdir(root .. "/2026/11/10"), false)
check("empty year dir removed", isdir(root .. "/2026"), false)
check("root kept", isdir(root), true)

-- a non-empty parent stays
put(key)
put(key2)
check("release with a sibling", run(vars, "release", key), "+OK")
check("sibling kept", exists(root .. "/" .. key2), true)
check("non-empty day dir kept", isdir(root .. "/2026/11/10"), true)

-- a missing file is success
check("missing file", run(vars, "release", key), "+OK")

-- the fallback variable and a trailing slash
check("recordings_dir fallback", run({ recordings_dir = root .. "/" }, "release", key2), "+OK")
check("fallback removed it", exists(root .. "/" .. key2), false)

-- bad keys touch nothing
put(key)
for name, bad in pairs({
  traversal = "../" .. key,
  inner_traversal = "2026/11/../10/" .. id .. ".wav",
  wrong_shape = "2026/11/10/recording.wav",
  absolute = root .. "/" .. key,
  root_level = id .. ".wav",
  trailing = key .. "x",
  newline = key .. "\n",
}) do
  local got = run(vars, "release", bad)
  check("refuses " .. name, got:match("^%-ERR BAD_KEY") ~= nil, true)
end
check("missing key", run(vars, "release"):match("^%-ERR BAD_KEY") ~= nil, true)
check("bad keys left the file", exists(root .. "/" .. key), true)

-- non-local spool
check("no root", run({}, "release", key):match("^%-ERR NOT_LOCAL_SPOOL") ~= nil, true)
check("url root", run({ aicc_recordings_dir = "http://store:8333/b" }, "release", key):match("^%-ERR NOT_LOCAL_SPOOL") ~= nil, true)
check("relative root", run({ aicc_recordings_dir = "recordings" }, "release", key):match("^%-ERR NOT_LOCAL_SPOOL") ~= nil, true)
check("undef root", run({ aicc_recordings_dir = "_undef_" }, "release", key):match("^%-ERR NOT_LOCAL_SPOOL") ~= nil, true)
check("non-local left the file", exists(root .. "/" .. key), true)

-- probes
local token = "0123456789abcdef"
check("probe-write", run(vars, "probe-write", token), "+OK")
check("probe file exists", exists(root .. "/.aicc-spool-probe-" .. token), true)
check("probe-remove", run(vars, "probe-remove", token), "+OK")
check("probe file gone", exists(root .. "/.aicc-spool-probe-" .. token), false)
check("probe-remove again", run(vars, "probe-remove", token), "+OK")
for _, bad in ipairs({ "ABCDEF0123456789", "short", "../../etc/passwd0", "0123456789abcdef0" }) do
  check("bad token " .. bad, run(vars, "probe-write", bad):match("^%-ERR BAD_TOKEN") ~= nil, true)
end
check("probe write into a missing root", run({ aicc_recordings_dir = root .. "/nope" }, "probe-write", token):match("^%-ERR WRITE_FAILED") ~= nil, true)

check("unknown command", run(vars, "frobnicate", "x"):match("^%-ERR BAD_COMMAND") ~= nil, true)

sh("rm -rf '" .. root .. "'")
if failures > 0 then
  print(failures .. " failure(s)")
  os.exit(1)
end
print("all passed")
