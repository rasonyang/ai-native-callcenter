# AGENTS.md

`CLAUDE.md` (Claude Code) and `AGENTS.md` (Codex) carry the same text; change them together.

## What this is

An open-source AI-native call center: one Go binary (chi/pgx/sqlc/slog/OTel) serving a REST API, an SSE stream and an embedded React SPA. It drives FreeSWITCH over ESL for human agents (WebRTC agents on the web-sip-phone Chrome extension, queues on mod_callcenter) and terminates its own SIP/RTP for AI calls. Single tenant: no `tenant_id` anywhere. Apache-2.0: new Go, SQL, Lua and script files start with an `SPDX-License-Identifier: Apache-2.0` line.

Requirements and owner decisions (A1, A6, A7, …): `docs/phase1-decisions.md`. Design: `docs/design/NN-*.md`. Live findings that amended the design: `docs/design/*-findings.md`; check them before trusting a design doc's original claim. `docs/design/07-naming.md` is the **mandatory naming spec**: Go `CallID` ↔ JSON `callId` ↔ TS `callId` ↔ DB `call_id`; SCREAMING_SNAKE enum values byte-identical across JSON/TS/DB; `xxxAt`/`xxxMs`/`xxxSec`; `is_`/`has_` booleans; no upstream FreeSWITCH/Genesys tokens outside boundary layers.

## The API is the product; the UI is optional (owner directive)

`docs/openapi.json` is the product surface. The embedded SPA is one consumer of it, with no more privilege than a customer's integration. When a screen and the contract disagree about what an operation means, the contract is right.

- No route exists that the contract does not declare, and every operation is routed (`TestEveryMountedRouteDeclaresItsAuthorization` in `internal/httpapi/contract_gate_test.go`, `TestEveryContractOperationIsRouted`).
- Anything a session cookie can reach, a properly scoped API key can reach (`TestASystemCanReachWhatAPersonCan`, two registered exceptions). A rule written because "the panel does not need it" is in the wrong place.
- Authorization is scopes. Each operation's `security` block is generated into `api.OperationSecurityByRoute` and enforced by one middleware inside the generated wrapper. There are no role guards on routes; a role only decides which scopes a login is granted (`grantedScopes`, derived by `docs/auth/scopemap.py`). A session cookie and `Authorization: Bearer <key>` are equal credentials. Design: `docs/design/04-api-sse.md` §2.

## Language

Commit messages, code comments and documentation are in English (owner directive; the repository is public). Other languages appear only in product content: the `zh` half of bilingual flows, prompts and UI labels, `README.zh-CN.md`, and the `zh` i18n resources.

## Commands

```sh
go build ./...
go test -race ./...                               # always -race (`make test` omits it)
go test -race -run TestName ./internal/voice/     # one test
go test -run XXX -bench . -benchmem ./internal/media/ ./internal/aicall/  # hot paths: 0 allocs/op, CI fails otherwise
make lint                                         # go vet + gofmt + oxlint
sqlc generate                                     # after editing internal/store/sql/*.sql
# migrations: add internal/store/migrations/NNNNN_name.sql (goose); they run at server startup

# dev server; prerequisites, order and ports: docs/dev-stack.md
make dev-up                                       # PostgreSQL 18 + SeaweedFS containers
deploy/dev/restart.sh                             # build web/dist and /tmp/aicc, restart, wait until it serves
/tmp/aicc useradd -username admin -password … -role ADMIN
/tmp/aicc flowadd -file internal/seed/flows/x.json -did 95001   # load + publish; same slug = update + republish
go run ./cmd/aicc-mockbackend                     # business APIs the reference flows call (127.0.0.1:8770); the app uses it when AICC_BOT_BACKEND_BASE points there
# logs: stderr and logs/aicc-<starttime>.log (read the file to analyse a run)

# live provider tests: real money (OPENAI_/ALIYUN_/DOUBAO_/GEMINI_API_KEY)
AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run Live -v ./internal/provider/...

cd web && npm run dev                             # Vite on 5173
cd web && npm run build                           # web/dist, embedded via go:embed
cd web && npm run test                            # vitest

make stack-up                                     # the whole product in containers, seeded (deploy/README.md)
```

Load harness: `docs/load-tests.md`.

## Database and migrations

PostgreSQL runs in a container (`make dev-up`), never as a host install. Database tests (store, seed, httpapi) skip unless `AICC_TEST_DATABASE_URL` is set; CI sets it.

```sh
AICC_TEST_DATABASE_URL='postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable' go test -race ./internal/store/
```

**A migration is not reviewed until it has run.** The store tests apply every migration from zero, roll them all back, re-apply them, and migrate databases that already hold rows. A migration that narrows a CHECK must rewrite existing rows before installing the constraint, or PostgreSQL rejects it ("is violated by some row") on every deployment with history. A migration that changes an enum's allowed values gets a fixture test in `migrate_test.go` or a sibling `migrate_*_test.go`: migrate to the previous version, insert old-shape rows, migrate up, assert.

## Config

`AICC_*` environment variables; `.env` in the working directory is loaded and the real environment wins. `.env.example` is the registry of every setting with its real default, kept in step with `internal/config/config.go`. An empty value means unset, so a non-empty default cannot be blanked. There is no inline-comment syntax: everything after the first `=` is the value. Provider API keys are not `AICC_*`: `OPENAI_API_KEY`, `ALIYUN_API_KEY`, `REALTIME_API_KEY` (gateway), `DOUBAO_API_KEY`, `GEMINI_API_KEY`. ESL is at `127.0.0.1:18021`, not the stock 8021.

## API contract: spec-first (mandatory)

`docs/openapi.json` (OpenAPI 3.1) is the single source of truth for every HTTP endpoint. The order is always: edit the contract → generate → implement → test.

```sh
make api-lint                 # Redocly, zero errors or warnings (pinned exceptions: .redocly.lint-ignore.yaml)
make api-generate             # regenerate internal/api/*.gen.go and web/src/generated/*; commit them with the spec
make api-check                # the CI gate: lint + regenerate + empty git diff
make api-breaking BASE=main   # oasdiff: no undeclared breaking change
```

- Never introduce code-first OpenAPI tooling (swaggo or any annotation/reflection generator), and never sync the contract from code. Generated files (`DO NOT EDIT`) are committed and never edited by hand. `scripts/api-generate.sh` is the only generation entry point.
- `httpapi.Server` must implement the generated `api.ServerInterface` (asserted in `internal/httpapi/api_server.go`), so a new operation breaks the build until the server has its method.
- Go initialisms (`CallID`, `ListCDRs`) come from `name-normalizer` + `additional-initialisms` in `oapi-codegen.yaml`; extend that list rather than adding `x-go-name`, which is reserved for genuine one-offs (the `Last-Event-ID` header/query collision).
- Frontend wire types come only from `web/src/generated/api.ts`, re-exported by `web/src/lib/*.ts`. No handwritten DTOs, except the flow spec types in `web/src/lib/flows.ts` (the contract keeps the spec opaque).
- sqlc models and `internal/store` types are never API types; handlers map to `api.*` at the boundary. Groups that still write `store.*` or domain types straight out (the ledger group among them) move onto `api.*` when they next change.
- SSE: the envelope is `SseEvent`; stable payload shapes are `Sse*Payload` components, lint-ignored as "unused". No AsyncAPI.
- Every route is mounted through the generated wrapper (`s.apiWrapper()`), and the contract operation is the handler, in the file that owns its subject (`UpdateExtension` in `catalog_handlers.go`). Parameters arrive parsed: never re-read a raw parameter, never add a delegating shim. `api_server.go` holds only the interface assertion, the wrapper and `writeParamError`. One exception: `StreamEvents` (`/events`) is mounted by hand and parses its own parameters, because an EventSource retries a rejected request forever and a mangled resume point must degrade to a fresh stream, not a reconnect loop.

## Architecture

Two telephony paths in one process; the AI path can be switched off (`AICC_BOT_ENABLED`).

1. **Human path.** `esl.Link` (one ESL inbound connection, auto-reconnecting) → `telephony.Normalize` (the only place raw ESL events become domain `SwitchEvent`s; raw FreeSWITCH events are read only inside `internal/telephony`) → `telephony.Coordinator` + `Registry` (an actor per call: one goroutine mutates it, snapshots go through its mailbox), `agents.Service` (presence FSM, mirrored into mod_callcenter) and `outbound.Service`. Every api/bgapi command to the switch is a `*telephony.Adapter` method.
2. **AI path.** FreeSWITCH bridges the caller to the `aicc_bot` gateway → `voice.UAS` (SIP on :6060, RTP 40000–40999) → `aicall.Session` bridges the leg to a `provider.VoiceSession` (OpenAI: G.711 passthrough; the other engines: PCM16 through `media.Converter`) while `flow.Engine`/`Runtime` steer phases and tools. `aicall.Orchestrator` resolves DID → flow (published revision only) and transfers by `uuid_transfer`-ing the caller's channel (named in the `X-AICC-Channel-ID` SIP header) to a queue extension.

### Providers (`docs/provider-extension.md`)

- **One provider per deployment, chosen at startup** (A1): `AICC_PROVIDER` = `openai` | `qwen` | `gateway` | `doubao` | `gemini`, with `AICC_PROVIDER_ENDPOINT` / `AICC_PROVIDER_MODEL`, resolved once by `provider.ProfileFor`. A call's `language` sets the greeting and the prompt language; it never selects the provider or the voice. A language→provider mapping is a regression.
- **The extension point is the wire protocol, not Go** (A6). A new engine on the OpenAI Realtime protocol is a new `Profile` and a new `AICC_PROVIDER` value, never a new client; `gateway` (a separate service composing ASR + LLM + TTS behind Realtime events) attached that way. Cascade never enters this repo.
- Only a new protocol earns a client. There are three: `provider.Realtime` (openai, qwen, gateway), `internal/provider/doubao` and `internal/provider/gemini` (reasons in `docs/design/doubao-findings.md` and `gemini-findings.md`). A new client lives in its own sub-package behind `provider.VoiceSession` and shares only transport and audio plumbing (`wsconn`, `Watchdog`, `MergeHint`, `SayExactly`, `pacer`), never a protocol event, decoder or dispatch. Do not generalise `VoiceSession`. The name→client choice is in `cmd/aicc/wiring.go`.

### Events

The domain model is Genesys-lineage: a Call aggregates Parties; leg events are `PARTY_*`, call-scoped events `CALL_*`; `CallType` is stamped at creation and never changes, even across transfers. `events.Hub` delivers to browsers: a global sequence from DB-reserved blocks, an in-memory ring for `Last-Event-ID` resume, `SYSTEM_RESET` when the resume point has left the ring, slow consumers disconnected. REST + SSE only; no application WebSocket (`internal/streamin` only ingests the switch's audio).

- **A leg event goes only to the agent whose leg it is** (owner directive). An agent's stream carries their own party's `PARTY_*` states and nobody else's, not a colleague's and not the customer's; otherwise a cockpit cannot tell which party is its own. A party with no agent reaches no agent's stream (it is in the CDR and the supervisor view). `CALL_*` events reach every agent on the call; supervisors and administrators see everything. **Never widen a party's scope to rescue a missing event**: a customer hanging up ends the agent's leg too, so if the agent's own `PARTY_RELEASED` does not arrive, that is the defect (`TestTheAgentIsToldTheirOwnLegEnded`).
- **A state event means a state changed.** The party FSM logs and drops an illegal or duplicate transition (`transition`); re-bridging a leg that is already TALKING publishes nothing (`establish`); `CALL_USER_DATA` is silent when nothing moved (`UserDataChange.IsEmpty`). Never publish an event to make a client refresh: if a screen is stale, the missing event is the defect. Two `PARTY_ESTABLISHED` on one call are two parties; check the `partyId`.

### FreeSWITCH configuration

FreeSWITCH gets its directory and `callcenter.conf` from PostgreSQL through mod_lua's XML handler; the Lua actions of its static dialplan (`freeswitch/scripts/*.lua`) query PostgreSQL through mod_pgsql on each call. The Go↔Lua contract is the `luacc.*` views (jsonb flattened; mod_lua has no JSON parser), read by a confined role (`deploy/sql/lua_role.sql`). mod_callcenter uses a dedicated `aicc_fs` database, because its tables are unqualified in `public` and would collide.

### Flow DSL (`internal/flow`; fields in `spec.go`, design in `docs/design/02-ai-voice.md` §3 and §6)

- The model owns the conversation; the flow owns the phase. Phases carry instructions and tool allowlists; transitions fire on tool results (conditions can test `result.ok`) and replace the tool's hint with the new phase's instruction. Everything checkable is validated at load, not mid-call.
- The flow owns the bot's voice (`global.voice`, versioned with the persona, A7), never an env var; empty falls back to the profile's voice.
- A phase's `announce` is a line said as written (bilingual, `{slots.x}` rendered): `SessionConfig.OpeningText` for the entry phase, `VoiceSession.SpeakText` after it. A line pre-empts and never queues. It is exact on doubao and best effort elsewhere. How each engine delivers a closing line is a profile trait (`NeedsDirectedLineInConversation`, `PutsTerminalAnnounceInToolResult`, `RequiresTerminalAnnounce`; table in `docs/provider-extension.md`). A deployment's rule (doubao's `RequiresTerminalAnnounce`) is enforced at publish, not at load.
- A built-in's refusal (a closed queue, a failed save) is a conversation, not an error.
- A caller's decline is invisible to the engine, whose only events are `TOOL_RESULT` and `NO_INPUT`. So `global.closingTarget` ends a call after three consecutive NO_INPUTs where no authored rule can fire on NO_INPUT, or when bot replies without a tool call exceed `global.maxTurnsWithoutTool`, through the same arm-then-speak terminal path as any other ending.

## Invariants from live debugging: do not regress

- **Turn end ≠ playback end.** `TURN_DONE` means the model stopped producing; `PLAYBACK_DONE` means the caller heard it. An armed transfer or hangup executes after the caller heard its turn (gated on turn identity), or when the caller speaks after a later turn has finished generating; the cap is 10 s from arming. A turn cut short never counts as the closing line having been said.
- **Barge-in's boundary is the last frame heard.** Speech over a finished turn's still-playing tail is an interruption: flush locally and trim the history (`conversation.item.truncate`), but never send `response.cancel` after generation has ended; the providers reject it.
- The drain watch and the dead-air timer use **separate** generation counters; sharing one made every goodbye end on the grace cap.
- An interrupt flushes the local TX queue first, then tells the provider; played time = frames queued − frames flushed. A keypress always interrupts; detected speech inside the 800 ms barge guard is ignored (line echo).
- Codec wiring is all-or-nothing per law (PCMU and PCMA both fully wired). Frames stay G.711 through the RTP session; the OpenAI path is byte passthrough. Per-frame paths (`internal/media`, the framer in `internal/aicall`) stay at 0 allocs/op.
- ESL: transfer with `uuid_transfer <uuid> 'm:^:bridge:{…}<endpoint>' inline` (`m:^:` stops a comma inside `{…}` from splitting the action), never originate + `uuid_bridge`. Browser phones get `uuid_phone_event talk|hold`, never `uuid_answer`. A codec pin rides the new leg's dial string: one codec on ESL paths (`{absolute_codec_string=PCMU}`), never on a loopback leg; the Lua dialplan's `bridge` pins `PCMU,PCMA`.
- The switch reaches the bot through the `aicc_bot` gateway at `$${aicc_bot_host}`, which must be an IP address the switch can send to: `$${local_ip_v4}` on a native install, the app's fixed `AICC_APP_IP` in the compose stack. Never loopback (a LAN-bound macOS socket cannot send to 127.0.0.1), and never a `sip:` scheme in the variable.
- Qwen: `response.create` on an empty conversation is rejected, so the `NeedsCueForFirstTurn` trait sends a synthetic cue (never on providers that greet unprompted). Turn detection is fixed once audio flows, so mid-call updates carry instructions only. Every call uses `server_vad` at 500 ms; `smart_turn` would force a 2000 ms hold.

## Frontend

`web/CLAUDE.md` is the binding design system; read it before any UI work. i18n: react-i18next, en (default) and zh, no hardcoded user-facing strings. Routes live under `web/src/routes/` (TanStack Router; a parent route needs `<Outlet/>`, or use `_app.section.index.tsx`). Screen guards use `requireRole` in `beforeLoad`.

## Packaging and release

- `deploy/` is the one-command stack (`make stack-up`, `deploy/README.md`): PostgreSQL, this repository's switch image and the app on one compose network. The app is built from the checkout unless `AICC_IMAGE` names a published image.
- A release is two Docker Hub images under one tag, `rasonyang/ai-native-callcenter` and `rasonyang/freeswitch-aicc`, built by `.github/workflows/release.yml`. Nothing else is published.
- `freeswitch/` is the switch: the complete v1.11.3 `conf/` tree (deviations from vanilla recorded in `CONF-DEVIATIONS.md`), `modules.conf`, and a `Dockerfile` that builds FreeSWITCH with `mod_audio_stream`. Nothing site-specific is baked in; the entrypoint injects a deployment's DSNs, addresses and passwords (`DOCKERHUB.md`). A trunk belongs to a deployment; the development machine's is in `deploy/dev/freeswitch/`.
- `AICC_SEED`: `demo` (the stack's default) seeds accounts, queues, published bilingual flows and a week of history; empty (the binary's default) seeds nothing; `fresh` removes exactly what the seed created.

## No performance claims (owner directive)

No capacity or latency figure goes in the README, the deployment guide, a release note or a commit message until the benchmark campaign (L2–L5 in `docs/load-tests.md`) has run on a host that is not a developer laptop, against a switch that is not the development one. The estimates in `docs/design/06-capacity.md` stay inside the design set, and numbers from the load harness's shakedown runs are not results.

Capacity metrics live in `internal/obs/callmetrics.go`: one place for every instrument name (`aicc_turn_latency_ms` in `internal/aicall/metrics.go` is the one exception).

## Related repositories

On the owner's machine; read them for lineage, never import them: `~/workspaces/cc/golang-bot` (SIP/RTP), `~/workspaces/cc/java-bot` (realtime providers, flow DSL), `~/workspaces/cc/cti-server` (domain protocol; its ESL mappings are unverified), `~/workspaces/github/ui-test` (UI). `~/workspaces/github/web-sip-phone` is the browser phone, integrated as-is; changes to it go through the session that owns that repository, never from here.
