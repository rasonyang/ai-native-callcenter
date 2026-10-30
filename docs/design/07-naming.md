# Design 07 — Naming Conventions (mandatory, no exceptions)

Normative for all code, schema, and contracts in this repo. Source: project owner directive (2026-08-13). Where other design docs conflict with this document, this document wins. Enforcement in §7.

## 1. JSON (API contract — the single source of truth)

- Field names: **lowerCamelCase**. Acronyms are treated as ordinary words: `callId`, `connId`, `userId`, `sipUrl`, `httpStatus`. Forbidden: `call_id`, `callID`, `CallId`, `SipURL`.
- Enum values: **SCREAMING_SNAKE_CASE strings**: `"RINGING"`, `"ON_HOLD"`, `"TRANSFER_FAILED"`.
- Boolean fields: `is`/`has`/`can` prefix: `isMuted`, `hasVoicemail`.
- Time fields: `xxxAt`, RFC 3339 UTC strings: `createdAt`, `answeredAt`.
- Duration fields: `xxxMs` / `xxxSec` — unit always explicit: `ringDurationMs`. Bare `duration` or `timeout` forbidden.

## 2. Go

- Exported identifiers: PascalCase with initialisms fully capitalized: `CallID`, `ConnID`, `SIPURL`, `HTTPClient`. Forbidden: `CallId`, `SipUrl`, `HttpClient`.
- Unexported: camelCase, same initialism rule: `callID`, `httpClient`.
- Struct tags must declare the JSON name explicitly: `` `json:"callId"` `` — relying on default serialization names is forbidden.
- Enums: a named string type; constant name = type name + state name (PascalCase); constant value = SCREAMING_SNAKE:
  ```go
  type CallState string
  const CallStateOnHold CallState = "ON_HOLD"
  ```
  Forbidden: constant names like `CALL_STATE_ON_HOLD`; integer/iota enums.
- Package names: single lowercase word, no underscores, no plurals: `callctrl`, not `call_controls`.
- File names: lowercase snake_case: `call.go`, `switchevent.go`, `backgroundjob.go`.
- Interfaces: single-method interfaces take the `-er` suffix: `Publisher` (`Publish`), `TrunkReader` (`Trunks`), `RecordingStreamer` (`Open`).

## 3. TypeScript

- Variables/functions/properties: camelCase; acronyms as ordinary words: `callId`, `parseSipUri`.
- Types/interfaces/classes: PascalCase: `CallState`, `SoftphoneBarProps`. No `I` prefix (`ICallState` ✗).
- Interface properties match JSON keys **byte-for-byte**: `callId` — no conversion layer.
- Enums: string-literal union types: `type CallState = 'RINGING' | 'ON_HOLD'`. The TS `enum` keyword is forbidden.
- Constants: SCREAMING_SNAKE_CASE: `MAX_RETRY_COUNT`.
- File names: kebab-case: `softphone-bar.tsx`, `phone-bridge.ts`, `phone.test.ts`.

## 4. Database (PostgreSQL)

- Table names: snake_case, **plural**: `calls`, `call_events`, `agent_sessions`.
- Column names: snake_case, singular: `call_id`, `conn_id`, `created_at`. camelCase columns forbidden.
- Primary key: always `id`. Foreign keys: `<singular_table>_id`: `agent_id`, `queue_id`.
- Boolean columns: `is_`/`has_` prefix: `is_muted`.
- Time columns: `xxx_at` (timestamptz): `answered_at`. Duration columns: `xxx_ms` / `xxx_sec`: `ring_duration_ms`.
- Enum columns: SCREAMING_SNAKE strings (varchar + CHECK constraint), byte-identical to the JSON enum values. Integer codes forbidden.
- Index names: `idx_<table>_<cols…>`: `idx_calls_agent_id`. Unique constraints: `uq_<table>_<cols…>`. Foreign-key constraints: `fk_<table>_<referenced_table>`.

## 5. Cross-layer mapping (the ambiguity killer)

- One concept has exactly one fixed form per layer:
  **Go `CallID` ↔ JSON `callId` ↔ TS `callId` ↔ DB `call_id`**.
- Enum values are byte-identical across JSON, TS, and DB (`"ON_HOLD"`); only the Go constant *name* differs (`CallStateOnHold`).
- Upstream system names (FreeSWITCH `Unique-ID`, Genesys `ConnID`/`ThisDN`, mod_callcenter tokens, provider wire fields) may appear **only in boundary parsing/rendering layers** (`internal/telephony` normalization, `internal/provider` clients, `freeswitch/*.lua`); they must be mapped to this convention before entering the domain model and may never leak into the API or frontend.
- New concept workflow: first fix the English singular noun (e.g. `transfer`), then derive all four layer forms by these rules. Layers never invent independent names.

## 6. Forbidden words

- Meaningless standalone names: `data`, `info`, `item`, `temp`, `obj`, `mgr`, `util`.
- Synonym drift: one concept, one word — `agent` (never `operator`/`rep`), `call` (never `phone_call`/`conversation`), `queue` (never `skill_group`).

## 7. Project applications & boundary rulings

Concrete consequences already applied across docs 01–10 (each ruling below is an *application* of the spec; the flagged items are interpretations awaiting owner veto):

| Area | Ruling |
|---|---|
| SSE event types | Enum values → SCREAMING_SNAKE, prefixed by scope: party-scoped `PARTY_RINGING`, call-scoped `CALL_CDR`, plus `QUEUE_*`, `AGENT_*`, `BOT_*`, `DEVICE_*`, `SYSTEM_*` (catalog in 04 §4). |
| API error codes | Enum values → SCREAMING_SNAKE — the vocabulary is the contract's `ErrorCode` enum and nothing else defines it: `NOT_FOUND`, `SWITCH_DOWN`; i18n keys `errors.<CODE>`. |
| Audit actions | SCREAMING_SNAKE verbs: `QUEUE_UPDATED`, `FLOW_PUBLISHED`, `AGENT_FORCE_LOGOUT`. |
| mod_callcenter strategy | DB/JSON store our enum (`LONGEST_IDLE_AGENT`, `ROUND_ROBIN`, …); Lua/Go boundary maps to callcenter tokens (`longest-idle-agent`). Upstream tokens never leave `freeswitch/` and the telephony adapter. |
| JSONB payloads | Keys inside jsonb columns are JSON → lowerCamelCase (`{"type":"BOT_FLOW"}`, `[{"weekday":1,"open":"09:00"}]`). |
| Flow DSL | **Spec v2**: keys are lowerCamelCase (`specVersion`, `initialNode`, `maxTurns`); enum-like values SCREAMING_SNAKE. The reference flows in `internal/seed/flows/` are v2; the ui-test v1 files were converted once and are not kept in this tree. |
| LLM tool names | `transfer_to_agent`, `take_message`, `hangup` stay snake_case — provider-ecosystem convention and explicitly specified in the project brief; treated as a boundary contract (provider wire), not domain naming. ⚠ flagged interpretation. |
| Locale codes | `en` / `zh` stay lowercase BCP 47 — external data standard consumed verbatim by i18next/Intl, not a project enum. ⚠ flagged interpretation. |
| `userData` / `switchData` | `userData` = business context (never mixed with switch concerns). `switchData` (**designed, not implemented** — no contract field or code carries it) = the Genesys **Extensions** analog, deliberately narrow: a request-side structure for switch-specific features not describable by the request's other parameters; allowed **only** on `POST /calls` and in Lua leg-creation scripts — never on events, the envelope, the CDR, or other endpoints (owner directive). Named `switchData` (not "extensions") per §5/§6 — `extension` = phone extension here. ⚠ flagged naming only. |
| API scopes | Not covered by the spec; project addition: `resource:action[:range]`, lowercase, colon-separated (`calls:read:own`, `config:write`, `keys:manage`). The **resource segment must name a domain resource**, and no single scope may be a role's alias — a scope equivalent to "one role's whole capability set, renamed" is forbidden (`supervisor:*`, `admin:*`, `role:agent`). `agent:read` / `agent:act` / `agent:manage` are compliant: `agent` here is the **agent resource** (presence and agent identity), not the `AGENT` role — the first two reach only the subject's own agent identity, and the three together are not that role's capability set. Vocabulary and per-scope descriptions live in `docs/openapi.json` root `x-scopes` (the source of truth); `role → scopes` is a convenience map for page login, never the model. |
| URL paths | Not covered by the spec; project addition: kebab-case segments, plural resources (`/api/v1/queues`, `/agent/not-ready`). Query params camelCase. |
| Metrics/log keys | Prometheus/OTel ecosystem conventions (`aicc_turn_latency_ms`, snake labels) — observability boundary, not API contract. |
| SIP X-headers / env vars | Boundary formats keep their ecosystem casing: `X-AICC-Call-ID`, `AICC_ESL_ADDR`. |

**Enforcement** — what actually exists, so a reader knows where a violation is caught:

- **Go** — `go vet` and `gofmt -l` run in CI and `make lint`; the contract gate (`internal/httpapi/contract_gate_test.go`) holds error codes and route authorization; `internal/store/naming_test.go` marshals the API and ledger structs and fails on a JSON key that is not lowerCamelCase. There is no staticcheck, golangci-lint or revive run — naming on Go identifiers is PR review.
- **TypeScript** — `npx oxlint .` in CI and `make lint` (`web/.oxlintrc.json`: correctness, no-explicit-any, consistent type imports, jsx-key, no-console). Wire types are generated, so they cannot drift; naming on hand-written code is review.
- **Database** — migration review against §4. There is no migration linter.
- **API** — the contract is the reference and `make api-check` fails when the generated Go/TS does not match it; PR review checks JSON tags against §1.

A claim of automated enforcement that no job runs is worse than none: a reader who believes it stops looking. If one of these checks is added, name it here with the file that runs it.
