# Design 01 — Telephony Core (ESL, State Machines, mod_callcenter, FreeSWITCH Diffs, Lua)

## 1. ESL integration

One ESL **inbound-mode** connection (default `127.0.0.1:18021`, password from env). In-repo client (`internal/esl`, adapted from cti-server's): auth handshake, `event plain` subscription, synchronous `api`/`bgapi` with FIFO reply matching, path-unescape preserving `+` in E.164.

**Subscriptions**: `CHANNEL_CREATE, CHANNEL_ANSWER, CHANNEL_PARK, CHANNEL_BRIDGE, CHANNEL_UNBRIDGE, CHANNEL_HOLD, CHANNEL_UNHOLD, CHANNEL_HANGUP_COMPLETE, DTMF, RECORD_START, RECORD_STOP, CUSTOM sofia::register sofia::unregister sofia::sip_user_state callcenter::info`.

**Command vocabulary** (`internal/telephony/adapter`; complete list — anything else needs a design change): `originate {origination_uuid=…}…`, `uuid_transfer <uuid> 'bridge:{…}<ep>' inline` (route-to-endpoint AND blind transfer — **never** `originate`+`uuid_bridge`, per cti-server's production lesson), `uuid_transfer <uuid> <ext> XML <ctx>` (send a live channel through dialplan, used for queue entry), `uuid_phone_event <uuid> talk|hold` (browser phones), `uuid_answer`, `uuid_hold [off]`, `uuid_break`, `uuid_broadcast`, `uuid_record start|stop`, `uuid_kill <cause>`, `uuid_setvar(_multi)`, `callcenter_config <…>`, `sofia profile external rescan`, `reloadxml`, `show channels as json`, `create_uuid`.

**Link management**: reconnect 500ms→30s exponential backoff; state observed everywhere; call-affecting endpoints return `503 switch_down` while down; agent-profile and read endpoints keep working. On (re)connect: recovery reconcile (§6).

**Normalization boundary**: raw FS events are mapped in one place to internal events; header hypotheses (from cti-server, never live-verified) are validated by the M0 spike before hardening. Switch-level facts needed by events (release cause, SIP status, codec) surface as normalized payload fields; business context lives in `userData`. `switchData` (the Genesys-Extensions analog) is strictly request-side — it exists only on `POST /calls` and in the Lua leg-creation scripts, applied to originate strings / channel variables at this boundary (04 §4).

## 2. State machines (table-driven tests mandatory)

**Call FSM** (aggregate): `CREATED → RUNNING → ENDING → ENDED`.

**CallType** (Genesys lineage; Go `CallType`, values `INBOUND | OUTBOUND | CONSULT | INTERNAL`): stamped at call creation, **immutable across transfers** — the caller-perspective in/out distinction survives every handoff (an AI outbound callback transferred to an agent still reads OUTBOUND on the agent's screen). Assignment: `INBOUND` — call arrives from a trunk/DID (public context); `OUTBOUND` — we originate to an external number (F4 AI outbound, F5 click-to-dial); `INTERNAL` — both parties are our extensions (e.g. `Local_Extension` dialing); `CONSULT` — a secondary leg created on behalf of an active call (reserved in the contract for the consult-transfer roadmap; MVP emits only the first three). Present on every call event envelope (04 §4).

**Party FSM** (per leg; `party_id` = channel UUID): `DIALING|RINGING → TALKING ⇄ HELD → RELEASED`. First party is the sole originator (starts DIALING); all later parties start RINGING. Illegal edges rejected and logged (`DIALING→RINGING`, out of `RELEASED`, …). Wire events are party-scoped: `PARTY_DIALING/RINGING/ESTABLISHED/HELD/RETRIEVED/RELEASED` plus `PARTY_CHANGED` (transfer replaces a party within the same call, `callId` stable) — `PARTY_ESTABLISHED` is the edge into `TALKING` (Genesys `EventEstablished` naming), **fired by the switch's `CHANNEL_BRIDGE`, not by `CHANNEL_ANSWER`** (owner's rule 2026-08-24, superseding the original answer-driven edge — see **C55** in `docs/verification/TASKS.md`): a leg answering is that leg's own fact and the ledger keeps it as one (`bill_sec`), while being on a call is a fact about two legs. An auto-answer phone picks up in front of nobody; a caller the switch answers to play queue music is talking to nobody. Every party state change goes through the transition table — the table is the only thing that may move a party.

**Agent FSM**:

| From | Event | To |
|---|---|---|
| LOGGED_OUT | login (agent enabled ∧ extension is an enabled `AGENT` extension ∧ both free) | NOT_READY(LOGIN) |
| NOT_READY(r) | ready request | READY |
| READY | not-ready request (reason) | NOT_READY(r) |
| READY/NOT_READY | logout / force-logout | LOGGED_OUT |
| (on queue call end, agent talked) | after-call work starts | NOT_READY(AFTER_CALL_WORK) until the agent files it — **no timer** (owner directive 2026-08-19; see below) |
| READY | RONA (mod_callcenter marks agent On Break after max-no-answer) | NOT_READY(SYSTEM) |

Reasons: `LOGIN, BREAK, LUNCH, TRAINING, AFTER_CALL_WORK, SYSTEM, SUPERVISOR`. Agent state rows are written **synchronously in-request** (fail `503 storage_down` rather than acknowledge unrecorded state); timer transitions apply in memory and persist best-effort. Derived `availability` (snapshot-only, wallboard vocabulary): `logged_out → on_call → wrap_up → not_ready → device_unreachable → ready`.

**Device observation**: `sofia::register/unregister` + `all-reg-options-ping` (`sofia::sip_user_state`) + call-setup failure → `in_service` flag → `device.in_service_changed` SSE + `DEVICE_UNAVAILABLE` handling. This is the "dead browser tab" detector.

**Agent state ↔ mod_callcenter sync** (Go is the state owner; callcenter mirrors):

| aicc state | callcenter agent status |
|---|---|
| READY | `Available` |
| NOT_READY(any reason, incl. ACW) | `On Break` |
| LOGGED_OUT | `Logged Out` |

Sync direction Go→FS via `callcenter_config agent set status '<agent_uuid>' '<status>'`; FS→Go only for RONA (observe `agent-state-change` → mirror as NOT_READY(SYSTEM)). ACW is owned by aicc (richer reasons/UI), so callcenter agents are configured `wrap-up-time=0`.

**Amendment — after-call work is opened for the agent and confirmed by them (owner directive 2026-08-19, second pass).** The record is no longer something the agent creates: the moment after-call work begins the platform writes a `wrap_ups` row with the default disposition (`RESOLVED`), an empty note and `is_confirmed = false`. Done applies whatever they changed and sets it true. Three consequences worth stating: a finished call *always* has a record, so "nobody wrote this up" is a fact a report can read rather than an absence; nothing is required of the agent, because the ordinary call is one press; and `is_confirmed` is the difference between a record somebody looked at and one standing on its defaults, which is what the day's completion rate (`GET /reports/me`) counts. The cockpit still holds an agent out of *going ready* and *dialling out* while a record is unconfirmed — guidance, not a lock, and read from the server so a page reload is not a way past it.

**Amendment — after-call work ends when the agent files it (owner directive 2026-08-19).** The original design gave ACW a per-agent countdown (`agents.wrap_up_time_sec`) that returned the agent to READY by itself. The directive is: enter ACW automatically when the call ends, show the *elapsed* time, require a disposition, offer a note, and end it with Done. A deadline contradicts a required disposition — an agent who waits it out files nothing — so the countdown, its column and `agent_states.wrap_up_ends_at` are gone (migration 00012). What keeps the queue safe is unchanged and is the switch's job: NOT_READY mirrors as `On Break`, so no call is delivered for as long as the agent is writing up the last one. An agent may still leave ACW by choosing another state, or be signed out by a supervisor; the call they were writing up stays theirs to file against until the next one is wrapped.

## 3. Call flows

**F1 Inbound AI call** (the only inbound path — every external number answers with a bot): trunk dials DID → dialplan `aicc_inbound.lua` → `luacc.dids` lookup → sets recording vars, `absolute_codec_string=PCMU,PCMA`, `hangup_after_bridge=true`, exports `sip_h_X-AICC-*` (call id minted by Lua via `create_uuid`, plus the DID and language) → `bridge sofia/gateway/aicc_bot/<did>` → in-app UAS answers, `aicall` session starts (02). The DID names only the number, not the flow: the application resolves DID → flow, so flow configuration never becomes part of the switch contract. If the bridge to the bot fails (provider outage, bot at capacity, gateway down), Lua falls back to the DID's `fallback_queue_ext_number` so the caller still reaches a human; a DID without a fallback gets the out-of-service announcement.

**F2 transfer_to_agent** (bot function call): `aicall` → telephony: (a) push rich handoff into the call's `userData` (SSE `CALL_USER_DATA`); (b) `uuid_transfer <caller_uuid> <queue_ext> XML default` → caller runs `aicc_queue.lua` → `callcenter(<queue>)`: MOH/announcements, member queued; (c) bot leg receives BYE → provider session closes. `callcenter::info member-queue-start` drives wallboard.

**Agent phone registration path** (verified live 2026-08-13): the web-sip-phone extension registers to `wss://ws.aicc.test/`; the operator-managed Caddy instance terminates TLS with a locally trusted certificate and forwards to FreeSWITCH's `wss-binding` on :7443 (TLS→TLS with `tls_insecure_skip_verify`, because sofia drops REGISTERs whose `Via` transport disagrees with the socket transport). A WebSocket upgrade carrying the `sip` subprotocol answers 200 through that path. Consequence for §5: the digest realm the phone presents is `ws.aicc.test` while the registration is stored under the profile domain, so the Lua directory handler keys on the user id alone. TLS lives entirely in that external proxy — this repo's dev loop stays HTTP-only.

**Answering takes about five seconds, by design of the phone** (measured 2026-08-13 on the wire): a remote-control `NOTIFY Event: talk` is answered by the extension in ~6 ms, but the `200 OK` for the INVITE follows ~5.1 s later. The delay is SIP.js's default `iceGatheringTimeout` of 5000 ms elapsing before it can produce answer SDP, not media acquisition, so it is a near-fixed ceiling rather than a variable cost — this environment offers candidate sources (a `198.18.0.1` pseudo-interface, an IPv6 ULA) that never settle, so gathering always runs to the cap. Consequences: the agent UI must show an explicit answering state rather than appearing unresponsive, and queue-delivered calls avoid the wait entirely because they answer from `sip_auto_answer` on the INVITE. If the extension ever lowers that timeout the ceiling drops accordingly; the number is theirs, not ours.

**F3 Agent delivery**: mod_callcenter originates the agent leg to contact `user/<ext>@<domain>` → extension rings; SPA shows popup (SSE `QUEUE_AGENT_OFFERED` derived from `agent-offering` + CHANNEL events with screen-pop payload from `userData`); agent clicks Answer → REST → `uuid_phone_event <agent_leg> talk` → extension auto-answers (BroadSoft NOTIFY) → callcenter bridges caller↔agent (`bridge-agent-start`). Auto-answer queues instead set `{sip_auto_answer=true}` on the agent contact. Wrap-up starts at `bridge-agent-end`.

**F4 Outbound AI**: `POST /api/v1/calls {call_id, kind:"ai_outbound", flow, to, …}` → `originate {origination_uuid=<b>,ignore_early_media=true,origination_caller_id_number=<clid>}sofia/gateway/<trunk>/<number> &park()` → on CHANNEL_ANSWER → `uuid_transfer <b> 'bridge:{…X-AICC-*}sofia/gateway/aicc_bot/<entry>' inline`. Dialing/retry/timeout stay in FS/ESL; AI resources engage only after answer. Idempotency: client-minted `call_id` + CDR ledger (a late retry never redials).

**F5 Agent click-to-dial**: REST → originate agent leg (`{origination_uuid,sip_auto_answer=true}user/<ext>` → `&park()`), on answer transfer-inline-bridge to `sofia/gateway/<trunk>/<number>`. Agent-first, per cti-server.

**F6 Monitor/whisper/barge**: originate supervisor leg `{sip_auto_answer=true}user/<sup_ext> &eavesdrop(<agent_party_uuid>)`; whisper/barge via `eavesdrop_enable_dtmf`-style control or re-originate with `queue_dtmf 1|2|3` (listen/whisper A/barge modes). mod_conference not required (absent from this build).

**F7 Overflow**: `callcenter()` returns with `cc_cause` (`timeout`/`no_agent`/abandoned) → `aicc_queue.lua` reads the queue's overflow action from PG: `bot_flow` (re-bridge to aicc_bot with the message-taking flow; creates a callback record via the flow's tool) / `announce_hangup` / `forward` (transfer to queue/number). After-hours check happens in `aicc_inbound.lua` before queue entry (queue `hours` config).

## 4. mod_callcenter contract

**Queue definitions** — rendered from PG by the Lua `configuration` xml-handler on demand; Go triggers `callcenter_config queue reload <name>` after config writes. Parameter mapping (PG → callcenter.conf):

| PG `queues` column | callcenter param |
|---|---|
| strategy (`LONGEST_IDLE_AGENT` default, `ROUND_ROBIN`, `TOP_DOWN`, `AGENT_WITH_LEAST_TALK_TIME`, `AGENT_WITH_FEWEST_CALLS`, `RANDOM` — our enum; mapped to callcenter tokens like `longest-idle-agent` only inside Lua/adapter, per 07 §5) | `strategy` |
| moh_sound (stream ref, default `$${hold_music}`) | `moh-sound` |
| max_wait_sec | `max-wait-time` |
| max_wait_no_agent_sec | `max-wait-time-with-no-agent` |
| announce_sound / announce_frequency_sec | `announce-sound` / `announce-frequency` |
| tier_rules (jsonb: `isApplied`, `waitSec`) | `tier-rules-apply` / `tier-rule-wait-second` |
| discard_abandoned_after_sec / is_abandoned_resume_allowed | `discard-abandoned-after` / `abandoned-resume-allowed` |

Live check 2026-08-13 confirmed this build's queue behavior fields, including `agent_no_answer_status` defaulting to **On Break** — matching the RONA mirroring in §2 — plus optional params we may map later (`ring_progressively_delay`, `skip_agents_with_external_calls`). The stock static `support@default` queue currently loaded is exactly what D4 replaces.

**Agents/tiers** — managed at runtime by Go over ESL: `callcenter_config agent add <uuid> callback` + `agent set contact` (`user/<ext>@<domain>` or `{sip_auto_answer=true}user/…`), `agent set status`, `tier add/set/del <queue> <agent> <level> <position>`; mirrors `queue_agents` (levels/positions from the staffing UI). Agent params: `max-no-answer=1`, `no-answer-delay-time=<per-queue RONA delay>`, `wrap-up-time=0` (aicc owns ACW), `reject-delay-time=3`.

**`odbc-dsn`** → `pgsql://` DSN so callcenter's runtime tables (`agents/tiers/members`) live in PG — **M0-verified on 1.11.1**, in a **dedicated `aicc_fs` database** (the module creates unqualified public-schema tables; a shared database would collide with our `agents`). Go reads live state from events and uses these tables only as a reconcile source.

**Events consumed** (`CUSTOM callcenter::info` → normalized): `member-queue-start/-end` (+cause), `agent-offering`, `bridge-agent-start/-end`, `agent-state-change`, `members-count`. These drive: wallboard waiting counts, `queue.*` SSE, per-call queue timings in CDR, RONA mirroring. Reconcile: periodic (30s) `callcenter_config queue list members|agents` diff against the registry (protects against missed events).

## 5. Lua deliverables (`freeswitch/scripts/`, versioned in repo)

| Script | Binding / entry | Function |
|---|---|---|
| `aicc_xml.lua` | `xml-handler-bindings="directory|configuration"` | `directory`: serve users from `luacc.directory` view (REGISTER auth + `user_context=default`, caller-id vars, per-agent `sip_auto_answer`) — lookups key on the user id, never realm equality (M0: WSS clients present realm `ws.aicc.test` while registrations store under the IP domain). `configuration`: serve **only** `callcenter.conf` from `luacc.queues`; return nothing for every other section/key → FS falls back to static files. |
| `aicc_inbound.lua` | dialplan `public` catch-all | DID lookup → hours check → recording vars → route to bot gateway (X-headers) or queue extension; unknown DID → reject. |
| `aicc_queue.lua` | dialplan `default`, queue extensions | `answer` → `record_session` (if enabled) → `callcenter(<queue>)` → overflow handling per `cc_cause` (F7). |

**DB access**: `freeswitch.Dbh("pgsql://…")` (mod_pgsql is loaded; no ODBC needed), read-only role `aicc_lua` with GRANT on `luacc.*` views only. **Caching strategy: none (per-lookup query), justified**: lookups are REGISTER auth (~50 agents / registration interval), inbound call setup (≤5/s at target load), and queue-conf render (only on reload) → worst case < 20 qps of single-row indexed SELECTs. Dbh handles are pooled by FS core. If measurements ever disagree, an in-Lua TTL memo (5s) is the documented escalation — correctness-first default is no cache.

**Contract rule**: Lua reads only `luacc.*` views; Go migrations may reshape base tables freely but every migration touching them must keep the views' shapes (reviewed as a pair — the "shared contract" from the mandate, see 03 §4). Upstream mod_callcenter tokens (`longest-idle-agent`, agent statuses `Available`/`On Break`) are produced only inside these scripts and the Go adapter from our SCREAMING_SNAKE enums — they never appear in views, domain code, or the API (07 §5).

## 6. Recovery & degradation

On start and every ESL reconnect: load live-call snapshots from PG, `show channels as json`, then adopt / adopt-as-new (grace window) / orphan-hangup / close-with-`system_recovery`; established media is never touched; HTTP binds only after recovery. mod_callcenter members survive an aicc restart untouched (queueing is FS-side — a benefit of T1). Degradation matrix: ESL down → 503 on call ops only, SSE keeps flowing; PG down → 503 on state-changing ops, events continue from memory; PG required at startup. Single-instance guard: PG advisory lock.

## 7. FreeSWITCH configuration diffs (for approval — the complete set)

**D1 `autoload_configs/modules.conf.xml`**: uncomment `<load module="mod_callcenter"/>`; delete the stale custom-TTS module `<load>` line (currently line 135 — its .so was removed; CRITs at every boot). *Status: `mod_callcenter.so` built, installed, and verified live 2026-08-13 (`module_exists` true; `callcenter_config queue list` answers with the stock `support@default` queue). The load is runtime-only until this diff lands — a FS restart would drop it.*

**D2 `vars.xml`**: `global_codec_prefs=OPUS,G722,PCMU,PCMA8` → `…,PCMA` (typo fix; restores A-law).

**D3 `autoload_configs/lua.conf.xml`**:
```xml
<param name="xml-handler-script" value="aicc_xml.lua"/>
<param name="xml-handler-bindings" value="directory|configuration"/>
```
Static directory users 1000–1019 remain as fallback during migration; removed in a later cleanup diff once DB directory is verified.

**D4 `autoload_configs/callcenter.conf.xml`**: replace stock content with `odbc-dsn` param (pgsql DSN) and no static queues (queues come from the xml-handler).

**D5 `sip_profiles/external/local6060.xml`** → replaced by `aicc_bot.xml`:
```xml
<gateway name="aicc_bot">
  <param name="proxy" value="sip:$${local_ip_v4}:6060"/>
  <param name="register" value="false"/>
  <param name="ping" value="30"/>
</gateway>
```
(`ping` gives real OPTIONS keepalive; the old `expire-seconds` was inert. Our UAS answers OPTIONS 200.)

**D6 dialplan** — ~~as originally written~~ **SUPERSEDED 2026-08-22 by D6a**. Original: remove custom extensions `outbound_test` (default.xml + public.xml), `bridge-to-local-9999`, `bridge-to-ai-6060` (public.xml), and delete the legacy bot dialplan file under `dialplan/public/` (the file defining the 95013/95015 routes; old bot retired per T6). Add `public/05_aicc.xml` (catch-all → `lua aicc_inbound.lua`) and `default/05_aicc.xml` (`^(7\d{3})$` queue extensions → `lua aicc_queue.lua $1`; **internal extension dialing stays on the stock `Local_Extension`**).

**D6a dialplan — aicc owns a context of its own (owner directive, 2026-08-22)**: every SIP account `aicc_xml.lua` serves is placed in the **`aicc`** context (`user_context`), and **all aicc routing lives there**. Shipped as one standalone file, `freeswitch/conf/dialplan/aicc.xml`; the stock dialplan is not edited, and the include D6 put into `dialplan/default/` is withdrawn. Inbound keeps its doorway at `public/05_aicc.xml`, because that is the context a carrier gateway hands calls to and forcing a custom one on every trunk would be the larger intrusion; everything it delegates to runs in `aicc`. The four places that named `default` — `adapter.go` `TransferToExtension`, `outbound.go` click-to-dial, `aicc_queue.lua`, `aicc_inbound.lua` — now name `aicc`.

> **Why D6 was wrong.** Leaving extension-to-extension dialling on the stock `Local_Extension` works as telephony and fails as a call centre. Those legs carry no `aicc_*` variables, so the application sees an unattributed pair of channels, mints a call around them, and gets both direction and roles wrong — the agent *being rung* was shown "Calling out", their caller's leg, and an OUTBOUND badge on an internal call (owner-reported 2026-08-22, live). The same rule falls through to voicemail on no-answer, and each successor leg becomes its own CDR row with a voicemail greeting booked as a handled agent call (**C24**). Both are consequences of routing aicc calls through rules that were never about aicc, and both disappear by not doing that. **Agent-to-agent internal calls go through the aicc routing chain like everything else.**
>
> **`user_context` alone does not get a phone into this context — the profile does.** Setting
> it on the directory user is necessary but inert here, and three live attempts on 2026-08-22
> established why, in this order:
>
> 1. `user_context=aicc` alone changed nothing. A phone's INVITE takes the **profile's**
>    context, `public`, and `public.xml`'s stock `public_extensions` rule transfers `10xx` to
>    `XML default` before anything of ours is reached. **A FreeSWITCH restart made no
>    difference**, which ruled caching out — the path is structural.
> 2. Ordering cannot win it. Our include sits at `public.xml:61`, *below* the stock rule at
>    `:46`, and an include's position is fixed in the file rather than by filename.
> 3. `internal_auth_calls=true` did not fix it either. `apply-inbound-acl="domains"` admits LAN
>    devices without a challenge regardless, so the directory is still never consulted.
>    (It was turned on anyway, on the owner's instruction: unauthenticated INVITEs from anything
>    on the LAN is not a posture a call centre should ship. It is hardening, **not** the fix.)
>
> What works is **`sip_profiles/internal.xml` → `<param name="context" value="aicc"/>`**: calls
> start in aicc and never pass through `public` at all. `user_context=aicc` stays set so both
> mechanisms agree wherever the directory *is* consulted. Inbound from a carrier keeps its
> doorway in `public`, because that profile is a different one and untrusted inbound belongs
> there.

> Nothing from the stock context is inherited. Feature codes an agent phone never uses — call pickup, park, redial, the voicemail keys — are absent by choice; anything an agent genuinely needs is added to `aicc.xml` explicitly. Deployment-specific trunk rules (this laptop's `pstn_sim`, a real carrier elsewhere) are added to the same context by the deployment, not to `default`.

**D7 `autoload_configs/switch.conf.xml`**: `sessions-per-second 30` → `100` (outbound AI ramp headroom). `max-sessions 1000`, RTP range 16384–32768, `uuid-version 7` unchanged.

**D8 deploy step (documented, not a diff)**: copy `freeswitch/scripts/*.lua` → `/usr/local/freeswitch/scripts/`, create `aicc_lua` PG role, `reloadxml`, then **restart FreeSWITCH once**: binding an xml-handler requires mod_lua to load with the new config, and mod_lua is not unloadable, so `reload mod_lua` answers "Module is not unloadable" and leaves the binding inactive (verified on 1.11.1 — a database-only extension stays invisible to `user_exists` until the restart). Afterwards `reload mod_callcenter` picks up the database-rendered queues, and later script edits need no reload at all. ESL stays `127.0.0.1:18021` (documented; configurable in aicc).

No changes to: sofia profile ports/WS bindings (already correct), ACLs, event_socket, directory XML (fallback until cleanup).
