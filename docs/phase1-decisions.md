# Phase 1 — Confirmed Requirements（需求确认记录）

Date: 2026-08-13. Every item below was explicitly confirmed in the Phase 1 clarification rounds; the Phase 2 design binds to this register. Changes require re-confirmation.

## Product scope (MVP)

| # | Decision |
|---|---|
| P1 | **Core loop**: inbound AI answering (flow-driven) → function-call transfer to human queues → agent cockpit (live transcript, wrap-up) → recording → CDR explorer. |
| P2 | **All extended features in MVP**: supervisor live-ops (wallboard, agent roster + force-logout, queue staffing, monitor/whisper/barge via `eavesdrop`), outbound AI calls (API/manually triggered; no campaign manager), quality review (playback + 5-criterion scoring), reports (queue/agent/disposition) + audit log. |
| P3 | **Bot configuration = Flow DSL v1** (ui-test `flows/*.json` ≡ java-bot FlowSpec): bilingual persona/rules, nodes + allowed tools + declarative transitions, HTTP tool specs; Flow Designer UI; hint-steering engine in Go. |
| P4 | **No traditional DTMF IVR** — AI is the IVR. DTMF still reaches the bot (RFC 2833) and flows may route on it. **Amended 2026-08-13 (owner)**: every external number answers with a bot flow; a DID has no direct-to-queue alternative. Queues are reached only by the bot's `transfer_to_agent`, or by the DID's `fallbackQueue` when the bot cannot run at all (provider outage, capacity). **Out-of-hours is handled by the bot in conversation** ("we're closed, I can take a message"), not by a dialplan branch: business hours stay on the queue and a refused transfer steers the flow (02 §6). |
| P5 | **No-agent overflow is per-queue config**: AI takes a message → callback record (default) / announce + hangup / overflow to another queue or number. No voicemail boxes. |

## Telephony

| # | Decision |
|---|---|
| T1 | **Queues run on FreeSWITCH mod_callcenter** (user directive, superseding app-side routing). Division of labor: mod_callcenter owns distribution/MOH/waiting; the Go app owns agent identity & state UI (synced via `callcenter_config`), SSE events, reporting off `callcenter::info` events, and overflow handling after `cc_cause` exits. |
| T2 | **mod_callcenter availability**: user rebuilds the local slim FS 1.11.1 to add the module *(done 2026-08-13: `mod_callcenter.so` installed and runtime-loaded; `modules.conf.xml` enable ships in design diff D1)*; the docker-compose demo uses a standard FS image (module included). |
| T3 | **Queue config path**: queue definitions rendered from PostgreSQL by a Lua `configuration`-section xml-handler; agents/tiers managed by Go via ESL `callcenter_config`; mod_callcenter `odbc-dsn` pointed at PG (pgsql:// support verified in Phase 3 spike; PG stays authoritative via Go sync regardless). |
| T4 | **Bot SIP stack**: port golang-bot `internal/voice/sip` (+tests) with the research fix list (200-OK retransmission until ACK, `a=ptime:20`, CANCEL-487 CSeq fix, no spurious BYE, MaxCalls ≥ 200 configurable, buffer pooling, RTP port range off FS's 16384–32768). |
| T5 | **Codecs: PCMU + PCMA both, fully wired at every layer** (negotiation → SDP answer → payload type → codec tables). |
| T6 | **Bot endpoint takes over port 6060** and the existing `local6060` gateway; old Python bot and its `95013/95015/8888/159999` routes retired (archived in the design diff). |
| T7 | **No real trunk in dev**; product ships generic SIP trunk config (register or IP-auth, PCMU/PCMA; Opus off on trunks). Real trunk wiring is a deployment-doc topic. |

## AI providers

| # | Decision |
|---|---|
| A1 | **Revised 2026-08-16 — the provider is a deployment-level setting, not a routing rule.** One provider is active per deployment, chosen at startup (`AICC_PROVIDER`, with `AICC_PROVIDER_ENDPOINT` / `AICC_PROVIDER_MODEL`): **Qwen** (`qwen-audio-3.0-realtime-plus`, `-flash` selectable) inside mainland China, **OpenAI** (`gpt-realtime-2.1`) elsewhere — the two are not reachable from the same network with acceptable latency, so this is a property of where the deployment runs. A DID's `language` governs greeting, prompt language and voice **only**; it never selects a provider, and the language→provider mapping is removed. **Verified live 2026-08-16 (owner, real call):** Qwen handles English well, so a mainland deployment serving English DIDs needs no OpenAI fallback. |
| A2 | **Language determined by per-DID/entry-point config** — it shapes the conversation, not the routing (A1). No mid-call provider switching (Qwen turn_detection freezes after first audio; a switch would mean re-bridging), and with one provider per deployment there is nothing to switch to. |
| A3 | **Latency budget: ≤1.2 s p50 / ≤2 s p95** caller-stops-speaking → bot-audio-at-caller, with per-hop allocation in the design. |
| A4 | **transfer_to_agent = rich contract**: target queue, reason category, conversation summary, collected slots → written to call `user_data` → agent screen-pop + CDR; bot speaks a bridge line during transfer. |
| A5 | Turn detection: Qwen `smart_turn` preferred (evaluated vs `server_vad` in Phase 3), OpenAI `semantic_vad`/`server_vad`; semantics normalized at the VoiceSession interface. |
| A6 | **Phase-2 extension strategy (open/closed): the cascaded pipeline (ASR + LLM + TTS) never enters this application.** Cascade is delivered by a separate **OpenAI Realtime Gateway** service that exposes the OpenAI Realtime protocol outward (WebSocket + the same event set) and orchestrates ASR/LLM/TTS inside. Building on the revised A1, the gateway is **another value of `AICC_PROVIDER`**, carrying its own endpoint, model and session dialect — it impersonates no vendor and does not borrow OpenAI's slot or credential. `internal/provider` stays one client for one wire protocol; adding the gateway is adding a profile, never a second client. Consequences that bind this repo permanently: no ASR/LLM/TTS types, interfaces, adapters, placeholders or TODOs; `VoiceSession` is a seam for the call actor and its test double, not a plug-in point for other engine kinds; a "cascade-shaped" symbol anywhere in the tree is drift by definition. The third value shipped on 2026-09-02 as `AICC_PROVIDER=gateway`, on the owner's instruction and verified on live calls; everything above it in this row is unchanged by that. (Owner directive, 2026-08-16; profile added 2026-09-02.) |
| A7 | **Voice (音色) is bot configuration, not deployment configuration**: `global.voice` in the flow spec, stored in the database with the rest of the bot and published with it — never an env var. It is not per language: both OpenAI Realtime and Qwen-Audio Realtime offer voices that carry Chinese and English well, so a bilingual bot keeps one voice. Voice names belong to the provider that answers, and a deployment runs one (A1), so a flow names a voice its own deployment offers; empty falls back to the profile's default. (Owner directive, 2026-08-16.) |

## Agent side & auth

| # | Decision |
|---|---|
| G1 | **web-sip-phone integrated as-is; coordination is backend-driven**: ESL observes/controls (auto-answer `Call-Info: answer-after`, `uuid_phone_event talk/hold`), SSE feeds the SPA softphone bar + incoming-call popup. The extension is never messaged programmatically; SPA origin must be in its Allow Sites. |
| G2 | **Dev topology (documented reference setup)**: external Caddy (`~/workspaces/github/proxy/Caddyfile`, outside this repo): `app.aicc.test`→Vite :5173, `api.aicc.test`→Go :8080, `ws.aicc.test`→FS wss :7443 (TLS-to-TLS with skip_verify; sofia requires Via transport = socket transport). The repo itself stays HTTP-only. |
| G3 | **Auth: local accounts** (argon2id in PG) + HTTP-only cookie session (SSE-friendly); admin-driven password reset; OIDC is roadmap. **Roles: Agent / Supervisor / Admin** with ui-test's permission groups. |

## Recording, storage, data

| # | Decision |
|---|---|
| R1 | **Record all calls by default, per-queue/flow off-switch**; one continuous stereo file per call (`record_session` + `recording_follow_transfer`, the pattern proven on this FS). |
| R2 | **AI transcripts persisted to PostgreSQL** (input + output + function-call traces) → live agent view, CDR transcript tab. No human-leg ASR in phase 1. Retention configurable. |
| R3 | **Storage abstraction: local filesystem (default) / any S3-compatible endpoint (config-switchable, prod)**. The docker-compose demo uses the local-filesystem backend — no MinIO bundled (owner directive 2026-08-13: keep the single-executable philosophy clean). |

## Open-source packaging

| # | Decision |
|---|---|
| O1 | **Name: `ai-native-callcenter`** (UI wordmark may use "AICC"). License Apache-2.0. Owner confirmed (2026-08-13) sole authorship of all reference code including the pipecat PR → no third-party attribution needed for ported code; NOTICE minimal; SPDX headers on source files. |
| O2 | **Docs in English + Chinese README/quick-start mirror** (`README.zh-CN.md`). UI i18n en/zh regardless. |
| O3 | **docker-compose demo ships seeded data**: demo accounts (admin/supervisor/agent), extensions, two queues, one English + one Chinese flow, small synthetic CDR/report history; reset flag for empty start. |

## Engineering conventions (post-round directives, 2026-08-13)

| # | Decision |
|---|---|
| E1 | **Mandatory naming conventions** across JSON/Go/TS/DB with fixed cross-layer mapping (Go `CallID` ↔ JSON `callId` ↔ TS `callId` ↔ DB `call_id`), SCREAMING_SNAKE enum values byte-identical across JSON/TS/DB, unit-suffixed durations (`xxxMs`/`xxxSec`), `is`/`has` booleans, `xxxAt` timestamps, forbidden filler words and synonym drift — codified in [design/07-naming.md](design/07-naming.md). Its §7 boundary rulings (lowercase BCP 47 locale codes, snake_case LLM tool names, Flow DSL re-keyed to camelCase as v2) are flagged there for owner veto. |
| E2 | **CallType borrows Genesys**: `INBOUND` / `OUTBOUND` / `CONSULT` / `INTERNAL` — caller-perspective, stamped at call creation, immutable across transfers, envelope-level on every call event. MVP emits the first three; `CONSULT` is reserved in the contract for the consult-transfer roadmap. |
| E3 | **Call/Party separation in the event model**: a Call aggregates Parties; leg lifecycle is `PARTY_*` (`PARTY_DIALING/RINGING/ESTABLISHED/HELD/RETRIEVED/RELEASED`, `PARTY_CHANGED` on transfer replacement, `PARTY_DTMF`), one event per leg with `partyId` set; call-scoped events stay `CALL_*` (`CALL_USER_DATA`, `CALL_RECORDING_*`, `CALL_CDR`). `ESTABLISHED` replaces `ANSWERED` (Genesys naming). |
| E4 | **`userData` vs `switchData` separation** (Genesys user-data / Extensions lineage): `userData` = business data (e.g. `ticketId`) — merge-patch, survives transfers, lands in CDR. `switchData` = the narrow Genesys-Extensions analog (switch-specific request info not describable by other parameters — e.g. special `sip_h_*` headers via `sipHeaders`; ordinary things like origination caller id are first-class parameters (`callerIdNumber`), never switchData) — **scope limited by owner directive to `POST /calls` (RequestMakeCall analog) and Lua leg-creation scripts; nowhere else** (not on events/envelope/CDR — release causes are event payload fields; the CDR technical jsonb stays `tech`). Named `switchData` (not "extensions") because `extension` means phone extension — naming flagged in 07 §7. |

## Standing constraints carried from prompts.md (not re-asked)

Single tenant; Go 1.26 / PG 18 / chi / pgx / sqlc / slog / OTel; React 19 / TS / Vite / TanStack Router+Query / Tailwind 4 / shadcn; REST + SSE only (no app WebSocket); no extra middleware (no Redis/MQ); go:embed SPA; ESL inbound mode; speech-to-speech only, and cascade never in this repo (A6 — a Realtime Gateway service is the phase-2 path); FS config changes only via approved diffs; dev is HTTP-only; capacity target 200 AI calls + 50 agents on 8c16GB for the Go process.
