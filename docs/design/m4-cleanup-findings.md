# M4 Cleanup Audit — Findings (2026-08-15, addenda to 2026-08-16)

Binds to: [phase1-decisions.md](../phase1-decisions.md). Companion to
[m0-findings.md](m0-findings.md): that one is the evidence record for the M0
verification spike, this one for the drift audit at the end of M4.

Full-repository audit against the hard invariants and the drift categories, with
a definitive verdict per finding. Every REMOVE and REFACTOR verdict was applied
in the commits that follow it, and the addenda record the directives that
arrived while it was being applied.

**Design documents amended as a consequence** — check here before trusting
their original wording: `03-data.md` (`dids.flow_id` is nullable, not `NOT
NULL`, plus the `ON DELETE RESTRICT` of migration 00008), `02-ai-voice.md` §1/
§3/§4 (the provider abstraction, rewritten around A1/A6/A7), `00-overview.md`
§1 (one provider per deployment), and `phase1-decisions.md` A1, A2, A6, A7.

**Scope.** All Go under `cmd/` and `internal/`, the SQL schema and query
sources, the Lua switch scripts, `web/src`, and `docs/`. Excluded by
instruction: sqlc output (`internal/store/queries/*.go`), oapi-codegen output
(`internal/api/api.gen.go`, `web/src/generated/`), `node_modules`, `web/dist`,
`web/src/routeTree.gen.ts`.

**Method.** `deadcode` reachability from `cmd/aicc` and from the test binaries;
per-symbol cross-reference sweeps for Go exports, sqlc queries, frontend
exports and i18n keys; enum cross-check across `docs/openapi.json`, Go
constants, TypeScript and DB `CHECK` constraints; live inspection of the
running dev schema.

---

## 1. Hard invariants

| # | Invariant | Result |
|---|---|---|
| 1 | Bot-first: every DID targets a flow, `flow_id NOT NULL` | **VIOLATED** — F-01 |
| 2 | Enum values SCREAMING_SNAKE and byte-identical across JSON/TS/DB | PASS |
| 3 | Naming per layer (JSON/TS lowerCamel, Go PascalCase + initialisms, DB snake) | PASS |
| 4 | No `tenant_id` or multi-tenant abstraction | PASS — zero occurrences |
| 5 | No absolute local paths in code, imports or docs | PASS — zero occurrences |
| 6 | Server is the single source of truth; no coordination in the extension | PASS — no extension messaging API anywhere in `web/src` |
| 7 | The Go app stays out of the agent WebRTC media path | PASS — agent audio is FreeSWITCH↔browser; Go terminates RTP only for the bot leg |
| 8 | No hardcoded UI strings; backend returns codes, never translated text | PASS |
| 9 | State changes go through the FSMs inside their actors | PASS |
| 10 | Provider connection config must accept a custom endpoint | **VIOLATED** — F-02 |
| 11 | Provider abstraction anchored on OpenAI Realtime; no Qwen leakage above the adapter | PASS |

Notes on the passes that were not trivial:

- **#2** — every enum in the contract is SCREAMING_SNAKE, and each one matches
  its Go constants and its DB `CHECK` list byte for byte (`AgentState`,
  `Availability`, `CallType`, `CallState`, `PartyState`, `PartyRole`,
  `CDRStatus`, `CallbackStatus`, `NotReadyReason`, `ExtensionKind`, `LegKind`,
  `OverflowType`, `Strategy`, `TranscriptKind`, `TranscriptRole`, `Role`,
  `ErrorCode`, `SseEventType`).
- **#3** — the only snake_case JSON tags in the tree are `item_id`, `call_id`,
  `input_tokens`, `output_tokens`, `total_tokens`, all in
  `internal/provider/transport.go`, which is the vendor boundary layer where
  07-naming requires upstream spelling. Correct as written.
- **#8** — `APIError` carries a machine code plus an English developer message;
  `web/src/lib/errors.ts` renders `errors.<CODE>` and never the message. The
  wording lives entirely in the translation files.
- **#11** — no `provider.Qwen*` symbol, dialect field or branch exists outside
  `internal/provider`. `aicall` reads only `Profile.Name` and
  `Profile.FormatsFor`; everything vendor-specific (`Style`,
  `NeedsCueForFirstTurn`, `SemanticTurnType`, `CancelsResponseItself`) is
  consumed inside the adapter.

---

## 2. Findings

### F-01 — `dids.flow_id` is nullable, so a number can exist with no bot — **REFACTOR (partial; the rest is blocked on unbuilt product surface)**

Migration `00003_did_flow_optional.sql` dropped `NOT NULL` because "the flow
catalogue simply does not exist yet"; `00004_flows.sql` created that catalogue
one migration later. A flowless DID fails its bot leg and falls through to
`fallback_queue_ext_number`, which is the direct-to-queue route
`phase1-decisions.md` P4 rules out.

**What restoring `NOT NULL` would take, and why it is not applied.** A flow is
not attached when a number is created — it is attached afterwards, by
`aicc flowadd -file … -did <number>`. The admin screen has no flow field, and
it could not have one: `docs/openapi.json` has no `/flows` path at all, so the
frontend cannot list flows to choose from. Making `flowId` required would
therefore make it impossible to create a number through the product. Closing
this properly needs a flow catalogue read operation and a picker in the DID
form — new product surface, which this pass is not for. Until then, NULL is
the gap between `POST /dids` and `flowadd`, not a routing mode, and that is
what the schema now says.

**What is applied.** Migration `00008` changes `fk_dids_flows` from
`ON DELETE SET NULL` to `ON DELETE RESTRICT`. `SET NULL` was the *silent* path
into the forbidden state: deleting a flow unhooked every number pointing at it
and turned each one into a queue-only number without anyone being told. Under
`RESTRICT` the delete is refused instead. Nothing in the tree deletes a flow
today — `DeleteFlow` was one of the unused queries removed in F-06 — so this
constraint changes no current behaviour; it closes the door before something
walks through it.

Two documents asserted the schema that was never restored, and are corrected
rather than the code being bent to them (see §5): `docs/design/03-data.md:46`
declared `flow_id uuid NOT NULL`, and its M4.8 note stated as fact that "its
`flow_id` is NOT NULL".

**Superseded 2026-08-24 (owner, D8 of TASKS W11).** Restoring `NOT NULL` is no
longer the plan, and the reason it was deferred no longer holds either. The
blocker named above — "`docs/openapi.json` has no `/flows` path at all" — was
closed by W3: `GET /flows` exists and the DID form can pick one. But a stricter
column would now be wrong for a different reason: numbers are gaining a
direction (`allow_inbound` / `allow_outbound`, D7), and **a purely outbound
number has no bot to bind** — `NOT NULL` would forbid a legitimate row. The
constraint becomes `CHECK (NOT allow_inbound OR flow_id IS NOT NULL)`: every
number that can be *called* answers with a flow, which is what P4 actually
rules on. This paragraph's "restore it once the picker exists" is retired.

### F-02 — provider endpoints are hardcoded — **REFACTOR**

`internal/provider/profile.go` pins `wss://api.openai.com/v1/realtime` and
`wss://dashscope.aliyuncs.com/api-ws/v1/realtime` with no override anywhere in
`internal/config`. A deployment behind a gateway, on a regional host, or
against a protocol-compatible server cannot connect at all. This also misses
`phase1-decisions.md` A1 ("selection is a routing-layer rule (never
hardcoded)").

Applied: `provider.Override{Endpoint, Model}` plus
`ProfileForLanguage(language, overrides)`; `AICC_OPENAI_ENDPOINT`,
`AICC_OPENAI_MODEL`, `AICC_QWEN_ENDPOINT`, `AICC_QWEN_MODEL` in config, wired
through `aicall.Config.ProviderOverrides`. Empty keeps the vendor's own value,
so the default path is unchanged.

### F-03 — the half-migrated API wrapper: 40 delegating shims and three parsers — **REFACTOR**

`internal/httpapi/api_server.go` implements all 47 contract operations, but 40
of them are shims that discard the parameters the generated wrapper already
parsed (`func (s *Server) UpdateExtension(w, r, _ uuid.UUID)`) and hand off to a
`handle*` function that parses the same path parameter again. The route table
compounds it: only call control, outbound and agent configuration are mounted
on `wrapper.X`; every other route is mounted on `s.handleX` directly, so those
40 interface methods are never invoked at runtime and exist only to satisfy the
compile-time assertion. Three separate parse-and-reject paths produce the same
400: `writeParamError`, `pathID`, and a bespoke one in
`handleAgentForceLogout`.

Applied: every operation is mounted through `s.apiWrapper()`, and the contract
operation *is* the handler, living in its own domain file under its contract
name (`UpdateExtension` in `catalog_handlers.go`, `GetCDR` in
`ledger_handlers.go`, and so on). `api_server.go` keeps only the interface
assertion, the wrapper constructor and `writeParamError`. `pathID` and the
bespoke agent parser are deleted.

**Path parameters behave exactly as before.** `pathID` and `writeParamError`
already produced the same envelope — 400 `VALIDATION_FAILED` with
`params.field` naming the parameter — and the force-logout parser differed only
in a message string the frontend never reads. Verified live: `GET /cdrs/not-a-uuid`
and `DELETE /dids/zzz` both return that envelope naming `callId` / `didId`.

**Query parameters are now bound by the contract**, which is the one
behavioural difference in this finding: `?limit=abc` or a `queueId` that is not
a uuid is a 400 rather than being silently ignored and defaulted. The hand
parser's leniency there was not asserted by any test, the frontend sends
generated-typed values, and a rejected list request does not retry itself.

**One deliberate exception: the event stream.** `StreamEvents` parses its own
`Last-Event-ID` and is mounted directly rather than on the wrapper. The
wrapper rejects a resume point it cannot parse, and an `EventSource` retries a
rejected request forever with the same header — a mangled value would become a
reconnect loop instead of a stream that starts fresh. The old parser's
leniency was deliberate and test-documented (`garbage`, `whitespace tolerated`
are named cases), so it is preserved and the reason is written where the
handler is. The first attempt at this commit kept the lenient handler but left
the route on the wrapper, which defeated it; the smoke run caught that, not a
unit test.

### F-04 — `presenceResponse` duplicates the generated `api.Presence` — **REFACTOR**

`internal/httpapi/agent_handlers.go` declares a handwritten wire struct with
hand-rolled `2006-01-02T15:04:05Z07:00` formatting for a shape the contract
already defines and oapi-codegen already generates. Applied: mapped onto
`api.Presence`, timestamps via `time.RFC3339`.

### F-05 — dead code reachable from nothing — **REMOVE**

Confirmed unreachable by `deadcode`, or reachable only from the test that
exists to cover them:

| Symbol | Note |
|---|---|
| `obs.Setup` | superseded by `SetupWithLogDir`, which `main` calls; folded into one `Setup` taking `logDir` |
| `flow.Condition.isLeaf` | unreachable including from tests |
| `flow.LoadDir` | flows load from the database via `flowadd`; no production caller |
| `media.GetSamples` / `PutSamples` / `samplePool` | the `[]int16` half of the pool; only `GetBytes`/`PutBytes` are on the hot path |
| `provider.ProfileFor` | superseded by `ProfileForLanguage`; no production caller |
| `telephony.Registry.CreateCall` | one-line wrapper over `CreateCallMinted`; merged into a single `CreateCall(..., isMinted bool)` |

### F-06 — 14 sqlc queries nothing calls — **REMOVE**

`CreateTrunk`, `UpdateTrunk`, `DeleteTrunk`, `ListTrunks` (M4's "trunks apply"
never grew an API or a UI), `GetSetting`, `UpsertSetting`, `PutSetting` (three
accessors for one unused table, two of them the same operation), `DeleteFlow`,
`DeleteUser`, `UpdateUserProfile`, `FindAgentByExtension`,
`GetExtensionByNumber`, `GetQueueByName`, `ListAgentStates`. Removed from
`internal/store/sql/*.sql` and regenerated.

The `trunks` and `settings` **tables** are left in place: dropping a table is
destructive and out of scope for a cleanup pass. They are listed here so the
next schema change can decide.

### F-07 — 12 unused i18n keys — **REMOVE**

`agent.goNotReady`, `agent.myQueue`, `agent.noTranscript`, `agent.queueEmpty`,
`agent.statsLater`, `agent.transcript`, `agent.waitingCount`,
`call.controlsInTopbar`, `common.retry`, `common.search`, `nav.trunks`,
`supervisor.serviceLevel` — referenced from no component, in either locale.
`nav.trunks` is the label of a navigation entry that no longer exists.

### F-08 — `requireSupervisorRole` used inconsistently — **REFACTOR**

The alias exists in `call_handlers.go` and is used on three route groups, while
a fourth spells out `requireRole(auth.RoleSupervisor)`. Made consistent.

---

## 3. Verdicts of KEEP, with the reasoning

These were examined and are **not** drift.

- **The 14 SSE event types that are declared but never published**
  (`PARTY_DIALING`, `CALL_USER_DATA`, `CALL_RECORDING_STARTED`,
  `CALL_RECORDING_STOPPED`, `QUEUE_JOINED`, `QUEUE_LEFT`, `QUEUE_COUNT`,
  `DEVICE_REGISTERED`, `DEVICE_UNREGISTERED`, `BOT_SESSION_STARTED`,
  `BOT_TRANSCRIPT`, `BOT_INTERRUPTED`, `BOT_SESSION_ENDED`, `SYSTEM_LINK`).
  **KEEP** — these are the published contract, catalogued in
  `docs/design/04-api-sse.md` §5 and enumerated in `SseEventType`. They are an
  *implementation gap*, not an orphan: deleting them would be a breaking
  contract change made to match an incomplete implementation, which is the
  inversion of "live behaviour wins". The gap is recorded in §5 below.
- **`isHarnessLeg` in `telephony.Coordinator`.** Reads like test scaffolding in
  production code, but `{aicc_harness=true}` is a designed operational marker
  for scripted verification and seed calls, specified in
  `docs/design/03-data.md` §Harness marker. **KEEP.**
- **`store.CDR` and friends serialized straight to the wire** by the ledger and
  report handlers, against the rule that handlers map explicitly to `api.*`
  types. **KEEP** for now: the shapes are already identical and guarded by
  `TestLedgerTypesMarshalPerTheNamingSpec`, and introducing ~300 lines of
  mapping would add complexity rather than reduce it. Recorded as a layering
  exception to settle when the ledger group next changes.
- **`esl.NewEvent`.** Exported for cross-package test construction and
  documented as such. **KEEP.**
- **Everything cascade- or gateway-shaped.** Searched for and not found: no
  cascade interface, placeholder, stub or TODO exists anywhere in the tree.
- **Test suite quality.** No test asserts on log text or on raw ESL event
  ordering. Every `slog` handler in a test is `io.Discard`. The telephony and
  agents suites assert FSM transitions and published event types; the httpapi
  suite asserts status codes and error envelopes over the real router. **KEEP.**

---

## 4. TODO / FIXME register

Empty. There is no `TODO`, `FIXME`, `XXX`, `HACK`, `WORKAROUND`, `legacy`,
`deprecated` or `compatibility` marker in any Go, TypeScript, SQL or Lua source
in the repository.

---

## 5. Doc bugs and gaps (code is right; the document is not)

1. **`internal/store/migrations/00003_did_flow_optional.sql`** — its rationale
   ("the flow catalogue simply does not exist yet") was already false one
   migration later. Migrations are history and are not edited; `00008` states
   the correction.
2. **`docs/design/03-data.md`** asserted a schema that does not exist: line 46
   declared `flow_id uuid NOT NULL`, and the M4.8 note stated as fact that
   "its `flow_id` is NOT NULL". The live column is nullable and, for the
   reasons in F-01, has to stay that way until the flow catalogue has a read
   operation. Both lines are corrected to describe the schema that runs,
   including the `ON DELETE RESTRICT` that 00008 adds.
3. **`CLAUDE.md` → API contract → "Migration state"** described call control and
   outbound as the only groups on the generated wrapper. F-03 moved every
   group; the paragraph is rewritten to state the rule that now holds — the
   contract operation is the handler, parameters arrive parsed, no delegating
   shims — including the event-stream exception and the remaining `store.*`
   response types in the ledger group.
4. **Implementation gap, not drift:** the 14 SSE event types above are
   specified in `docs/design/04-api-sse.md` §5 and are not emitted. In
   particular the queue events are *observed* — `KindQueueMemberJoined/Left`
   update the CDR's timings in `registry.applySwitchEvent` — but never
   published, so a supervisor wallboard cannot show live queue depth. Left as
   is: implementing them is a feature, not a cleanup.

---

## 6. Verification

Real execution, on this machine, against PostgreSQL 18 in `deploy/dev` and the
FreeSWITCH running natively on the host.

| Gate | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `gofmt -l internal/ cmd/` | clean |
| `go test -race ./...` | pass — 17 packages with tests, 0 failures |
| `go test -bench . -benchmem ./internal/media/` | all 7 frame-path benchmarks still **0 B/op, 0 allocs/op** |
| `sqlc generate` | zero diff after regeneration |
| `make api-lint` | valid, 0 errors, 2 pinned `Sse*Payload` warnings |
| `make api-check` | pass — regenerated Go and TypeScript match the committed files |
| `npm run typecheck` | pass |
| `npx oxlint .` | clean |
| `npm run build` | pass — `web/dist` emitted |
| `npm test` | pass — 43 tests, 3 files |

**Migration 00008 against the live database.** Applied on boot;
`goose_db_version` reaches 8 and `\d dids` reports
`fk_dids_flows … ON DELETE RESTRICT`.

**Smoke run.** Server booted on `127.0.0.1:18099`: migrations ran, the SPA was
served, ESL connected to `127.0.0.1:18021`, agent presence mirrored to the
switch, registrations reconciled. `/healthz` and `/readyz` both 200.

- Signed in as `admin`, then read every migrated group: `auth/me`,
  `system/health`, `extensions`, `queues`, `dids`, `agents`, `users`, `cdrs`,
  `callbacks`, `reports/overview`, `reports/queues`, `reports/daily`, `calls` —
  all 200 with the expected payloads.
- Write round-trip through the wrapper: `POST /dids` → 201, `PUT /dids/{id}` →
  200, `DELETE /dids/{id}` → 204.
- Error paths unchanged: duplicate number → 409 `CONFLICT`, non-digit number →
  422 `VALIDATION_FAILED`, missing CSRF header → 403, agent-only route as an
  admin without an agent profile → 403.
- Parameter binding: `GET /cdrs/not-a-uuid` and `DELETE /dids/zzz` → 400
  `VALIDATION_FAILED` naming `callId` / `didId`, exactly as `pathID` did.
  `?limit=abc` → 400 naming `limit` (the documented change); `?limit=2` →
  2 of 8; `?status=ANSWERED&from=…&to=…` and the report windows all aggregate
  correctly.
- Presence FSM on `api.Presence`: sign in → `NOT_READY/LOGIN`, ready →
  `READY`, not-ready → `NOT_READY/LUNCH`, sign out → `LOGGED_OUT`, with
  `extensionNumber` and `reason` omitted exactly where the handwritten struct
  omitted them and timestamps still at second precision.
- SSE: the stream delivered `AGENT_LOGGED_IN`, `AGENT_READY`,
  `AGENT_LOGGED_OUT` in order with monotonic `seq` and agent scoping; resuming
  with a valid `Last-Event-ID` replayed from that point; resuming past the ring
  produced `SYSTEM_RESET`; `?types=` narrowing still applied.
- The event-stream exception: `Last-Event-ID: not-a-number` returns **200 and a
  stream**, and a whitespace-padded value still resumes. This is the case that
  failed on the first attempt and is why the route no longer goes through the
  wrapper.

Not exercised: a live call through FreeSWITCH end to end, and the two live
provider paths (`AICC_LIVE_PROVIDER_TEST=1`, which spends real API money). No
change in this pass touches the media path, the SIP/RTP stack or the provider
protocol; the only provider change is where the endpoint string comes from,
covered by `TestConnectionDetailsCanBeOverridden`.

---

## Addendum (2026-08-16) — phase-2 provider strategy recorded, abstraction narrowed

Owner directive, now `phase1-decisions.md` **A6**: the cascaded pipeline
(ASR + LLM + TTS) never enters this application. Phase 2 delivers it as a
separate **OpenAI Realtime Gateway** service exposing the Realtime protocol
outward; this app attaches it as one more endpoint. F-02's `provider.Override`
turned out to be exactly that attachment point.

Synced: `02-ai-voice.md` §1/§3/§4 (the "cascade-proof interface" argument is
replaced by "the wire protocol is the extension point; `VoiceSession` is a
seam, not a plug-in"), `00-overview.md` §1, `CLAUDE.md` Architecture, and the
`provider` package/`VoiceSession`/`Override` doc comments, which had argued for
an in-process cascade behind the interface.

Simplified accordingly (all dead, zero behaviour change): `TurnModeNone`
(never selected by anything; its only effect was `turn_detection: null`) and
`EventTypeSessionWarning` (declared, never emitted, never consumed — the
long-call wrap-up in 02 §7 is driven app-side from `Usage`, so it needs no
provider event); their entries in the design's closed sets are removed too.

**One prerequisite the strategy still has, not built here.** Endpoint override
is keyed by *provider profile*, and language → profile is a fixed rule
(`zh` → Qwen, Beta dialect + greeting cue + linear audio). Pointing English at
a gateway is `AICC_OPENAI_ENDPOINT` and nothing else. Pointing *Chinese* at a
GA-dialect gateway is not yet a pure config change: `AICC_QWEN_ENDPOINT` would
still send the Beta-dialect `session.update`, the synthetic cue and 16 k/24 k
PCM. Making "which profile answers a language" configurable (A1 already says
selection is a routing rule, never hardcoded) is the small change that makes
A6's "zero code change" hold for both languages; it is a routing decision, so
it is recorded rather than assumed.

---

## Addendum 2 (2026-08-16) — A1 revised: the provider is a deployment setting

Owner directive. One provider is active per deployment, selected at startup —
Qwen inside mainland China, OpenAI elsewhere, because the two are not both
reachable from one network with acceptable latency. A DID's `language` governs
greeting, prompt language and voice only. **The language→provider mapping is
removed**, and reintroducing one is a regression.

Applied:

- `provider.ProfileForLanguage(language, map[string]Override)` →
  `provider.ProfileFor(name, Override)`, which resolves one profile by name and
  rejects an unknown one. (This function was deleted as dead code in F-05
  yesterday; it returns now because it finally has a caller — the earlier
  removal was correct on the evidence then available.)
- `AICC_OPENAI_ENDPOINT` / `_MODEL` / `AICC_QWEN_ENDPOINT` / `_MODEL` (four
  vars, one per vendor) → `AICC_PROVIDER` plus one `AICC_PROVIDER_ENDPOINT` /
  `AICC_PROVIDER_MODEL` pair. With one provider live, per-vendor slots were
  surface without purpose.
- `aicall.OrchestratorConfig.ProviderOverrides map[string]provider.Override` →
  `Profile provider.Profile`. `runCall` no longer resolves a profile per call;
  `NewOrchestrator` refuses a config without one, so a missing provider is a
  startup error rather than a call that fails when the phone rings.
- `main` resolves the profile once and logs it (`voice provider selected`).
- The contract described `language` as picking the voice provider; corrected in
  `docs/openapi.json` and regenerated, and the same claim fixed in the DID
  domain comment and the admin UI hint in both locales.

Verified live: default boots as `openai/gpt-realtime-2.1`; `AICC_PROVIDER=qwen`
with an endpoint and model override reports exactly those; `AICC_PROVIDER=nonesuch`
refuses to start with `unknown provider "nonesuch" (openai, qwen)`; and with
`AICC_BOT_ENABLED=false` the provider is not resolved at all, so a people-only
deployment needs no valid value.

**A6 restated on this basis.** The gateway becomes another value of
`AICC_PROVIDER`, carrying its own endpoint, model and session dialect,
impersonating no vendor and borrowing no vendor's credential slot — rather
than masquerading as OpenAI behind an endpoint override, which is what the
previous shape would have forced. **No gateway profile ships in phase 1**: the
third value arrives with the service, so that adding it stays a profile
addition and never a redesign.

**Open at the time of writing — since closed, see Addendum 4.** Qwen's English
handling has not been measured. A mainland deployment has no OpenAI to fall
back to, so an English-language DID there is answered by Qwen with its Chinese
voice (`longanqian`) — the profile's voice is per provider, not per language,
and nothing in this change makes it per language. Answering this needs live
calls against the real provider (`AICC_LIVE_PROVIDER_TEST=1`, real credit);
if the quality is not acceptable, the follow-up is a per-language voice and
possibly a per-language model on the profile, which is a design decision rather
than a cleanup.

---

## Addendum 3 (2026-08-16) — voice is bot configuration (A7)

Owner directive, correcting the open item Addendum 2 left. The concern there —
a mainland deployment answering an English DID in a Chinese voice — is not
solved by making voice per language. Both OpenAI Realtime and Qwen-Audio
Realtime offer voices that carry Chinese and English well, so a bilingual bot
wants **one** voice; what was missing is that the voice was not configurable at
all. And it does not belong in `.env`: it is part of the bot's character, so it
lives in the database with the rest of the bot.

Applied: `global.voice` in the flow spec (`internal/flow.Global.Voice`), which
`aicall` passes as `SessionConfig.Voice`. It is therefore stored in `flows`
(draft) and `flow_revisions` (published), versioned and published with the
persona it belongs to — changing a bot's voice goes through publish exactly
like changing its rules. Empty falls back to the provider profile's default, so
every existing flow keeps the voice it has today; the seeded flow names none.

No env var was added. An earlier draft of this change put voice on
`provider.Override` alongside endpoint and model — the reference implementation
does exactly that (`QWEN_AUDIO_VOICE`, `profile.voiceEnv()`) — and that was the
wrong level: endpoint and model describe *where the deployment connects*, voice
describes *who the bot is*.

Covered by tests at both ends: a flow round-trips its voice through load, and
the session update carries it in both dialects (`session.audio.output.voice`
for GA, `session.voice` for the older one), falling back to the profile's when
the flow names none.

**Open at the time of writing — since closed, see Addendum 4:** Qwen's English quality is unmeasured. Choosing a
bilingual voice removes the timbre mismatch, not the question of how well the
model itself handles English; that needs live calls
(`AICC_LIVE_PROVIDER_TEST=1`, real credit).

---

## Addendum 4 (2026-08-16) — the Qwen English question is closed

Verified by the owner on a real call: Qwen's English is good enough. A
mainland deployment can serve English DIDs on Qwen alone, which is what the
revised A1 assumes — one provider per deployment, with no OpenAI to fall back
to inside mainland China.

That closes the item Addenda 2 and 3 carried. Nothing in the code changes: A1
never routed by language, and A7 already let a bilingual bot pick one voice
that carries both. What changes is that the assumption underneath both is now
measured rather than hoped for, and `phase1-decisions.md` A1 records it as
live-verified rather than open.

No open items remain from this audit.
