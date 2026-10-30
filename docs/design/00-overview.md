# Design 00 — System Overview

Binds to [phase1-decisions.md](../phase1-decisions.md). Reading order: 00 → 01
(telephony) → 02 (AI voice) → 03 (data) → 04 (API/SSE) → 05 (frontend) → 06
(capacity) → 07 (naming — mandatory for every layer) → 09 ([CDR
webhooks](09-webhooks.md)) → 10 ([AI outbound campaigns](10-campaigns.md)).
Live providers and packaging have evidence records alongside: [m0-findings.md](m0-findings.md),
[m5-findings.md](m5-findings.md), [qwen-findings.md](qwen-findings.md),
[doubao-findings.md](doubao-findings.md) and [grok-findings.md](grok-findings.md).
The `08-` document is now an as-built note: the gap analysis it used to hold
described a tree that no longer existed, and the feature lives in
`internal/streamin`, `internal/transcribe`, `internal/transcript` and the contract.
`AGENTS.md` is the architecture reference the code is kept in step with; this
document explains the shape, not the package inventory.

## 1. System context

```
                       PSTN / SIP trunk (via an SBC, production only)
                                   │
                            ┌──────▼──────┐  SIP/WSS (agents' browser phones)
  Agent browser ────────────►             ◄──────────────┐
  ├─ SPA (React, SSE) ──┐   │ FreeSWITCH  │              │
  └─ web-sip-phone ext ─┘   │  1.11.3     │   G.711 RTP  │
     (SIP.js, audio only)   │ mod_callcenter  ┌──────────▼─────────┐
                            │ mod_lua ────────►                    │
                            └──────▲──────┘   │   aicc (Go, one    │
                                   │ ESL      │   process, 8c16g)  │
                          inbound  │ :18021   │  ┌──────────────┐  │
                                   └──────────┤  │ SIP UAS :6060│  │
        PostgreSQL 18 ◄────────────────────────┤ │ RTP pool     │  │
        (schema owned by Go migrations;        │ └──────┬───────┘  │
         mod_lua reads views read-only)        └────────┼──────────┘
                                                        │ WSS — one provider
                                                        │ per deployment
                                        ┌───────────────┴───────────────┐
                                        │ AICC_PROVIDER, resolved once: │
                                        │ openai | grok | qwen | gateway│
                                        │  (the Realtime client's       │
                                        │   profiles)                   │
                                        │ doubao (its own client, a     │
                                        │   different wire protocol)    │
                                        └───────────────────────────────┘
```

- **FreeSWITCH owns**: SIP registration (agents), trunks, bridging, mod_callcenter queues (waiting/MOH/distribution), recording capture, transcoding on non-bot legs.
- **aicc (Go) owns**: call control (ESL), agent/queue/flow/user configuration, the AI voice leg (SIP UAS + RTP + provider bridge), flow engine, SSE fan-out, CDR/transcripts/reports, recording storage/lifecycle, SPA.
- **PostgreSQL owns**: all durable state. mod_lua reads dedicated `luacc.*` views (contract, see 03).
- **The provider is chosen at startup, not per call (A1)**: `AICC_PROVIDER` picks one value for the whole deployment; a DID's `language` sets the greeting and the prompt language only, never the provider or the voice (the flow's `global.voice` owns the voice, A7). Four values (`openai`, `grok`, `qwen`, `gateway`) are profiles of the OpenAI-Realtime client; `doubao` speaks ByteDance's protocol and has a client of its own (A6 as amended, `doubao-findings.md`).
- **Outside this repo, by design (phase1-decisions A6)**: any cascaded speech pipeline. ASR + LLM + TTS compose inside a separate *OpenAI Realtime Gateway* service that exposes the Realtime protocol; aicc reaches it exactly as it reaches OpenAI — an endpoint override — and carries no cascade code, interface, stub or TODO.

## 2. Runtime topologies

**Production**: single `aicc` binary (SPA embedded via go:embed) + PostgreSQL + FreeSWITCH (+ S3-compatible store). TLS/reverse proxy is the deployer's concern (`deploy/`).

**Dev (reference setup, documented as-is)**: `go run ./cmd/aicc` on :8080 (HTTP), `vite dev` on :5173 proxying `/api` → :8080; user-managed Caddy outside this repo maps `app.aicc.test`→5173, `api.aicc.test`→8080, `ws.aicc.test`→FS wss :7443 (TLS→TLS, skip_verify — sofia drops REGISTERs whose Via transport mismatches the socket). The repo itself never handles TLS. The SPA origin (`app.aicc.test`) must be in web-sip-phone's Allow Sites. Details: [dev-stack.md](../dev-stack.md).

## 3. Layering rules

Dependencies point downward only: `internal/httpapi` sits at the top and is
imported by nobody but the composition root (`cmd/aicc`); `store`/`media`/`esl` sit at the bottom. `internal/voice`
and `internal/provider` never import `telephony` (they are driven by `aicall`);
nothing imports `httpapi`; raw FreeSWITCH event names never escape
`internal/telephony` (the normalization boundary).

The packages themselves are `ls internal`; `AGENTS.md` §Architecture names what
each boundary is for and which package owns which subject. Do not restate that
inventory here — it is the copy that drifts.

Non-Go deliverables: `freeswitch/` (Lua scripts, config tree, install notes —
see 01), `web/` (SPA — see 05 and `web/CLAUDE.md`), `deploy/` (compose stack,
installer, seed).

## 4. Third-party dependencies (approval list — per "no undiscussed deps")

Backend: `go-chi/chi/v5`, `jackc/pgx/v5` (mandated); `pressly/goose/v3` (migrations, embedded SQL); `pion/rtp` + `pion/rtcp` (RTP wire format; golang-bot lineage); `gorilla/websocket` (provider WS; golang-bot/java-bot lineage); `google/uuid` (UUIDv7); `golang.org/x/crypto` (argon2id); `minio-go/v7` (any-S3 *client* library — no MinIO server ships anywhere; the stack uses the filesystem backend); `oapi-codegen/runtime` (generated API glue); OTel (`go.opentelemetry.io/otel` + SDK + OTLP exporters + prometheus metrics exporter). Build-time: `sqlc`, `oxlint` (frontend). Explicitly avoided: SIP libraries (custom UAS), Redis/MQ (mandate), heavyweight media stacks.

Frontend: react 19, @tanstack/react-router + react-query, tailwindcss 4, shadcn/ui (radix-nova style, consolidated radix-ui), lucide-react, recharts, @fontsource-variable/inter, i18next + react-i18next.

## 5. Cross-cutting conventions

- **IDs**: UUIDv7 everywhere; `callId` client-mintable (idempotent originate); a party's id is the FreeSWITCH channel UUID, pre-assigned via `origination_uuid` (FS `uuid-version 7`).
- **Errors**: API returns `{"error":{"code":"NOT_FOUND","message":"…","params":{…}}}`; codes are the contract's `ErrorCode` enum (SCREAMING_SNAKE) and the i18n contract (04 §5); the backend never localizes.
- **Naming**: [07-naming.md](07-naming.md) is normative for every identifier in every layer (Go `CallID` ↔ JSON `callId` ↔ TS `callId` ↔ DB `call_id`).
- **Logging**: slog JSON; a log in a call context carries `callId`/`partyId`/`agentId`; `traceId` is correlated through the OTel handler.
- **OTel**: the HTTP handler is traced (`otelhttp`); turn latency is the `aicc_turn_latency_ms` histogram (02 §8); the instrument names live in `internal/obs/callmetrics.go` (06 §8). There is no pgx tracer and no per-ESL-command span.
- **Config**: every setting is an `AICC_*` variable, with `.env.example` the registry of names and defaults. Key defaults for a dev box: `AICC_ESL_ADDR=127.0.0.1:18021`, `AICC_BOT_SIP_PORT=6060`, `AICC_BOT_RTP_PORT_LOW=40000`, `AICC_BOT_RTP_PORT_HIGH=40999`, `AICC_HTTP_ADDR=:8080`. Provider API keys are env-only (never DB) — those are credentials *this* system presents to a vendor. The keys integrations present to *us* are the opposite direction and live in the `api_keys` table, issued and revoked through the API (04 §2, 03 §2).
- **Licensing**: Apache-2.0. All reference code (cti-server, golang-bot, java-bot, web-sip-phone, ui-test, and the pipecat PR #3859 transport) is owner-authored — confirmed 2026-08-13 — so the ported SIP/RTP code needs no third-party attribution. `NOTICE` stays minimal (project + copyright line). Source files carry `// SPDX-License-Identifier: Apache-2.0`. Any line actually translated from pion's samplebuilder (rather than modelled on its design) carries an MIT attribution comment — MIT/BSD deps are Apache-compatible and live as Go modules with their own license files; no GPL-family deps anywhere.

## 6. Delivery history

M0–M5 are complete; their reports are [m0-findings.md](m0-findings.md) and
[m5-findings.md](m5-findings.md), and the release notes are `CHANGELOG.md`.
This section used to carry the milestone plan and a risk register; both were
plans, and a plan kept after the work is done is a document that lies.
