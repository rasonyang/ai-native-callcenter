<!-- SPDX-License-Identifier: Apache-2.0 -->
<!--
  The Docker Hub overview for rasonyang/freeswitch-aicc.

  Publishing it is a separate step that needs a Hub credential; the script that
  does it is the owner's (/usr/local/src/freeswitch/docker/aicc/hub-overview.sh,
  outside this repository, reading ~/.docker/config.json) and nothing here
  automates it.
-->

# rasonyang/freeswitch-aicc

FreeSWITCH **v1.11.3**, built from source for [aicc](https://github.com/rasonyang/ai-native-callcenter), an AI-native call center. The module set, the configuration tree and the Lua scripts inside the image come from that repository; every external dependency — PostgreSQL, S3, the voice bot, the PSTN trunk — is injected at container start through environment variables.

Nothing site-specific is baked in. The image carries no database credential, no ACL subnet belonging to anyone's office, no LAN address and no trunk.

- Platforms: `linux/amd64` and `linux/arm64`
- FreeSWITCH: tag `v1.11.3` (sofia-sip and spandsp pinned to commits — see the image labels)
- Extra module: `mod_audio_stream` at a pinned upstream commit, with `libwsc` linked statically and SpeexDSP linked explicitly
- Base image: `debian:bookworm-slim`

## Tags

| Tag | Description |
|---|---|
| `v1.11.3` | Multi-arch manifest: `linux/amd64` + `linux/arm64` |
| `latest` | Same as the newest version, multi-arch (`linux/amd64` + `linux/arm64`) |

## Quick start

```bash
docker run -d --name freeswitch --network host \
  -e AICC_LUA_DSN="pgsql://hostaddr=10.0.0.5 dbname=aicc user=aicc_lua password='secret'" \
  -e AICC_CC_DSN="pgsql://hostaddr=10.0.0.5 dbname=aicc_fs user=aicc password='secret'" \
  -e AICC_BOT_HOST=10.0.0.6 \
  -e FS_ESL_PASSWORD="$(openssl rand -hex 16)" \
  -v fs-db:/usr/local/freeswitch/db -v fs-log:/usr/local/freeswitch/log \
  rasonyang/freeswitch-aicc:v1.11.3

docker exec freeswitch fs_cli -P 18021 -p "$FS_ESL_PASSWORD" -x status
```

## Environment variables

Each variable is one of three things.

**Required.** The container refuses to start without these, and says why. The switch reads its directory — which SIP accounts exist, their passwords, the context they land in — and its queues from PostgreSQL through `aicc_xml.lua`. Without a DSN it answers nothing, and a switch in that state looks entirely healthy while telling every agent phone that its extension does not exist.

| Variable | Injected into | Description |
|---|---|---|
| `AICC_LUA_DSN` | vars.xml `aicc_lua_dsn` | Business database, read by the `aicc_*.lua` scripts |
| `AICC_CC_DSN` | vars.xml `aicc_cc_dsn` | Call center database |

DSN format: `pgsql://hostaddr=10.0.0.5 dbname=aicc user=aicc password='secret'`.

**Defaulted.** Unset gives a working self-contained switch, and the default is written into the configuration explicitly rather than left to whatever the file said.

**Optional.** Unset leaves the shipped configuration alone, because what it ships is already a working value.

### Network and identity (vars.xml)

| Variable | Injected into | Unset |
|---|---|---|
| `FS_LOCAL_IP` | `local_ip_v4` (SIP/RTP bind address) | FreeSWITCH's own detection: the container's primary interface. Any `local_ip_v4` setting in the tree is removed so the core value wins. |
| `FS_EXTERNAL_IP` | `external_rtp_ip`, `external_sip_ip` | `$${local_ip_v4}`. Vanilla resolves these over STUN, which behind NAT advertises the whole network's public address and sends the media of a purely internal call there. |
| `FS_DOMAIN` | `domain` (the SIP realm phones register to) | `$${local_ip_v4}` |
| `FS_DEFAULT_PASSWORD` | `default_password` | Optional; the shipped value. aicc accounts come from PostgreSQL with their own passwords, so this affects only file-based directory entries, of which the tree has none. |

### Event socket (event_socket.conf.xml)

| Variable | Injected into | Unset |
|---|---|---|
| `FS_ESL_LISTEN_IP` | `listen-ip` | `0.0.0.0` — the application is in another container. What keeps that safe is the ACL and the fact that the port is not published, not the bind address. **`--network host` removes both halves of that**: nothing is published because everything is, and the shipped `aicc_esl` list allows the RFC1918 ranges, so any machine on the LAN reaches the event socket and is answered with `auth/request`. Set `127.0.0.1` when the application shares the host, or narrow `FS_ESL_ACL` to the one address it connects from. |
| `FS_ESL_PORT` | `listen-port` | `18021` (not the stock 8021; the project documents 18021 everywhere) |
| `FS_ESL_PASSWORD` | `password` | `ClueCon`, the stock FreeSWITCH default, with a warning on every start. Set it. |
| `FS_ESL_ACL` | `apply-inbound-acl` | `aicc_esl` — loopback plus the RFC1918 ranges a compose network draws from, defined in the shipped `acl.conf.xml` |

### Voice bot

| Variable | Injected into | Unset |
|---|---|---|
| `AICC_BOT_HOST` | vars.xml `aicc_bot_host` | `$${local_ip_v4}`, i.e. this container — wrong for every real deployment, and it fails visibly on the first AI call rather than quietly. Set it to where the application runs. |
| `AICC_BOT_PORT` | vars.xml `aicc_bot_port` | `6060` |

### PSTN trunk

There is no trunk in the image.

| Variable | Injected into | Unset |
|---|---|---|
| `PSTN_GATEWAY_HOST` | vars.xml `pstn_gateway_host` | `192.0.2.1` — TEST-NET-1, reserved by RFC 5737 and routable from nowhere. An outbound PSTN call then fails to reach a gateway, which is what should happen when no trunk was configured. |
| `PSTN_GATEWAY_PORT` | vars.xml `pstn_gateway_port` | `5080` |
| `PSTN_GATEWAY_CALLER_ID` | vars.xml `pstn_gateway_caller_id` | empty, so the dialplan passes the call's own `effective_caller_id_number` through instead of stamping every outbound call with one number |

### Recordings and S3

| Variable | Injected into | Description |
|---|---|---|
| `AICC_RECORDINGS_DIR` | vars.xml `aicc_recordings_dir` | A directory or a URL. With `mod_http_cache` serving file formats, `http://filer:8888/buckets/aicc-recordings` makes the switch upload each finished recording itself, with no volume shared with the application. Unset: `/usr/local/freeswitch/recordings`, which is a declared volume. |
| `FS_S3_ACCESS_KEY_ID` | http_cache.conf.xml `aws-s3` profile | S3 signing is configured only when all three of these are set |
| `FS_S3_SECRET_ACCESS_KEY` | same | |
| `FS_S3_BASE_DOMAIN` | the profile's `base-domain` and `domains` | e.g. `s3.example.com` or `minio.internal:9000` |
| `FS_S3_BUCKET` | the profile's `domains` (optional) | When set the profile domain is `<bucket>.<base-domain>`, otherwise `<base-domain>` |
| `FS_S3_REGION` | the profile's `region` | `us-east-1` |

`mod_http_cache` matches a profile by exact URL hostname, so the host formed from `FS_S3_BUCKET` and `FS_S3_BASE_DOMAIN` must equal the host in `AICC_RECORDINGS_DIR`. `AICC_RECORDINGS_DIR=http://aicc-recordings.s3.example.com` goes with `FS_S3_BUCKET=aicc-recordings` and `FS_S3_BASE_DOMAIN=s3.example.com`.

### Databases the switch keeps for itself

Both optional. Unset, each uses the SQLite database inside the container, which works and does not survive the container.

| Variable | Injected into | Description |
|---|---|---|
| `FS_CORE_DB_DSN` | switch.conf.xml `core-db-dsn` | The core database. It must be reachable at startup or FreeSWITCH exits with `Cannot Initialize`. |
| `FS_CALLCENTER_DSN` | callcenter.conf.xml `odbc-dsn` | `mod_callcenter`'s own tables. Unreachable, the module fails to load; the rest of the switch is unaffected. Its tables are unqualified in `public`, which is why aicc gives it a database of its own. |

### RTP

| Variable | Injected into | Description |
|---|---|---|
| `FS_RTP_START_PORT` / `FS_RTP_END_PORT` | switch.conf.xml | Optional. A widened range has to be published on the container too, or media from outside the host arrives at a port nothing is listening on. |

### Live transcription

| Variable | Description |
|---|---|
| `AICC_TRANSCRIPTION_ENABLED` | `true` loads `mod_audio_stream` and, if it is not in the image, refuses to start. This image builds the module, so the check looks redundant — it is kept because the guarantee that matters is not "the image has the module" but "a stack told to transcribe cannot start without it". Without it the switch cannot tap an agent's leg and nothing reports a fault: the application is healthy, calls connect, and the transcript panel says "Connecting..." for ever. |

At startup the entrypoint prints one `[entrypoint] injected: ...` line naming the variables it used. Values are never printed.

## Ports

| Port | Purpose |
|---|---|
| 5060/udp,tcp | SIP internal |
| 5080/udp,tcp | SIP external |
| 5066/tcp | SIP over WebSocket |
| 7443/tcp | SIP over WSS |
| 18021/tcp | ESL |
| 16384-32768/udp | RTP |

`--network host` is the simplest arrangement for SIP and RTP. On a bridge network, set `FS_EXTERNAL_IP`, narrow `FS_RTP_START_PORT`/`FS_RTP_END_PORT`, and publish that range.

## Volumes

| Path | Contents |
|---|---|
| `/usr/local/freeswitch/db` | SQLite (core, sofia registrations, callcenter, voicemail) |
| `/usr/local/freeswitch/log` | Logs and CDRs |
| `/usr/local/freeswitch/recordings` | Local recordings |
| `/usr/local/freeswitch/storage` | Voicemail |
| `/usr/local/freeswitch/certs` | Optional. Mount `wss.pem` and `dtls-srtp.pem`; self-signed certificates are generated when they are missing. |

## Modules (27)

mod_console, mod_logfile, mod_cdr_csv, mod_event_socket, mod_sofia, mod_loopback, mod_commands, mod_dptools, mod_expr, mod_fifo, mod_hash, mod_voicemail, mod_valet_parking, mod_dialplan_xml, mod_g723_1, mod_g729, mod_opus, mod_sndfile, mod_native_file, mod_local_stream, mod_tone_stream, mod_lua, mod_http_cache, mod_say_en, mod_callcenter, mod_pgsql, mod_audio_stream.

The build fails if the shipped configuration loads a module the image does not contain.

## Image contents

- `/usr/local/freeswitch/conf` — the aicc configuration tree, from the repository's `freeswitch/conf/`
- `/usr/local/freeswitch/scripts` — `aicc_inbound.lua`, `aicc_queue.lua`, `aicc_track.lua`, `aicc_xml.lua` (the `mod_lua` XML handler that serves the directory and the queues from PostgreSQL)
- `/usr/local/freeswitch/sounds` — 8 kHz only: `freeswitch-sounds-en-us-callie-8000-1.0.53` and `freeswitch-sounds-music-8000-1.0.52`, downloaded from files.freeswitch.org during the build and verified by sha256. The wideband sets are another ~450MB and nothing here plays a prompt above narrowband.

FreeSWITCH runs in the foreground as the `freeswitch` user (`-nonat -nf -nc`); `HEALTHCHECK` is `fs_cli -x status`.

## Build

```bash
freeswitch/build.sh                              # v1.11.3, both platforms, --push
LOAD=1 PLATFORM=linux/arm64 freeswitch/build.sh  # one platform, into the local daemon
MAKE_JOBS=3 freeswitch/build.sh                  # emulated builds want 2-3
SOUNDS=none freeswitch/build.sh                  # no sound files (queue hold music stops working)
```

The build context is the repository's `freeswitch/` directory as it stands in the working tree. Nothing is read from the machine running the build: the FreeSWITCH and `mod_audio_stream` sources are cloned at pinned refs and the sound files are downloaded at pinned versions and checksummed. A push from a dirty working tree is refused, because `org.opencontainers.image.revision` would then name a commit nobody can check out.

On Apple Silicon, run Colima with `--vz-rosetta` so the amd64 half compiles under Rosetta; QEMU user-mode emulation segfaults gcc at random.
