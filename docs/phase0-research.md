# Phase 0 Research Notes

Date: 2026-08-13 · Status: Phase 0 complete → feeds Phase 1 clarification
Scope: findings from the six reference sources, the live FreeSWITCH dev environment, pipecat PR #3859, and current OpenAI/Qwen realtime provider documentation. No design decisions are final here; open questions are listed in §8.

---

## 1. Sources reviewed

| Source | Role | One-line verdict |
|---|---|---|
| cti-server `/docs` + code | Domain model, call flow, event protocol | Near-complete blueprint for our telephony core (REST+SSE, ESL inbound, actor-per-call) — but never verified against live FreeSWITCH |
| golang-bot | AI-call SIP/RTP media path | `internal/voice/sip` (~1.9k lines + tests) is the port-as-is asset; provider layer does NOT transfer (it's a cascade bot) |
| java-bot | SIP bot comparison | Surprise: the best **realtime s2s provider reference** — live-verified `qwen-audio-3.0-realtime-plus` and `gpt-realtime-2.1` GA integrations |
| web-sip-phone | Agent softphone (integrate as-is) | Deliberately has **no external API**; coordination must be backend-driven (ESL → SSE), which its design doc anticipates |
| ui-test | Finalized UI to imitate | Complete visual system + screen inventory + domain types + bot-flow DSL; missing i18n and TanStack Query (both must be added fresh) |
| FreeSWITCH `/usr/local/freeswitch/conf` | Live dev switch | 1.11.1, running; ESL on **127.0.0.1:18021**; WS 5066/WSS 7443 live; `local6060` gateway exists; mod_lua+mod_pgsql loaded; slim build without mod_callcenter/conference/audio_fork |
| pipecat PR #3859 | Media-path design cross-check | **Closed unmerged** (redirected to community integration); design remains valid; reviewer findings are a checklist of pitfalls |
| OpenAI/Qwen realtime docs | Provider protocol facts | Both protocols confirmed same-shaped (OpenAI Realtime event model); details in §5 |

---

## 2. Domain concept inventory

From cti-server (runtime) + ui-test (product surface). Terminology we will keep:

**Runtime entities**
- **Call** — aggregate of Parties; stable `call_id` (UUIDv7, client-mintable for idempotency) across transfers; `type` inbound/outbound/internal; ANI/DNIS immutable; FSM `CREATED → RUNNING → ENDING → ENDED`.
- **Party** — one leg; `party_id` == FreeSWITCH channel UUID (pre-assigned via `origination_uuid`); FSM `DIALING|RINGING → TALKING ⇄ HELD → RELEASED`; first party is the unique originator.
- **Agent** — person; FSM `LOGGED_OUT → NOT_READY ⇄ READY`; NOT_READY carries a reason (`LOGIN, BREAK, LUNCH, TRAINING, AFTER_CALL_WORK, SYSTEM, …`); ACW auto-timer per agent (`wrap_up_time_s`); RONA → `NOT_READY(SYSTEM)`. Free seating: agent↔DN binding happens at login, not in config.
- **DN / Extension** — registration FSM + usage FSM + observed `in_service` (OPTIONS ping / call-setup failure). `Available = registered ∧ in_service ∧ idle ∧ enabled`. Derived `availability` (one word for wallboards): `logged_out → on_call → wrap_up → not_ready → device_unreachable → ready`.
- **Treatment** — what a parked caller hears (`RINGBACK|MUSIC|SILENCE|ANNOUNCEMENT`); mandatory while parked (a parked channel emits no audio).
- **CDR** — a derived verdict emitted exactly once per call: `answered`, normative `missed_reason` (from recorded facts, never cause-string parsing), `agent_ids`, final `user_data`. Bot containment is derived, not stamped.
- **user_data** — business context on a call (RFC 7386 merge-patch, size-bounded, survives transfer, lands in CDR); the AI's collected slots ride here to the agent screen-pop.

**Catalog entities**
- **Trunk** — gateway + number plan (DID ranges → target), outbound caller-id/strip/prefix.
- **Extension** — `number, kind: agent|bot|plain, auth (write-only password), register policy`.
- **Queue / RoutePoint** — parks the caller and asks the router; `target, timeout_s, no_answer_timeout_s, park_treatment, overflow DN`. cti-server proved "route point *is* the queue" works; ui-test adds product-level queue config (priority, ring strategy, SLA threshold, business hours, failover).
- **Bot Flow (spec v1)** — from ui-test `flows/*.json` ≡ java-bot `FlowSpec`: bilingual persona/rules, nodes (instruction + allowed tools + `tool:`-keyed transitions + terminal flags), declarative HTTP tools (path/body template, success predicate, result slot mapping), global fallback/max-turns. Five worked examples (NovaNet ISP domain).
- **ui-test additions** — Recording (+ 5-criterion quality score), AdminUser/roles (Agent/Supervisor/Admin + permission groups), AuditEntry (`dot.namespaced` actions), rich CDR (legs journey: trunk/dialing/bot/queue/agent, botSec/queueWaitSec/talkSec, transcript incl. function-call traces, SIP technical tab), reports (queue/agent/disposition), wallboard KPIs + service-level history.

---

## 3. Per-reference findings — what to borrow, what to drop

### 3.1 cti-server (highest-priority reference)

**Borrow**
- **Event contract wholesale** (`docs/tserver-protocol.md`): one ordered SSE stream; envelope `{version, seq, type, call_id, party_id, this_dn, other_dn, ani, dnis, call_type, time, payload, user_data}`; global `seq` via PostgreSQL hi/lo blocks (DB never on the per-event path); `Last-Event-ID` resume within an in-memory ring, `reset` event + collection-snapshot resync beyond it; slow consumers disconnected, never allowed to stall the publisher; **the HTTP response is the acknowledgement** — no EventACK/ref_id correlation; call events are self-sufficient for screen-pop (no follow-up GETs).
- **Concurrency architecture**: one actor goroutine per call (bounded mailbox, sole mutator, FSM transition tables), single dispatcher, ESL reader that only normalizes, router on its own goroutine deciding while actors execute; snapshot reads through the mailbox (no locks).
- **The three hard-won ESL rules**: (1) route execution = `uuid_transfer <uuid> 'bridge:{origination_uuid=…}<ep>' inline` — never `originate`+`uuid_bridge` (silently fails with RINGBACK treatment); (2) browser/WebRTC phones need `uuid_phone_event talk|hold` + `Call-Info: answer-after=0` auto-answer, not `uuid_answer`/`uuid_hold`; (3) never set `park_timeout` in dialplan — wait budget belongs to the router; parked channels need explicit treatments.
- UUIDv7 everywhere; `origination_uuid` pre-assignment so leg identity precedes the channel; client-minted `call_id` + CDR ledger = idempotent outbound ("a late retry must not redial the customer").
- Persistence split: agent state written synchronously in-request (`503 storage_down` over lying); call snapshots async; explicit degradation matrix (ESL down → 503 on call ops only; PG down → events keep flowing); advisory-lock single-instance guard; recovery via `show channels as json` reconcile (adopt/orphan/close), media never touched.
- `in_service` observation (OPTIONS ping + call-setup failure) + derived `availability` — directly targets our #1 agent failure mode (dead browser tab still "registered").
- CDR-as-derived-verdict semantics and `user_data` merge-patch with full-map events.
- Directory served live from PostgreSQL via **mod_lua xml-handler** (their `cti_directory.lua` is a working reference for our Lua deliverable).
- Config as validated immutable snapshot + atomic pointer swap; write-only SIP password columns.

**Discard**
- "No queue subsystem / longest-idle only / no groups" scope — right for 5 agents, wrong for 50. We need product-level queues (priority, ring strategy, SLA) per ui-test. Keep the escape-hatch *shape* (small selector function), not a queue engine port.
- External-bot-over-SIP-REFER machinery + `Refer-To` param context smuggling — our bot is in-process; handoff is an internal transfer with direct `user_data` writes.
- Single flat API bearer token — we need real per-user auth (SPA login), and SSE auth that works from `EventSource` (cookie/query-token) from day one.
- stdlib-only substitutions where our stack is fixed (ServeMux→chi, raw SQL→sqlc, Prometheus-only→OTel), hand-rolled ESL parser quirks.
- "No durable CDR store" stance — our product includes reporting; CDRs (and AI transcripts) persist in PostgreSQL from the start.

**Key caveat**: cti-server has **never run against live telephony**. Its sofia event-header mappings are documented hypotheses. Phase 3 must start with a live event-shape verification spike before building on them.

### 3.2 golang-bot (media-path primary reference)

**Borrow (port nearly as-is, with tests)** — `internal/voice/sip/`:
- Custom SIP UAS (no external SIP lib): parser with case-insensitive headers/compact forms/multi-Via, INVITE→100→SDP negotiate→200(+SDP)→ACK-timer flow, OPTIONS→200 (gateway keepalive), CANCEL/BYE, INFO DTMF; X-header extraction for call metadata.
- RTP: pion/rtp marshal; per-call socket; even/odd RTP/RTCP port pairing; 20ms `time.Ticker` pacing with late-tick resync (no catch-up bursts); TX prebuffer (3 frames = 60ms) + 2-tick underrun grace; wrap-safe reorder-only jitter buffer (4-frame window, ≥25-frame gap = resync without silence flood, codec-correct silence fill 0xFF/0xD5); SSRC/seq/ts randomization + SSRC collision regeneration + regen on remote address change; unknown PT ignored; RFC 2833 receive (end-bit dedup by timestamp) merged with SIP INFO into one DTMF queue; RTP dead timeout (5s default) circuit breaker; optional minimal RTCP SR/RR (catches one-way-dead audio).
- G.711 µ-law + A-law LUT codecs (process-wide singletons); advertise-IP selection via UDP route-probe **toward the peer's SDP media IP** (fixes a real one-way-audio failure).
- Barge-in flush idiom: `ClearTx()` drains TX queue non-blocking + provider-side cancel — since we terminate RTP ourselves, only ~2 frames remain in flight to FreeSWITCH; nothing needs to be sent to clear FS-side audio (~40–60ms to silence).
- Generation-counter timers (race-free no-input timers without lock-across-callback), echo-substring filter gated by play state, TTS stall watchdog pattern (progress deadline + length-derived overall deadline + "got audio → force-complete"), per-call end-of-call media-health log line + latency metric rings.

**Discard / rewrite**
- **Entire provider layer** (Doubao STT/TTS + Qwen text LLM = cascade; explicitly out of scope for phase 1). Keep only patterns: per-call WS ownership, single-writer mutex, watchdog, cancel-ack race guard.
- Known SIP gaps to fix in the port: no 200-OK retransmission until ACK (lost ACK kills the call), CANCEL's 487 carries wrong CSeq method, spurious BYE after remote BYE, `MaxCalls` default 100 (we need 200+, configurable), no `a=ptime:20` in SDP answer (pin it), INVITE retransmission dropped silently instead of idempotent re-200.
- Hot-path allocation churn (~5–8 small allocs per direction per 20ms frame; ~100–200k allocs/s at 200 calls) — add `sync.Pool`/reuse and a pre-encoded silence frame.
- Resampler is linear-up/moving-average-down (integer factors) — acceptable for 8k↔16k; weak anti-aliasing for 24k→8k (see java-bot's windowed-sinc).
- Its WS clients have no ping/pong, no read deadlines — must not be inherited for long-lived realtime sessions.

**Licensing**: `codecs.go` header credits pipecat PR #3859 (BSD 2-Clause, Daily). *Resolved 2026-08-13: the PR is owner-authored, so no BSD attribution is required in the new repo; the header is not carried over (see 00-overview §5).*

### 3.3 java-bot (comparison → became the s2s provider reference)

**Borrow**
- **Provider abstraction proven live**: one `RealtimeProvider` interface; one `OpenAiCompatRealtimeProvider` parameterized by a `Profile` record (endpoint/model/voice env, `SessionStyle` GA|BETA, `ToolStyle`, in/out sample rates) covering OpenAI/StepFun/Grok; a bespoke Qwen client only for DashScope quirks (e.g., rejected `session.update` fields stripped and resent once). `RealtimeEvents.mapCommon()` folds GA/beta event-name variants into one internal enum.
- Live-verified facts: `qwen-audio-3.0-realtime-plus` (its default) and `gpt-realtime-2.1` GA protocol both work over this shape; smart_turn tradeoff documented from real calls (backchannel-safe but slow onset — short commands can be swallowed as `turn_invalid`).
- Barge-in: local play-queue + RTP TX flush on `SPEECH_STARTED`, with a **backstop flush on `RESPONSE_INTERRUPTED`** in case `SPEECH_STARTED` never arrives.
- **FSM-over-realtime-model steering** (`hint` pattern): flow nodes constrain tools; when a transition fires, the tool result's `hint` field carries the next node's instruction; out-of-phase tool calls return `ok:0` + current phase instruction. Keeps the model on rails without fighting its turn-taking. Same flow DSL as ui-test `flows/*.json`.
- Preflight self-check (connect + `session.update` accepted + one greeting + one function-call round-trip) as a deploy gate; windowed-sinc downsampling for 16k/24k→8k (avoids "muffled/metallic" output); DTMF-during-response race fix (`response.cancel` + deferred inject, never block the actor).
- Real-world warning: line echo (~3700 RMS) defeated both energy thresholds and server VAD → they built a PBFDAF AEC. We should not need AEC in phase 1 (our RTP leg is FS-bridged, no acoustic loop), but the echo-vs-barge-in interaction is a design topic.

**Avoid**
- No WS reconnect/resume (drop = hangup), no session-cap mitigation (300s Qwen limit unhandled), fake transfer (speaks a script and hangs up), zero metrics export.

### 3.4 web-sip-phone (integrate as-is)

**Findings that shape our design**
- MV3; SIP.js runs in an **offscreen document** (single UA/registration/session); SIP.js is a **pinned personal fork** implementing BroadSoft remote-control (NOTIFY `Event: talk|hold` ⇒ works with `uuid_phone_event`); auto-answer via `Call-Info: ;answer-after=N`.
- **No external integration surface exists — by explicit design** (design.md §20/§21: no CTI API, no page messaging, no local answer/hangup UI, no notifications). A second concurrent INVITE gets `486 Busy`. The extension is an audio endpoint + status dot; "the agent softphone bar lives on the business page" (i.e., our SPA).
- Therefore the coordination model is: **FreeSWITCH is the call-control authority; Go app drives via ESL (`uuid_phone_event talk/hold`, auto-answer originate vars); SPA renders ringing/answer state from SSE**; the extension is never talked to programmatically. ui-test's `softphone-provider.tsx` already models exactly this contract.
- Constraints: WSS URL is derived as `wss://<domain>/` (port 443, path `/`); CA-trusted cert required (Chrome refuses self-signed in extension context); registration is gated on an Allow-Site tab being open (our SPA's origin must be an Allow Site). Server must never send two concurrent INVITEs to one extension.

**Discard**: nothing to port (it's a black box we integrate). We will NOT modify it in phase 1 unless the user decides otherwise (§8 Q-R4).

### 3.5 ui-test (visual contract)

**Imitate (hard rules)**
- Tokens: 13px base type scale (12/13/14/16/20), single 6px radius, single accent `#4F46E5`, borders-not-shadows, no colored card fills; **7 semantic state colors** (available/oncall/ringing/acw/aux/offline/breach) used only as dots/pills/thin bars; `tabular-nums` on every number; Inter font.
- Shell: 220px sidebar (grouped nav, uppercase labels) + 48px breadcrumb topbar (fixed link targets, never history-back) + role-scoped nav.
- Patterns: 36px table rows w/ uppercase 12px headers; hand-rolled `rounded-md border bg-card p-4` cards; Sheet drawers (right, 380–560px) for edit/inspect; Dialog only for short creates; Popover-confirm for light destructive actions; custom "line" Tabs variant.
- Screens to rebuild faithfully: **Agent Cockpit** (3-column: ringing banner/active call/queue · contact/live transcript/history tabs · wrap-up/stats), Supervisor Wallboard/Agents/Queues/Quality, Admin Users/Routing/Bots + **Flow Designer** (JSON editor + SVG graph + node inspector + validation), Trunks, **CDR explorer** (journey stacked bar, transcript with function-call traces, technical tab), Reports, Audit.
- Components to build first: `PageHeader`, `KpiCard`, softphone-bar (draggable, persisted position), waveform player.
- Its `lib/mock.ts` fetch functions + types are effectively the intended API surface; `flows/*.json` is the bot-flow DSL source of truth.

**Not present (we add fresh)**: i18n (no library, all strings hardcoded EN — only flow *content* is bilingual `{zh,en}`), TanStack Query (loaders + mock only), login/auth screens (role is a `<Select>` stand-in), dark mode (scaffolded, unimplemented), SSE plumbing (all "realtime" is `setInterval` simulation), agent sub-pages (calls/contacts/voicemail/schedule/preferences are placeholders), responsive layouts, tests.

### 3.6 pipecat PR #3859 (design cross-check)

- **Status: closed 2026-07-13 without merging** — maintainers redirected it toward community-integration packaging; never technically rejected. golang-bot is the Go embodiment and was field-tested; treat the PR as design provenance + reviewer checklist.
- Confirms golang-bot's mechanisms (pacing accumulator with >40ms resync; prebuffer 3 frames; rx drop-oldest / tx blocking backpressure; OPTIONS 200; 5s dead timeout; DTMF end-bit dedup).
- Reviewer/self-found pitfalls to design around: **half-wired codec support** (PCMA declared but never negotiated/encoded — "all-or-nothing per codec, at every layer, or don't declare it"); unknown-PT and SSRC-collision RFC MUSTs initially missed (do a deliberate RFC 3550 §5.1 checklist pass); DTMF PT hardcoded 101 instead of negotiated from SDP; INVITE retransmissions dropped instead of idempotently re-answered; CANCEL defined but not dispatched (golang-bot does handle CANCEL); case-sensitive header parsing (golang-bot fixed); no RTCP = liveness risk if any peer expects it (golang-bot added minimal RTCP).
- Validated architecture pattern we keep: **dial-out stays in FreeSWITCH** — originate the external leg, bridge to the bot only after answer; AI resources are never spent on unanswered calls.

---

## 4. Live FreeSWITCH environment (must stay compatible)

| Item | Current value | Notes |
|---|---|---|
| Version / state | 1.11.1-release (built 2026-05-26), running, `-nonat`, macOS | Clock-skew CRITs on Mac sleep — keep box awake during soak tests |
| Domain / IP | `192.168.31.248` hardcoded in vars.xml | Drives realm, rtp/sip-ip, gateway proxy |
| **ESL** | **127.0.0.1:18021**, pw `ClueCon`, ACL `lan` | Non-stock port — app config must default to this env |
| Internal profile | 5060; **ws :5066 and wss :7443 both live**; `auth-calls=false` + ACL `domains`; `inbound-late-negotiation=true` | A SIP.js-signature client has registered over WSS already |
| Codecs | `global_codec_prefs=OPUS,G722,PCMU,PCMA8` | **`PCMA8` is a typo** → PCMA effectively absent; agent legs prefer OPUS → FS transcodes OPUS↔PCMU on agent↔bot bridges |
| Gateway | `local6060`: `register=false`, proxy `sip:192.168.31.248:6060` | Exactly the planned bot-gateway shape; its `expire-seconds` is inert (`ping` param absent). Port 6060 was the old Python bot's |
| Existing AI routing | `8888` → `record_session` + bridge to gateway; `95013/95015` → direct `bridge sofia/internal/<user>@127.0.0.1:6060` with `absolute_codec_string=PCMU` + `sip_h_X-*` headers | Both patterns proven live; X-headers carry entry/DNIS/caller |
| Directory | Stock static XML users 1000–1019, pw `aicc@123`, `user_context=default` | No dynamic directory yet |
| mod_lua / mod_pgsql | **Both loaded**; `lua.conf.xml` xml-handler params present but commented; `scripts/` empty | `freeswitch.Dbh("pgsql://…")` works without ODBC — clean path for PG-backed directory/dialplan |
| Missing binaries | mod_callcenter, mod_conference, mod_xml_curl, mod_audio_fork/stream, mod_av, mod_shout, mod_spandsp | Slim custom build — queueing must be app-side (matches plan); supervisor listen-in only via dptools `eavesdrop` |
| Recording | `record_session` proven (243 wavs, stereo) to `/usr/local/freeswitch/recordings` | mod_sndfile WAV; works today |
| Limits | `max-sessions 1000`, `sessions-per-second 30`, RTP ports 16384–32768 (defaults), `uuid-version 7` | sps=30 may throttle bursty AI outbound ramps; uuid v7 already set (cti-server requirement) |
| Cruft | a stale custom-TTS module `<load>` line whose .so was removed (CRIT at boot); `159999 → socket 127.0.0.1:8084` ESL-outbound routes; `9999` speak route broken | Cleanup candidates — proposed as diffs in Phase 2 |

**Compatibility verdict**: (a) agent-over-WS path ready; (b) bot gateway pattern ready (decide port 6060 reuse vs new); (c) Lua→PG groundwork loaded but unwired; (d) recording proven; (e) queueing correctly forced into our app; (f) capacity headroom fine (250 calls ≈ 500 channels < 1000 max-sessions; RTP range ~8k pairs). Our bot's RTP range must avoid FS's 16384–32768 when co-located (golang-bot's default 10000–20000 overlaps it — change in port).

---

## 5. Realtime provider facts (as of 2026-08-13)

| Aspect | OpenAI Realtime | Qwen-Audio 3.0 Realtime |
|---|---|---|
| Models | `gpt-realtime-2.1` (flagship), `gpt-realtime-2.1-mini` | `qwen-audio-3.0-realtime-plus` / `-flash` (no public price/latency delta; console-gated) |
| Endpoint | `wss://api.openai.com/v1/realtime` (+ WebRTC, + native SIP `sip.api.openai.com`) | `wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime?model=…` (Beijing; Singapore exists) |
| Audio in | GA naming `audio/pcm` @24k; **`audio/pcmu` confirmed** (`audio/pcma` unconfirmed) | 16kHz PCM16 mono |
| Audio out | `audio/pcm` @24k (pcmu out likely, unconfirmed) | 24kHz PCM16 mono |
| Turn detection | `server_vad` / `semantic_vad` / null; flags `interrupt_response`, `create_response` | `server_vad` (threshold [-1,1] def 0.5; silence 200–6000ms def 800) / **`smart_turn`** (acoustic+semantic; an "mm-hm" backchannel does not interrupt) / null. **Settable only before first audio** — mode switch = reconnect |
| Barge-in | `speech_started` → server auto-cancels (`response.cancelled`) → client `conversation.item.truncate {audio_end_ms}`; manual: `response.cancel` + `output_audio_buffer.clear` | `speech_started` → client sends `response.cancel` → `response.done{status:cancelled, reason:turn_detected\|client_cancelled}` |
| Function calling | Same shape both: `tools` in `session.update` → `response.function_call_arguments.delta/.done` → item in `response.done` → client `conversation.item.create{function_call_output}` → `response.create`. Qwen excludes tool content from TTS | ← |
| Transcripts | `response.output_audio_transcript.delta/done`; input transcription configurable | input `conversation.item.input_audio_transcription.delta/completed`; output `response.audio_transcript.delta/done`; smart_turn-only `ambient_audio_transcription.*` |
| Session limits | **60 min max session** | **50 audio turns / 300s cumulative audio**; `max_history_turns` 1–50 (def 20); **older history silently discarded** (no forced disconnect) |
| Pricing (audio) | 2.1: $32/M in + $64/M out tokens ≈ **$0.019/min in + $0.077/min out**; mini ≈ $0.006 + $0.024 | Not published |

**Implications**
- One provider interface fits both: the protocols are same-shaped; differences are (a) who sends the cancel on barge-in, (b) truncate vs done-cancelled bookkeeping, (c) audio formats, (d) session-limit models. java-bot's `Profile` + event-fold pattern is the template. Interruption semantics must be normalized at the `VoiceSession` interface (single `Interrupted` event; local TX flush is ours in all cases).
- **OpenAI g711 passthrough**: if `audio/pcmu` works both directions, the OpenAI path can skip resampling entirely (8k µ-law straight through) — major CPU/bandwidth win; verify early, and verify `audio/pcma`.
- **Qwen path resampling**: up 8k→16k (linear OK), down 24k→8k (needs decent anti-aliasing — windowed-sinc per java-bot).
- **Qwen long calls**: server silently drops old history past 50 turns/300s audio — the session survives, but memory fades. Strategy topic for Phase 2: pin durable facts via `instructions`/slots in our app rather than trusting conversation history; decide max-AI-call policy (also OpenAI 60-min cap).
- Qwen quirks to handle: `turn_detection` frozen after first audio (choose smart_turn/server_vad per entry point pre-call); DashScope may reject unknown `session.update` fields (strip-and-retry pattern).

---

## 6. Cross-cutting adoptions proposed (to be confirmed in the Phase 2 design)

1. **Telephony core** = cti-server architecture: ESL inbound + actor-per-call + FSM tables + single ordered SSE stream with the tserver envelope/resume semantics (modernized: chi, sqlc, OTel, real user auth, per-agent dynamic SSE filters — the one thing cti-server couldn't retrofit).
2. **AI media leg** = golang-bot `internal/voice/sip` ported (with the §3.2 fix list), UAS-only behind a `register=false` gateway, PCMU 20ms, X-headers for correlation (`sip_h_X-*` in the bridge string).
3. **Provider layer** = java-bot's shape in Go: `VoiceSession` interface (bidirectional audio, events, function calling, interrupt) + profile-parameterized OpenAI-compatible client + Qwen quirks client. Cascade (ASR+LLM+TTS) later = another `VoiceSession` implementation; no cascade code in phase 1.
4. **Bot configuration** = flow DSL v1 (ui-test `flows/*.json` ≡ java-bot FlowSpec) + hint-steering engine — pending scope confirmation (§8).
5. **Directory/dialplan from PostgreSQL** via mod_lua xml-handler (`cti_directory.lua` as reference), schema owned by Go migrations, Lua read-only, with per-lookup query + short TTL cache decision documented.
6. **UI** = rebuild ui-test's system in our stack (React 19 + TanStack Router/Query + Tailwind 4 + shadcn radix-nova) + react-i18next keys from day one (en default / zh).
7. **Recording** = FS `record_session` (stereo) at bridge time; storage abstraction local-FS/S3 (metadata in PG).

## 7. Risks & Phase 2 design topics

- **cti-server's ESL mappings are unverified** → Phase 3 opens with a live event-shape verification spike (register, park, bridge, transfer, hangup headers) before the core hardens.
- **Audio bridge paths** (per provider, both directions): resample choices, allocation pooling, pre-encoded silence, prebuffer sizing; OpenAI pcmu-passthrough verification.
- **Barge-in unification** across providers + FS-side behavior (local TX flush is sufficient — we terminate RTP; ~40–60ms residual).
- **Qwen context limits** + OpenAI 60-min cap → long-call policy (fact pinning, proactive wrap-up/transfer).
- **Capacity sketch @200 AI calls** (full budget table due in Phase 2): ~10–15 goroutines/call (≈2.5–3k total); ~0.3–1MB/call working set (≈100–200MB) + alloc churn to pool; RTP+RTCP = 400 UDP ports; LAN RTP ≈ 32 Mbps; provider WSS worst-case all-Qwen ≈ 0.9 Mbps/call ≈ **~170 Mbps uplink** (OpenAI pcmu passthrough drops its share ~5×) — uplink is a real sizing item; CPU dominated by resampling + base64 + JSON of audio deltas (all cheap per-unit; churn is the risk). FS machine: 250 bridged channels, OPUS↔PCMU transcode on agent legs unless pinned.
- **Licensing**: *Resolved 2026-08-13 — owner confirmed sole authorship of all reference repos and the pipecat PR.* Apache-2.0 applies cleanly; NOTICE minimal; no external attribution for ported reference code (00-overview §5). Residual check: authorship must also mean ownership (no employment/client copyright assignment covering that code).
- **web-sip-phone WSS**: extension derives `wss://<domain>/:443` and needs a CA-trusted cert — how agents connect in this dev env needs confirming (§8).

## 8. Open questions → Phase 1 (asked in rounds)

- **R1 Product scope**: MVP feature set (outbound AI / supervisor live-ops / quality review / reports); bot config model (flow DSL vs prompt+tools); traditional IVR yes/no; no-agent-available behavior.
- **R2 Telephony**: bot SIP stack choice; PCMU-only vs +PCMA; bot port 6060 reuse & old-bot routes fate; trunk reality in dev + assumed codecs.
- **R3 AI providers**: Qwen plus vs flash default; latency budget; `transfer_to_agent` contract richness; language→provider routing basis.
- **R4 Agent side & auth**: extension coordination model confirmation; dev WSS/cert story; auth method (roles assumed Agent/Supervisor/Admin per ui-test).
- **R5 Recording & storage**: scope; transcript persistence; S3 implementation; project name.
- **R6 Open-source packaging**: docs language; demo seed data. (docker-compose demo itself is already mandated.)
