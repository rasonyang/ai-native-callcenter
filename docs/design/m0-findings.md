# M0 Verification Spike — Findings (2026-08-13)

All four checks ran against the live dev environment (FreeSWITCH 1.11.1 @ ESL 127.0.0.1:18021; PostgreSQL 18.4 via `deploy/dev/docker-compose.yml`; OpenAI GA WebSocket API). Design docs 01/02/03/06 have been amended where findings differed; this document is the evidence record.

| # | Check | Result |
|---|---|---|
| M0.1 | Live ESL event shapes | ✅ verified (one item deferred to M2: `sip_hangup_disposition` needs real SIP legs) |
| M0.2 | mod_callcenter `odbc-dsn` → `pgsql://` | ✅ works on 1.11.1 — **with a schema-collision finding** (design amended: dedicated `aicc_fs` database) |
| M0.3 | OpenAI `audio/pcmu` / `audio/pcma` | ✅✅ both laws accepted **and stored**, both directions, on `gpt-realtime-2.1` — zero-resample path for the whole OpenAI leg |
| M0.4 | Lua `freeswitch.Dbh("pgsql://…")` | ✅ queried PostgreSQL 18.4 from mod_lua, no ODBC |

## M0.1 — ESL event shapes (capture: 8× CREATE/ANSWER/HANGUP, 3× PARK, BRIDGE/UNBRIDGE, HOLD, DTMF, RECORD_START/STOP, 6× sofia::register, 15× callcenter::info)

Verified header facts (samples in the capture digest; raw log was session-scratch):

- **`sofia::register`** (live SIP.js + native softphone registrations observed): headers `profile-name, from-user, from-host, contact, call-id, status ("Registered(UDP)"/"(WSS-NAT)"), expires, network-ip, network-port, username, realm, user-agent`, plus **all directory variables flattened in** (`user_context`, `effective_caller_id_*`, `callgroup`, …). cti-server's "username vs sip_user" doubt: it is `username`.
- **Registration realm nuance**: the web-sip-phone extension registered with `Auth-Realm: ws.aicc.test` (the Caddy hostname) while the stored registration is `1008@192.168.31.248` (thanks to `force-register-db-domain`). The Lua directory handler must therefore serve users for **whatever realm the challenge produced** — key the lookup on user id, not on domain equality (design 01 §5 note).
- **DTMF**: `DTMF-Digit`, `DTMF-Duration` (in 8kHz ticks — captured `2000` = 250ms default), `DTMF-Source`. The ÷8→ms mapping is confirmed.
- **CHANNEL_BRIDGE**: `Bridge-A-Unique-ID` / `Bridge-B-Unique-ID` + `Other-Leg-Unique-ID` / `Other-Leg-Channel-Name`.
- **CHANNEL_PARK**: full caller profile (`Caller-Context`, `Caller-ANI`, `Caller-Destination-Number`) **and channel variables are readable at park time** (`variable_current_application: park`) — the Lua-sets-vars → app-reads-at-park handoff pattern is valid.
- **CHANNEL_HANGUP_COMPLETE**: `Hangup-Cause` + `variable_hangup_cause` + **`variable_hangup_cause_q850`** (Q.850 code available directly) + `variable_transfer_history` / `variable_transfer_source` (`bl_xfer:9196/default/XML` — blind transfers leave a machine-readable trace).
- **ANI vs CID divergence** visible on loopback legs — cti-server's fallback chain (`Caller-ANI` → `Caller-Caller-ID-Number`) is the right read order.
- **`callcenter::info`** — full CC-header sets captured per action:
  - `member-queue-start/-end`: `CC-Queue, CC-Member-UUID, CC-Member-Session-UUID` (= caller channel UUID), `CC-Member-CID-*`, on end `CC-Cause` (`Cancel`) + `CC-Cancel-Reason` (`BREAK_OUT`) + joined/leaving times (epoch seconds).
  - `agent-offering`: `CC-Agent, CC-Agent-System, CC-Member-Session-UUID` — the screen-pop correlation key.
  - `bridge-agent-fail`: `CC-Hangup-Cause` (`USER_NOT_REGISTERED` captured), called/aborted times — RONA raw material.
  - `agent-state-change` (`CC-Agent-State: Receiving`), `agent-status-change` (`CC-Agent-Status: Available`), `agent-add`, `agent-contact-change`, `members-count` (`CC-Count`).
- **Deferred to M2** (needs real SIP legs, e.g. the two live extensions): `sip_hangup_disposition` values (`recv_refer`/transfer dispositions), `CHANNEL_UNHOLD` shape (loopback `uuid_hold off` returns `-ERR Operation failed`), sofia `sip_user_state` ping events.

Environment quirks recorded: with colima running, FS event headers report `FreeSWITCH-IPv4: 198.18.0.1` (colima's shared interface wins core auto-detection) — cosmetic here because `vars.xml` pins `local_ip_v4`, but a reason to keep that pin forever.

## M0.2 — mod_callcenter on PostgreSQL

`odbc-dsn` = `pgsql://hostaddr=127.0.0.1 dbname=… user=… password='…'` works on FS 1.11.1 + mod_pgsql: after `reload mod_callcenter` the module **created tables `agents`, `tiers`, `members`** and queue operations kept working.

**Finding that changed the design**: the tables are created **unqualified in the `public` schema** — in a shared database they would collide with our own `agents` table. Resolution (03 §1 amended): mod_callcenter gets a **dedicated database `aicc_fs`** in the same PostgreSQL instance (not a schema). Applied to the live dev box: `callcenter.conf.xml` now carries the `aicc_fs` DSN (original backed up as `callcenter.conf.xml.m0bak`); spike tables dropped from the app database. The repo's `freeswitch/` deliverable formalizes this in M2 (design diff D4).

## M0.3 — OpenAI Realtime G.711 passthrough

GA WebSocket (`wss://api.openai.com/v1/realtime?model=gpt-realtime-2.1`), GA session shape. `session.update` with `audio.input.format.type` / `audio.output.format.type` set to `audio/pcmu`, then `audio/pcma`, both returned `session.updated` **echoing the stored formats** — acceptance proven, both laws, both directions.

Consequences (02 §2 amended): the OpenAI leg is a pure RTP-payload passthrough for **both PCMU and PCMA** — no decode, no resample, no A↔µ transmap; the 24k PCM contingency path is retained but not needed. Capacity: the all-OpenAI WAN figure (~0.12 Mbps/call) is now verified engineering, not hope (06 §1).

Also observed (GA defaults on session.created): input/output default `audio/pcm` @ 24000; `turn_detection` default `server_vad {threshold: 0.5, prefix_padding_ms: 300, silence_duration_ms: 500, create_response: true, interrupt_response: true, idle_timeout_ms: null}` — the 500ms default silence matches the latency-budget assumption in 02 §8; `idle_timeout_ms` is a new knob worth evaluating for dead-air handling; default voice `marin`; `output.speed` exists.

## M0.4 — Lua → PostgreSQL

`freeswitch.Dbh("pgsql://hostaddr=127.0.0.1 dbname=aicc user=aicc password='aicc'")` from a mod_lua script returned `PostgreSQL 18.4 … user=aicc` via the api `lua` command. The directory/dialplan/queue-config path in design 01 §5 is executable exactly as written (mod_pgsql in-core, no ODBC layer).

## Environment state after M0 (dev box)

- PostgreSQL 18.4 runs via `deploy/dev/docker-compose.yml` (`aicc-postgres`, 127.0.0.1:5432, user/db `aicc` + callcenter db `aicc_fs`). **Never via brew** (owner directive; the transient brew install was reverted).
- FreeSWITCH config deltas now live: `callcenter.conf.xml` odbc-dsn (backup `.m0bak`). Still pending from D1: `modules.conf.xml` (mod_callcenter uncomment + stale custom-TTS line removal — the module remains runtime-loaded only).
- Go toolchain on the box is **1.25.7**; the stack pins Go 1.26 — install/bump at M1 module init.
- Provider keys: `OPENAI_API_KEY` present; no DashScope key yet → Qwen connect smoke happens when one is provided (Qwen formats are documented fixed 16k-in/24k-out, so no design ambiguity blocks on it).
