# FreeSWITCH integration

Everything FreeSWITCH needs from this project: three Lua scripts that read
configuration from PostgreSQL, and a small set of configuration files. Adding
an extension, a queue or a DID afterwards is a database change — FreeSWITCH is
never edited again.

## What lives where

| Path | Installed to | Purpose |
|---|---|---|
| `scripts/aicc_xml.lua` | `<fs>/scripts/` | Serves the `directory` section (SIP users) and `callcenter.conf` from the database |
| `scripts/aicc_inbound.lua` | `<fs>/scripts/` | Inbound entry point: answers every external number with a bot |
| `scripts/aicc_queue.lua` | `<fs>/scripts/` | Queue entry point and overflow handling |
| `conf/sip_profiles/external/aicc_bot.xml` | `<fs>/conf/sip_profiles/external/` | Gateway pointing at the application's SIP endpoint |
| `conf/dialplan/public/05_aicc.xml` | `<fs>/conf/dialplan/public/` | Routes inbound numbers to `aicc_inbound.lua` |
| `conf/dialplan/default/05_aicc.xml` | `<fs>/conf/dialplan/default/` | Routes queue extensions to `aicc_queue.lua` |

## Prerequisites

* FreeSWITCH 1.10.13 or newer with `mod_lua`, `mod_pgsql` and `mod_callcenter`
  loaded, and `uuid-version` set to 7 in `switch.conf.xml`.
* The application's migrations applied, so the `luacc` views exist.
* The read-only database role created:
  `psql "$AICC_DATABASE_URL" -v lua_password="'…'" -f deploy/sql/lua_role.sql`
* A database for mod_callcenter's own tables. It creates unqualified
  `agents`/`tiers`/`members` tables, so it needs a database of its own rather
  than sharing the application's.

## Configuration to add

Add to `vars.xml`, so no credential is ever committed here:

```xml
<X-PRE-PROCESS cmd="set" data="aicc_lua_dsn=pgsql://hostaddr=127.0.0.1 dbname=aicc user=aicc_lua password='…'"/>
<X-PRE-PROCESS cmd="set" data="aicc_cc_dsn=pgsql://hostaddr=127.0.0.1 dbname=aicc_fs user=aicc password='…'"/>
<X-PRE-PROCESS cmd="set" data="aicc_bot_host=sip:127.0.0.1"/>
<X-PRE-PROCESS cmd="set" data="aicc_bot_port=6060"/>
<X-PRE-PROCESS cmd="set" data="aicc_recordings_dir=$${base_dir}/recordings"/>
```

Bind the handler in `autoload_configs/lua.conf.xml`:

```xml
<param name="xml-handler-script" value="aicc_xml.lua"/>
<param name="xml-handler-bindings" value="directory|configuration"/>
```

`callcenter.conf.xml` may keep its stock content: the handler answers for that
key first, and everything it does not answer falls through to the file.

## Install

```sh
FS=/usr/local/freeswitch
cp freeswitch/scripts/*.lua                        "$FS/scripts/"
cp freeswitch/conf/sip_profiles/external/*.xml     "$FS/conf/sip_profiles/external/"
cp freeswitch/conf/dialplan/public/05_aicc.xml     "$FS/conf/dialplan/public/"
cp freeswitch/conf/dialplan/default/05_aicc.xml    "$FS/conf/dialplan/default/"

fs_cli -x "reloadxml"
fs_cli -x "sofia profile external rescan"

# Binding the xml-handler takes a restart, not a reload: mod_lua reads
# xml-handler-script when it loads, and FreeSWITCH reports it as not
# unloadable, so "reload mod_lua" answers "Module is not unloadable" and the
# binding silently stays inactive. Verified on 1.11.1.
fs_cli -x "shutdown restart"      # or: systemctl restart freeswitch

# mod_callcenter caches its queues at load time; once the handler is live this
# re-reads them from the database, without another restart.
fs_cli -x "reload mod_callcenter"
```

Editing the scripts afterwards needs no reload at all — the handler reads the
file per request. Only the initial binding needs the restart.

Verify:

```sh
fs_cli -x "callcenter_config queue list"                 # queues from the database
fs_cli -x "sofia status gateway aicc_bot"                # UP once the application listens
fs_cli -x "user_exists id 1001 \$\${domain}"             # directory served from the database
```

## Notes

* The Lua scripts read the `luacc` views only. Those views flatten jsonb and
  pre-join deliberately: `mod_lua` ships no JSON parser, and the switch should
  never need to understand the application's data model.
* Directory lookups key on the extension number alone. A softphone reaching
  FreeSWITCH through a TLS proxy authenticates against that proxy's hostname,
  which is not the domain the registration is stored under.
* No caching: a registration or a call setup is one indexed single-row query,
  which at this scale stays far below any level worth caching for. Should that
  change, the escape hatch is a short time-to-live memo inside the scripts.
