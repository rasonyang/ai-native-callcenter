# Documentation audit — where the docs and the code disagree

Date: 2026-10-30 · Scope: every `.md` under `docs/` plus `README.md`,
`CONTRIBUTING.md`, `AGENTS.md`/`CLAUDE.md` · Method: read each document and check
every checkable claim against the tree — environment variables, file paths, HTTP
operations, database objects, enum values, metric names, make targets, CI jobs and
Go symbols. `CHANGELOG.md` is a historical record and was not audited.

> **Status: applied on 2026-10-30.** This file is the audit trail, not a to-do
> list.
>
> - **Deleted:** `phase0-research.md` and `m4-cleanup-findings.md` — the one the
>   table marks ARCHIVE was deleted rather than moved, because nothing in it is
>   the only copy. `08-transcription.md`'s 3,151-line gap analysis was replaced
>   by a short as-built note; its §B.5 is kept there because
>   `freeswitch/docker-entrypoint.sh` names it in a comment.
> - **Corrected in place:** `00`, `01`, `02`, `03`, `04`, `05`, `07`, `10`,
>   `phase1-decisions`, `dev-stack`, `grok-findings`; `freeswitch/README.md`
>   lost its reference to the deleted transcription document, and one
>   `.env.example` comment (the switch's RTP range) was corrected.
> - Every relative link in the tree still resolves.
>
> The line numbers below describe the tree **before** those edits; a citation
> that no longer matches is expected, and the quoted sentence is the
> authoritative half of each row.

The purpose is a **deletion list**: which documents can go, which must be fixed
before they can stay, and which statements a reader would currently get wrong.
Nothing here is a style review.

Verdicts:

| Verdict | Meaning |
|---|---|
| **KEEP** | Accurate enough to be read as current; no material mismatch found. |
| **FIX** | Still useful, but carries statements the code contradicts. |
| **ARCHIVE** | Accurate only as a dated record of work already done; must stop being read as a description of today. |
| **DELETE** | Superseded, and every living fact in it exists somewhere authoritative. |

Counts below are from `docs/openapi.json`, `internal/store/migrations/`,
`internal/config/config.go`, `docs/auth/scopemap.py` and the tree on `main` at
`d3abd07`.

---

## 1. Verdict table

| Document | Lines | Verdict | Findings |
|---|---:|---|---:|
| `README.md` / `README.zh-CN.md` | 164 / 145 | **KEEP** | 0 |
| `CONTRIBUTING.md` | 91 | **KEEP** | 0 |
| `AGENTS.md` / `CLAUDE.md` | 160 | **KEEP** | 0 (they are the current canon; two documents contradict *them*, see 02 §5 and `phase1-decisions`) |
| `docs/design/00-overview.md` | 117 | **FIX** | 9 |
| `docs/design/01-telephony.md` | 228 | **FIX** | 8 |
| `docs/design/02-ai-voice.md` | 198 | **FIX** | 5 |
| `docs/design/03-data.md` | 326 | **FIX** | 7 |
| `docs/design/04-api-sse.md` | 105 | **FIX** | 9 |
| `docs/design/05-frontend.md` | 49 | **FIX** | 4 |
| `docs/design/06-capacity.md` | 77 | **KEEP** (estimates, self-annotated) | 0 |
| `docs/design/07-naming.md` | 80 | **FIX** | 4 |
| `docs/design/08-transcription.md` | 3151 | **ARCHIVE** | 6 |
| `docs/design/09-webhooks.md` | 554 | **KEEP** | 0 |
| `docs/design/10-campaigns.md` | 1273 | **FIX** | 4 |
| `docs/design/gemini-findings.md` | 64 | **KEEP** (archive note) | 0 |
| `docs/design/grok-findings.md` | 261 | **FIX** (header only) | 1 |
| `docs/design/m0-findings.md` | 102 | **KEEP** (dated evidence) | 0 |
| `docs/design/m4-cleanup-findings.md` | 500 | **ARCHIVE** | 2 |
| `docs/design/m5-findings.md` | 242 | **KEEP** (dated evidence) | 0 |
| `docs/design/qwen-findings.md` | 1506 | **KEEP** (dated evidence) | 0 |
| `docs/design/doubao-findings.md` | 755 | **KEEP** (dated evidence) | 0 |
| `docs/dev-stack.md` | 194 | **FIX** | 1 |
| `docs/flow-authoring.md` | 784 | **KEEP** | 0 |
| `docs/install-verification.md` | 374 | **KEEP** | 0 |
| `docs/load-tests.md` | 206 | **KEEP** | 0 |
| `docs/phase0-research.md` | 206 | **DELETE** (or archive) | 3 |
| `docs/phase1-decisions.md` | 75 | **FIX** | 5 |
| `docs/provider-extension.md` | 371 | **KEEP** | 0 |
| `docs/web-sip-phone-protocol.md` | 239 | **KEEP** | 0 |

A "finding" is one statement a reader would act on wrongly. Several documents
have many more **cosmetic** drifts (line-number citations into files that have
since grown, symbols renamed inside a historical quote); those are grouped under
§4 instead of being counted one by one.

---

## 2. The removal list

Ordered by how much a reader is misled today, not by size.

### 2.1 Delete outright

**`docs/phase0-research.md`** — Phase 0 notes about six *other* repositories and
a FreeSWITCH box as it was on 2026-08-13. It states design facts the project then
rejected:

- L20: the call FSM is `CREATED → RUNNING → ENDING → ENDED`. `ENDING` was never
  assigned and was removed on 2026-08-24 (design 01 §2, "C4"); the contract's
  `CallState` has three values: `CREATED, RUNNING, ENDED`.
- L20: after-call work is "ACW auto-timer per agent (`wrap_up_time_s`)". The
  column was dropped in migration `00012`; ACW now has no deadline and is ended
  by the agent confirming the record (01 §2, `internal/telephony/aftercallwork*`).
- L90: the provider layer is `RealtimeProvider` / `OpenAiCompatRealtimeProvider`
  with `SessionStyle`/`ToolStyle`. Those are **java-bot's** symbols; this repo
  has `provider.VoiceSession` + `provider.Profile` and no such types
  (`rg 'SessionStyle|ToolStyle|RealtimeProvider' internal/` → no match).
- It also still describes `local6060` and a slim FreeSWITCH without
  mod_callcenter, both superseded (T2/T6).

Nothing in it is the only copy of anything: the same facts are in
`m0-findings.md` (verified) or in the decision register.

### 2.2 Archive — **done as deletion** (nothing here was the only copy)

**`docs/design/08-transcription.md`** — 3,151 lines of gap analysis for a
feature that shipped. It carried a reading note in §4 ("records the tree as the
gap analysis found it"), but only §4 — §10, §11, §12.1 and §13 still read as
current descriptions and are wrong (§3.8 below). **Applied:** the body was
deleted and replaced by a short as-built note; §B.5 (the module guard) is kept
because `freeswitch/docker-entrypoint.sh` names it.

**`docs/design/m4-cleanup-findings.md`** — the M4 drift audit. Its verdicts were
applied; as a record it is honest, but two of its load-bearing claims are now
false (§3.11). Keep only if the project wants a history of the audit.

**`docs/design/00-overview.md` §6–§7** — the milestone list and risk register.
Every milestone is done; "M2 — human telephony", "M3 — AI voice leg" read as
future work. Either delete those two sections or label them "historical plan".

### 2.3 Fix, then keep

`00`, `01`, `02`, `03`, `04`, `05`, `07`, `10`, `phase1-decisions`, `dev-stack`,
`grok-findings` — see §3. `04` deserves a special mention: it is a **second copy
of the contract** (operation list, event catalog, error codes, scope counts), and
every one of those copies has drifted. The contract itself is served at
`GET /openapi.json` and generated into `internal/api`, so the correct fix is to
delete the duplicated inventories from 04 and keep only the rationale that the
contract cannot carry.

### 2.4 Facts that exist only in a document you are about to delete

Move these into `AGENTS.md` (or the contract) first:

- **After-call work rules** (01 §2, 05 §2): no timer; the platform opens an
  unconfirmed `wrap_ups` row with the default disposition the moment ACW begins;
  the agent confirms; a finished call always has a record; the cockpit greys
  *Go ready* and the dialler while unconfirmed. `AGENTS.md` does not mention
  ACW at all — this is the one owner directive that lives only in design docs.
- **CDR ownership and containment** (03 §3, 03 §6): which path writes the row
  (`botJoined` / `isAgentPlaced` / `Call.IsBotAbandoned`), and that reaching a
  terminal phase contains the call exactly like `hangup`. Only in 03.
- **Party FSM edge rules** (01 §2): `PARTY_ESTABLISHED` is `CHANNEL_BRIDGE`, not
  `CHANNEL_ANSWER`; `Dialing → Ringing` is forbidden; `Queued` is not a state.
  The table exists in code and tests, but the *reasons* only in 01.
- **The seed's cast, numbers and password rules** (03 §6): `demoFlows` in
  `internal/seed/seed.go` is authoritative, but the "why" (assigned 555-01XX
  block, one main line, `AICC_SEED=fresh` deleting exactly `demoNumbers()`)
  lives in 03.
- **The capacity estimates** (06): deliberately kept in the design set and
  nowhere else; do not delete 06 while the no-performance-claims directive
  stands.

---

## 3. Findings, document by document

Line references are to the file at `main` `d3abd07`. "Contract" means
`docs/openapi.json`; "generated" means `internal/api/api.gen.go`. Rows marked
**Keep** are confirmations — a statement often read as wrong that the code backs
— and are included so a later edit does not "fix" a correct sentence.

### 3.1 `docs/design/00-overview.md` — 9 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 0-1 | 20 | Diagram: bot RTP `40000-999` | `40000–40999` | `.env.example` `AICC_BOT_RTP_PORT_LOW=40000`, `_HIGH=40999`; `design 02` §1 says 40999 |
| 0-2 | 15, 39 | One provider per deployment is "qwen or openai" | Five values: `openai`, `grok`, `qwen`, `gateway`, `doubao`; `doubao` selects a different client | `internal/provider/profile.go` `NameOpenAI/NameQwen/NameGrok/NameGateway/NameDoubao`; `cmd/aicc/wiring.go` |
| 0-3 | 50–83 | Module map lists 29 packages | 15 more exist and are load-bearing: `campaign`, `dialer`, `postcall`, `sipsession`, `wrapupdraft`, `phonenum`, `dayrange`, `egress`, `textllm`, `switchsyntax`, `flowsim`, `calleesim`, `mockbackend`, `e2e`, `testdb` | `ls internal` |
| 0-4 | 60 | `internal/provider` profiles are "(openai, qwen, gateway)" | list omits `grok` | `profile.go:451-462` |
| 0-5 | 72 | `internal/auth` holds "`Role` and the `role → scopes` grant map" | The map is in `internal/httpapi/grants.go` (`grantedScopes`, `agentScopes`, `supervisorScopes`); `internal/auth` defines `Role`, `roleRank` and `Valid` and nothing about scopes | `internal/httpapi/grants.go:27-69`; `internal/auth/auth.go:22-35` |
| 0-6 | 88 | Build-time dependency: "sqlc, golangci-lint" | sqlc yes; **golangci-lint is used nowhere** — no `.golangci.yml`, no invocation in `Makefile` or CI | `rg golangci` → only docs; `make lint` = vet + gofmt + oxlint |
| 0-7 | 95 | Error envelope example uses `QUEUE_NOT_FOUND` | Not an `ErrorCode`; the enum is 40 values and has no such code | contract `ErrorCode`; 04 §5 itself says `QUEUE_NOT_FOUND` was "guessed and never needed" |
| 0-8 | 98 | "spans on REST handlers, **ESL commands, provider turns**, DB via **pgx tracer**" | Only the HTTP handler is traced (`otelhttp.NewHandler`). No pgx tracer in `go.mod`; provider turns are a histogram (`aicc_turn_latency_ms`), not spans | `internal/httpapi/server.go:193`; `go.mod` |
| 0-9 | 99 | Dev defaults `AICC_SIP_LISTEN=:6060`, `AICC_RTP_PORT_MIN/MAX=40000/40999`; "full table in the deployment doc" | Neither variable exists. Real names: `AICC_BOT_SIP_PORT=6060`, `AICC_BOT_RTP_PORT_LOW/_HIGH`. There is no full config table in `deploy/README.md` — the registry is `.env.example` | `internal/config/config.go`; `.env.example` |

Minor, same document: L104's risk list is historical; L107 still lists a
"smart_turn vs server_vad evaluation" that never shipped (see 02-2).

### 3.2 `docs/design/01-telephony.md` — 8 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 1-1 | 7 | Subscription set: `…, DTMF, RECORD_START, RECORD_STOP, CUSTOM sofia::register sofia::unregister sofia::sip_user_state callcenter::info` | Also `BACKGROUND_JOB`, `sofia::expire`, and five `mod_audio_stream::*` events | `internal/telephony/switchevent.go` `Subscriptions` |
| 1-2 | 9 | Command vocabulary is "complete — anything else needs a design change" | `Adapter` also sends `uuid_dump` (reconcile), `uuid_exists` (`ChannelExists`) and `uuid_setvar_multi` (`StartCallRecording`). `eavesdrop` carries mode flags this line does not name | `internal/telephony/adapter.go:388,438`; `internal/telephony/reconcile.go:123` |
| 1-3 | 81 | NOT_READY reasons: `LOGIN, BREAK, LUNCH, TRAINING, AFTER_CALL_WORK, SYSTEM, SUPERVISOR` | Eight values; `DEVICE_LOST` is missing | contract `NotReadyReason`; `internal/api/api.gen.go:756-758` |
| 1-4 | 101 | Inbound Lua sets `hangup_after_bridge=true` | The script deliberately sets **false**, with a comment: with it on, a dying bot leg takes the caller down and the fallback never runs. It sets `true` only on the fallback transfer path | `freeswitch/scripts/aicc_inbound.lua:85-92,161` |
| 1-5 | 120 | `POST /api/v1/calls {call_id, kind:"ai_outbound", flow, to, …}` | `callId`, `kind` ∈ `AI_OUTBOUND` or `AGENT_OUTBOUND`, `to`, `did`, `extensionNumber`, `language`, `callbackId`, `userData`. There is **no `flow` field** — the DID names the flow | contract `CreateCallRequest`; 04 §3 already says so |
| 1-6 | 172 | D1 status: "the load is runtime-only until this diff lands — a FS restart would drop it" | The diff landed: `modules.conf.xml` loads `mod_callcenter` | `freeswitch/conf/autoload_configs/modules.conf.xml:158` |
| 1-7 | 19, 47 | `CONSULT` is "reserved in the contract… MVP emits only the first three" | Still true today (`coordinator.go:977` says not emitted). **Keep**; listed because readers ask. | `rg CONSULT internal/telephony` |
| 1-8 | 197 | D6a names the four files that said `default`: "`adapter.go` `TransferToExtension`, `outbound.go` click-to-dial, `aicc_queue.lua`, `aicc_inbound.lua`" | True. **Keep.** | `aicc_queue.lua`, `aicc_inbound.lua` use `XML aicc` |

Note on 1-5: it also violates 07 §1 (`call_id` is not a JSON field name). The
whole line predates the contract and should be rewritten rather than patched.

### 3.3 `docs/design/02-ai-voice.md` — 5 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 2-1 | 3–8 | Architecture: "`internal/provider` — one OpenAI-Realtime client × Profile (openai \| grok \| qwen \| any endpoint)" | Two clients; `doubao` (ByteDance protocol) is a sibling package, and `gateway` is a fourth Realtime value the diagram omits | `internal/provider/doubao`; `profile.go` |
| 2-2 | 192 | Latency table: "`smart_turn` is 2000 ms fixed … flows selecting it target ≈2.7 s p50" | Semantic turn detection was removed (#225). There is no flow field that selects it; `DefaultTurnDetection()` is `server_vad` at 500 ms and every call uses it | `internal/provider/session.go:174-186`; `rg smart_turn internal` → no match; AGENTS.md "Every call uses `server_vad` at 500 ms" |
| 2-3 | 111 | "Preflight … on aicc startup and on config change, a background check dials each configured provider, round-trips one greeting + one tool call, and surfaces status on the admin health panel" | No preflight exists. `GET /system/health` returns `sseClients`, `oldestSeq`, `trunks` and nothing else | `internal/httpapi/auth_handlers.go:156-179`; contract `SystemHealth` |
| 2-4 | 175 | Transfer refusal codes `QUEUE_UNKNOWN` / `QUEUE_CLOSED`; `NO_AGENTS_STAFFED` not implemented | Correct as written. **Keep.** | `internal/aicall/actions.go:174-178` |
| 2-5 | 133 | DSL v2 `global` includes `apiBaseEnv (parsed, no effect)` | Correct, and `flow-authoring` §2.2 repeats it. **Keep.** | `internal/flow/spec.go:544` |

### 3.4 `docs/design/03-data.md` — 7 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 3-1 | 7 | sqlc sources are "`internal/store/queries/*.sql`" | Sources are `internal/store/sql/*.sql`; `internal/store/queries/` holds the **generated** `.go` | `sqlc.yaml` (`queries: internal/store/sql`, `out: internal/store/queries`) |
| 3-2 | 296 | Retention deletes where `age > settings.retentionDays` | The `settings` table was dropped (00022, as §2 of this same document says). Retention is `AICC_RECORDING_RETENTION_DAYS` | `internal/recording/retention.go`; `internal/config/config.go` |
| 3-3 | 284–287 | `luacc.queues (…, tier_rules, …, overflow, …)` keeps the jsonb columns | The view flattens them: `is_tier_rules_applied`, `tier_rule_wait_sec`, `overflow_type`, `overflow_target`, `overflow_sound` — mod_lua has no JSON parser | `internal/store/migrations/00002_telephony.sql:183-202` |
| 3-4 | 39 | `agent_states.reason` ∈ seven values | Eight; `DEVICE_LOST` missing | contract `NotReadyReason`; migration `00032_device_lost_is_a_reason.sql` |
| 3-5 | 19–253 | Table inventory | Missing every table added after 2026-08-20: `sip_sessions` (00031), `call_transcriptions` and `call_transcription_segments` (00035), `campaigns`, `campaign_contacts`, `campaign_attempts`, `dnc_numbers` (00036–00038), plus `campaign_id` on `cdrs` (00039) and `dids.max_outbound_channels` (00040). No pointer to 08/10 | `internal/store/migrations` |
| 3-6 | 183 | `quality_reviews` is "written to and never read back" | It is read: `GET /calls/{callId}/reviews` → `ListQualityReviewsByCall`. What is missing is the **UI**, not the read path | `internal/store/sql/ledger.sql:182`; contract `listCallReviews` |
| 3-7 | 296 | `GET /api/v1/recordings/{id}/audio` | Path is `/recordings/{recordingId}/audio` (parameter name differs) | contract |

§6 (seed) matches `internal/seed/seed.go` on the cast, the password rule, the
555-01XX block and `AICC_SEED=fresh`; no finding.

### 3.5 `docs/design/04-api-sse.md` — 9 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 4-1 | 18 | "AGENT 8, SUPERVISOR 16, ADMIN 20 of the 20 in the vocabulary" | AGENT 8, SUPERVISOR 20, ADMIN 24 of **24** | `python3 docs/auth/scopemap.py` |
| 4-2 | 18 | "Exactly nine deviations" | Ten widenings, all `config:read` | same |
| 4-3 | 30 | Contract has "**91 operations**" | 84 paths, **112 operations** — 109 in `scopemap.py`'s derivation, 1 in-handler decision, 2 anonymous (`POST /auth/login`, `GET /openapi.json`) | contract; `python3 docs/auth/scopemap.py` |
| 4-4 | 99 | "**26 codes**" and the list that follows | `ErrorCode` has **40** values; missing from the list: `TERMINAL_ANNOUNCE_REQUIRED`, `NUMBER_ON_DNC`, `DID_CHANNELS_EXHAUSTED`, `DID_NOT_OUTBOUND`, `OUTBOUND_NOT_CONFIGURED`, `CAMPAIGN_FLOW_HAS_TRANSFER`, `CAMPAIGN_DID_NOT_OUTBOUND`, `CAMPAIGN_RING_TIMEOUT_TOO_LONG`, `CAMPAIGN_WINDOW_OUTSIDE_LIMIT`, `CAMPAIGN_FIELD_LOCKED`, `CAMPAIGN_INVALID_TRANSITION`, `DEVICE_NOT_REGISTERED`, `TRANSCRIPTION_DISABLED`, `TRANSFER_IN_PROGRESS` | contract `ErrorCode` |
| 4-5 | 91 | `DEVICE_REGISTERED / DEVICE_UNREGISTERED / DEVICE_IN_SERVICE` (isInService, reason) | The types are `DEVICE_REACHABLE` / `DEVICE_UNREACHABLE`; there is no `isInService` on the wire — `DEVICE_*` carries the agent-presence payload (`isDeviceRegistered`, `deviceAccount`, …) | contract `SseEventType`; `internal/events/event.go:74-75`; `internal/agents/service.go:1211-1231` |
| 4-6 | 92 | `BOT_SESSION_STARTED (flowSlug, provider, model)` · `BOT_TRANSCRIPT` · `BOT_INTERRUPTED` · `BOT_SESSION_ENDED` | `BOT_SESSION_STARTED` is published with `{"flowId": …}` only; the other three do not exist. The transcript events are `CALL_TRANSCRIPT` and `CALL_TRANSCRIPTION_STATE` | `cmd/aicc/wiring.go:671-679`; contract `SseEventType` |
| 4-7 | 75–92 | Event catalog presented as a closed set | Of its 34 names, 4 do not exist (`BOT_TRANSCRIPT`, `BOT_INTERRUPTED`, `BOT_SESSION_ENDED`, `DEVICE_IN_SERVICE`) and 4 real ones are missing (`CALL_TRANSCRIPT`, `CALL_TRANSCRIPTION_STATE`, `DEVICE_REACHABLE`, `DEVICE_UNREACHABLE`) — 30 correct against a 34-value contract enum | contract |
| 4-8 | 30 | "`GET /system/health` (ESL link, PG, provider preflights, trunks, registration count)" | Returns `sseClients`, `oldestSeq`, `trunks` only | `internal/httpapi/auth_handlers.go:156-179` |
| 4-9 | 61 | SSE endpoint is `GET /api/v1/events`; resume ring 65,536; 15 s heartbeat; 64-frame buffer | All four correct. **Keep.** | `internal/events/hub.go:15,18`; `internal/httpapi/events_handler.go:20` |

The rest of 04 (auth, scopes-as-the-message, the two credentials, the erased
`switchData`, the agent-workspace table) is accurate and is worth keeping.

### 3.6 `docs/design/05-frontend.md` — 4 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 5-1 | 9, 11 | Route inventory ends at `/admin/webhooks`, `/admin/keys` | Four more routes shipped with design 10: `/admin/campaigns`, `/admin/campaigns/new`, `/admin/campaigns/$campaignId`, `/admin/dnc` — and they are in the sidebar for ADMIN and SUPERVISOR | `web/src/routes/_app.admin.campaigns*`, `_app.admin.dnc.tsx`; `web/src/lib/nav.ts:81-82` |
| 5-2 | 33 | "ACW countdown from `wrap_up_until`" | No countdown and no `wrap_up_until` anywhere: `agent_states.wrap_up_ends_at` was dropped in 00012, ACW counts **up** and ends when the agent confirms | `rg wrap_up_until` → no match; 01 §2 |
| 5-3 | 33 | "`softphone-provider` implements ui-test's `ISoftphoneAdapter`-shaped contract" | No such type and no such module: the adapter is `web/src/lib/phone-bridge.ts`, rendered by `components/softphone-bar.tsx` | `rg ISoftphoneAdapter web/src` → no match |
| 5-4 | 29 | "`reset` event → invalidate live queries + re-tail" | The event is `SYSTEM_RESET`; wiring is in `web/src/lib/events.ts`/app provider. Substantively correct. | contract `SseEventType` |

### 3.7 `docs/design/07-naming.md` — 4 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 7-1 | 80 | Enforcement: "Go — staticcheck ST1003 + golangci-lint in CI; `revive` rule for struct-tag presence. TS — oxlint naming rules + a repo eslint-style check forbidding `enum` and `I`-prefixed interfaces. DB — … a CI script linting new migration files … CI fails on violations; no waivers." | None of that exists. CI runs `go vet`, `gofmt -l`, `npx oxlint .` and the Go tests. `web/.oxlintrc.json` has four rules, none about naming or `enum`. There is no migration linter | `.github/workflows/ci.yml:88-137`; `Makefile:193-196`; `web/.oxlintrc.json` |
| 7-2 | 15, 25, 26, 35 | Example file names: `call_state.go`, `esl_client.go`, `call-state.ts`, `softphone-bar.tsx`; interface example `Transferer` | None of these files/symbol exist (`softphone-bar.tsx` does; the other three do not). They are illustrations, but a reader greps them and finds nothing | `ls internal/telephony internal/esl web/src/lib` |
| 7-3 | 35 | TS enum rule "already ui-test practice" | No code enforces it; the rule survives as review discipline only | 7-1 |
| 7-4 | 74 | "SSE event types … (catalog in 04 §4)" | The catalog it points at is the stale one (4-5…4-7) | §3.5 |

§1–§6 themselves are still the best statement of the naming contract, and
`internal/store/naming_test.go` plus the contract gate do enforce the important
half. Fix the enforcement paragraph; do not delete the document.

### 3.8 `docs/design/08-transcription.md` — 6 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 8-1 | 1311 | Setting `AICC_TRANSCRIBE_PARTIALS` (default true) | No such variable. Partials are always published; there is no switch | `rg TRANSCRIBE_PARTIALS internal` → no match; `internal/transcript/actor.go:216-268` |
| 8-2 | 945, 963, 1075 | `TranscriptLine` gains `text`, `isFinal` | REST `TranscriptLine` has `content` and **no** `isFinal`; `text` + `isFinal` are the SSE payload's fields | contract `TranscriptLine`, `SseTranscriptPayload` |
| 8-3 | 1248 | "The quality UI is not built … the route does not exist" — and, by extension, no live transcript screen | `web/src/components/live-transcript.tsx` ships, the cockpit renders it, and the CDR page reuses it | `web/src/components/live-transcript.tsx`; 08 §12.2's own plan was executed |
| 8-4 | 222–262 | "The caller is very likely not transcribed at all today" | Human-leg ASR shipped: `internal/streamin` (mod_audio_stream tap) + `internal/transcribe` + the transcript actor. The reading note in §4 covers the *code quotes*, not this conclusion | `internal/streamin`, `internal/transcribe` |
| 8-5 | 1903–1904 | D9 "PROVISIONAL, default-on via `AICC_TRANSCRIBE_PARTIALS`" | Settled without the flag; see 8-1 | as above |
| 8-6 | 1085, 1342, 1351 | Line-number citations into `internal/httpapi/server.go`, `internal/events/hub.go`, `internal/provider/realtime.go`, `web/src/lib/events.ts` | The files were restructured (the handler file no longer exists; roles became scopes) | `ls internal/httpapi` |

The document is otherwise a strong record of *how* the decisions were reached,
which is why the verdict is ARCHIVE rather than DELETE.

### 3.9 `docs/design/10-campaigns.md` — 4 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| 10-1 | 3 | "Status: **accepted design; phase 1 not built** — no code, no contract change yet" | Built: `internal/campaign` (24 files), `internal/dialer` (12 files), migrations 00036–00040/00042/00045, 16 contract operations (`/campaigns`, `/campaigns/{id}`, `/contacts`, `/attempts`, `/dnc-numbers`, `CAMPAIGN_*` SSE, SPA screens) | `ls internal/campaign internal/dialer`; contract paths; `web/src/routes/_app.admin.campaigns*` |
| 10-2 | 1091–1096 | Metric names: `campaign_attempts_total`, `dialer_in_flight`, `bot_slots`, `outbound_overflow_hold_seconds`, … | Every instrument is `aicc_`-prefixed (design 06 §8: "one place for every instrument name"). `aicc_bot_overflow_hold_ms` is not `outbound_overflow_hold_seconds`; `aicc_bot_overflow_total`, `aicc_campaign_unmapped_cause_total`, `aicc_provider_response_unanswered_total` are not in the list at all | `internal/obs/callmetrics.go` |
| 10-3 | 48–64 | §0 facts "verified against `main` at `83e26a3`" with file:line citations | The cited lines have moved (`outbound.go:515-533`, `adapter.go:323-331`, `orchestrator.go:336`); the facts still hold, the pointers do not | spot-checked |
| 10-4 | 1022 | `POST/GET /campaigns`; `…/start`, `/pause`, `/resume`, `/cancel`; `/campaigns/settings` | Matches, and `PATCH`/`DELETE /campaigns/{campaignId}` also exist as described | contract |

The body is the *only* written account of why the dialer admits by expected
answers and how overflow is held; fix the header and the metric names, keep it.

### 3.10 `docs/phase1-decisions.md` — 5 findings

The header says "Changes require re-confirmation". Five rows have been
superseded by later owner decisions or by code, and none carries an amendment
pointer:

| # | Line | Decision | Reality |
|---:|---|---|---|
| P-1 | 10 (P2) | "outbound AI calls (API/manually triggered; **no campaign manager**)" | Campaigns shipped (design 10, `internal/campaign`, 16 operations). The decision was reopened and settled in design 10, but the register was never amended |
| P-2 | 11 (P3) | "Bot configuration = **Flow DSL v1**" | The spec is v2 (`specVersion: "v2"`, lowerCamelCase); 07 §7 records the conversion |
| P-3 | 35 (A5) | "Qwen `smart_turn` preferred … OpenAI `semantic_vad`/`server_vad`" | Removed (#225). Every call is `server_vad` 500 ms; AGENTS.md states it, 02 §5 states it |
| P-4 | 53 (R2) | "No human-leg ASR in phase 1" | Human-leg ASR shipped (design 08): `internal/streamin`, `internal/transcribe`, `call_transcriptions` |
| P-5 | 62 (O3) | Seed ships "two queues, **one English + one Chinese flow**, small synthetic CDR/report history" | Two queues, but **seven** published flows (six inbound, one outbound-only campaign flow) and seven days of history (`internal/seed/seed.go:75-85,168-174`) |

The other rows (A1, A6, A7, T1–T7, G1–G4, R1/R3, E1–E4, O1/O2) match the code
and the amendments recorded in their own text.

### 3.11 `docs/design/m4-cleanup-findings.md` — 2 findings

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| m4-1 | 57–58 | "`internal/provider/transport.go`" holds the only vendor-spelled JSON tags | That file does not exist; transport is `wsconn` + `internal/provider/wire.go` | `ls internal/provider` |
| m4-2 | 64–66 | Invariant #11 PASS: "`aicall` reads only `Profile.Name` and `Profile.FormatsFor`" | `aicall` also reads `Profile.RequiresTerminalAnnounce` and `Profile.DropsToolResultAfterLine` (and the client reads `NeedsCueForFirstTurn`/`AcceptsG711`) | `rg 'Profile\.' internal/aicall` |

Both are true of the date on the document; they are listed because the document
asks to be trusted over the design docs.

### 3.12 `docs/dev-stack.md` — 1 finding

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| ds-1 | 95–99 | Demo seed: "**six** published bilingual flows, each behind an English, a Chinese and a toll-free number (95001/95002/8005550199 … 95051/95052/8005550195)" | **Seven** flows are seeded. The seventh, `campaign_lead_followup`, is outbound-only: numbers 95061/95062 and no toll-free (its block comment explains why) | `internal/seed/seed.go:75-85` |

Everything else checked out: ports, the two compose containers, the FreeSWITCH
split, `aicc-mockbackend`'s fixtures and flags, `dnc_country_code`, the
`AICC_TOOL_SECRET_KB` / `{secrets.NAME}` pair, the `AICC_PROVIDER=gateway` notes.

### 3.13 `docs/design/grok-findings.md` — 1 finding

| # | Line | Document says | Code says | Evidence |
|---:|---|---|---|---|
| g-1 | 3 | "No Grok `Profile` exists and none was added; this records what the API did …" | `GrokProfile()` exists, `AICC_PROVIDER=grok` is a fifth value, and `AGENTS.md`/README list it | `internal/provider/profile.go:405-421,454,499`; commit `44cce39` (2026-10-15) |

The measurement body is still the evidence for the profile's trait values
(`IgnoresOverrideOnceTalking` etc.); only the header is wrong.

---

## 4. Cross-cutting patterns

1. **Duplicated inventories drift; single copies do not.** Every count that 04
   restates from the contract is wrong within two months (operations, scopes,
   codes, event types). The rule the repository already applies to `x-scopes`
   should apply here: the contract is the inventory, and prose links to it.
2. **"Proposed" knobs that never landed are indistinguishable from real
   settings.** `AICC_TRANSCRIBE_PARTIALS`, `AICC_SIP_LISTEN`,
   `AICC_RTP_PORT_MIN/MAX` read exactly like the 84 settings `internal/config`
   reads. The
   fix is mechanical: a docs test that extracts `AICC_*` from the design set and
   fails on names `internal/config` does not read.
3. **File:line citations rot faster than anything else.** 08, 10 and the
   findings records all cite line numbers; none resolve today. Cite a symbol
   (`Profile.FormatsFor`, `Subscriptions`) instead.
4. **A "status:" header is the first thing to lie.** 09's is correct
   ("built, commit `03efe73`"); 10's says "not built" about ~6,000 lines of
   shipped code.
5. **Enforcement paragraphs describe intent, not CI.** 07 §7 and 00 §4 name
   staticcheck, golangci-lint, revive, an eslint-style TS check and a migration
   linter. None exists. Either add them to `make lint`/CI or delete the claim —
   a spec whose stated enforcement is imaginary is worse than one that says
   "review + the tests named below".
6. **Historical records are fine — until they make present-tense claims.**
   `m0`/`m5`/`qwen`/`doubao`/`gemini` read as dated evidence and are safe.
   `grok-findings` opens with a present-tense claim that is now false, and
   `phase0-research`'s whole vocabulary section reads as a specification.

---

## 5. What was checked mechanically

| Check | Result |
|---|---|
| `AICC_*` names in docs vs `internal/config` + `.env.example` | 4 non-existent settings: `AICC_SIP_LISTEN`, `AICC_RTP_PORT_MIN`, `AICC_RTP_PORT_MAX` (00 §5) and `AICC_TRANSCRIBE_PARTIALS` (08 §13). `m4-cleanup-findings` also names four `AICC_OPENAI_*`/`AICC_QWEN_*` variables — correctly, as ones that were removed |
| File paths in backticks vs the tree | 68 unresolved; 11 also have no file of that basename anywhere (naming examples, java-bot symbols, one deleted file) |
| Relative links between documents | 0 dead links |
| `METHOD /path` mentions vs the contract | 0 wrong routes (`/api/v1` is the mount; `/api/kb/*` belongs to the mock backend) |
| `docs/openapi.json` vs `internal/api` generated code | in step (`make api-check` gate) |
| Contract enum values vs 04's catalog | 4 event types wrong, 14 error codes missing |
| DB objects named in 03 vs migrations | 2 dropped objects referenced as live; 7 live tables undocumented; 1 view shape wrong |
| Go symbols in backticks vs the tree | 66 unresolved, all in dated records or naming examples except the grok header and m4 |
| `make` targets in docs vs the Makefile | all exist; `make demo-up` (08 §2865) does not |
| Metric names in docs vs `internal/obs/callmetrics.go` | 06 in step; 10 predates the `aicc_` prefix |

## 6. Not audited

- `CHANGELOG.md` (release history; entries are dated on purpose).
- `docs/design/qwen-findings.md` and `doubao-findings.md` measurement tables —
  spot-checked for symbol names, not re-run.
- Anything requiring a live provider, a live switch or a database: every
  "MEASURED" claim here is accepted as recorded (the live tests are gated behind
  `AICC_LIVE_PROVIDER_TEST`, `AICC_LIVE_FLOWSIM`).
- `web/CLAUDE.md` (the binding design system; it is a separate document family
  and was only consulted for 05).
