# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

An open-source AI-native call center: one Go binary (chi/pgx/sqlc/slog/OTel) serving a REST API + SSE stream + embedded React SPA, driving FreeSWITCH over ESL for human agents (50 WebRTC agents via the web-sip-phone Chrome extension, queues via mod_callcenter) and terminating its own SIP/RTP for AI calls (target: 200 concurrent on 8c/16GB). Single tenant — no `tenant_id` anywhere. Apache-2.0; every source file carries `// SPDX-License-Identifier: Apache-2.0`.

The build is strictly phased. Requirements live in `docs/phase1-decisions.md`, the approved design in `docs/design/00-…07` (07 is the **mandatory naming spec** — 4-layer mapping Go `CallID` ↔ JSON `callId` ↔ TS `callId` ↔ DB `call_id`, SCREAMING_SNAKE enum values byte-identical across JSON/TS/DB, `xxxAt`/`xxxMs`/`xxxSec`, `is_`/`has_` booleans, no upstream FreeSWITCH/Genesys tokens outside boundary layers). Live-verified findings that amended the design are recorded in `docs/design/m0-findings.md` — check it before trusting a doc's original claim.

## Commands

```sh
go build ./...                                # build everything
go test -race ./...                           # full test suite (always -race; it has caught real bugs here)
go test -race -run TestName ./internal/voice/ # one test
go test -run XXX -bench . -benchmem ./internal/media/  # allocation benchmarks (hot paths must stay 0 allocs/op)
go vet ./... && gofmt -l internal/ cmd/       # lint gate

sqlc generate                                 # after editing internal/store/sql/*.sql (schema comes from migrations/)
# migrations: add internal/store/migrations/NNNNN_name.sql (goose format); they run at server startup

# dev server (PostgreSQL 18 must be up: deploy/dev/docker-compose.yml — never brew)
go build -o /tmp/aicc ./cmd/aicc && /tmp/aicc # logs also land in logs/aicc-<starttime>.log (read these to analyze runs)
/tmp/aicc useradd -username admin -password … -role ADMIN   # bootstrap first user
/tmp/aicc flowadd -file deploy/seed/flows/x.json -did 95011 # load/publish a flow; same slug = update+republish

# live provider verification (spends real API money; OPENAI_API_KEY / ALIYUN_API_KEY)
AICC_LIVE_PROVIDER_TEST=1 go test ./internal/provider/ -run Live -v

# frontend
cd web && npm run dev                         # Vite dev server on 5173
cd web && npm run build                       # emits web/dist for go:embed
```

Config is `AICC_*` env vars (`.env` in cwd is loaded; real env wins) — see `internal/config/config.go` for the complete list. ESL is at `127.0.0.1:18021` (not stock 8021).

## API contract — spec-first (mandatory)

`docs/openapi.json` (OpenAPI 3.1) is the **single source of truth** for every HTTP endpoint. The order is always: edit the contract → generate → implement → test. Never the reverse.

```sh
make api-lint      # Redocly lint; zero errors/warnings (pinned exceptions: .redocly.lint-ignore.yaml)
make api-generate  # regen internal/api/api.gen.go + web/src/generated/api.ts — commit them with the spec
make api-check     # the CI gate: api-lint + regenerate + git diff must be empty
make api-breaking BASE=main  # oasdiff: no undeclared breaking changes vs the base branch
```

- **Never** introduce code-first OpenAPI tooling (swaggo or any annotation/reflection generator), and never "sync" the contract from code. Generated files carry `DO NOT EDIT` and are committed; hand-editing them is forbidden — CI (`.github/workflows/api.yml`) regenerates and fails on any diff.
- Tool versions are pinned: `go.mod` `tool` directives (oapi-codegen, oasdiff), `web/package.json` exact devDependencies (openapi-typescript, @redocly/cli). `scripts/api-generate.sh` is the only generation entry point (`go generate ./internal/api` calls it too).
- `httpapi.Server` must implement the generated `api.ServerInterface` (compile-locked in `internal/httpapi/api_server.go`): a new spec operation breaks the build until the server grows its method — that is the point.
- Naming: the contract follows 07-naming on the wire; Go initialisms (`CallID`, `ListCDRs`) come from `name-normalizer` + `additional-initialisms` in `oapi-codegen.yaml` — extend that list for a new initialism, don't scatter `x-go-name` (reserved for genuine one-offs like the Last-Event-ID header/query collision).
- Frontend wire types come only from `web/src/generated/api.ts`, re-exported by `web/src/lib/*.ts` under their established names. No handwritten DTO interfaces.
- sqlc models and `internal/store` types are never exposed as API types; handlers map explicitly to `api.*` types at the boundary.
- SSE: OpenAPI cannot express per-event-type payloads. The envelope is `SseEvent`; stable payload shapes are components named `Sse*Payload` (linked by convention, lint-ignored as "unused" — keep them registered there, no AsyncAPI second system).
- Every route is mounted through the generated wrapper (`s.apiWrapper()`), and **the contract operation is the handler**, living in the file that owns its subject (`UpdateExtension` in `catalog_handlers.go`, `GetCDR` in `ledger_handlers.go`). Path/query/header parameters arrive parsed — never re-read a raw parameter, never add a delegating shim. `api_server.go` holds only the interface assertion, the wrapper and `writeParamError`. One deliberate exception: `StreamEvents` parses its own `Last-Event-ID`, because an EventSource retries a rejected request forever and a mangled resume point must degrade to a fresh stream, not a reconnect loop.
- Bodies/responses are `api.*` types wherever a group has been migrated; the ledger group still writes `store.*` types straight out (shapes are identical and guarded by `TestLedgerTypesMarshalPerTheNamingSpec`) — move it onto `api.*` when it next changes.

## Architecture

Two telephony paths meet in one process:

1. **Human path**: `esl.Link` (one ESL inbound connection, auto-reconnect) → `telephony.Normalize` (the *only* place raw FreeSWITCH events become domain `SwitchEvent`s) → `telephony.Coordinator` + `Registry` (actor-per-call: each call has one goroutine as sole mutator; snapshots via mailbox) and `agents.Service` (presence FSM, mirrored into mod_callcenter). Commands back to the switch go through `telephony.Adapter` — the complete FS command vocabulary lives there and nowhere else.
2. **AI path**: FreeSWITCH bridges a caller to the `aicc_bot` gateway → `voice.UAS` (custom SIP server on :6060, RTP pool 40000–40999) → `aicall.Session` bridges the leg to a `provider.VoiceSession` (OpenAI = G.711 passthrough, Qwen = 16k/24k PCM16 via `media.Converter`) while `flow.Engine`/`Runtime` steer phases and tools → `aicall.Orchestrator` composes it all, resolves DID→flow (published revision only) and executes transfers by `uuid_transfer`-ing the *caller's* channel (carried in the `X-AICC-Channel-ID` SIP header) to a queue extension.

Domain model is Genesys-lineage: a Call aggregates Parties; leg events are `PARTY_*`, call-scoped are `CALL_*`; `CallType` (INBOUND/OUTBOUND/…) is stamped at creation and immutable across transfers. `userData` = business data; `switchData` is deliberately narrow (only `POST /calls` + Lua leg creation — do not widen it).

Events flow to browsers through `events.Hub`: global sequence via DB-reserved hi/lo blocks, in-memory ring for `Last-Event-ID` resume, `SYSTEM_RESET` on gap, slow consumers disconnected. REST + SSE only — no application WebSocket.

FreeSWITCH reads its directory/dialplan/queue config *from PostgreSQL* via mod_lua + mod_pgsql (`freeswitch/scripts/*.lua`). The Go↔Lua contract is the `luacc.*` views (jsonb flattened — mod_lua has no JSON parser), accessed by a confined DB role (`deploy/sql/lua_role.sql`). mod_callcenter runs against a dedicated `aicc_fs` database (its tables are unqualified in `public` and would collide).

Flow DSL v2 (`internal/flow`): the model owns the conversation, the flow owns the phase. Phases carry instructions + tool allowlists; transitions fire on tool results with structured-operator conditions (rules can test `result.ok`); a fired transition overrides the tool's hint with the new phase's instruction. Built-ins (`transfer_to_agent`, `take_message`, `hangup`) may *refuse*, and a refusal is a conversation ("queue closed → offer to take a message"), not an error. Everything checkable is validated at load, not mid-call.

## Invariants that came from live debugging — do not regress

- **Turn end ≠ playback end.** `TURN_DONE` means the model stopped producing; `PLAYBACK_DONE` means the caller heard it. Transfers/hangups execute only after the latter (gated on turn identity), with a 10s cap. Barge-in's boundary is the last frame *heard*: speech over a finished turn's still-playing tail is an interruption (local flush only — never send the provider a cancel once generation ended; Qwen errors on it).
- The drain watch and the dead-air timer use **separate** generation counters; sharing one made every goodbye end on the grace cap.
- Interrupt flushes the local TX queue *first*; played-ms = frames queued − frames flushed. A keypress always interrupts; detected speech is ignored inside the 800ms barge guard (line echo).
- Codec wiring is all-or-nothing per law (PCMU and PCMA both fully wired); frames stay G.711-encoded through the RTP session (the OpenAI path is byte passthrough). Per-frame paths (`internal/media`, framer) must stay zero-alloc — benchmarks enforce this.
- ESL rules: `uuid_transfer … 'bridge:{…}' inline` (never originate+uuid_bridge); `uuid_phone_event talk|hold` for browser phones (never `uuid_answer`); codec pins ride the new leg inline (`{absolute_codec_string=PCMU,PCMA}sofia/gateway/…`).
- The `aicc_bot` gateway must target `$${local_ip_v4}`, never loopback (a LAN-bound macOS socket cannot send to 127.0.0.1) and never with a `sip:` scheme in the var. If calls break, check `ifconfig` against the `local_ip_v4` pin in vars.xml first — DHCP has moved this box before.
- Qwen: `response.create` on an empty conversation is rejected (profile trait `NeedsCueForFirstTurn` sends a synthetic cue — never on providers that greet unprompted); turn detection freezes at first audio; `smart_turn` forces a 2000ms hold, so `server_vad@500ms` is the default.

## Frontend

`web/CLAUDE.md` is a **binding design system** (13px base, single accent #4F46E5, borders not shadows, 6px radius, tabular-nums for all numbers, nav config drives breadcrumbs) — read it before any UI work. i18n via react-i18next, en (default) / zh, no hardcoded user-facing strings. Routes live under `web/src/routes/` (TanStack Router file conventions; a parent route needs `<Outlet/>` or use `_app.section.index.tsx`). Role guards via `requireRole` in `beforeLoad`.

## Reference repos (read for lineage, never import)

`~/workspaces/cc/golang-bot` (SIP/RTP — **first reference for the voice path**), `~/workspaces/cc/java-bot` (realtime s2s providers + **flow DSL reference**), `~/workspaces/cc/cti-server` (domain protocol; its ESL mappings are unverified hypotheses), `~/workspaces/github/web-sip-phone` (integrate as-is; changes go through the Claude session named `web-sip-phone`), `~/workspaces/github/ui-test` (UI reference). All owner-authored; Apache-2.0 clean.
