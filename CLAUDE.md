# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

An open-source AI-native call center: one Go binary (chi/pgx/sqlc/slog/OTel) serving a REST API + SSE stream + embedded React SPA, driving FreeSWITCH over ESL for human agents (50 WebRTC agents via the web-sip-phone Chrome extension, queues via mod_callcenter) and terminating its own SIP/RTP for AI calls. Single tenant — no `tenant_id` anywhere. Apache-2.0; every source file carries `// SPDX-License-Identifier: Apache-2.0`.

**UI is optional. API is the product (owner directive 2026-08-31).** `docs/openapi.json` is not documentation of the product — it *is* the product surface; the embedded SPA is one consumer of it, no more privileged than a customer's integration. In practice: no route exists that the contract does not declare (91 = 91 today, both diffs empty, `TestEveryContractOperationIsRouted` and `internal/httpapi/contract_gate_test.go`); no capability is reachable by a session cookie that a properly-scoped API key cannot reach (two registered exceptions, `TestASystemCanReachWhatAPersonCan`); and a rule written because "the panel does not need it" is a rule in the wrong place. **Authorization is scopes, and the contract states them**: each operation's `security` block is generated into `api.OperationSecurityByRoute` and applied by one middleware inside the generated wrapper — there are no role guards on the routing table, and a role only decides which scopes a login is granted (`grantedScopes`, derived by `docs/auth/scopemap.py`). Two credentials, equals: a session cookie and `Authorization: Bearer <key>`. When a screen and the contract disagree about what an operation means, the contract is right.

The build is strictly phased. Requirements live in `docs/phase1-decisions.md`, the approved design in `docs/design/00-…07` (07 is the **mandatory naming spec** — 4-layer mapping Go `CallID` ↔ JSON `callId` ↔ TS `callId` ↔ DB `call_id`, SCREAMING_SNAKE enum values byte-identical across JSON/TS/DB, `xxxAt`/`xxxMs`/`xxxSec`, `is_`/`has_` booleans, no upstream FreeSWITCH/Genesys tokens outside boundary layers). Live-verified findings that amended the design are recorded in `docs/design/m0-findings.md` (M0 spike) and `docs/design/m4-cleanup-findings.md` (M4 drift audit + the A1/A6/A7 provider directives) — check both before trusting a doc's original claim.

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
# what has to be running, in what order, and which ports: docs/dev-stack.md
go build -o /tmp/aicc ./cmd/aicc && /tmp/aicc # logs also land in logs/aicc-<starttime>.log (read these to analyze runs)
/tmp/aicc useradd -username admin -password … -role ADMIN   # bootstrap first user
/tmp/aicc flowadd -file internal/seed/flows/x.json -did 95001 # load/publish a flow; same slug = update+republish
go run ./cmd/aicc-mockbackend -addr 127.0.0.1:8770 # the business APIs the reference flows call; without it every tool fails (correctly, and uselessly)

# live provider verification (spends real API money; OPENAI_API_KEY / ALIYUN_API_KEY)
AICC_LIVE_PROVIDER_TEST=1 go test ./internal/provider/ -run Live -v

# frontend
cd web && npm run dev                         # Vite dev server on 5173
cd web && npm run build                       # emits web/dist for go:embed

# the whole product in containers (app + FreeSWITCH + PostgreSQL, seeded)
make demo-up                                  # deploy/demo/README.md; screens on 127.0.0.1:8080
make image VERSION=v0.1.0                     # the release image, SPA embedded

# load testing (docs/load-tests.md) — the mock provider is a Realtime *server*,
# reached through the same endpoint override a real deployment would use
go run ./cmd/aicc-mockprovider -addr 127.0.0.1:9099
go run ./cmd/aicc-loadgen -target 127.0.0.1:6060 -calls 200 -ramp 60s -duration 240s -total 30m
```

## Database & migrations

PostgreSQL runs in a container, never from `brew`. The dev server, `make demo-up` and the migration tests all need it up first.

```sh
docker compose -f deploy/dev/docker-compose.yml up -d   # PostgreSQL 18 on 127.0.0.1:5432
# migrations run for real against it; without the env var these tests SKIP
AICC_TEST_DATABASE_URL='postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable' \
  go test ./internal/store/ -run TestMigrations -v
```

**A migration is not reviewed until it has run** (`internal/store/migrate_test.go`): the tests apply every migration from zero, roll them all back and re-apply them, and — the case a fresh database cannot detect — migrate a database that already holds rows. A migration that narrows a CHECK must rewrite existing rows *before* installing the constraint, or PostgreSQL rejects it with "is violated by some row" on every deployment that has history. Add a fixture there for any migration that changes an enum's allowed values.

## Config

Config is `AICC_*` env vars (`.env` in cwd is loaded; real env wins). **`.env.example` is the registry** — every setting with its real default, kept in step with `internal/config/config.go` (`cp .env.example .env`, then uncomment what changes). Two non-`AICC_` credentials matter: `OPENAI_API_KEY` / `ALIYUN_API_KEY`. Note the loader's semantics: an empty value means *unset* (the default wins, so a non-empty default cannot be blanked), and there is no inline-comment syntax — everything after the first `=` is the value. ESL is at `127.0.0.1:18021` (not stock 8021).

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
2. **AI path**: FreeSWITCH bridges a caller to the `aicc_bot` gateway → `voice.UAS` (custom SIP server on :6060, RTP pool 40000–40999) → `aicall.Session` bridges the leg to a `provider.VoiceSession` (OpenAI = G.711 passthrough, Qwen = 16k/24k PCM16 and the gateway 24k both ways, via `media.Converter`) while `flow.Engine`/`Runtime` steer phases and tools → `aicall.Orchestrator` composes it all, resolves DID→flow (published revision only) and executes transfers by `uuid_transfer`-ing the *caller's* channel (carried in the `X-AICC-Channel-ID` SIP header) to a queue extension.

   **One provider per deployment, chosen at startup** (phase1-decisions A1): `AICC_PROVIDER` (`qwen` in mainland China, `openai` elsewhere) plus `AICC_PROVIDER_ENDPOINT` / `AICC_PROVIDER_MODEL`, resolved once in `main` via `provider.ProfileFor` and handed to the orchestrator. **A call's `language` never selects a provider** — it sets greeting, prompt language and voice only; there is no language→provider mapping and reintroducing one is a regression.

   **The provider extension point is the wire protocol, not Go.** `internal/provider` is one OpenAI-Realtime client × `Profile`. A new engine is a new *profile* — name, endpoint, model, dialect — and a new `AICC_PROVIDER` value, never a second client. That is how the phase-2 **OpenAI Realtime Gateway** attaches (a *separate service* composing ASR + LLM + TTS behind the same events, impersonating no vendor — A6). That profile exists: `AICC_PROVIDER=gateway` (`GatewayProfile`, 24 kHz linear both ways, GA dialect), verified on live calls 2026-09-02. Cascade never enters this repo: no ASR/LLM/TTS types, interfaces, adapters, stubs or TODOs. `VoiceSession` is the seam between the call actor and that one client (plus its test fake); do not "generalise" it.

Domain model is Genesys-lineage: a Call aggregates Parties; leg events are `PARTY_*`, call-scoped are `CALL_*`; `CallType` (INBOUND/OUTBOUND/…) is stamped at creation and immutable across transfers. `userData` = business data; `switchData` is deliberately narrow (only `POST /calls` + Lua leg creation — do not widen it).

Events flow to browsers through `events.Hub`: global sequence via DB-reserved hi/lo blocks, in-memory ring for `Last-Event-ID` resume, `SYSTEM_RESET` on gap, slow consumers disconnected. REST + SSE only — no application WebSocket.

**A leg event goes to the agent whose leg it is** (owner directive 2026-08-22). An agent works one leg, so their stream carries that leg's states — `PARTY_RINGING`/`PARTY_DIALING` → `PARTY_ESTABLISHED` → `PARTY_RELEASED` — and **nobody else's**, not a colleague's and not the customer's. Two agents on one internal call each see three events about themselves, never six about both; when both parties were addressed to every agent on the call, a cockpit could not tell which party was its own and showed the agent being rung their caller's leg. A **party with no agent** therefore reaches no agent's stream at all — a caller who abandons a queue is in the CDR and in a supervisor's view, which is where it belongs. **Do not widen a party's scope to rescue a missing event**: a customer hanging up ends the agent's leg too, so the agent's own `PARTY_RELEASED` is always coming, and if a bar ever stays up after the customer leaves, that absent event is the defect (`TestTheAgentIsToldTheirOwnLegEnded` pins it; a 2026-08-18 incident was once patched by widening, which hid the cause and created the cockpit bug). Only **call-scoped `CALL_*` events** keep the whole conversation, being about the call rather than a leg of it. Supervisors and administrators still see everything.

**A state event means a state changed.** No transition, no event. A party's transitions are the FSM's to allow: an illegal or duplicate one is logged and dropped, never published (`transition`), and a leg already TALKING that is re-bridged — a transfer, a re-invite — has nothing new to say (`establish`). The same rule outside the FSM: `CALL_USER_DATA` announces movement only, so setting a key to the value it already holds and deleting a key that was not there are both silent (`UserDataChange.IsEmpty`) — without that, every consultation transfer would report a change nobody made. **Never publish an event unconditionally to make a client refresh.** That turns the stream from a record of what happened into a polling trigger, makes the `state` in a `PARTY_*` payload a lie, and leaves subscribers unable to tell "this really changed" from "someone wanted me to refetch". If a screen is stale, the missing event is the defect — the same reasoning as the scope rule above. Two `PARTY_ESTABLISHED` on one call are two *parties* establishing, not one party transitioning twice; check the `partyId` before concluding otherwise.

FreeSWITCH reads its directory/dialplan/queue config *from PostgreSQL* via mod_lua + mod_pgsql (`freeswitch/scripts/*.lua`). The Go↔Lua contract is the `luacc.*` views (jsonb flattened — mod_lua has no JSON parser), accessed by a confined DB role (`deploy/sql/lua_role.sql`). mod_callcenter runs against a dedicated `aicc_fs` database (its tables are unqualified in `public` and would collide).

Flow DSL v2 (`internal/flow`): the model owns the conversation, the flow owns the phase. The flow also owns the bot's **voice** — `global.voice`, published and versioned with the persona (A7), never an env var; empty falls back to the provider profile's. Voice names are provider-specific and a deployment runs one provider, so a flow names a voice its own deployment offers. Phases carry instructions + tool allowlists; transitions fire on tool results with structured-operator conditions (rules can test `result.ok`); a fired transition overrides the tool's hint with the new phase's instruction. Built-ins (`transfer_to_agent`, `take_message`, `hangup`) may *refuse*, and a refusal is a conversation ("queue closed → offer to take a message"), not an error. Everything checkable is validated at load, not mid-call.

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

## Packaging (M5)

`deploy/demo/` is the one-command stack: PostgreSQL + this repository's own switch image + the application, all on a compose network so an AI call never leaves the host. `freeswitch/` **is** the switch — the complete v1.11.3 configuration tree in `conf/` (design 01 §7's diffs made in the files, not patched in at boot; `CONF-DEVIATIONS.md` is the record against vanilla), `modules.conf`, and a `Dockerfile` that builds FreeSWITCH from source with `mod_audio_stream` (`build.sh`, multi-arch, `rasonyang/freeswitch-aicc`). Nothing site-specific is baked in: the entrypoint injects a deployment's DSNs, addresses and passwords (`DOCKERHUB.md` is that contract), and a trunk belongs to a deployment — this box's is `deploy/dev/freeswitch/`. `AICC_SEED=demo` seeds it complete — accounts, queues, a published bilingual flow behind 95001/95002, a week of history — and `AICC_SEED=fresh` removes exactly that again. The audience-facing docs are `README.md` / `README.zh-CN.md`, `deploy/README.md` (deployment), `docs/provider-extension.md` (a provider is a profile, never a second client) and `docs/load-tests.md` (the plan for L1–L5).

**No performance claim, anywhere (owner directive 2026-08-16).** The benchmark campaign is deferred: L2–L5 need a host that is not a developer laptop, a switch that is not the development one, `sipp` and provider credit. Until it has run, no capacity or latency figure goes in the README, the deployment guide, a release note or a commit message — no "200 concurrent on 8c/16GB", no core counts, no measured latency. The estimates in 06 are an internal design aid and stay inside the design set. The harness exists (`cmd/aicc-mockprovider` — a Realtime *server* reached through `AICC_PROVIDER_ENDPOINT`, not an in-process fake — and `cmd/aicc-loadgen`), and a shakedown run of it is what found the watchdog bug in `m5-findings.md` §1; its numbers are not results.

Capacity metrics live in `internal/obs/callmetrics.go` — one place for every instrument name, fed from the call paths (see 06 §8 for what is implemented and what is deliberately not).

## Reference repos (read for lineage, never import)

`~/workspaces/cc/golang-bot` (SIP/RTP — **first reference for the voice path**), `~/workspaces/cc/java-bot` (realtime s2s providers + **flow DSL reference**), `~/workspaces/cc/cti-server` (domain protocol; its ESL mappings are unverified hypotheses), `~/workspaces/github/web-sip-phone` (integrate as-is; changes go through the Claude session named `web-sip-phone`), `~/workspaces/github/ui-test` (UI reference). All owner-authored; Apache-2.0 clean.
