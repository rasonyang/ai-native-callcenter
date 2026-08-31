# Design 04 — REST API & SSE Contract

Naming per [07-naming.md](07-naming.md): JSON fields lowerCamelCase, enum values SCREAMING_SNAKE_CASE, times `xxxAt` RFC 3339 UTC, durations `xxxMs`/`xxxSec`, booleans `is`/`has`. TS mirrors JSON byte-for-byte. Paths: kebab-case, plural resources; query params camelCase.

## 1. Conventions

Base `/api/v1`, JSON only. Cursor pagination (`?cursor=&limit=`, response `{items, nextCursor}`). Mutations idempotent where it matters (`POST /calls` requires client-minted UUIDv7 `callId`; replay live → 200 snapshot, replay ended → 409). Errors:

```json
{ "error": { "code": "QUEUE_NOT_FOUND", "message": "queue 7005 does not exist", "params": {"queue": "7005"} } }
```
`code`+`params` are the i18n contract — the SPA translates (`errors.<CODE>` keys); `message` is English debug text. HTTP status mirrors class (400/401/403/404/409/422/503 `SWITCH_DOWN`/`STORAGE_DOWN`).

## 2. AuthN/AuthZ

`POST /auth/login {username, password}` → argon2id verify → `aicc_session` cookie (HttpOnly, SameSite=Lax, Secure behind TLS), server-side `sessions` row (revocable); `POST /auth/logout`; `GET /auth/me` → user + role + agent profile. CSRF: mutations require header `X-AICC-Csrf: 1` (non-simple request → CORS preflight blocks cross-origin) in addition to SameSite=Lax. Roles no longer gate route groups — see the next paragraph; an account's role decides which scopes its login is granted (**AGENT** own state/calls/callbacks, **SUPERVISOR** + live ops and quality, **ADMIN** everything) and nothing more. SSE auth = same cookie (`EventSource` sends it; dev works via the Vite same-origin proxy).

**Two credentials, one authorization model (2026-08-31)**: a browser presents an HttpOnly session cookie, a system presents `Authorization: Bearer <key>`, and neither is the privileged one. What either may do is a set of **scopes** — `resource:action[:range]`, vocabulary in the contract's root `x-scopes` — and the contract states them operation by operation, in `security`. The server obeys that statement rather than restating it: `scripts/gen-opsecurity.mjs` turns each operation's `security` block into `api.OperationSecurityByRoute`, and one middleware inside the generated wrapper reads the matched route and applies it. There are no role guards on the routing table any more; a role is only the map that turns a login into scopes (`grantedScopes`), and nothing below authentication asks what role anybody has.

A key names the agent it works for in `X-AICC-Agent-ID`; a session may never send that header and is refused `AGENT_IMPERSONATION_NOT_ALLOWED` if it does. The CSRF header is asked of the cookie alone — it exists to protect a credential the browser attaches by itself. Two operations have no bearer alternative at all and say why in the contract: `POST /auth/logout` (a key has no session to end) and `POST /recordings/{recordingId}/reviews` (a score is a person's judgement). A credential with no alternative is answered `401 INVALID_CREDENTIALS`, which is a different failure from `403 INSUFFICIENT_SCOPE` and reads differently to an integration. The old model — one shared `AICC_API_KEY` in the environment, sent as `X-AICC-Api-Key`, reaching two operations as a hard-coded SUPERVISOR — is gone, with no migration path: keys are issued through `POST /api-keys`, each with its own name and scopes.

## 3. REST surface (method · path · min role)

**Auth/session**: POST `/auth/login` (public) · POST `/auth/logout` · GET `/auth/me`.
**Agent state**: POST `/agent/login {extensionNumber}` · POST `/agent/logout` · POST `/agent/ready` · POST `/agent/not-ready {reason}` · POST `/agent/wrap-up/extend` — agent. GET `/agents` (roster + availability) — supervisor; POST `/agents/{id}/force-logout` — supervisor.
**Calls (live)**: GET `/calls` (filters: `queueId`, `agentId`, `callType`) — supervisor; agent sees own. POST `/calls` `{callId?, kind: "AI_OUTBOUND"|"AGENT_OUTBOUND", to, did?, extensionNumber?, language?, userData?}` (both create `callType:"OUTBOUND"` calls) — agent (own) / supervisor. **Shipped 2026-08-26**, and the field list above is the corrected one: the AI leg names a `did`, which is where its flow, language and caller id all come from, rather than the `flowId`/`queueId`/`callerIdNumber` this line first guessed at (m0-findings §AI outbound). Click-to-dial had grown a `/calls/dial` of its own in the meantime; folding it back in is what closed the drift. The role check is the handler's, not the router's, because one route now answers two questions: AI outbound is an operations decision (SUPERVISOR), an agent's click-to-dial is their own (`extensionNumber` omitted, or their own phone named — never another's). A supervisor or an API-key caller has no phone, so they name the extension and it is checked for registration against the switch rather than against presence. Both kinds are idempotent by the client-minted `callId`. Per-call ops (role-checked by involvement): POST `/calls/{id}/answer` (→ `uuid_phone_event talk`) · `/hold` · `/retrieve` · `/hangup` · `/transfer {queueId|extensionNumber}` (blind, F2/uuid_transfer) · `/user-data` (merge-patch). POST `/calls/{id}/monitor {mode: "LISTEN"|"WHISPER"|"BARGE", agentId, extensionNumber?}` — supervisor (F6). **Shipped 2026-08-29**: the agent leg is named (an internal call has two), and the supervisor's phone is the one they are signed in at as an agent or the `extensionNumber` they name, checked for registration like a supervisor's outbound dial. The roster row carries `currentCallId` while `isOnCall`, which is what the request names. The supervisor's leg is stamped `aicc_observer=<mode>` and never `aicc_call_id`: the coordinator drops its events, so it is no party, reaches no stream and no CDR — the audit log has it. Whisper flags are read from the *agent* leg's side: `eavesdrop_whisper_bleg` is heard by the agent, `_aleg` by the far end, BARGE sets both; `eavesdrop_bridge_*` only picks what the supervisor hears and is left alone. One originate per request; changing mode is hang up + ask again.
**Config (admin)**: CRUD `/users`, `/extensions`, `/queues` (+ PUT `/queues/{id}/agents` staffing: `[{agentId, level, position}]`), `/dids`, `/trunks` (+ POST `/trunks/{id}/apply` → render gateway include + rescan), `/settings`.
**Flows**: GET/POST `/flows`, GET/PUT `/flows/{id}/draft`, POST `/flows/{id}/validate`, POST `/flows/{id}/publish {note}`, GET `/flows/{id}/revisions` — admin.
**CDR & artifacts**: GET `/cdrs` (rich filters + cursor), GET `/cdrs/{callId}` (detail incl. legs/userData/tech), GET `/cdrs/{callId}/transcript`; GET `/recordings/{id}/audio` (stream/presign redirect); quality: GET `/quality/recordings` (review queue), POST `/quality/reviews` — supervisor.
**Callbacks**: GET `/callbacks?status=OPEN` — agent/supervisor; POST `/callbacks/{id}/claim|done|dismiss` — agent.
**Reports**: GET `/reports/queues?from&to`, `/reports/agents?…`, `/reports/dispositions?…` (aggregates from `cdrs`+`queue_events`+`agent_state_logs`) — supervisor.
**Ops**: GET `/wallboard` (KPI snapshot) — supervisor; GET `/system/health` (ESL link, PG, provider preflights, registration count) — admin; GET `/audit` — admin. `/healthz` `/readyz` `/metrics` (unauth, separate listener).

**Agent workspace (added 2026-08-19)** — the four endpoints behind My Calls, Contacts, My queue and after-call work. Every one of them answers about *the caller*, resolved from the session: none takes an agent id, because an endpoint that did would hand any agent anybody's calls, queues or dispositions.

| Method · path | Min role | What it answers |
|---|---|---|
| GET `/cdrs/mine` | agent | The caller's own finished calls, same filters as `/cdrs` minus the ones that name other people. Each row carries *their* wrap-up. |
| GET `/calls/waiting` | agent | Who is queued in the queues this agent staffs, longest wait first, with the queue's `slaThresholdSec` so a breach is the queue's own promise. |
| GET `/agent/wrap-up` | agent | The record waiting on this agent — opened when the call ended, with the default disposition — or `204` when there is nothing. This is what a reloaded cockpit reads: the state is the server's, so refreshing is not a way past the confirmation. |
| POST `/agent/wrap-up` `{dispositionCode?, note?}` | agent | Confirms that record, applying whatever the agent changed, and returns them to READY. **Nothing is required**: the disposition already has a value, and confirming the defaults is a legitimate answer. Accepted after the agent has moved on — the last wrapped call stays addressable until the next one starts — and `409 AGENT_NOT_IN_WRAP_UP` when there is no call to confirm against. |
| GET `/dispositions` | any | The wrap-up vocabulary: one flat list of enabled codes, in display order. |
| GET `/reports/me` | agent | The caller's own day — calls handled, average handle time, average after-call work, occupancy, and how much of the day's after-call work was confirmed rather than left on its defaults — from the ledger and their presence history. Everything else under `/reports` is supervision. |
| GET/POST `/contacts`, PUT/DELETE `/contacts/{contactId}` | any | The customer record book, unique by phone number (`?phoneNumber=` is the cockpit's exact-match lookup). |

## 4. SSE — `GET /api/v1/events`

One stream per browser session. Envelope (cti-server lineage, renamed per convention):

```
id: 48213              ← global seq (hi/lo blocks; gaps after crash are meaningless)
event: PARTY_RINGING   ← type
data: {"version":1,"seq":48213,"type":"PARTY_RINGING","occurredAt":"2026-08-13T07:21:04Z",
       "callId":"…","callType":"INBOUND","partyId":"…","agentId":"…","queueId":"…",
       "payload":{…},"userData":{…}}
```

`callType` (`INBOUND | OUTBOUND | CONSULT | INTERNAL`, Genesys lineage) is present on **every** call event — the agent-facing in/out distinction is envelope-level, never buried in payload, and never changes across transfers.

Event scoping mirrors the domain model — **a Call is an aggregate of Parties**: leg-lifecycle events are party-scoped (`PARTY_*`, `partyId` always set, one event per leg), while aggregate facts stay call-scoped (`CALL_USER_DATA`, `CALL_RECORDING_*`, `CALL_CDR`).

**`userData` vs `switchData` — two concepts, never mixed** (Genesys user-data / Extensions lineage): `userData` is **business** context (`ticketId`, customer fields, AI-collected slots) — RFC 7386 merge-patched via `/calls/{id}/user-data`, size-bounded, survives transfers, lands in the CDR. `switchData` is the narrow Genesys-Extensions equivalent — *a data structure for switch-specific features and information that cannot be described by the other parameters of a request* — and exists in **exactly two places**: the `POST /calls` request (`switchData.sipHeaders` → special `sip_h_*` headers on the new leg; anything expressible as a normal parameter — like origination caller id, which is the first-class `callerIdNumber` field — never goes in `switchData`) and the Lua dialplan scripts when they create/route legs (X-headers, codec pinning, channel variables). It never appears on events, the envelope, the CDR, or any other endpoint. Switch facts that events do need (release cause, SIP status) are ordinary payload fields of those events (`PARTY_RELEASED` payload: `q850Cause`, `sipStatus`); the CDR's technical record is the `tech` column. We deliberately do not reuse Genesys's word "extensions" — in this project `extension` exclusively means a phone extension (07 §7).

Mechanics: 15s `: hb` heartbeat; `Last-Event-ID` resume from the in-memory ring (65,536); older → `SYSTEM_RESET {oldestSeq}` and the client resyncs via snapshots (`/calls`, `/agents`, `/wallboard`) then re-tails; slow consumers (64-frame buffer full) are disconnected — the publisher never blocks; the HTTP write is the ack. **Scoping is identity-derived, not client-chosen** (fixes cti-server's per-agent-filter gap): agents receive their own `AGENT_*`, calls they're a party to, their queues' `QUEUE_*`, and `CALLBACK_*` for their queues; supervisors/admins receive everything; `?types=` narrows further (server-side AND).

**Event catalog** (payload beyond envelope in parens):

| Type | When |
|---|---|
| `PARTY_DIALING / PARTY_RINGING / PARTY_ESTABLISHED / PARTY_HELD / PARTY_RETRIEVED / PARTY_RELEASED` (cause, party role) | party lifecycle — one event per leg (a bridged call emits e.g. two `PARTY_ESTABLISHED`, **both at the bridge**: `PARTY_ESTABLISHED` follows `CHANNEL_BRIDGE`, not `CHANNEL_ANSWER`, so a call nobody answered emits none at all — owner's rule 2026-08-24, C55); `PARTY_RINGING` to an agent carries the full screen-pop payload (ANI/DNIS, queue, userData) — zero follow-up GETs |
| `PARTY_CHANGED` (replacedPartyId, to, reason: `TRANSFER`\|`NO_ANSWER` — a new party replaces an old one within the same call, `callId` stable) · `PARTY_DTMF` · `CALL_USER_DATA` (full map, call-scoped) | in-call |
| `CALL_RECORDING_STARTED / CALL_RECORDING_STOPPED` · `CALL_CDR` (full CDR) | facts |
| `QUEUE_JOINED / QUEUE_LEFT` (cause) · `QUEUE_COUNT` (waiting, longestWaitAt) · `QUEUE_AGENT_OFFERED` | mod_callcenter events normalized. **Published since 2026-08-19** (the first three were declared and never emitted): scoped to the queue, so the agents staffing it and supervision receive them and nobody else does. A caller stops being "waiting" at the *bridge*, not at `member-queue-end` — the queue only announces that once the conversation is over — and a hangup removes them whether or not the queue ever says so. |
| `AGENT_LOGGED_IN / AGENT_LOGGED_OUT / AGENT_READY / AGENT_NOT_READY` (reason) · `AGENT_AVAILABILITY` (derived word) | agent FSM |
| `DEVICE_REGISTERED / DEVICE_UNREGISTERED / DEVICE_IN_SERVICE` (isInService, reason) | sofia + OPTIONS ping |
| `BOT_SESSION_STARTED` (flowSlug, provider, model) · `BOT_TRANSCRIPT` (role, kind, isFinal, text/tool) · `BOT_INTERRUPTED` · `BOT_SESSION_ENDED` (reason, usage) | AI leg; finals mirror `transcripts` rows; agents receive them live during/after transfer (cockpit transcript view) |
| `CALLBACK_CREATED / CALLBACK_UPDATED` | overflow message-taking |
| `SYSTEM_LINK` (isUp) · `SYSTEM_RESET` (oldestSeq) | ops |

## 5. i18n error codes (initial set)

`INVALID_CREDENTIALS, SESSION_EXPIRED, FORBIDDEN, VALIDATION_FAILED(field), NOT_FOUND(resource), CONFLICT(resource), EXTENSION_IN_USE, AGENT_ALREADY_LOGGED_IN, QUEUE_NOT_FOUND, FLOW_INVALID(errors[]), FLOW_UNPUBLISHED, CALL_NOT_FOUND, CALL_NOT_ACTIVE, NOT_CALL_PARTY, SWITCH_DOWN, STORAGE_DOWN, PROVIDER_UNAVAILABLE(provider), RECORDING_UNAVAILABLE, TRUNK_APPLY_FAILED, RATE_LIMITED`. Frontend owns all human wording (en/zh); the backend never concatenates sentences.
