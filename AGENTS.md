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
go test -race ./...                               # always -race (`make test` runs this, then the web tests)
go test -race -run TestName ./internal/voice/     # one test
make test-stress PKG=./internal/x RUN=TestY       # -race -count=20 -cpu 1,2,8 on 4 CPUs kept busy as on CI (CONTRIBUTING.md)
go test -run XXX -bench . -benchmem ./internal/media/ ./internal/aicall/  # hot paths: 0 allocs/op, CI fails otherwise
make lint                                         # go vet + gofmt + oxlint
sqlc generate                                     # after editing internal/store/sql/*.sql or a migration (pinned version: SQLC_VERSION in Makefile)
make sqlc-check                                   # the CI gate: regenerate + empty git diff
# migrations: add internal/store/migrations/NNNNN_name.sql (goose); they run at server startup

# dev server; prerequisites, order and ports: docs/dev-stack.md
make dev-up                                       # PostgreSQL 18 + SeaweedFS containers
deploy/dev/restart.sh                             # build web/dist and /tmp/aicc, restart, wait until it serves
/tmp/aicc useradd -username admin -password … -role ADMIN
/tmp/aicc flowadd -file internal/seed/flows/x.json -did 95001   # load + publish; same slug = update + republish
/tmp/aicc flowsim -flow internal/seed/flows/x.json -scenario internal/seed/scenarios/x.json   # deterministic scenario replay on the real engine (docs/flow-authoring.md section 8); go test ./internal/flowsim runs every seed flow
go run ./cmd/aicc-mockbackend                     # business APIs the reference flows call (127.0.0.1:8770); the app uses it when AICC_BOT_BACKEND_BASE points there
# logs: stderr and logs/aicc-<starttime>.log (read the file to analyse a run)

# live provider tests: real money (OPENAI_/XAI_/ALIYUN_/DOUBAO_API_KEY)
AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run Live -v ./internal/provider/...

cd web && npm run dev                             # Vite on 5173
cd web && npm run build                           # web/dist, embedded via go:embed
cd web && npm run test                            # vitest
cd web && npm run test:browser                    # layout tests in headless Chromium (make test-browser; once: npx playwright install chromium)

make stack-up                                     # the whole product in containers, seeded (deploy/README.md)
make e2e-up && make e2e-smoke; make e2e-down    # isolated outbound-call smoke test: internal network, mock provider, callee simulator (deploy/e2e/README.md)
```

Load harness: `docs/load-tests.md`.

A PR that adds or changes a test with goroutines, fakes, sockets or timers runs `make test-stress` on the touched packages and the injected-sleep check, and its description says so (`CONTRIBUTING.md`).

## Database and migrations

PostgreSQL runs in a container (`make dev-up`), never as a host install. Database tests (store, seed, httpapi) skip unless `AICC_TEST_DATABASE_URL` is set; CI sets it.

```sh
AICC_TEST_DATABASE_URL='postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable' go test -race ./internal/store/
```

**A migration is not reviewed until it has run.** The store tests apply every migration from zero, roll them all back, re-apply them, and migrate databases that already hold rows. A migration that narrows a CHECK must rewrite existing rows before installing the constraint, or PostgreSQL rejects it ("is violated by some row") on every deployment with history. A migration that changes an enum's allowed values gets a fixture test in `migrate_test.go` or a sibling `migrate_*_test.go`: migrate to the previous version, insert old-shape rows, migrate up, assert.

## Config

`AICC_*` environment variables; `.env` in the working directory is loaded and the real environment wins. `.env.example` is the registry of every setting with its real default, kept in step with `internal/config/config.go`. An empty value means unset, so a non-empty default cannot be blanked. There is no inline-comment syntax. Provider API keys are not `AICC_*`: `OPENAI_API_KEY`, `XAI_API_KEY` (grok), `ALIYUN_API_KEY`, `REALTIME_API_KEY` (gateway), `DOUBAO_API_KEY`. ESL is at `127.0.0.1:18021`, not the stock 8021.

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
- sqlc models and `internal/store` types are never API types; handlers map to `api.*` at the boundary. Groups that still write `store.*` or domain types straight out move onto `api.*` when they next change.
- SSE: the envelope is `SseEvent`; stable payload shapes are `Sse*Payload` components, lint-ignored as "unused". No AsyncAPI.
- Every route is mounted through the generated wrapper (`s.apiWrapper()`), and the contract operation is the handler, in the file that owns its subject (`UpdateExtension` in `catalog_handlers.go`). Parameters arrive parsed: never re-read a raw parameter, never add a delegating shim. `api_server.go` holds only the interface assertion, the wrapper and `writeParamError`. One exception: `StreamEvents` (`/events`) is mounted by hand and parses its own parameters, because an EventSource retries a rejected request forever and a mangled resume point must degrade to a fresh stream, not a reconnect loop.

## Architecture

Two telephony paths in one process; the AI path can be switched off (`AICC_BOT_ENABLED`).

Topology: FreeSWITCH always sits on the LAN and reaches the public network only through an edge proxy or SBC (OpenSIPS, Kamailio, an SBC). The app is on the LAN beside the switch, so nothing sits between the switch and the bot: the bot's INVITE comes straight from the `aicc_bot` gateway. A proxy between the switch and the app is not a supported topology; the record-routing hop that matters is carrier ↔ edge ↔ switch.

1. **Human path.** `esl.Link` (one ESL inbound connection, auto-reconnecting) → `telephony.Normalize` (the only place raw ESL events become domain `SwitchEvent`s; raw FreeSWITCH events are read only inside `internal/telephony`) → `telephony.Coordinator` + `Registry` (an actor per call: one goroutine mutates it, snapshots go through its mailbox), `agents.Service` (presence FSM, mirrored into mod_callcenter) and `outbound.Service`. Every api/bgapi command to the switch is a `*telephony.Adapter` method. Prefer `bgapi`, with the result taken from its `BACKGROUND_JOB`: a blocking `api` runs on the switch's ESL listener thread and holds the one connection the events share, and a `BACKGROUND_JOB` fires only after the command has finished (an originate's `+OK` means the originate has let go of the leg, #333). `bgapi` gives no ordering, because each job runs on its own thread: commands that must apply in order (`uuid_setvar_multi` before `uuid_record`, `callcenter_config agent add` before its `set`s) await each job's result before the next is sent, or stay on `api`. A read whose caller needs the reply inline (`uuid_getvar`, `show channels`) stays on `api`. Which writes still block the link: #407.
2. **AI path.** FreeSWITCH bridges the caller to the `aicc_bot` gateway → `voice.UAS` (SIP on :6060, RTP 26000–29999) → `aicall.Session` bridges the leg to a `provider.VoiceSession` (OpenAI: G.711 passthrough; the other engines: PCM16 through `media.Converter`) while `flow.Engine`/`Runtime` steer phases and tools. `aicall.Orchestrator` resolves DID → flow (published revision only) and transfers by `uuid_transfer`-ing the caller's channel (named in the `X-AICC-Channel-ID` SIP header) to a queue extension.

### Providers (`docs/provider-extension.md`)

- **One provider per deployment, chosen at startup** (A1): `AICC_PROVIDER` = `openai` | `grok` | `qwen` | `gateway` | `doubao`, with `AICC_PROVIDER_ENDPOINT` / `AICC_PROVIDER_MODEL`, resolved once by `provider.ProfileFor`. A call's `language` sets the greeting and the prompt language; it never selects the provider or the voice. A language→provider mapping is a regression.
- **The extension point is the wire protocol, not Go** (A6). A new engine on the OpenAI Realtime protocol is a new `Profile` and a new `AICC_PROVIDER` value, never a new client; `gateway` (a separate service composing ASR + LLM + TTS behind Realtime events) attached that way. Cascade never enters this repo.
- Only a new protocol earns a client. There are two: `provider.Realtime` (openai, grok, qwen, gateway) and `internal/provider/doubao` (reasons in `docs/design/doubao-findings.md`). A new client lives in its own sub-package behind `provider.VoiceSession` and shares only transport and audio plumbing (`wsconn`, `Watchdog`, `MergeHint`, `SayExactly`, `pacer`), never a protocol event, decoder or dispatch. Do not generalise `VoiceSession`. The name→client choice is in `cmd/aicc/wiring.go`.

### Events

The domain model is Genesys-lineage: a Call aggregates Parties; leg events are `PARTY_*`, call-scoped events `CALL_*`; `CallType` is stamped at creation and never changes, even across transfers. `events.Hub` delivers to browsers: a global sequence from DB-reserved blocks, an in-memory ring for `Last-Event-ID` resume, `SYSTEM_RESET` when the resume point has left the ring, slow consumers disconnected. REST + SSE only; no application WebSocket (`internal/streamin` only ingests the switch's audio).

- **A leg event goes only to the agent whose leg it is** (owner directive). An agent's stream carries their own party's `PARTY_*` states and nobody else's, not a colleague's and not the customer's; otherwise a cockpit cannot tell which party is its own. A party with no agent reaches no agent's stream (it is in the CDR and the supervisor view). `CALL_*` events reach every agent on the call; supervisors and administrators see everything. **Never widen a party's scope to rescue a missing event**: a customer hanging up ends the agent's leg too, so if the agent's own `PARTY_RELEASED` does not arrive, that is the defect (`TestTheAgentIsToldTheirOwnLegEnded`).
- **A state event means a state changed.** The party FSM logs and drops an illegal or duplicate transition (`transition`); re-bridging a leg that is already TALKING publishes nothing (`establish`); `CALL_USER_DATA` is silent when nothing moved (`UserDataChange.IsEmpty`). Never publish an event to make a client refresh: if a screen is stale, the missing event is the defect. Two `PARTY_ESTABLISHED` on one call are two parties; check the `partyId`.

### FreeSWITCH configuration

FreeSWITCH gets its directory and `callcenter.conf` from PostgreSQL through mod_lua's XML handler; the Lua actions of its static dialplan (`freeswitch/scripts/*.lua`) query PostgreSQL through mod_pgsql on each call. The Go↔Lua contract is the `luacc.*` views (jsonb flattened; mod_lua has no JSON parser), read by a confined role (`deploy/sql/lua_role.sql`). mod_callcenter uses a dedicated `aicc_fs` database, because its tables are unqualified in `public` and would collide.

### Flow DSL (`internal/flow`; fields in `spec.go`, design in `docs/design/02-ai-voice.md` §6 and §7)

- The model owns the conversation; the flow owns the phase. Phases carry instructions and tool allowlists. The tools of one response are judged in the phase it was made in (the last move wins), and a move's `announce` waits for that response to end. Everything checkable is validated at load, not mid-call.
- The flow owns the bot's voice (`global.voice`, versioned with the persona, A7), never an env var; empty falls back to the profile's voice.
- A phase's `announce` is a line said as written (bilingual, `{slots.x}` rendered): `SessionConfig.OpeningText` for the entry phase, `VoiceSession.SpeakText` after it. A line pre-empts and never queues. It is exact on doubao and best effort elsewhere. How each engine delivers a closing line is a profile trait (`NeedsDirectedLineInConversation` and `RequiresTerminalAnnounce`, the one capability that also decides whether a tool result carries the closing line; table in `docs/provider-extension.md`). A deployment's rule (doubao's `RequiresTerminalAnnounce`) is enforced at publish, not at load.
- A built-in's refusal (a closed queue, a failed save) is a conversation, not an error.
- A caller's decline is invisible to the engine. So `global.closingTarget` ends a call after three consecutive NO_INPUTs where no authored rule can fire on NO_INPUT, or when bot replies without a tool call exceed `global.maxTurnsWithoutTool`, through the same arm-then-speak terminal path as any other ending.
- Dead air after a caller turn the engine dropped is not silence when the turn was transcribed with words: the session marks that NO_INPUT (`IsCallerUnanswered`, the turn's words in `Text`), the flow is told nothing (no NO_INPUT rule, no count, no unhung-farewell close) and the model is asked to answer, in one cue with a standing unbacked claim's (#448). Without words it stays a silence.
- A model that says goodbye in words and makes no tool call is not asked again: its goodbye is the goodbye (#453). An unhung `FAREWELL` that played in full and was not talked over ends the call after 2 s of caller silence (a speech start, a keypress or a transcript cancels it), and the second one in a row ends it as soon as it has played. The engine still moves to `global.closingTarget` (record and final node as ever), but the target's `announce` and the goodbye cue are not said, on campaign calls, doubao, terminal phases and flows without a target too; the 8 s dead-air backstop closes the same way. A turn that ends in a question is not a goodbye. Not over an armed ending, a settling handover or a standing claim.
- `isCallerNext` on a phase says the caller speaks next. A slot tool the model called in a response that already spoke has its result sent without asking for a turn (`provider.QuietToolAnswerer`, an optional interface `*provider.Realtime` implements and doubao does not), when its whole batch is slot tools with `ok:1` and ends in such a phase, with no line owed and nothing armed. It is decided per batch at the response's TURN_DONE (`closeToolBatch`): answers are held on the batch until then, and any other answer releases them the ordinary way first. A quiet result claims no floor, does not mark the result unheard, and clears `isToolReplyNext` in the engine and the claim watch so the caller's next reply counts towards `maxTurnsWithoutTool` and is judged for claims. Load rejects the field on a terminal phase and on one with an `announce`. A flow without it behaves as before; `lead_qualification` relies on the follow-up turn and must not declare it.
- A `hangup` made in a response that has already answered the caller is refused as a conversation, once per caller turn: the answer plays on and nothing is armed (#484, `internal/aicall/answer_guard.go`). On a quiet-answering provider the refusal never asks for a turn: it is held to the response's TURN_DONE and sent quietly whenever the batch can go quiet, with the text for a refusal read with the caller's next words. A statement-ending answer followed by caller silence gets one dedicated dead-air cue (ask only whether anything else is needed, do not repeat the answer) in place of the silent-caller cue; the NO_INPUT counts as any other, and caller words, a keypress, a barge-in, another bot turn, a move or an ending drop it (#555).
- `global.maxDurationSec` (absent 900, `0` off, else 60–3600) is the call's wall clock from the bot answering, owned by the orchestrator: at 80% a wrap-up line joins the standing instructions (never a text cue); at 100% the DID's queue, if enabled, gets `transfer_to_agent` with `reason=SESSION_LIMIT`, else `global.closingTarget`. It never overrides an armed ending, and it locks the one it chooses. Never contained. CDR: a call the bot ends (`global.closingTarget`, or the caller hanging up before the handover) has `hangupCause=SESSION_LIMIT`; a transferred call has `userData.botReason=SESSION_LIMIT`, and its `hangupCause` is the final leg's, how the whole call ended.

## Invariants from live debugging: do not regress

- **Turn end ≠ playback end.** `TURN_DONE` means the model stopped producing; `PLAYBACK_DONE` means the caller heard it. An armed transfer or hangup executes after the caller heard its turn (gated on turn identity), or when the caller speaks after a later turn has finished generating; the cap is 10 s from arming, moved out once the line has finished generating to cover the audio still queued plus 2 s (never earlier). A turn cut short never counts as the closing line having been said, except a cut inside the final sentence of a line that had finished generating (estimated from its share of the text) and every cut from the second interruption on, after which the running cap is neither restarted nor moved (#465). An interruption counts whichever side reported it (the session's barge-in or the provider's own cancellation, one cut once) and whether or not the line had finished generating; a cut of a line still being generated never runs the armed action (#493).
- **A one-word acknowledgement does not take the floor** (#403, `docs/design/11-backchannels.md`). Where the client controls interruption (`Profile.ControlsInterruption`, openai only) speech over audible playback is held, not cut: no flush, `Interrupt`, `BARGE_IN`, `CALLER_SPEECH` or dead-air change until its transcript says it is words (or 2000 ms pass, or a key is pressed); an acknowledgement from the built-in per-language list (`classifyBackchannel`) is declined, unanswered, recorded flagged `IsBackchannel` and never reaches `OnCallerSpoke`, the farewell watch or `timer.onCallerWords`. The client then asks for every caller turn itself (`AnswerCallerTurn`, after the commit), so the caller-turn re-ask must never be armed there. Engines that cut by themselves keep it, and an acknowledged cut cannot move the flow (a moving tool is refused as a conversation) or restart an armed closing line (it counts as the second interruption of #465); once the line counts as said an acknowledged cut runs the armed action at once, so the model's reply to it is never heard. Measured milliseconds stay in the design doc, never in release text.
- **Barge-in's boundary is the last frame heard.** Speech over a finished turn's still-playing tail is an interruption: flush locally and trim the history (`conversation.item.truncate`), but never send `response.cancel` after generation has ended; the providers reject it.
- The drain watch and the dead-air timer use **separate** generation counters; sharing one made every goodbye end on the grace cap.
- An interrupt flushes the local TX queue first, then tells the provider; played time = frames queued − frames flushed. Detected speech inside the 800 ms barge guard is ignored (line echo).
- Codec wiring is all-or-nothing per law (PCMU and PCMA both fully wired). Frames stay G.711 through the RTP session; the OpenAI path is byte passthrough. Per-frame paths (`internal/media`, the framer in `internal/aicall`) stay at 0 allocs/op.
- **The AI leg's uplink is a 20 ms wall clock.** The provider gets exactly one frame per 20 ms of elapsed time (`uplinkClock` in `internal/aicall`): the oldest queued real frame, else silence in its input format; the backlog is bounded at 100 ms and drops the oldest, never catching up faster than real time; nothing is sent while hushed; `RTPDeadTimeout` and first media stay on real packets, never on fills.
- **Echo suppression sits after the echo measurement and is off by default (#539).** The measurement, its interruption verdicts (`isEchoLikely`) and the dump see the raw uplink: the suppressor judges a slot only after `FeedUplink` has taken it, and changes only the frame `feedSlot` is about to send. With `AICC_BOT_ECHO_SUPPRESS` off the provider gets the same bytes as before on every path, the G.711 passthrough included. A suppressed slot is silence in the leg's law (faded at the gate's edges), never a dropped slot: the provider still gets one frame per 20 ms, fills and hushed slots are untouched, and media liveness stays on the RTP session. A suppressor failure turns it off for that call and passes the uplink unchanged; it never blocks or ends the call.
- ESL: transfer with `uuid_transfer <uuid> 'm:^:bridge:{…}<endpoint>' inline` (`m:^:` stops a comma inside `{…}` from splitting the action), never originate + `uuid_bridge`. Browser phones get `uuid_phone_event talk|hold`, never `uuid_answer`. A codec pin rides the new leg's dial string: one codec on ESL paths (`{absolute_codec_string=PCMU}`), never on a loopback leg; the Lua dialplan's `bridge` pins `PCMU,PCMA`.
- The switch reaches the bot through the `aicc_bot` gateway at `$${aicc_bot_host}`, which must be an IP address the switch can send to: `$${local_ip_v4}` on a native install, the app's fixed `AICC_APP_IP` in the compose stack, the host's LAN address under host networking (`deploy/compose.linux.yml`), the shared namespace's own address on macOS (`deploy/compose.macos.yml`). Never loopback (a LAN-bound macOS socket cannot send to 127.0.0.1), and never a `sip:` scheme in the variable.
- **An abandoned response is cancelled, and it still holds the floor.** When the watchdog gives a response up it sends `response.cancel` and keeps the floor until the provider's `response.done`, a refused cancel, a new `response.created` or 3 s (qwen held one for 20 s and refused every request meanwhile, #310). A response nothing of which was heard owes the caller's turn, asked for once per caller turn, and does not count as answering them (#448's path takes the dead air after it). A caller turn with words that no `response.created` follows within 3 s of `speech_stopped` is asked for once, and qwen's "Server VAD turn committed but no response was created" owes the turn (#197). All of them ride the one owed-turn flag with tool results and cues: one turn, one `response.create`.
- Qwen: `response.create` on an empty conversation is rejected, so the `NeedsCueForFirstTurn` trait sends a synthetic cue (never on providers that greet unprompted). Turn detection is fixed once audio flows, so mid-call updates carry instructions only. Every call uses `server_vad` at 500 ms; semantic turn detection is not offered (qwen's `smart_turn` would force a 2000 ms hold).

## Frontend

Node 24 / npm 11 own `web/package-lock.json`: CI and the image install with them and `devEngines` rejects another npm major (`CONTRIBUTING.md`, #389). `web/CLAUDE.md` is the binding design system; read it before any UI work. i18n: react-i18next, en (default) and zh, no hardcoded user-facing strings. Routes live under `web/src/routes/` (TanStack Router; a parent route needs `<Outlet/>`, or use `_app.section.index.tsx`). Screen guards use `requireRole` in `beforeLoad`.

## Packaging and release

- `deploy/` is the one-command stack (`make stack-up`, `deploy/README.md`): PostgreSQL, this repository's switch image and the app on one compose network. The app is built from the checkout unless `AICC_IMAGE` names a published image. `deploy/install.sh` is the one-line installer: it installs a release's bundle with the published images and runs `aicc doctor` (`make installer-check` runs shellcheck, its unit tests, the compose validation and a dry release bundle). Two overlays change the topology: `compose.linux.yml` puts the app and the switch on the host network, `compose.macos.yml` puts them in one shared network namespace (`make stack-config` validates all combinations).
- A release is two Docker Hub images under one tag, `rasonyang/ai-native-callcenter` and `rasonyang/freeswitch-aicc`, built by `.github/workflows/release.yml`, plus three assets on the GitHub release of the same tag, built by `scripts/release-bundle.sh`: `install.sh`, `aicc-deploy-<tag>.tar.gz` and `checksums.txt`. Nothing else is published.
- `freeswitch/` is the switch: the complete v1.11.3 `conf/` tree (deviations from vanilla recorded in `CONF-DEVIATIONS.md`), `modules.conf`, and a `Dockerfile` that builds FreeSWITCH with `mod_audio_stream`. Nothing site-specific is baked in; the entrypoint injects a deployment's DSNs, addresses and passwords. A trunk belongs to a deployment; the development machine's is in `deploy/dev/freeswitch/`.
- `AICC_SEED`: `demo` (the stack's default) seeds accounts, queues, published bilingual flows and a week of history; empty (the binary's default) seeds nothing; `fresh` removes exactly what the seed created. `AICC_SEED_PASSWORD` is the password of the accounts and extensions the seed creates; the seed never overwrites an existing account.

## No performance claims (owner directive)

No capacity or latency figure goes in the README, the deployment guide, a release note or a commit message until the benchmark campaign (L2–L5 in `docs/load-tests.md`) has run on a host that is not a developer laptop, against a switch that is not the development one. The estimates in `docs/design/06-capacity.md` stay inside the design set, and numbers from the load harness's shakedown runs are not results.

Capacity metrics live in `internal/obs/callmetrics.go`: one place for every instrument name.

## Related repositories

On the owner's machine; read them for lineage, never import them: `~/workspaces/cc/golang-bot` (SIP/RTP), `~/workspaces/cc/java-bot` (realtime providers, flow DSL), `~/workspaces/cc/cti-server` (domain protocol; its ESL mappings are unverified), `~/workspaces/github/ui-test` (UI). `~/workspaces/github/web-sip-phone` is the browser phone, integrated as-is; changes to it go through the session that owns that repository, never from here.
