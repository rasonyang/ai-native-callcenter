# Design 00 — System Overview & Module Map

Binds to: [phase1-decisions.md](../phase1-decisions.md). Reading order: 00 → 01 (telephony) → 02 (AI voice) → 03 (data) → 04 (API/SSE) → 05 (frontend) → 06 (capacity) → 07 (naming — mandatory for every layer). Two evidence records sit alongside and **outrank the originals where they differ**: [m0-findings.md](m0-findings.md) (M0 live verification) and [m4-cleanup-findings.md](m4-cleanup-findings.md) (the M4 drift audit, and the A1/A6/A7 directives that landed with it).

## 1. System context

```
                       PSTN / SIP trunk (production only)
                                   │
                            ┌──────▼──────┐  SIP/RTP (WSS :7443 for agents)
  Agent browser ────────────►             ◄──────────────┐
  ├─ SPA (React, SSE) ──┐   │ FreeSWITCH  │              │
  └─ web-sip-phone ext ─┘   │  1.11.x     │   G.711 RTP  │ 20ms PCMU/PCMA
     (SIP.js, audio only)   │ mod_callcenter  ┌──────────▼─────────┐
                            │ mod_lua ────────►                    │
                            └──────▲──────┘   │   aicc (Go, one    │
                                   │ ESL      │   process, 8c16g)  │
                          inbound  │ :18021   │  ┌──────────────┐  │
                                   └──────────┤  │ SIP UAS :6060│  │
        PostgreSQL 18 ◄────────────────────────┤ │ RTP 40000-999│  │
        (schema owned by Go migrations;        │ └──────┬───────┘  │
         mod_lua reads views read-only)        └────────┼──────────┘
                                                        │ WSS — OpenAI Realtime protocol
                                                        │ (PCM/G.711 + JSON events)
                                        ┌───────────────┴───────────────┐
                                        │ ONE provider per deployment:  │
                                        │ Qwen-Audio 3.0 (mainland) or  │
                                        │ OpenAI gpt-realtime-2.1 (else)│
                                        │ … or any endpoint speaking    │
                                        │ the same protocol (phase 2:   │
                                        │ Realtime Gateway = ASR+LLM+TTS│
                                        │ behind it, outside this repo) │
                                        └───────────────────────────────┘
```

- **FreeSWITCH owns**: SIP registration (agents), trunks, bridging, mod_callcenter queues (waiting/MOH/distribution), recording capture, transcoding on non-bot legs.
- **aicc (Go) owns**: call control (ESL), agent/queue/flow/user configuration, the AI voice leg (SIP UAS + RTP + provider bridge), flow engine, SSE fan-out, CDR/transcripts/reports, recording storage/lifecycle, SPA.
- **PostgreSQL owns**: all durable state. mod_lua reads dedicated `luacc.*` views (contract, see 03).
- **The provider is chosen at startup, not per call (A1)**: `AICC_PROVIDER` picks qwen or openai for the whole deployment; a DID's `language` shapes greeting, prompt and voice only.
- **Outside this repo, by design (phase1-decisions A6)**: any cascaded speech pipeline. ASR + LLM + TTS compose inside a separate *OpenAI Realtime Gateway* service that exposes the Realtime protocol; aicc reaches it exactly as it reaches OpenAI — an endpoint override — and carries no cascade code, interface, stub or TODO.

## 2. Runtime topologies

**Production**: single `aicc` binary (SPA embedded via go:embed) + PostgreSQL + FreeSWITCH (+ S3-compatible store). TLS/reverse proxy is the deployer's concern (deployment doc).

**Dev (reference setup, documented as-is)**: `go run ./cmd/aicc` on :8080 (HTTP), `vite dev` on :5173 proxying `/api` → :8080; user-managed Caddy outside this repo maps `app.aicc.test`→5173, `api.aicc.test`→8080, `ws.aicc.test`→FS wss :7443 (TLS→TLS, skip_verify — sofia drops REGISTERs whose Via transport mismatches the socket). The repo itself never handles TLS. The SPA origin (`app.aicc.test`) must be in web-sip-phone's Allow Sites.

## 3. Module map (Go packages)

| Package | Responsibility | May import |
|---|---|---|
| `cmd/aicc` | wiring: config → store(migrate) → modules → listeners | everything |
| `internal/config` | env-based config (AICC_* vars), validation | — |
| `internal/obs` | slog setup, OTel init (traces+metrics), /metrics | config |
| `internal/store` | sqlc-generated queries, repositories, embedded goose migrations, tx helpers, seq hi/lo | config |
| `internal/events` | event envelope, global seq, in-memory ring, SSE hub (fan-out; delivery is scoped per subscriber by their agent identity, their staffed queues and a resolved `SeesEveryCall` capability — the hub is handed the answer, never a role or a scope name) | store |
| `internal/esl` | minimal ESL inbound client: auth, `event plain` subscribe, FIFO api replies, reconnect/backoff | — |
| `internal/media` | shared audio primitives: PCM16 frames, G.711 LUT codecs, resamplers, buffer pools | — |
| `internal/voice` | SIP UAS, SDP, RTP/RTCP, jitter buffer, DTMF (golang-bot port) | media |
| `internal/provider` | one OpenAI-Realtime client × `Profile` (`openai`, `qwen` are profiles, not clients); `VoiceSession` is the seam to `aicall` | media |
| `internal/mockprovider` | a stand-in Realtime **server** for load testing, reached through `AICC_PROVIDER_ENDPOINT` like a real one — not an in-process fake (`cmd/aicc-mockprovider`) | provider, media |
| `internal/loadgen` | the load generator's SIP UAC and run loop (`cmd/aicc-loadgen`) | media, voice |
| `internal/flow` | Flow DSL v1: schema, validation, engine (hint steering), HTTP tool runner | — |
| `internal/telephony` | FS adapter (command vocabulary), event normalization, call registry + per-call actors, call/party FSMs, callcenter sync, recording control, originate/transfer/eavesdrop ops | esl, store, events |
| `internal/agents` | agent state service (login/ready/ACW/RONA), device in_service observation | telephony, store, events |
| `internal/aicall` | AI session manager: voice leg + provider session + flow engine per call; barge-in & long-call policy; executes transfer_to_agent/take_message via telephony | voice, provider, flow, telephony, store, events, media |
| `internal/recording` | storage abstraction (fs / S3-compatible), key naming, retention job | store, config |
| `internal/api` | **generated** from `docs/openapi.json`: request/response types, `ServerInterface`, the routing wrapper, `OperationSecurityByRoute`, the scope vocabulary (`scopes.gen.go`). `DO NOT EDIT` | — |
| `internal/auth` | password hashing (argon2id), users, revocable cookie sessions, API keys (SHA-256 lookup, issue/revoke), `Role` and the `role → scopes` grant map | store |
| `internal/catalog` | the configuration a call centre runs on: extensions, queues, the numbers that reach them, who staffs what | — (its own repository interfaces) |
| `internal/outbound` | originating calls: an agent's click-to-dial and the AI's outbound leg, plus the per-DID rate limiter | catalog, telephony |
| `internal/transcript` | the order of a call's conversation — one actor serialising transcript rows and their events | store, events |
| `internal/transcribe` | speech recognition on a human leg (DashScope / OpenAI Realtime transcription) | — |
| `internal/streamin` | receives the audio the switch taps from an agent's leg and feeds it to transcription | telephony, transcribe, transcript, events, store, obs |
| `internal/webhook` | delivers finished calls to a customer's own system: subscriptions, signed delivery, retries, retention | store |
| `internal/seed` | `AICC_SEED=demo` fills an empty installation deterministically; `fresh` removes exactly that again | auth, store |
| `internal/httpapi` | chi router mounted through the generated wrapper, handlers, the two credentials (cookie sessions + `Authorization: Bearer` API keys) and the single scope middleware that applies the contract's `security`, CSRF, SSE endpoint, SPA embed/serving, `GET /openapi.json` | all services (never imported by anyone) |

Rules: dependencies point downward only (httpapi at top, store/media/esl at bottom); `internal/voice` and `internal/provider` never import `telephony` (they are driven by `aicall`); nothing imports `httpapi`; FS raw event names never escape `telephony` (normalization boundary, same rule as cti-server).

Non-Go deliverables: `freeswitch/` (Lua scripts, config diffs/templates, install notes — see 01), `web/` (SPA — see 05), `deploy/` (docker-compose demo + seed — see 03/06).

## 4. Third-party dependencies (approval list — per "no undiscussed deps")

Backend: `go-chi/chi/v5`, `jackc/pgx/v5` (mandated); `pressly/goose/v3` (migrations, embedded SQL — proposal); `pion/rtp` + `pion/rtcp` (RTP wire format; golang-bot lineage); `gorilla/websocket` (provider WS; golang-bot/java-bot lineage); `google/uuid` (UUIDv7); `golang.org/x/crypto` (argon2id); `minio-go/v7` (any-S3 *client* library, lighter than AWS SDK — no MinIO server ships anywhere; the demo uses the filesystem backend); OTel (`go.opentelemetry.io/otel` + SDK + OTLP exporters + prometheus metrics exporter). Build-time: sqlc, golangci-lint. Explicitly avoided: SIP libraries (custom UAS), Redis/MQ (mandate), heavyweight media stacks.

Frontend: react 19, @tanstack/react-router + react-query, tailwindcss 4, shadcn/ui (radix-nova style, consolidated radix-ui), lucide-react, recharts, @fontsource-variable/inter, i18next + react-i18next. Lint: oxlint (mirrors ui-test).

## 5. Cross-cutting conventions

- **IDs**: UUIDv7 everywhere; `call_id` client-mintable (idempotent originate); `party_id` = FS channel UUID via `origination_uuid` (FS `uuid-version 7` already set).
- **Errors**: API returns `{"error":{"code":"QUEUE_NOT_FOUND","message":"…","params":{…}}}`; codes are SCREAMING_SNAKE enum values and the i18n contract (04 §5); backend never localizes.
- **Naming**: [07-naming.md](07-naming.md) is normative for every identifier in every layer (Go `CallID` ↔ JSON `callId` ↔ TS `callId` ↔ DB `call_id`); CI-enforced.
- **Logging**: slog JSON, every log in a call context carries `call_id`/`party_id`/`agent_id`; trace_id correlated via OTel handler.
- **OTel**: spans on REST handlers, ESL commands, provider turns (see 02 §8), DB via pgx tracer; metrics listed in 06 §6.
- **Config**: env vars `AICC_*` with `.env` file support for dev; full table in the deployment doc. Key defaults for this dev env: `AICC_ESL_ADDR=127.0.0.1:18021`, `AICC_SIP_LISTEN=:6060`, `AICC_RTP_PORT_MIN/MAX=40000/40999`, `AICC_HTTP_ADDR=:8080`. Provider API keys env-only (never DB) — those are credentials *this* system presents to a vendor. The keys integrations present to *us* are the opposite direction and live in the `api_keys` table, issued and revoked through the API (04 §2, 03 §2).
- **Licensing**: Apache-2.0. All reference code (cti-server, golang-bot, java-bot, web-sip-phone, ui-test, and the pipecat PR #3859 transport) is owner-authored — confirmed 2026-08-13 — so the ported SIP/RTP code needs no third-party attribution; the old BSD header in golang-bot's `codecs.go` is not carried over. `NOTICE` stays minimal (project + copyright line). Source files carry `// SPDX-License-Identifier: Apache-2.0`. During the port, any line actually translated from pion's samplebuilder (vs merely modeled on its design) gets an MIT attribution comment — MIT/BSD deps are Apache-compatible and live as Go modules with their own license files; no GPL-family deps anywhere.

## 6. Phase 3 delivery milestones (each ends with a short report, per workflow)

- **M0 — verification spike** (before anything hardens): live ESL event-shape check (register/park/bridge/transfer/hangup headers vs our mapping), mod_callcenter `odbc-dsn` pgsql:// on 1.11.1, OpenAI `audio/pcmu`+`audio/pcma` bidirectional check, Lua `Dbh("pgsql://")` smoke. Findings amend 01/02 before M2/M3 build on them. **Done 2026-08-13 → [m0-findings.md](m0-findings.md).**
- **M1 — foundation**: config/obs, store+goose+sqlc, auth/sessions/users, SSE hub+envelope+ring, SPA shell (login, nav, i18n scaffold, tokens).
- **M2 — human telephony**: ESL link/adapter/normalization, call registry+actors+FSMs, agent service + callcenter sync + Lua directory/queue rendering (FS diffs D1–D8 applied), extensions/queues/DIDs admin, agent cockpit + softphone bar, device observation, supervisor roster.
- **M3 — AI voice leg**: `internal/voice` port (+fix list), providers, flow engine + Designer, `aicall` orchestration, transfer/overflow flows, latency tuning gate (A3), smart_turn vs server_vad evaluation.
- **M4 — product surface**: CDR explorer + transcripts, recordings (fs/S3) + quality review, reports, callbacks, audit, trunks apply, wallboard polish, outbound AI + click-to-dial.
- **M5 — packaging**: docker-compose demo + seed, CI/release matrix, README/deploy/provider-extension docs, load tests L2–L5 (06 §7).

## 7. Top risks (tracked through Phase 3)

1. ESL event-shape assumptions — **largely resolved in M0** (register/park/bridge/DTMF/hangup/callcenter shapes captured live; see m0-findings.md); residual: `sip_hangup_disposition` + UNHOLD on real SIP legs, closed early in M2.
2. mod_callcenter `odbc-dsn` — **resolved in M0**: pgsql:// works on 1.11.1; runs in a dedicated `aicc_fs` database (collision finding).
3. OpenAI G.711 passthrough — **resolved in M0**: `audio/pcmu` and `audio/pcma` both verified, both directions, on `gpt-realtime-2.1`.
4. Provider account concurrency quotas (200 simultaneous realtime sessions) are an **external** limit — must be raised with OpenAI/DashScope; load tests use the mock provider.
5. Latency budget ≤1.2s p50 depends on turn-detection hold time (provider-side, 400–800ms) — measured early, knobs documented in 02 §8.
