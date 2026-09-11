# FreeSWITCH

Everything FreeSWITCH needs from this project is in this directory: a complete
configuration tree, four Lua scripts that read the switch's configuration out
of PostgreSQL, and a Dockerfile that builds the switch itself. Adding an
extension, a queue or a DID afterwards is a database change — FreeSWITCH is
never edited again.

There are two ways to get a configured switch: run the image, or install this
tree onto a FreeSWITCH you built yourself.

## What lives where

| Path | Purpose |
|---|---|
| `conf/` | The complete FreeSWITCH configuration tree, 184 files. Its baseline is `git archive v1.11.3 conf/vanilla`; this project's changes are made in the files themselves |
| `CONF-DEVIATIONS.md` | The file-by-file record of every difference from that baseline, and of what was left vanilla that could still surprise a deployment. Read it before changing anything under `conf/` |
| `scripts/aicc_xml.lua` | Serves the `directory` section (SIP users) and `callcenter.conf` from the database |
| `scripts/aicc_inbound.lua` | Inbound entry point: answers every external number with a bot |
| `scripts/aicc_queue.lua` | Queue entry point and overflow handling |
| `scripts/aicc_track.lua` | Names the agent an internal call is ringing, so mod_callcenter stops offering them queue calls |
| `Dockerfile` | Builds FreeSWITCH v1.11.3 from source, with this tree and `mod_audio_stream` inside it |
| `modules.conf` | The 26 modules that build compiles. The build fails if the shipped `modules.conf.xml` loads anything not on the list |
| `docker-entrypoint.sh` | Injects a deployment's own values into the shipped configuration at every container start. Its header comment is the contract |
| `assert-audio-stream.sh` | Fails the build if the built `mod_audio_stream` does not declare SpeexDSP |
| `build.sh` | Builds and pushes `rasonyang/freeswitch-aicc` |
| `DOCKERHUB.md` | The image's own documentation: every environment variable, port and volume |

Nothing is patched at runtime. It used to be: a third-party image was turned
into this project's switch by an entrypoint hook that applied design 01 §7's
diffs with `sed` at every boot, against whatever that image happened to unpack.
Those diffs now live in the files. The entrypoint substitutes values and does
nothing else.

## Prerequisites, either way

* The application's migrations applied, so the `luacc` views exist.
* The read-only database role created:
  `psql "$AICC_DATABASE_URL" -v lua_password="'…'" -f deploy/sql/lua_role.sql`
  That file is in the repository and **not in the release tarball**, which
  carries the binary, `.env.example`, the licence and the notices and nothing
  else. Fetch it at the tag being deployed:
  `curl -fsSLO https://raw.githubusercontent.com/rasonyang/ai-native-callcenter/v0.1.0/deploy/sql/lua_role.sql`.
  It has to run as a superuser and after the migrations, because it grants on
  the `luacc` views.
* **A database of its own for mod_callcenter.** It creates unqualified
  `agents`/`tiers`/`members` tables in `public`, which collide with the
  application's, so it gets `aicc_fs` rather than sharing.

## 1. The image

```sh
make fs-image      # one architecture, loaded into the local Docker daemon
make fs-push       # linux/amd64 + linux/arm64, pushed
```

Both call `build.sh`, whose header comment lists what it reads — `IMAGE`,
`FS_REF`, `PLATFORM`, `SOUNDS`, `MAKE_JOBS`. The build context is this
directory, not the repository root.

Nothing is read from the machine running the build. The FreeSWITCH sources
(tag `v1.11.3`, with sofia-sip and spandsp pinned to commits),
`mod_audio_stream` and the sound files are all fetched inside the build at pins
the Dockerfile names, and the sound tarballs are verified by sha256. A push
from a dirty working tree is refused, because the image is labelled with the
commit it came from and that label would name a commit nobody can check out.

`mod_audio_stream` is compiled in the builder stage from public upstream
(`amigniter/mod_audio_stream`) at a pinned commit, with its `libs/libwsc`
submodule pinned too, and `assert-audio-stream.sh` checks the result before it
is copied into the runtime stage. That check is on the module's `DT_NEEDED`
entry rather than on its symbols: undefined `speex_resampler_*` symbols are
normal and resolve at load from FreeSWITCH's own copy, so a module that never
linked SpeexDSP loads happily and then resamples nothing while claiming to — a
stream asked for at 24 kHz arrives as 8 kHz wearing a 24 kHz label and the
recogniser returns confident, wrong words.

Sound files come from the official tarballs at pinned versions, 8 kHz only.
`SOUNDS=none` leaves them out; queue hold music then stops working, because
`mod_local_stream` serves `music/8000` as `moh_stream`.

### Running it

`DOCKERHUB.md` is the image's contract: every environment variable with what it
is injected into and what leaving it unset means, plus the ports and the
volumes. Two variables are required — `AICC_LUA_DSN` and `AICC_CC_DSN` — and
the container refuses to start without them, because a switch that started
without a database would look entirely healthy while telling every agent phone
that its extension does not exist. Everything else is defaulted or optional.

`deploy/demo/docker-compose.yml` is a worked example of the whole set.

## 2. A native install

Still a real path: this project's development box runs a native switch, and
[`deploy/dev/freeswitch/README.md`](../deploy/dev/freeswitch/README.md) assumes
it.

Build or install FreeSWITCH v1.11.3 or newer with at least the modules in
`modules.conf`. `mod_lua`, `mod_pgsql` and `mod_callcenter` are the ones
nothing works without; `mod_http_cache` matters only if recordings go to an
object store; `mod_audio_stream` only if the human phase is transcribed, and it
is not part of the FreeSWITCH tree — the `mod_audio_stream` stage in the
`Dockerfile` is the recipe.

Then install this tree over the switch's own:

```sh
FS=/usr/local/freeswitch
cp -R freeswitch/conf/.        "$FS/conf/"
cp    freeswitch/scripts/*.lua "$FS/scripts/"
```

`conf/vars.xml` carries this deployment's own values, so a copy overwrites
them. Save the existing one first, and put back anything local afterwards —
on a box that has the development overlay installed, that means re-adding its
`X-PRE-PROCESS` lines and re-running `deploy/dev/freeswitch/install.sh`.

### What the copy leaves you to fill in

The shipped tree carries placeholders where a container would take an
environment variable. Until they are real values the switch is not usable.

In `conf/vars.xml`:

* `aicc_lua_dsn` and `aicc_cc_dsn` — both say `password='REPLACE_ME'`, and
  `host=127.0.0.1`, which is right for a switch on the database's own machine
  and wrong otherwise.
* `aicc_bot_host` — ships as `127.0.0.1` and must become `$${local_ip_v4}` on
  a native box. Never loopback, never `localhost`, never with a `sip:` scheme
  in the value: the application binds the LAN address, a LAN-bound macOS socket
  cannot send to loopback, and the result is a gateway that reports UP and
  calls that never arrive.
* `domain` — `$${local_ip_v4}` as shipped. Whatever it becomes must equal the
  application's `AICC_SWITCH_DOMAIN`: queue names are rendered with it on both
  sides and mod_callcenter matches them literally.
* `aicc_recordings_dir` — a directory or a URL; see below.
* `default_password` — `REPLACE_ME`. It applies only to file-based directory
  entries, of which this tree has none, but leave nothing usable there.

In `conf/autoload_configs/event_socket.conf.xml`: the password ships as the
stock `ClueCon`, deliberately, so that a published image carries no credential
anyone uses. Change it, and change `AICC_ESL_PASSWORD` with it. The port is
18021, not the stock 8021.

In `conf/autoload_configs/acl.conf.xml`: the `aicc_esl` list is loopback plus
the RFC1918 ranges, written for a container network. On a host switch, narrow
it to the address the application actually connects from.

### Applying it

```sh
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

Editing the Lua scripts afterwards needs no reload at all — the handler reads
the file per request. Only the initial binding needs the restart.

Verify:

```sh
fs_cli -x "callcenter_config queue list"                 # queues from the database
fs_cli -x "sofia status gateway aicc_bot"                # UP once the application listens
fs_cli -x "user_exists id 1000 \$\${domain}"             # directory served from the database
fs_cli -x "sofia status profile internal" | grep -i context   # aicc, not public
```

The third one asks about an extension, so it answers `false` until an account
exists — on a database that has only been migrated there are none, and that is
not a fault in the switch. Create an agent or a supervisor first
(`POST /users`); the number it is given is the lowest free one in
`AICC_EXTENSION_RANGE`, so on a fresh deployment it is 1000. Ask about that
number, not a number from the demo dataset.

That last one is the check worth keeping: a phone's INVITE takes its profile's
context, and in `public` the stock rules hand internal numbers to the stock
`default` dialplan before any aicc rule is reached — every rule installed,
valid, and never executed.

## 3. What a deployment adds on top

### Its own dialplan rules

`conf/dialplan/aicc.xml` ends with `<X-PRE-PROCESS cmd="include"
data="aicc/*.xml"/>`. That directory is the seam: a carrier trunk, a simulated
PSTN, a feature code one floor needs. The image ships the directory with a
README in it and nothing else, and the entrypoint never writes there. `conf` is
not a volume, so mount files into that path rather than copying them into a
running container — a copy is lost when the container is replaced.

It has to be an include rather than a second file declaring `<context
name="aicc">` again — two files that both open the same context do not merge,
the first wins and the second is silently ignored while still being visible to
`xml_locate`.

[`deploy/dev/freeswitch/`](../deploy/dev/freeswitch/README.md) is this
repository's own worked example: the development box's simulated PSTN trunk, as
a fragment for this seam plus the gateway that names the peer. It is version
controlled and deliberately not shipped in the image, because a trunk belongs
to a deployment.

### Where recordings go

`aicc_recordings_dir` is where `record_session` writes, and it takes a URL as
readily as a directory. With `mod_http_cache` loaded and `enable-file-formats`
true — which is how the shipped tree has it — pointing it at an HTTP store
makes the switch upload each finished recording itself, with no shared volume
and no spool:

```xml
<X-PRE-PROCESS cmd="set" data="aicc_recordings_dir=http://127.0.0.1:8888/buckets/aicc-recordings"/>
```

That address is the SeaweedFS filer from `deploy/dev/docker-compose.yml`; the
application reads the same files back over the S3 port
(`AICC_RECORDING_BACKEND=S3`, `AICC_S3_ENDPOINT=127.0.0.1:8333`, no
`AICC_RECORDING_DIR`). The same mechanism plays files *from* the store —
`playback http://…/prompt.wav` — so prompts can live there too. mod_http_cache
speaks plain HTTP(S); front the store with a TLS proxy if the network between
the switch and the store is not trusted.

In a container this is `AICC_RECORDINGS_DIR`, and S3 signing is configured
through the `FS_S3_*` variables in `DOCKERHUB.md`. The entrypoint creates and
chowns the directory when the value is a filesystem path, and leaves it alone
when it is a URL.

### Live transcription

`mod_audio_stream` is in the image, so the human phase can be transcribed. The
switch still refuses to start when `AICC_TRANSCRIPTION_ENABLED` is true and the
module is not found, and that guard stays even though this image has it. The
guarantee it makes is not "the image has the module" — that is a property of
whatever built the image, and a future one can drop it without anybody noticing
— but "a stack told to transcribe cannot start without it". The failure it
prevents is the quiet one: the application healthy, the switch healthy, calls
connecting, agents answering, and the transcript panel saying "Connecting…" for
ever while nothing anywhere reports a fault
(`docs/design/08-transcription.md` §B.5).

If you build the module yourself, run `assert-audio-stream.sh` against the
result.

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
