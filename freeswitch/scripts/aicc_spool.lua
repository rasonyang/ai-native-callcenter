-- SPDX-License-Identifier: Apache-2.0
--
-- The recording spool, from the switch's side.
--
-- With the S3 recording backend and a spool, the switch records into a local
-- directory and the application uploads, books and then clears each file. The
-- switch creates the dated directories itself with a fixed mode (0750, owner
-- only for writing), so an application running as another user can read them
-- but never unlink in them. This script lets the application ask the switch,
-- which owns every file and directory in the spool, to do the removal:
--
--   lua aicc_spool.lua release <key>         remove a booked recording
--   lua aicc_spool.lua probe-write <token>   write a probe file at the root
--   lua aicc_spool.lua probe-remove <token>  remove it again
--
-- <key> is a recording key, YYYY/MM/DD/<uuid>.wav (recording.Key); <token> is
-- 16 lowercase hex digits. The probe pair backs the application's connect-time
-- self-check and `aicc doctor`.
--
-- The reply on the API stream is "+OK", or "-ERR <CAUSE> <detail>" with one of:
--   NOT_LOCAL_SPOOL   the recordings directory is not an absolute local path
--   BAD_COMMAND       unknown command or missing argument
--   BAD_KEY           the key is not a recording key
--   BAD_TOKEN         the token is not 16 lowercase hex digits
--   REMOVE_FAILED     the operating system refused the removal
--   WRITE_FAILED      the operating system refused the probe write
--
-- Works on Lua 5.1 through 5.4; it needs no library beyond the standard one.
-- Test: lua freeswitch/tests/aicc_spool_test.lua

local KEY_PATTERN = "^%d%d%d%d/%d%d/%d%d/" ..
  string.rep("%x", 8) .. "%-" .. string.rep("%x", 4) .. "%-" .. string.rep("%x", 4) .. "%-" ..
  string.rep("%x", 4) .. "%-" .. string.rep("%x", 12) .. "%.wav$"
local TOKEN_PATTERN = "^" .. string.rep("[0-9a-f]", 16) .. "$"
local PROBE_PREFIX = ".aicc-spool-probe-"

local function log(level, message)
  freeswitch.consoleLog(level, "aicc_spool: " .. message .. "\n")
end

local function reply(text)
  stream:write(text .. "\n")
end

local function refuse(cause, detail)
  log("warning", cause .. " " .. detail)
  reply("-ERR " .. cause .. " " .. detail)
end

-- The spool root, resolved as aicc_inbound.lua and Adapter.RecordingsDir do.
local function spoolRoot()
  for _, name in ipairs({ "aicc_recordings_dir", "recordings_dir" }) do
    local v = freeswitch.getGlobalVariable(name)
    if v ~= nil and v ~= "" and v ~= "_undef_" then
      return v
    end
  end
  return nil
end

local function main()
  local command, arg = argv[1], argv[2]
  local root = spoolRoot()
  if root == nil or root:sub(1, 1) ~= "/" or root:find("://", 1, true) then
    return refuse("NOT_LOCAL_SPOOL", tostring(root))
  end
  root = root:gsub("/+$", "")
  if root == "" then
    return refuse("NOT_LOCAL_SPOOL", "/")
  end

  if command == "release" then
    if type(arg) ~= "string" or not arg:match(KEY_PATTERN) then
      return refuse("BAD_KEY", tostring(arg))
    end
    local file = root .. "/" .. arg
    local ok, err, code = os.remove(file)
    if not ok and code ~= 2 and not tostring(err):find("No such file", 1, true) then
      return refuse("REMOVE_FAILED", tostring(err))
    end
    -- Tidy the dated directories the file leaves behind, never the root.
    -- os.remove removes an empty directory and refuses a full one.
    local dir = arg:match("^(.*)/[^/]*$")
    while dir ~= nil and dir ~= "" do
      if not os.remove(root .. "/" .. dir) then break end
      dir = dir:match("^(.*)/[^/]*$")
    end
    return reply("+OK")
  elseif command == "probe-write" or command == "probe-remove" then
    if type(arg) ~= "string" or not arg:match(TOKEN_PATTERN) then
      return refuse("BAD_TOKEN", tostring(arg))
    end
    local file = root .. "/" .. PROBE_PREFIX .. arg
    if command == "probe-write" then
      local f, err = io.open(file, "wb")
      if not f then return refuse("WRITE_FAILED", tostring(err)) end
      f:write("aicc spool probe\n")
      f:close()
    else
      local ok, err, code = os.remove(file)
      if not ok and code ~= 2 and not tostring(err):find("No such file", 1, true) then
        return refuse("REMOVE_FAILED", tostring(err))
      end
    end
    return reply("+OK")
  end
  return refuse("BAD_COMMAND", tostring(command))
end

main()
