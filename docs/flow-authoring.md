# Authoring flows

A guide for writing a flow that stays small and readable when the business behind it is not. It covers what the flow DSL does, where each kind of logic belongs, and what the DSL cannot do yet.

Every field, built-in and behaviour named here exists in the code today. Where something a business may want is not supported, the guide says so and links the issue. It does not describe designs that are not built.

Related reading: the field reference is `internal/flow/spec.go`; the engine design and the live findings that amended it are `docs/design/02-ai-voice.md` §6 and §7; the seeded flows in `internal/seed/flows/` are working examples; the mock backend they call is `cmd/aicc-mockbackend`. The worked example (7) is the seeded `plan_change` flow.

## 1. What a flow is, and what it is not

A flow is one JSON document: a persona, a set of **phases** (`nodes`), the **tools** the model may call in each phase, and **transitions** that move the call from one phase to another.

The division of labour is the whole design:

- **The model owns the conversation.** What to say, how to ask again, how to phrase a number.
- **The flow owns the phase.** Which instruction the model is under now, which tools it may call, and when that changes. The engine never writes dialogue.
- **The backend owns the business rules and the facts.** Eligibility, prices, availability, policy text.

The flow is configuration, not a program. Conditions are a closed set of operators over slots, with no expressions and no derived values (`internal/flow/predicate.go`). That is deliberate, and it is the reason the patterns in section 4 exist: when you need a decision, put it in a service and let the flow branch on the answer.

A flow that has grown past a handful of phases usually has one of three symptoms, and each has a cure in section 4:

| Symptom | Cure |
| --- | --- |
| Chains of phases and `condition` trees spelling out eligibility, pricing or multi-step validation | Thick tools, thin flow (4.1) |
| Policies, tariffs or FAQ text pasted into `persona`, `rules` or an `instruction` | Knowledge through a tool (4.2) |
| An operation that is slow, or that the bot should not complete alone, modelled as an ordinary tool | Long operations (4.3), risky operations (4.4) |

## 2. Anatomy of a flow

The loader (`flow.Load`, `internal/flow/load.go`) rejects a flow that cannot run, and reports every problem at once. Dialect version is `"specVersion": "v2"`.

### 2.1 Top level

| Field | Meaning |
| --- | --- |
| `id` | Required. The flow's slug; `aicc flowadd` updates an existing flow by it. |
| `specVersion` | Required, must be `"v2"`. |
| `entry` | Optional name of the business entry point. Seeded into the `entry` slot. |
| `initialNode` | Required. A call starts in this phase. |
| `global` | Persona, rules and guards that apply across every phase (2.2). |
| `nodes` | Map of phase id to phase (2.3). At least one. |
| `tools` | Map of tool name to a declarative HTTP tool (2.4). |

### 2.2 `global`

| Field | Meaning |
| --- | --- |
| `persona` | Required. The character the model adopts. Part of the standing instructions on every turn. |
| `rules` | Standing rules, one string each (or a per-language list). Rendered as a bullet list after the persona. |
| `voice` | The bot's voice, versioned with the flow. Empty uses the provider's default. |
| `temperature` | Sampling temperature, `0 <= t < 2`. Providers without the setting ignore it. |
| `fallbackTarget` | A phase the engine moves to when `maxTurns` is exceeded. |
| `maxTurns` | Bound on tool dispatches for the whole call, against a model that loops. |
| `closingTarget` | The flow's own goodbye: a **terminal** phase the engine moves to by itself (5.3). |
| `maxTurnsWithoutTool` | Bound on tool-less replies in one phase (5.3). Requires `closingTarget`. |
| `maxDurationSec` | The call's wall clock (5.4). Absent means 900, `0` is off, otherwise 60 to 3600. |
| `toolWaitLine` | The words of the wait line said while a slow HTTP tool runs (2.4.2). Optional, bilingual, said as written, `{slots.x}` allowed; must be a statement. A language left empty keeps that language's default. |
| `alwaysAllowedTools` | Tools allowed in every phase, so a request for a person is never blocked by the phase. |
| `transitions` | Rules that apply in every non-terminal phase, after the phase's own. |
| `apiBaseEnv` | Parsed but has no effect. The backend address is `AICC_BOT_BACKEND_BASE` (`docs/dev-stack.md`), or a tool's own `http.baseUrl` (2.4.1). |

### 2.3 A phase

| Field | Meaning |
| --- | --- |
| `instruction` | Required. What the model is trying to accomplish now. A brief the model writes from. May reference `{slots.name}`. |
| `announce` | Optional. A line spoken **as written** on entering the phase, also rendered with `{slots.name}`. |
| `tools` | The allowlist for this phase. `"*"` allows every tool. Empty allows only `alwaysAllowedTools`. |
| `transitions` | Rules that move the call out of this phase (2.5). |
| `isTerminal` | The conversation does not leave this phase. A terminal phase may not declare `transitions`. |
| `lead` | Optional. `{"intent": "HIGH", "notes": "{slots.x}"}`: the campaign lead the call records on entering this phase (2.7.1). `intent` is one of `HIGH`, `MEDIUM`, `LOW`, `NONE`, `DO_NOT_CALL`, in capitals; `notes` is optional text (a string or per language) rendered like `announce`. Ignored on a call that is not a campaign call. |

Every user-facing string (`persona`, `instruction`, `announce`, tool `description`) is either a plain string, meaning the same text in both languages, or an object `{"en": "...", "zh": "..."}`. A missing translation falls back to the other language rather than to silence. One flow serves every language; business logic is never duplicated per language, only wording. The call's `language` picks the wording and nothing else; it never selects the provider or the voice.

`{slots.name}` is substituted into `instruction`, `announce` and a declared lead's `notes` only. It is not substituted into `persona` or `rules`. A slot that is not set renders as nothing.

### 2.4 A tool

A declarative tool is by default an HTTP call to your backend (a second kind, the slot tool, records what the caller said and calls nothing: see 2.8). The model sees `description` and `parameters` (a JSON Schema object, passed to the model unchanged); the engine runs the `http` block.

| Field | Meaning |
| --- | --- |
| `description` | Required. The model reads this to decide when to call the tool. |
| `parameters` | JSON Schema of the arguments. |
| `http.baseUrl` | Optional. This tool's backend, overriding `AICC_BOT_BACKEND_BASE`: an absolute `http://` or `https://` URL, which may have a path but no user, query or fragment. See 2.4.1. |
| `http.path` | Required. Starts with a literal `/` and is appended to the base URL's path. May reference `{args.x}` (an argument the schema `required`s) and `{slots.y}`; each value is escaped into one path segment (2.4.1). A literal `?query` written in the path is sent as written (prefer `http.query`; a parameter in both is a load error, and a literal query that references a value is read as query templates); a `#fragment` is dropped. |
| `http.method` | Defaults to `POST`. |
| `http.query` | Optional. Map of query parameter to a template (`{args.x}`, `{slots.y}`, text). Each value is encoded as one form value. A parameter that references an argument the model did not pass is left out. |
| `http.headers` | Optional. Map of header name to a literal, or to text with `{secrets.NAME}` references (`"Bearer {secrets.KB_KEY}"`). A credential header must use a secret. See 2.4.1. |
| `http.timeoutMs` | Optional. 500 to 10000; absent means 5000. |
| `http.body` | A template. String values may reference `{args.x}` (the model's arguments) and `{slots.y}` (the call's slots). A value that is exactly one reference keeps its type; a reference inside text becomes text. Absent sends `{}` as JSON for `POST`, `PUT` and `PATCH`, and no body for `GET`, `DELETE`, `HEAD` and other methods. |
| `http.successWhen` | `{ "path": "retCode", "equals": "000000" }`. Absent means any 2xx counts. |
| `http.errorFrom` | Dotted path to the backend's own error. When `successWhen` fails it is the message the model reads. On a 4xx or 5xx answer a scalar at this path (for aicc-knowledge `error.code`) is the backend's error code: it is logged as `backendError` and named in the tool's error (`/v1/search returned 503 (INDEX_UNAVAILABLE)`), cut to 128 characters, and the body and the backend's message are never logged or shown to the model. |
| `http.result` | Map of slot name to dotted path in the response. **Only these fields are exposed.** They reach the model and are written to the slots (`result.<name>`, `<tool>.<name>`). |
| `http.modelOnly` | Optional. Map of output name to part of the response that reaches the model and the call transcript but is **never written to a slot**. Each entry is a dotted path (a string), passed through as it is (object, list or scalar), or a projection `{ "path": "items", "each": { "answer": "answer", "sourceRef": "sourceRef" } }`: `path` names an array, and each element that is an object is cut down to the `each` fields (a field that does not resolve is left out of that element; other elements are skipped). Applied on success only; a name that does not resolve is omitted. |
| `collect` | Optional, HTTP tools only (a slot tool with `collect` does not load). Keeps a list of values across the tool's calls: `{ "slot": "repair_status.foundOrders", "value": "{args.rma}", "when": { "slot": "result.found", "op": "EQ", "value": "1" } }`. See 2.6.1. |

Runtime behaviour worth designing around (`internal/flow/httptool.go`):

- A call that has not returned **1 second** after it started is covered by the wait line, once per batch of tool calls and only if the caller is not already hearing the bot (2.4.2).
- A call is bounded by `http.timeoutMs`, **5 seconds** unless the tool says otherwise and never more than 10. A timeout, a connection failure, a connection the host policy refuses (2.4.1), a redirect (3xx, never followed), a status of 400 or above, or a body that is not a JSON object is a tool failure: the model is told the operation could not be completed and, per the built-in hint, to offer a person if it fails again.
- A path value that cannot be a path segment (empty, `.`, `..`) or any path or query value that is an object or a list is refused before a request is made, as `ok: "0"` naming the reference (`{args.id}`) and not the value, with no recovery hint: the model can call again with a proper value.
- A `successWhen` that does not hold is a **refusal**, not a failure: the result is `ok: "0"` with the `errorFrom` message. No `result` field is mapped on a refusal.
- Only the fields in `http.result` and `http.modelOnly` ever reach the model, and only `http.result` reaches the slots. A whole backend response is never handed over.
- Use `modelOnly` for what the model should read once and the flow should forget: a list of passages kept as a slot would stay for the rest of the call and could be rendered into a later instruction. The price is that no transition, template or `collect` can read a `modelOnly` name (`result.<name>` and `<tool>.<name>` are refused at load, saying it is handed to the model only); map a field in `result` as well, under another name, when a condition needs it. A `modelOnly` name may not be `ok`, `error` or `hint`, nor a key of `result`.
- A tool is offered to the model only if some phase's allowlist, or `alwaysAllowedTools`, names it. A tool called outside its phase is answered with `ok: "0"` and the current phase's instruction, not an error.

#### 2.4.1 Another backend, credentials, templates in the URL, and the host policy

A tool that needs its own service, a key, or more time says so in its `http` block:

```json
"kb_search": {
  "description": { "en": "Search the knowledge base.", "zh": "检索知识库。" },
  "parameters": {
    "type": "object",
    "properties": {
      "collection": { "type": "string" },
      "query": { "type": "string" },
      "top": { "type": "integer" }
    },
    "required": ["collection", "query"]
  },
  "http": {
    "baseUrl": "http://kb.internal:8000/v1",
    "path": "/collections/{args.collection}/search",
    "method": "GET",
    "query": { "q": "{args.query}", "top": "{args.top}", "lang": "{slots.lang}" },
    "headers": { "Authorization": "Bearer {secrets.KB_KEY}" },
    "timeoutMs": 8000,
    "result": { "answer": "answer", "found": "found" }
  }
}
```

**Templates in the URL.** Every value substituted into `path` goes through URL path escaping, so `/`, `?`, `#` and `%` stay inside its one segment: the model passing `a/b` sends `/collections/a%2Fb/search`, and `@evil.com/x` cannot change the host, because the URL is assembled from its parts with the host taken only from the base. A `query` value is encoded as a form value (`&`, `=`, `+`, spaces and Chinese characters arrive intact). A path reference must be an argument the schema lists in `required`, or a slot, because a path segment cannot be left out; an optional argument belongs in `query`, where its parameter is dropped when the model does not pass it (`top` above). The log records the path template and the query parameter names, the base and the timeout, never the rendered values: a phone number or an order id in a URL reaches the backend's access log and no other.

**Secrets.** A flow never holds a credential: every `config:read` holder can read it, and its revisions are kept. A header value references one as `{secrets.NAME}`, alone or inside text, and it resolves from the environment variable `AICC_TOOL_SECRET_<NAME>` (`NAME` is capital letters, digits and underscores) when the call is made. Rules, checked at load:

- `Authorization`, `Cookie`, and any header whose name contains `auth`, `token`, `key`, `secret` or `password` (case-insensitive) must reference a secret; a literal there does not load.
- `{secrets.NAME}` may appear only in a header value: not in `baseUrl`, `path`, `query`, `body` or a `collect` value. A backend that wants its key elsewhere, or a signed request, is not supported.
- A header may not reference `{args.x}` or `{slots.y}`, and `Host`, `Content-Length`, `Transfer-Encoding`, `Connection` and the other headers the HTTP client sets itself cannot be given.
- A reference to a variable that is not set on this deployment is refused at **publish** (the problem names the tool, the header and the variable) and only logged as a warning when a **draft** is saved, since the operator may set it after the draft is written.

The value never appears in the stored flow, an API response (`GET /flows/{flowId}` shows the reference), a log line, an error or what the model is told about the call; an error names the header and the variable. Secrets live only in the environment, so rotating one takes a restart. A variable unset at call time fails the tool.

**Who can use a secret.** Keeping the value out of the flow does not limit who can use it. A secret is not tied to a host: anyone who can write flows (`config:write`) can give a tool any configured secret in a header, point it at any host the deployment's host policy lets tools reach (the whole LAN, by default), and map whatever that backend echoes back into what the bot says or the slots record. So `config:write` amounts to the use of every tool secret on the deployment. Grant it only to people you would trust with those secrets, and give each secret the least privilege at its backend: a read-only key scoped to the one service the tool calls, never an administrator credential.

**The host policy.** Which hosts a tool may reach is the deployment's decision, not the flow author's. It is decided on the address the host resolves to, as the connection is made:

- Private and loopback addresses (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `127.0.0.0/8`, `::1`, `fc00::/7`) need no configuration.
- A public address is reached only when the tool's host is listed in `AICC_TOOL_ALLOWED_HOSTS` (`host`, `host:port`, `*.domain` for subdomains, or an IP address). The host of `AICC_BOT_BACKEND_BASE` is always allowed.
- Link-local addresses (`169.254.0.0/16`, `fe80::/10`, the cloud metadata address `169.254.169.254` among them), AWS's IPv6 metadata range `fd00:ec2::/32` (instance metadata and EKS Pod Identity, refused before the private check), unspecified ones (`0.0.0.0`, `::`) and multicast are refused even when listed. An IPv4-mapped or NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`) IPv6 address is judged by its IPv4 part, and a host written as an IP in a non-standard form (`2130706433`, `0x7f.1`) is refused.
- Redirects are never followed; a 3xx is a tool failure.

A refusal is a tool failure, decided before any connection is opened, and the log line `http tool connection refused by the outbound host policy` names the host, the resolved address and the rule (`PUBLIC_NOT_LISTED`, `LINK_LOCAL`, `CLOUD_METADATA`, `UNSPECIFIED`, `MULTICAST`, `NON_CANONICAL_ADDRESS`). The simulator (8) always calls its mock backend, whatever a tool's `baseUrl` says, and sends a placeholder for every secret; with `-backend URL` it applies `AICC_TOOL_ALLOWED_HOSTS` and reads secrets from its environment, like a call.

#### 2.4.2 The wait line

A caller must not sit in silence while a backend works. When an HTTP tool has not returned **1 second** after it started, the bot says a short wait line: by default "One moment, let me check that." in English and "请稍等，我查一下。" in Chinese. The threshold is fixed, not a setting. The rules:

- **Once per batch.** One model response can call several tools; two slow ones in it get one line.
- **Only into silence.** If the caller is still hearing the bot at the 1-second mark, nothing is added. That includes a turn still being generated, a turn asked for and not started yet, and audio still playing after its turn has finished. The model often says its own filler ("let me look that up") in the turn that calls the tool. No line is said over an armed transfer or hangup either.
- **A statement, never a question.** A filler phrased as a question gets answered, and the answer talks over the result. The loader refuses a `global.toolWaitLine` with a `?` or `？` in it.
- **The result is never lost.** A result that arrives while the line is playing is answered after the line, in a turn of its own. A caller who talks over the line still gets the result. The model answers them with the result in view.
- **Not a reply and not a closing line.** The line's turn does not count towards `maxTurnsWithoutTool`, is not checked for unbacked claims, and never ends a call whose ending is armed. The call still waits for the turn after it.
- **Built-in tools, slot tools and `record_lead`** answer at once and never get it.
- **Not on doubao.** On that engine a line spoken while a tool call is still owed ends the round the call belongs to, and the result sent afterwards is never answered: the caller would hear the line and then silence (`docs/design/doubao-findings.md` W-D10). A doubao deployment therefore says no wait line, and `global.toolWaitLine` has no effect there. A caller waits through a slow tool in silence, as before, so keep tools on doubao well under their `timeoutMs`.

`global.toolWaitLine` replaces the wording, per language: `{"en": "Bear with me a second.", "zh": "稍等片刻，马上就好。"}`. Like an `announce`, the line goes through `VoiceSession.SpeakText`, best effort on the Realtime engines (5.2). A phase's `announce` that the slow tool's result leads into pre-empts the wait line if it is still playing, as an announce pre-empts anything. The transcript records the line as the bot's words. The log has one line for each slow tool, naming the tool and never its arguments: `a tool is running long; said the wait line`, or why it was not said (`no wait line, the caller is hearing the bot`, `no wait line over an armed ending`).

The mock backend's `POST /api/kb/secure-search` is a slow, authenticated backend to try this against (`docs/dev-stack.md`).

### 2.5 Transitions and conditions

A transition has `on`, `tool`, `condition`, `target` and `priority`.

- `on` is `TOOL_RESULT` (a tool ran, whatever its outcome) or `NO_INPUT` (the caller said nothing). Empty matches any event. Nothing else is an event: there is no event for what the caller said, and none for a keypress (see 6).
- `tool` narrows a `TOOL_RESULT` rule to one tool. A `NO_INPUT` rule that names a tool is refused at load, because a silence carries no tool.
- `target` must be an existing phase.
- Rules are tried in ascending `priority`, equal priorities in declared order, the phase's own rules before the flow-wide ones. The first whose event and condition both hold wins. If none holds, the call stays in the phase and the model tries again, which reads to the caller as being asked again, not as an error.
- A rule whose `target` is the current phase is an explicit stay.
- A model can ask for several tools in one response. All of them are judged in the phase the response was made in; moves apply one at a time and the last one wins.

A condition is a leaf `{ "slot", "op", "value" }`, or `{ "all": [...] }`, or `{ "any": [...] }`. Omitted means always true. The operators are `EQ NE GT LT GTE LTE IS_NULL IS_NOT_NULL IS_EMPTY IS_NOT_EMPTY IN NOT_IN CONTAINS NOT_CONTAINS`. A mistyped operator is a load error. `EQ` compares numbers by value, so a backend returning `"1"` matches a rule written `1`.

### 2.6 Slots

Slots are the call's structured state. They live in this application, not in the provider's history, and are re-pinned into the model's standing instructions on a phase change, so they survive a provider truncating a long conversation. The names the engine maintains:

| Slot | Value |
| --- | --- |
| `caller`, `did`, `entry`, `lang` | Seeded at the start of the call. `lang` is `en` or `zh`. |
| `language` | The call's language in the form a service expects: `EN` or `ZH`, derived from `lang`. Reserved: no slot tool, `collect` or mapping can write it, and the model never chooses it. Send it as `"language": "{slots.language}"`. |
| `node` | The current phase id. |
| `result.<field>` | The mapped fields of the **last** tool result, including `result.ok` (`"1"` or `"0"`). Replaced wholesale on every tool result. |
| `<tool>.<field>` | The same fields, kept per tool until that tool runs again. |
| `<tool>.calls` | How many times that tool has run. |
| `lastTool`, `turns` | The last tool that ran; the count of tool dispatches. |
| `<node>.visits` | How many times a phase has been entered. |
| `noInput.count` | Consecutive silences. Reset by words, a keypress or a tool result. |
| `turnsWithoutTool` | Tool-less replies in the current phase (5.3). |

A tool result that changes what the current phase's instruction renders from slots re-pins the instruction without a phase change, so an instruction never quotes a superseded value.

A value becomes a slot in five ways: a `http.result` mapping over a backend response; a slot tool recording a fact the caller said (2.8); the arguments of any tool call, stored as `<tool>.args.<k>`; a `collect` list over an HTTP tool's calls (2.6.1); and the business data an outbound call was placed with, seeded as `userData.<k>`.

| Slot | Value |
| --- | --- |
| `<slot>` | What a slot tool (2.8) recorded. |
| `<tool>.args.<k>` | The scalar arguments of the tool's **last** call. Replaced wholesale on every call, so an argument left out this time is not read from the time before. Strings are cut to 256 characters; objects and arrays are not stored. |
| `<collect.slot>`, `<collect.slot>.count` | What a tool's `collect` list (2.6.1) holds, joined with `, `, and how many values it holds. |
| `userData.<k>` | Business data the call was placed with. Read-only: the model cannot write it. Nested objects are flattened with dots (`userData.order.id`); arrays are skipped. |

#### 2.6.1 `collect`: a list of values across calls

Every other slot keeps only the latest value, so "which orders did the caller look up?" would be left to the model's memory of the conversation, which recalls numbers that were not found, or never asked. An HTTP tool can instead declare a `collect` rule, and the engine keeps the list:

```json
"collect": {
  "slot": "repair_status.foundOrders",
  "value": "{args.rma}",
  "when": { "slot": "result.found", "op": "EQ", "value": "1" }
}
```

After the tool's result is recorded (`result.*`, `<tool>.<field>`, `<tool>.calls`, `<tool>.args.*`), and before any transition is evaluated, so a condition or the re-rendered instruction already sees the new list:

1. Nothing happens unless the call succeeded (`result.ok` is `"1"`).
2. If `when` is present it is evaluated like a transition condition against the slots (`result.*` included); the value is skipped when it does not hold.
3. `value` is rendered like an `http.body` string (`{args.x}`, `{slots.y}`) and trimmed; an empty result is skipped.
4. It is appended unless an equal value (exact match after trimming) is already in the list; the first occurrence keeps its place.
5. Bounds: at most 20 values (a new value is dropped, and logged, once the list is full, so the first 20 are kept) and each value at most 256 characters (`MaxSlotValueLen`; a longer one is cut, and logged).

`{slots.<slot>}` renders the values joined with `, `, and renders empty while nothing was collected. `<slot>.count` is the number of values (0 at the start), so a transition can say `"slot": "repair_status.foundOrders.count", "op": "GTE", "value": 2`. Both exist from the start of the call.

Rendering the list is not enough on its own. On live calls (qwen, issue #116) a model handed the list and told "one order: answer it; several: ask which" still counted from the conversation and offered a number that was not found. Let the flow do the counting: branch the phase on `<slot>.count`, so each phase's instruction states one case without a condition. The seed `mobile_support` flow stays in `repair_report` ("the order they mean is {slots.repair_status.foundOrders}") while one order was found, and moves to `repair_report_multi` ("ask which of these") once `count` reaches 2. Word the rule about the list as a rule about answering, not looking up: "choose only from the orders found" made a model refuse to look up a new number.

Checked at load: only an HTTP tool may declare it; `slot` is required and made of letters, digits and underscores (dots allowed between segments); neither `slot` nor `<slot>.count` may be a name something else writes (the engine's own slots, `result.*`, `userData.*`, `<tool>.calls`, `<tool>.ok`, `<tool>.args.*`, a tool's `http.result` field, `<node>.visits`, a slot tool's slot, another tool's `collect`); `value` is required, every `{args.x}` in it must be a declared parameter of the tool and every `{slots.y}` a slot the flow writes; `when` must use known operators and slots the flow writes.

### 2.7 Built-in tools

Three tools exist in every flow, and a fourth on campaign calls. They are not HTTP calls; they act on the call itself. A phase allows them by name, or `alwaysAllowedTools` does.

| Tool | Arguments | Behaviour |
| --- | --- | --- |
| `transfer_to_agent` | `queue` (an enum of the deployment's queues, narrowed to the number's own queue when it has one), `reason`, `summary` (at most 600 characters, caller's language), `slots` (an object) | Arms a transfer that runs after the caller has heard the model's line. The `summary`, `reason` and `slots` ride the caller's channel to the agent. A closed or unknown queue is a refusal (`QUEUE_CLOSED`, `QUEUE_UNKNOWN`) with a hint to offer a message. A transfer the switch accepted and never ran (a stalled switch) puts the call back in the phase the tool was called in, without its `announce` and without counting a visit, and the model is told to offer a message; from then on the tool answers `TRANSFER_FAILED`. |
| `take_message` | `message`, `callbackNumber` (optional) | Records an open callback request, defaulting the number to the caller's. |
| `hangup` | `isFarewellSpoken` | Ends the call once the goodbye has been heard. |
| `record_lead` | `intent` (`HIGH`, `MEDIUM`, `LOW`, `NONE` or `DO_NOT_CALL`), `notes` (optional, at most 1000 characters; longer is cut) | **Campaign calls only.** Records what the call concluded about the customer on the call's campaign attempt; the last call wins, so call it again when the customer changes their mind. It is bound to no phase: it is offered and callable in every phase whatever `tools` and `alwaysAllowedTools` say, it never moves the phase and never counts as a turn, and it is not offered on any other call. An intent outside the enum comes back as a tool error and nothing is written. The call is not ended by it. The side effects run once, when the call ends: `DO_NOT_CALL` puts the number on the do-not-call list, `HIGH` files one callback on the campaign's follow-up queue (design 10 section 11). A campaign call is offered neither `transfer_to_agent` nor `take_message`, so a flow meant for campaigns lists neither (`seed/flows/campaign_lead_followup.json` is the reference). A phase can also record the lead itself when the call enters it (2.7.1); both are writes to the same lead, and whichever came last is kept. |

Two details that matter when authoring:

- `slots` in `transfer_to_agent` is an argument the **model** fills in. The engine's slots are not attached automatically. If the agent must receive a decision or an id, say so in the phase instruction so the model passes it, or put it in the `summary`.
- `reason` is a free string, not an enum, even though the design text in `docs/design/02-ai-voice.md` §6 shows one.

A built-in's refusal is a conversation, not an error. When a queue is closed the model explains and offers a message. Business hours therefore belong to the queue, not to a branch in the flow.

#### 2.7.1 A lead the phase declares

`record_lead` depends on the model calling it, and one flow shape leaves the model no turn to do so: a tool classifies the customer and its transition moves straight into a terminal phase whose `announce` is the goodbye. Asking for `record_lead` in the same response as the classification is a request the model may ignore; on a live qwen call it called only the classification tool, the call closed, and the attempt finished with no lead, so no callback was filed (a do-not-call request would have been lost the same way). For that shape, let the phase record the lead:

```json
"close_interested": {
  "instruction": "The customer wants the offer.",
  "announce": {"en": "Great, we will send you the details. Goodbye.", "zh": "好的，我们会把详情发给您。再见。"},
  "isTerminal": true,
  "lead": {"intent": "HIGH", "notes": "Agreed: {slots.agree_words}"}
}
```

- **When it is recorded.** Every time the call enters the phase, on a campaign call: a tool's transition, a `NO_INPUT` rule, the guards that move the call to `closingTarget` or `fallbackTarget` (5.3), the time limit (5.4), and the initial phase when the call starts. A phase entered twice records twice. Returning to a phase after a transfer the switch never ran (2.7, `transfer_to_agent`) is not entering it and records nothing.
- **Last write wins.** A declared lead is one more write to the same lead `record_lead` writes, in the order things happened: a `record_lead` call after the call entered the phase replaces the phase's lead, and entering a phase that declares a lead replaces what the model recorded before. Inside one response the tools are taken in order, as for phase moves.
- **Same effect as the tool.** It is stored on the attempt exactly as a `record_lead` call would store it; the side effects still run once, from the final value, when the call ends (`DO_NOT_CALL` to the do-not-call list, `HIGH` one callback).
- **`notes`** is rendered from the slots when the phase is entered, in the call's language, then trimmed and cut to 1000 characters. It may be left out, and a rendering that comes out empty records empty notes.
- **Not a campaign call.** The same flow may serve single outbound calls, so the declaration is not an error there; it is ignored (logged at debug level).
- **Checked at load.** `lead` must be an object with `intent` and optionally `notes`, and nothing else; `intent` must be one of the five values exactly as written (`high` is refused); every `{slots.x}` in `notes` must be a slot the flow writes. A refusal names the phase, alongside the flow's other problems.

When to declare and when to leave it to the model: declare the lead on a phase whose meaning is the outcome (the customer agreed, asked to be called later, asked never to be called again), above all on a terminal phase the classification moves into, where the model has nothing left to say. Leave it to `record_lead` where the stance is the model's judgement of a conversation that goes on, or changes as it goes ("I'll think about it", then "send me the offer"); `campaign_lead_followup` records that way, in its one working phase. The two combine: a phase records the outcome its meaning implies, and a later `record_lead` can still replace it.

### 2.8 Slot tools: facts the caller states

A fact the caller merely said ("my budget is 5000", "next month") lives only in the model's conversation history, which a long call can truncate. A slot tool writes it down. It is declared in `tools` with `"kind": "slot"` and calls nothing:

```json
"record_budget": {
  "kind": "slot",
  "slot": "budget",
  "description": {
    "en": "Record the budget the caller gave, in their own words, as soon as they answer. Do not call it if they did not answer.",
    "zh": "记录用户说的预算，用用户自己的说法。用户没回答就不要调用。"
  }
},
"record_interest": {
  "kind": "slot", "slot": "interest", "enum": ["high", "mid", "low", "none"],
  "description": { "en": "...", "zh": "..." }
}
```

| Field | Meaning |
| --- | --- |
| `kind` | `"slot"`. Absent or `"http"` is an HTTP tool; anything else is a load error. |
| `slot` | Required. The slot written: `slots.<slot>`. A single name of letters, digits and underscores, unique among slot tools, and never one the engine owns (`caller`, `did`, `entry`, `lang`, `node`, `turns`, `turnsWithoutTool`, `lastTool`, `result`, `userData`, `noInput`). |
| `enum` | Optional. The only values accepted; passed to the model as the enum of its one argument. |
| `description` | Required, bilingual like any tool. Say when to call it and when not to. |

A slot tool has no `http` and no `parameters`; declaring either is a load error. The model is offered one string argument, `value`, generated from the declaration. List the tool in the allowlist of each phase where the fact may be stated, like any other tool.

How it behaves (`internal/flow/slottool.go`):

- **Reading it back.** `{slots.<slot>}` in an `instruction`, an `announce` or an `http.body` renders the value, and a condition can test it (`IS_EMPTY`, `IS_NOT_EMPTY`, `EQ` and the rest). A tool that stays in the phase but changes what the instruction renders re-pins the instruction (2.6), so the bot can be told what is already answered: `Recorded so far (blank means not answered yet): budget={slots.budget}; ...`. A fact recorded in one phase is still there in every later phase and in an HTTP tool's body.
- **Branching.** A slot call is a tool result: `result.ok` is `"1"` and `<tool>.calls` counts calls, so a `TOOL_RESULT` transition on the tool fires, with or without a condition on the slot.
- **The latest call overwrites.** Recording a correction ("not 5000, 8000") replaces the earlier value.
- **Refusals.** An empty or whitespace-only value, a value over **256 characters**, and a value outside the `enum` are answered with `ok: "0"` and the reason. Nothing is stored, no slot changes, and a `TOOL_RESULT` rule that requires `result.ok` of `"1"` does not fire. Over-long is rejected, not truncated: half a fact is never recorded. The model reads the reason and can retry.
- **A declined question stays blank.** If the caller will not answer, the model should not call the tool (say so in its `description`), and the slot stays unset. Write the instruction so a blank is a state: do not ask again, submit what you have. In an `http.body`, a value that is nothing but `{slots.<slot>}` arrives as JSON `null` when the slot is unset (a lone reference keeps the value's type, and there is none); a reference inside other text renders as an empty string. The backend should read `null` as "not answered".
- **A late answer is lost.** A slot tool is an ordinary tool, so it is refused (`ok: "0"`, the current phase's instruction) in any phase whose allowlist does not name it. Once the flow has moved on, for example from the question phase to `done` after the submit, a late answer or correction from the caller is refused and lost. A flow that must accept corrections keeps the slot tools allowed in the later phase and submits again; the seed `lead_qualification` flow does not do that.
- **Models record what you do not mean.** On qwen a model has recorded "it depends" as a budget, invented a value from a noisy transcript, re-asked a declined question, and offered example amounts. Say in the `description` and the instruction what counts as an answer (a concrete amount or range, a concrete time), that "don't know" is a decline, that only the caller's own words are recorded, and that a declined question is not asked again. `lead_qualification` is the reference wording.
- **Normalisation.** Surrounding space is trimmed and line breaks become spaces, so a value cannot add lines to the instruction it is rendered into.
- **Values are untrusted caller text.** They are what a person said, transcribed, and they are rendered into the model's instructions and sent to your backend. Treat them as input: validate them in the backend, and do not let a rule's meaning depend on a slot's content.

Other slots you can read back:

- **`<tool>.args.<k>`** exists for every tool, built-in ones included. It is readable in a template or condition **only for arguments the tool declares** in its `parameters.properties` (a slot tool declares `value`). Referencing anything else is a load error.
- **`userData.<k>`** is accepted for any `k`, because the keys come from whoever placed the call.

**Declared-slot rule, checked at load.** `{slots.x}` in an `instruction`, an `announce`, an `http.body`, an `http.path` or an `http.query` value, and the `slot` of a transition condition, must name something the flow can write, or the flow does not load (a typo would otherwise be heard on a call as an empty gap). The accepted names are: `caller`, `entry`, `did`, `lang`, `node`, `turns`, `turnsWithoutTool`, `lastTool`, `noInput.count`; `userData.<k>`; a slot tool's `slot`; a `collect` list's `slot` and `<slot>.count`; `<node>.visits` for a phase of the flow; `<tool>.calls` and `<tool>.ok`; `<tool>.<field>` where `field` is a key of that HTTP tool's `http.result`; `<tool>.args.<k>` where `k` is a declared parameter; and `result.<field>` for a field some tool maps (`ok` included). `persona` and `rules` are not rendered, so they are not checked.

**Providers.** A slot is engine state, so it is exact in templates and HTTP bodies on every engine. Every engine (OpenAI, Qwen, the gateway and Doubao) re-pins at once: a changed instruction applies from the next reply, measured live on each. Even so, the model's own memory of an earlier turn is not a record, so keep the "never ask a recorded question again" rule cheap to obey, and carry a fact that must not be lost in an HTTP body rather than only in the prompt.

## 3. What the platform already does for you

Do not spend flow text on these.

- **Standing platform rules.** Every flow gets two rules ahead of its persona (`Runtime.Instructions`): the model keeps its instructions private, and it must not say it is transferring, looking something up, saving a message or ending the call unless it calls the matching tool in the same reply. Do not repeat them in `rules`.
- **Hint steering.** When a transition fires, the tool result the model receives carries the new phase's instruction as `hint`, which replaces anything the tool would have said. The model reads it as something it just learned.
- **Queue names.** `transfer_to_agent`'s `queue` argument is an enum of real queues, so the model cannot invent one. A phase instruction may still tell it which queue fits (`novanet_support` names `support-en` and `support-zh`).
- **A slow tool.** The wait line (2.4.2) is said when an HTTP tool runs past a second.
- **Dead air and loops.** Section 5.

## 4. Patterns for a business that is not simple

### 4.1 Thick tools, thin flow

Put a business rule in the backend. One composite endpoint makes the decision and returns it as a field. `http.result` maps the field to a slot, and the flow branches on it.

Instead of a phase per rule and a `condition` tree over `in_contract`, `tenure` and `balance`, the backend answers:

```json
{ "retCode": "000000", "data": { "decision": "NEED_ID_VERIFY", "reason": "CHANGE_OVER_THRESHOLD", "spoken": "This change needs a quick identity check first." } }
```

and the flow only reads `result.decision`. Changing the rule changes the backend; the published flow does not change and does not need re-publishing.

Guidance:

- **Return the decision on a success envelope.** A backend that answers `NEED_HUMAN` as a failure (`successWhen` not holding) loses every mapped field, because a refusal maps none. Use the failure path for "the backend could not decide", and a field for "the backend decided no".
- **Use a small closed vocabulary** of decision values (`OK_TO_CHANGE`, `NEED_ID_VERIFY`, `NEED_HUMAN`) and one transition per value. A phase with a transition for every value the backend can return, plus a fallback rule with a higher `priority` number, cannot dead-end silently.
- **Let the backend also return what the model should say**, in a short spoken-ready field (`spoken`, `reason`), mapped into the result. The model reads it from the tool result, so the wording of a rule lives next to the rule.
- **Keep the model out of the arithmetic.** A rule such as "if `in_contract` is 1, transfer" in an instruction is a business rule held in prompt text, and it is the shape this pattern replaces: a `decision` field and one transition make it deterministic. The seeded `plan_change` flow used to carry exactly that rule and now reads `NEED_HUMAN` from `plan_check` instead (7).

### 4.2 Knowledge through a tool

Policies, tariffs and FAQs do not belong in `persona`, `rules` or an `instruction`. A long standing prompt costs instruction-following and is resent whenever the instructions are re-pinned, which is every phase change.

The knowledge base is a service that lives **outside this repository** (the same boundary as the speech gateway: the extension point is a service behind the flow's backend URL, never code in this tree). The reference service is [aicc-knowledge](https://github.com/rasonyang/aicc-knowledge); its contract is its `docs/openapi.json`. It does **retrieval only**: the realtime model writes the answer, so no second generation runs inside the tool call. It has two operations, each one an ordinary HTTP tool with a `baseUrl`, a secret `Authorization` header and a `timeoutMs` (2.4.1). A complete flow that uses both is the reference flow `knowledge_support` (see below).

**The reference flow.** `docs/examples/flows/knowledge_support.json` (bilingual, three phases: `help`, `finish_transfer`, `farewell`) carries both tools below exactly as shown, plus the instruction pattern. It lives outside `internal/seed/flows` on purpose: the demo seed publishes every embedded flow and must not depend on aicc-knowledge running. Its scenarios are `docs/examples/scenarios/knowledge_support.json`, and `go test ./internal/flowsim` replays them against the mock backend, which answers `/v1/search` and `/v1/facts/plans/lookup` in the service's shapes (`internal/mockbackend/knowledge.go`; a search for `kb-down` answers 503 `INDEX_UNAVAILABLE`). In development, with aicc-knowledge on its default `127.0.0.1:18480` and `AICC_TOOL_SECRET_KB_API_KEY` set in `.env` (publishing a flow that references an unset secret is refused), publish it on two test numbers:

```sh
/tmp/aicc flowadd -file docs/examples/flows/knowledge_support.json -did 95077 -language en
/tmp/aicc flowadd -file docs/examples/flows/knowledge_support.json -did 95078 -language zh
```

**Search the FAQ (`searchFaq`).** The model supplies only the question; the language comes from the call, never from the model.

```json
"search_kb": {
  "description": {
    "en": "Search the FAQ and policy knowledge base. Call it for any question about prices, terms, fees or policy before answering.",
    "zh": "检索常见问题与政策知识库。凡涉及价格、条款、费用或政策的问题，先调用本工具再回答。"
  },
  "parameters": {
    "type": "object",
    "properties": { "query": { "type": "string", "description": "The caller's question, in a few words" } },
    "required": ["query"]
  },
  "http": {
    "baseUrl": "http://127.0.0.1:18480",
    "path": "/v1/search",
    "headers": { "Authorization": "Bearer {secrets.KB_API_KEY}" },
    "timeoutMs": 3000,
    "body": { "query": "{args.query}", "language": "{slots.language}", "topK": 3, "timeoutMs": 2500 },
    "errorFrom": "error.code",
    "result": { "kbStatus": "status" },
    "modelOnly": {
      "status": "status",
      "items": { "path": "items", "each": { "answer": "answer", "sourceRef": "sourceRef" } }
    }
  }
}
```

When the deployment configures scope keys in the service, add `"scope": { "<key>": "<value>" }` to the body as a literal. `topK` is 1 to 3.

**Look up a fact (`lookupFact`).** One HTTP tool per fact table, which declares that table's key columns as typed parameters. A single `lookup_fact(table, key)` would leave the model guessing the column names.

```json
"lookup_plan": {
  "description": {
    "en": "Look up the price and terms of a plan from the plan table. Use it for any question about what a plan costs or includes.",
    "zh": "查询套餐表中某个套餐的价格与条款。凡问到套餐多少钱、包含什么，都用本工具。"
  },
  "parameters": {
    "type": "object",
    "properties": {
      "plan": {
        "type": "string",
        "enum": ["Basic", "Plan 59", "Plan 59 Plus", "Plan 99", "Fiber 500", "Fiber 1000"],
        "description": "Plan key. 基础套餐 = Basic; 59元套餐 = Plan 59; 59元Plus套餐 = Plan 59 Plus; 99元套餐 = Plan 99; 光纤500 or 500兆光纤 = Fiber 500; 光纤1000 or 千兆光纤 = Fiber 1000."
      }
    },
    "required": ["plan"]
  },
  "http": {
    "baseUrl": "http://127.0.0.1:18480",
    "path": "/v1/facts/plans/lookup",
    "headers": { "Authorization": "Bearer {secrets.KB_API_KEY}" },
    "timeoutMs": 3000,
    "body": { "key": { "plan": "{args.plan}" } },
    "errorFrom": "error.code",
    "modelOnly": { "status": "status", "row": "row", "validFrom": "validFrom", "validTo": "validTo" }
  }
}
```

- **Key values are an enum, maintained by hand.** A lookup matches exactly, but a caller says "基础套餐" while the table holds `Basic`. Declare each key parameter's valid values as an `enum` with zh and en descriptions, so the model can map what the caller said. When the table gains a row the enum does not list yet, the lookup answers `NOT_FOUND` and the bot handles it as an ordinary conversation; publish does not check the enum against the service.
- **Name the units in the tool's description.** A `row` holds bare numbers (`"monthly_fee": 129`), and the service does not say what they count. A model left to guess supplies a unit of its own: on a live call an English bot quoted a yuan fee as "$129". Say in the description what each value column is measured in (yuan, GB, minutes, months), as the reference flow does.
- **`at` is never sent.** Every lookup uses the service's today. A question about a future date would need the model to work out a date, which it cannot do reliably; the instruction should say the bot can quote today's price only.
- **A fact table holds facts shared by every caller.** A question about the caller's own data ("my plan", "my bill") goes to a business API tool or to a person, never to a fact lookup.

**Credentials and hosts.** The key is `Authorization: Bearer {secrets.KB_API_KEY}`, read from `AICC_TOOL_SECRET_KB_API_KEY` (2.4.1). A service on the LAN or on the same host needs no entry in `AICC_TOOL_ALLOWED_HOSTS`; a public one does.

**What the model gets.** The results go through `modelOnly`, so the passages reach the model for that turn and are not kept in any slot for the rest of the call. Search hands the model `status` (`HIT` or `NO_MATCH`) and each item's `answer` and `sourceRef`. `score` is withheld on purpose: the service decides `NO_MATCH` against a per-index threshold, so its `status` already applies it, and a flow must not apply a threshold of its own. A fact lookup hands over `status` (`FOUND` or `NOT_FOUND`), `row`, `validFrom` and `validTo`. The only slot written is what `result` maps (`kbStatus` above, readable by a condition as `result.kbStatus`).

**Two budgets, set together.** The body's `timeoutMs` is the service's own bound on the search; the tool's `http.timeoutMs` is the engine's. Set the body's below the tool's (2500 and 3000 above), so a slow upstream is answered by the service as 504 `UPSTREAM_TIMEOUT` and not by the engine dropping the connection. The engine does not check the pair (the body is opaque to it). A search slower than 1 second gets the wait line (2.4.2); there is no retrieval-specific filler.

**Failures.** A 4xx or 5xx is a tool failure the model reads as `ok: "0"`, and the service's error code is in the log, so `INDEX_UNAVAILABLE`, `UPSTREAM_UNAVAILABLE`, `UPSTREAM_TIMEOUT`, `UNAUTHORIZED` and `UNKNOWN_SCOPE_KEY` can be told apart: the log line `http tool answered with an error status` carries `status` and `backendError`.

**The instruction pattern.** Say in the phase instruction (or `rules`):

- On `HIT`, answer only from the `answer` texts, and never state a price, date or limit that is not in them.
- An answer limited in time ("until 30 June 2026", "this offer has ended") is said with its limit, never as if it applied now, and when answers disagree the one with no end date wins. The model does not know today's date (issue [#153](https://github.com/rasonyang/ai-native-callcenter/issues/153)), so it cannot tell on its own that a date has passed; the limit in the text is all it has.
- On `NO_MATCH` or a failure, say so honestly and offer a transfer or a message.
- A question that needs the caller's own account goes to a person.
- A fact comes from the lookup tool, never from FAQ text.
- A question about the caller's own data ("my plan", "my bill") goes to a business API tool or to a person, never to a fact lookup.

Further guidance:

- **Allow the tools in the phases where questions arise** and no wider. A search tool in a phase does not move the call: with no transition for it, the call stays put and the conversation goes on.
- **Do not map passages into `result` on purpose.** Mapped fields are slots, and a slot named in an instruction is re-pinned into the standing instructions; that is what `modelOnly` avoids.
- Other backends (a vendor-neutral search contract, per-vendor templates, a signing proxy) are not built: see 6.

### 4.3 Long operations

An HTTP tool is bounded by its `timeoutMs` (5 seconds by default, 10 at most) and a caller is waiting on the line. Never write a tool that blocks on a slow job. Two shapes work with what exists:

1. **Submit and check.** One tool starts the work and returns at once with a ticket; a second tool reports its status. The phase instruction says to submit, read out the ticket, and check status when the caller asks. The instruction should say what to tell the caller when the state is still pending. Both calls are ordinary tools, so a pending state is just a field the model reads.

   ```json
   {
     "tools": {
       "plan_submit": {
         "description": "Submit the confirmed plan change. Returns a ticket immediately; processing continues in the background. Only call after the caller explicitly confirms.",
         "parameters": {
           "type": "object",
           "properties": {
             "service_no": { "type": "string" },
             "plan_id": { "type": "string", "enum": ["1", "2", "3"] }
           },
           "required": ["service_no", "plan_id"]
         },
         "http": {
           "path": "/api/plan/submit",
           "body": { "serviceNo": "{args.service_no}", "planId": "{args.plan_id}" },
           "successWhen": { "path": "retCode", "equals": "000000" },
           "errorFrom": "retMsg",
           "result": { "ticket": "data.ticket", "state": "data.state" }
         }
       },
       "plan_status": {
         "description": "Check the progress of a submitted plan change by its ticket.",
         "parameters": {
           "type": "object",
           "properties": { "ticket": { "type": "string" } },
           "required": ["ticket"]
         },
         "http": {
           "path": "/api/plan/status",
           "body": { "ticket": "{args.ticket}" },
           "successWhen": { "path": "retCode", "equals": "000000" },
           "errorFrom": "retMsg",
           "result": { "state": "data.state", "eta": "data.eta" }
         }
       }
     }
   }
   ```

2. **Call back.** When the work cannot finish during the call, tell the caller they will be called back and use `take_message`, which records an open callback an agent works later. Say so in the phase instruction, and offer the same when a tool fails twice.

The wait line (2.4.2) covers a tool that runs past a second; it is a bridge over a few seconds, not a reason to write a slow tool. Do not rely on the caller waiting through silence. `NO_INPUT` fires after the dead-air timeout (`AICC_BOT_NO_INPUT_TIMEOUT`, 8 seconds by default), and `maxDurationSec` (5.4) still runs.

### 4.4 Risky operations

Whether a step needs a person is a business decision, so the backend makes it, not the prompt. Have the decision tool return `NEED_HUMAN` and let a transition send the call to a phase whose only job is to hand over:

```json
{
  "nodes": {
    "need_transfer": {
      "instruction": {
        "en": "This cannot proceed via self-service. Apologise briefly, then immediately call transfer_to_agent. Put the decision and the reason in the summary.",
        "zh": "当前业务无法自助办理：向用户简短道歉，立即调用 transfer_to_agent，并把判定结果和原因写进 summary。"
      },
      "tools": []
    }
  }
}
```

`tools: []` is enough because `transfer_to_agent` is in `alwaysAllowedTools` in the seeded flows. The global transition on a successful `transfer_to_agent` then moves the call to a terminal phase whose `announce` is the hand-over line (the seeded `finish_transfer`). The same phase is the right `fallbackTarget`: a model that loops, or a tool that fails repeatedly, ends in a person. The seeded flows do this by counting calls (`plan_verify.calls GTE 2`, priority 1).

Do not let a prompt rule such as "never change a plan over 200" be the only guard. The model can be talked past a prompt; it cannot call a tool the phase does not allow, and it cannot make the backend return a different decision.

## 5. Rules the platform already enforces for flows

### 5.1 Tool allowlists

Keep each phase's allowlist small. A tool that is not allowed cannot run, so the allowlist is what stops the model booking before it has identified the caller. Prefer naming tools to `"*"`, and put only the tools a person could ask for at any time (`transfer_to_agent`, `hangup`) in `alwaysAllowedTools`.

### 5.2 Lines a business has signed off: `announce`

An `announce` is spoken as written, in the call's language, with `{slots.x}` rendered. Use it for a regulated disclosure, the hand-over line or the goodbye, where improvising is not acceptable. For the entry phase it is the opening line (`SessionConfig.OpeningText`); afterwards it is `VoiceSession.SpeakText`. It pre-empts and never queues. How exact it is depends on the engine: exact on doubao, best effort elsewhere (`docs/provider-extension.md` has the per-engine table).

Leave `announce` off phases where the model is answering the caller. It exists for the two ends of a call.

A deployment whose provider cannot be cued by text (doubao) refuses to **publish** a flow whose terminal phases have no `announce` (422 `TERMINAL_ANNOUNCE_REQUIRED`, naming the phases). Loading is unaffected. Writing an `announce` on every terminal phase makes a flow portable.

### 5.3 Closing phases and the two guards

Rule for a closing phase (`docs/design/02-ai-voice.md` §6): after "anything else?", a decline ("no", "that's all") means call `hangup` at once and never ask a second time; a follow-up or a new request is handled normally; a request to repeat is honoured. Write it into the phase instruction, as `plan_change`'s `submitted` does. Say what is *not* a decline as well: an acknowledgement or filler ("okay", "um", "嗯", "好的") or an utterance the model did not catch. On a live qwen call a caller's "嗯" before a follow-up question, cut off as a turn of its own by the server VAD, was taken as a decline and the call was hung up; one sentence in the instruction ("only a clear decline ends the call; a filler or something you did not catch is not one, ask what else they need") fixed it.

A model-spoken goodbye followed by `hangup` is unreliable ("say the closing line, then call hangup"). Some models, qwen among them, speak the line, end the turn without the tool call and leave the call open until dead air (issue [#147](https://github.com/rasonyang/ai-native-callcenter/issues/147)). Two shapes work:

- **A phase whose whole job is to close is a terminal phase with an `announce`.** The platform arms the ending before the line is said, speaks the line as written and hangs up after the caller has heard it; the model does no part of it. Reach it with a transition on the tool that finishes the work. Everything the line needs must be renderable in `announce` (`{slots.x}`, `{slots.<tool>.<result field>}`, `{slots.<tool>.args.<k>}`); when the line depends on a result, branch with conditioned transitions to separate terminal phases (the seeded `lead_qualification` goes to `finish_advisor` on `result.lead_grade EQ A` and to `finish_thanks` otherwise, `early_collections` ends in `promised` and `no_bill`).
- **In a phase where the conversation goes on, call the tool first.** Word the instruction as the `hangup` built-in does: call `hangup` immediately, do not say goodbye yourself, the closing line follows (`global.closingTarget`'s `announce`, or a phase-local `hangup` transition to a more specific terminal phase, as `early_collections` does for a caller who refuses to talk). A rule that says "apologize and call hangup" has the same defect as "say goodbye and call hangup".

`closingTarget` and the dead-air path remain backstops, not the normal ending.

The engine cannot see a caller's decline, only tool results and silence, so three guards back the rule up. All go to `global.closingTarget`, a terminal phase with its own `announce`:

- **Silence.** In a phase where no authored rule can fire on `NO_INPUT`, three consecutive silences move the call to `closingTarget`. A rule you wrote that can fire on `NO_INPUT` in that phase, however it is conditioned, switches this default off for the phase.
- **Tool-less replies.** `maxTurnsWithoutTool` bounds replies in one phase that follow the caller speaking and make no tool call. When the count exceeds it, the call moves to `closingTarget` after the caller has heard the reply. It resets on a tool call and on any phase change. Set it above the longest tool-less stretch any phase is meant to hold; the seeded flows use 10.
- **A goodbye with no hangup.** If the model said goodbye in words, made no tool call, and the caller then goes quiet, the dead-air path moves the call to `closingTarget` instead of asking the model anything (issue [#147](https://github.com/rasonyang/ai-native-callcenter/issues/147)). It does not apply to a campaign call, a terminal phase, an armed ending or a flow with no `closingTarget`.

These are backstops for a model that ignores the closing instruction, not the mechanism that normally ends a call. `maxTurns` with `fallbackTarget` is the equivalent backstop for a model that calls tools in a loop.

### 5.4 The wall clock

`maxDurationSec` counts from the moment the bot answers. At 80% a wrap-up line is added to the standing instructions; at 100% the call goes to the number's queue with `reason=SESSION_LIMIT` if that queue is open, otherwise to `closingTarget`. Absent means 900 seconds, not unlimited. Long-running flows (4.3) should keep this in mind. With no open queue and no `closingTarget`, the call gets a plain goodbye and a hangup.

### 5.5 Keep the standing prompt short

`persona` and `rules` are part of the standing instructions, which are re-pinned on every phase change, and `{slots.x}` is not rendered in them. Put in them only what holds for the whole call: who the bot is, the spoken style, the languages, and the handful of rules that apply everywhere (spoken output only, never invent numbers, confirm before a transaction). Everything else belongs in a phase instruction, a tool description, or the backend.

A rule that says "never invent plans, prices or order numbers; they must come from tool results" is worth keeping in `rules`, and is the reason a tool, not the prompt, should hold the facts.

## 6. What the DSL does not do yet

Say these to the business rather than working around them in the prompt.

| Need | Status |
| --- | --- |
| **Facts the caller states.** "I'm calling about order 1234", "my budget is 200". | Supported: a slot tool records the fact, see 2.8. Tool arguments are also kept as `<tool>.args.<k>`, and an outbound call's business data as `userData.<k>` (2.6). A list of values derived from a tool's calls ("all the orders looked up") is a `collect` list on that tool (2.6.1). Still absent: a list of values the caller merely said (a slot tool keeps only the latest), and a start-of-call lookup (issue [#84](https://github.com/rasonyang/ai-native-callcenter/issues/84)). Issue [#13](https://github.com/rasonyang/ai-native-callcenter/issues/13). |
| **Collecting digits, a keypad menu, masked secrets.** | Not supported. Keypad input is not a flow event. Today each key reaches the model as a text cue (`(The caller pressed 5 on their keypad.)`), interrupts the bot, and asks for a reply; the model assembles the digits and follows your prompt. That is not deterministic, and digits pass through the model, the transcript and the logs. Do not collect PINs or card numbers this way. Issue [#91](https://github.com/rasonyang/ai-native-callcenter/issues/91). |
| **Knowledge retrieval as a platform feature.** | Supported against aicc-knowledge only: FAQ search and fact lookup are ordinary HTTP tools (4.2), issue [#92](https://github.com/rasonyang/ai-native-callcenter/issues/92). Other backends, a vendor-neutral contract and store templates are not built. |
| **Testing a flow with a model in the loop.** | `aicc flowsim` replays scripted scenarios deterministically (8). Tier 2 (a text model as the bot and another as the caller) is opt-in and spends money (8). |
| **Expressions, derived slots, arithmetic.** | By design the DSL has none; use a decision field (4.1). |
| **Asynchronous tools.** | A tool call is synchronous, bounded by its `timeoutMs` (10 seconds at most); there is no way to be notified when a background job finishes. Use submit and check, or a callback (4.3). |
| **Transitions on what the caller said.** | The only events are `TOOL_RESULT` and `NO_INPUT`. To branch on the caller's intent, have the model call a tool whose result carries the branch. |

## 7. A worked example

The worked example is a real flow: `internal/seed/flows/plan_change.json`, a broadband plan change (DIDs 95021 English and 95022 Chinese). It is served by the mock backend (`go run ./cmd/aicc-mockbackend`, handlers in `internal/mockbackend/mockbackend.go`), walked by `internal/seed/scenarios/plan_change.json`, and replayable with no database or model:

```sh
aicc flowsim -flow internal/seed/flows/plan_change.json -scenario internal/seed/scenarios/plan_change.json
```

It uses the patterns of section 4 together: a decision tool (4.1), a knowledge tool (4.2), submit-and-check (4.3), a backend-decided hand-over (4.4) and the closing rule (5.3). Read the file for the full text; this section shows the parts that carry the design.

### 7.1 The call

Seven phases:

| Phase | Job | Leaves on |
| --- | --- | --- |
| `identity` (initial) | Announce, take the service number, call `plan_check`. `kb_search` allowed. | `OK_TO_CHANGE` to `confirm`; `NEED_ID_VERIFY` to `verify`; `NEED_HUMAN` to `need_transfer`; a third `plan_check` that decided none of those (an unknown number, `NOT_FOUND`) to `need_transfer`. A `NOT_FOUND` earlier stays here and the model asks again. |
| `verify` | Explain the check, ask the last 4 digits of the registered phone, call `plan_verify`. | `match` of `1` to `confirm`; a second call that did not match to `need_transfer`. |
| `confirm` | Read the options, restate the choice, get a yes, call `plan_submit`. `kb_search` allowed. | A submit that worked to `submitted`; a refused or failed one to `need_transfer`. |
| `submitted` | Read the ticket, answer `plan_status`, ask "anything else?". | Global `hangup` rule to `finish`. |
| `need_transfer` | Apologise, call `transfer_to_agent` with the decision and reason in the summary. `take_message` allowed for a closed queue. | Global rule on a successful transfer to `finish_transfer`. |
| `finish_transfer`, `finish` | Terminal, each with an `announce`. | |

The old version of this flow had a `bb_account` lookup, a `bb_verify` for every caller, an `offer` phase and a prompt rule "if `in_contract` is 1, transfer". The business rules are now the backend's, so there is one decision tool and one transition per decision value.

### 7.2 The decision tool and what the flow does with it

`plan_check` answers on a success envelope, with the decision, the reason, a sentence the model can say, the plan options as a field of their own, and the current plan. The backend answers in the call's language (`lang` in the body), so no wording is duplicated in the flow:

```json
"plan_check": {
  "http": {
    "path": "/api/plan/check",
    "body": { "serviceNo": "{args.service_no}", "caller": "{slots.caller}", "lang": "{slots.lang}" },
    "successWhen": { "path": "retCode", "equals": "000000" },
    "errorFrom": "retMsg",
    "result": {
      "decision": "data.decision", "reason": "data.reason", "spoken": "data.spoken",
      "options": "data.options", "current_plan": "data.current_plan"
    }
  }
}
```

The entry phase branches on it. The last rule is the guard behind the decisions: rules are tried in ascending `priority`, so it only fires when no decision rule did, which for an unknown number means the third failed attempt:

```json
"identity": {
  "tools": ["plan_check", "kb_search"],
  "transitions": [
    { "on": "TOOL_RESULT", "tool": "plan_check", "condition": { "slot": "result.decision", "op": "EQ", "value": "OK_TO_CHANGE" },   "target": "confirm" },
    { "on": "TOOL_RESULT", "tool": "plan_check", "condition": { "slot": "result.decision", "op": "EQ", "value": "NEED_ID_VERIFY" }, "target": "verify" },
    { "on": "TOOL_RESULT", "tool": "plan_check", "condition": { "slot": "result.decision", "op": "EQ", "value": "NEED_HUMAN" },      "target": "need_transfer" },
    { "on": "TOOL_RESULT", "tool": "plan_check", "condition": { "slot": "plan_check.calls", "op": "GTE", "value": 3 }, "target": "need_transfer", "priority": 1 }
  ]
}
```

`NOT_FOUND` has no rule of its own on purpose: the call stays in `identity` and the instruction tells the model to ask for the number again.

`confirm` and `verify` render what the backend said into their instructions:

```json
"verify":  { "instruction": { "en": "An identity check is needed first. Tell the caller: {slots.plan_check.spoken} Ask for the last 4 digits ..." } },
"confirm": { "instruction": { "en": "The change is allowed. Current plan: {slots.plan_check.current_plan}. Available plans: {slots.plan_check.options} Let the caller pick plan 1, 2 or 3 ..." } }
```

`spoken` and `options` are separate fields because they say different things. An earlier draft of this example rendered `spoken` as "what the caller can choose"; for `NEED_ID_VERIFY` that field holds the explanation of the identity check, so the caller was told about the check where the plans belonged. One field, one meaning.

### 7.3 Knowledge, long operations and the hand-over

`kb_search` is allowed in `identity`, `confirm` and `submitted`, the phases where a caller asks about fees and terms, and the phase instructions say when to call it and what a miss means: with `found` of `0`, say you do not have that information and offer an agent; never answer from memory. The tariffs, fees and policy text are not in `persona` or `rules`; the mock backend holds a handful of bilingual entries (`kbEntries` in `internal/mockbackend/mockbackend.go`), a real deployment puts a retrieval service behind the same path.

`plan_submit` returns a ticket at once and a state of `PROCESSING`; `plan_status` reports the progress. `submitted` renders `{slots.plan_submit.ticket}` and `{slots.plan_submit.effective}`, and tells the model to call `plan_status` only when the caller asks. A refused submit (`result.ok` of `"0"`) goes to `need_transfer`:

```json
"confirm": {
  "tools": ["plan_submit", "kb_search"],
  "transitions": [
    { "on": "TOOL_RESULT", "tool": "plan_submit", "condition": { "slot": "result.ok", "op": "EQ", "value": "1" }, "target": "submitted" },
    { "on": "TOOL_RESULT", "tool": "plan_submit", "condition": { "slot": "result.ok", "op": "EQ", "value": "0" }, "target": "need_transfer", "priority": 1 }
  ]
}
```

The hand-over phase is also `fallbackTarget`. Its instruction renders `{slots.plan_check.decision}` and `{slots.plan_check.reason}` and tells the model to put them in the `summary` of `transfer_to_agent`, because the engine's slots are not attached to a transfer on their own (2.7). The slots are the last `plan_check` result, so a caller who failed the phone check arrives with `NEED_ID_VERIFY` and the reason for the check, and the instruction also asks the model to say whether the check failed or a submit was refused.

`need_transfer` allows `take_message`. That built-in records an open callback for an agent to work later; it neither moves the phase nor ends the call. It is there for one case: a closed queue refuses the transfer (2.7) and the model offers a message, which needs the tool in the phase. The instruction ends that path with `hangup`.

### 7.4 The closing

`submitted` is the "anything else?" phase: a decline means call `hangup` first, with no goodbye of the model's own, and a global rule on `hangup` moves the call to `finish`, a terminal phase whose `announce` says goodbye. `closingTarget` is `finish` and `maxTurnsWithoutTool` is 10, behind it (5.3).

### 7.5 The mock backend's fixtures

| Service number | `plan_check` decision | Last 4 of phone | Notes |
| --- | --- | --- | --- |
| `10086001` | `OK_TO_CHANGE` | `3333` | Plan 3 is refused at submit (not available at the address). |
| `10086002` | `NEED_HUMAN`, `IN_CONTRACT_EARLY_TERMINATION_FEE` | `7777` | Inside the contract term. |
| `10086003` | `NEED_ID_VERIFY` | `5555` | Verify, then any plan. |
| `10086004` | `NEED_HUMAN`, `ARREARS` | `9999` | Overdue balance. |
| anything else | `NOT_FOUND` | | The flow asks again; the third failed check hands over. |

A ticket's first `plan_status` is `PROCESSING` with an `eta`; every later one is `DONE`. `kb_search` matches keywords (plan prices and speeds, installation fee, early-termination fee, when a change takes effect, contract term) and answers in the request's `lang`; anything else is `found` of `0`.

### 7.6 What to notice

- **No business rule is in the flow.** The thresholds behind `NEED_HUMAN` and `NEED_ID_VERIFY` live in the backend. The flow has one transition per decision value and a guard.
- **`<tool>.<field>` slots persist**, `result.*` does not. `{slots.plan_check.options}` still renders in `confirm` after `plan_verify` has run, because `plan_check.options` stays until `plan_check` runs again.
- **A refusal is a branch.** A refused `plan_submit` goes to the hand-over rather than leaving the model to improvise.
- **The standing prompt is short.** No tariff is in it; every price the bot says came from a tool result.
- **A number arrives in pieces.** Callers pause while reading a number, and the server VAD ends a turn at each pause, so the model sees "100" and then "86003". The `identity` instruction says the number has 8 digits, to join consecutive pieces and to call `plan_check` only with all 8; without it the model looked up the fragment, got `NOT_FOUND` and spent one of the three checks the guard allows.
- **A progress question is asked again.** `submitted` tells the model to call `plan_status` on every progress question and never reuse an earlier answer; a status is a fact that changes, and a model that has one in its history will otherwise repeat it.
- **Every `{slots.x}` in the file is checked at load** (2.8), and the scenarios assert the rendered instruction (`instructionContains`), so a slot that stops being filled fails the test.

## 8. Verifying a flow

- **Load.** `aicc flowadd -file <flow>.json -did <number>` loads and publishes (the same slug updates and republishes). With `-did`, a number that does not exist is created **inbound only** (`allowInbound=true`, `allowOutbound=false`), and the command prints that; it cannot be the caller id of an AI outbound call (`POST /calls` with `AI_OUTBOUND` answers `422 DID_NOT_OUTBOUND`) until `allowOutbound` is set on it with `PUT /dids/{didId}` (`updateDID`). A number that already exists keeps all its flags and only its flow changes; the command prints the flags it kept. The same validation runs on `POST /flows` and `PUT /flows/{flowId}` (a spec the loader rejects is refused with 422 listing every problem). Publishing can additionally refuse with `TERMINAL_ANNOUNCE_REQUIRED` (5.2).
- **Backend.** Run `go run ./cmd/aicc-mockbackend` and point `AICC_BOT_BACKEND_BASE` at it to walk the seeded flows against deterministic fixtures (`docs/dev-stack.md`). For a new flow, write the equivalent handlers for your endpoints. Test the failure paths on purpose: a tool that times out, a `found: 0`, an unknown decision value.
- **Scenarios (the flow simulator).** `aicc flowsim` replays a scripted call against your flow on the real `flow.Engine` and `flow.Runtime`, with the flow's real HTTP tools served by the mock backend, and needs no database, switch or model:

  ```sh
  aicc flowsim -flow my_flow.json -scenario my_flow.scenarios.json      # add -v for every step, -only "name" for one scenario
  go test -race ./internal/flowsim/                                   # every seed flow against internal/seed/scenarios/
  ```

  A scenario fixes the call's metadata (`lang`, `caller`, `did`, `userData`), whether it is a campaign call (`isCampaign`: `record_lead` is offered, the leads phases declare are recorded, `transfer_to_agent` and `take_message` are withdrawn), the queues the deployment has, the answers of the built-ins (`actions`), and a list of `steps`. A step is exactly one event: `say` (a caller utterance), `keypress`, `exchange` (the caller speaks and the bot answers in words with no tool call), `noInput`, `botTurn` (`reply` or `interrupted`), `sessionLimit`, `call` with `args` (a tool the bot calls) or `batch` (several tools in one response; the last move wins). `repeat: N` runs a step N times. A step may carry `expect`: `phase`, `isOk`, `isRefused`, `result` (a subset of what the model receives), `slots` (a subset), `announce`, `instructionContains` (the rendered instruction, so a lost `{slots.x}` is caught). The scenario's own `expect` checks the whole call: `path` (phases entered, in order), `phase`, `ending` (`hangup`, `transfer_to_agent`, `take_message` or `none`), `transfer` (`queue`, `reason`), `toolsRun`, `slots`, `lead` (`intent`, `notes`, or `isUnrecorded` for a call that never recorded one; the last write wins, `record_lead` or a phase's declared lead). Unknown fields are an error, so a misspelt expectation cannot pass by checking nothing.

  ```json
  {"scenarios": [{
    "name": "caller disputes the bill",
    "lang": "en", "caller": "90000000", "queues": ["support-en"],
    "steps": [
      {"call": "bill_lookup", "expect": {"phase": "remind", "result": {"days_overdue": "21"}}},
      {"call": "dispute_record", "args": {"reason": "already paid"}, "expect": {"phase": "disputed"}},
      {"call": "transfer_to_agent", "args": {"queue": "support-en", "reason": "BILL_DISPUTE", "summary": "paid"}, "expect": {"phase": "finish_transfer"}}
    ],
    "expect": {"path": ["greet", "remind", "disputed", "finish_transfer"], "ending": "transfer_to_agent"}
  }]}
  ```

  What to cover: the main path, each failure (a tool answering `found: 0` or `ok: 0`, a refused transfer, an unknown queue), silence (`noInput` three times), the turn limits (`repeat` a tool past `maxTurns`; `exchange` past `maxTurnsWithoutTool`), and a tool called in a phase that does not allow it (`isRefused`). The run prints each scenario's phase path and ending, then the phases and phase-local transitions no scenario reached. The reference flow's scenarios (`docs/examples/scenarios/knowledge_support.json`) cover a covered and an uncovered question, a fact found and not found, a plan outside the enum, a question about the caller's own account and a backend that is down, each in en and zh. Seed scenarios live in `internal/seed/scenarios/<flow>.json` (outside the embedded `flows/` directory), and `go test` requires a scenario file per seed flow that reaches every phase.

  The simulator checks the flow's logic: transitions, allowlists, slot rendering, built-in results, the guards. It is deterministic, so it cannot tell you what a model would say or whether it would call the tool you scripted; it does not exercise audio, barge-in, playback timing or provider quirks either. Passing it does not verify a call-path change.

- **Model in the loop (tier 2, opt-in, spends money).** A scenario with a `callerPersona` and a `model` block is also run by a text model playing the bot against a second text model playing the caller. The bot's model sees what a real call's model sees, taken from the same public `Runtime` methods the voice path uses: `Instructions()` (the platform rules, persona, rules and the phase) as the system prompt, `Tools()` as the tool list, `Announce()` as the line said as written, and every tool call goes through `Dispatch`, so phase moves, hints, allowlist refusals and the guards are the real ones. The caller model sees only its persona and what the bot said.

  ```json
  {"name": "model: english caller asks for a person", "lang": "en", "caller": "8005550100",
   "queues": ["support-en", "support-zh"],
   "callerPersona": {"goal": "Your internet is down; ask for a human agent.", "behaviour": "Slightly impatient, but polite."},
   "model": {"ending": "transfer_to_agent", "transfer": {"queue": "support-en"}, "phase": "finish_transfer", "passRate": 0.67}}
  ```

  `callerPersona` is `goal`, `persona` (facts the caller knows), `behaviour` (cooperative, declining, off-topic, or silent: the caller model answers `<silence>` and the engine gets a `NO_INPUT`) and an optional `maxTurns`. `model` states outcomes, because a model varies: `phase`, `ending`, `transfer`, `visits` (phases the call must enter), `avoids`, `slots`, and `passRate` (default 0.67). Each scenario is run `-runs N` times and passes when that share of runs meets every expectation. A scenario with `steps` still runs in tier 1; one with only a persona is skipped there.

  ```sh
  export ALIYUN_API_KEY=...            # exported, not read from .env
  AICC_LIVE_FLOWSIM=1 aicc flowsim -tier 2 -runs 3 -flow my_flow.json -scenario my_flow.scenarios.json
  AICC_LIVE_FLOWSIM=1 go test -count=1 -run Live -v ./internal/flowsim/    # every seed flow, 1 run each
  ```

  Settings, read by the simulator only (not by the server, so they are not in `.env.example`): `AICC_LIVE_FLOWSIM=1` (required; nothing is requested without it), `ALIYUN_API_KEY` (the qwen profile's key), `AICC_FLOWSIM_BOT_MODEL` (default `qwen-plus`), `AICC_FLOWSIM_CALLER_MODEL` (default `qwen-turbo`, the cheapest Qwen text tier, enough for a short persona), `AICC_FLOWSIM_ENDPOINT`. Without an endpoint it is the DashScope host of `AICC_PROVIDER_ENDPOINT` when that names one, else of the qwen realtime profile, with the OpenAI-compatible chat path. Flags override: `-bot-model`, `-caller-model`, `-endpoint`. Caps against a looping model: `-max-turns` (caller turns per run, default 20, and at most 6 model rounds inside one bot turn), `-max-calls` (model requests per invocation, default 300) and `-max-tokens` (default 1,000,000); the invocation stops at the first API error or when a cap is reached, and says what it did not run. Every tier 2 run (and every tier 1 scenario) gets a fresh in-process mock backend, so a booking one run makes is not found by the next. With `-backend URL` the simulator cannot reset the backend, so state carries between runs; it prints a warning when `-runs` is above 1 (tier 1: when the file has several scenarios). Output is stdout only: the phase path and ending of every run, the failure and transcript of a failing one (`-v` for all), and the requests and tokens spent.

  A text model is not the deployment's voice provider, so a tier 2 pass predicts a call only as far as the two resemble each other. It catches an instruction the model misreads, a tool description it misuses, and a branch it never reaches; it still cannot see audio, barge-in or provider quirks.
- **Live.** A flow is not verified until real calls have walked every branch, including the failure and transfer paths, and the logs and CDRs have been read. The number (DID) a caller dials picks the flow.

## 9. Checklist before publishing

- Is every business rule in the backend, with the flow branching on a field?
- Is every fact the bot may state coming from a tool result? Is anything long pasted into `persona`, `rules` or an `instruction`?
- Does each phase allow only the tools it needs, and does each tool's `description` say when to call it?
- Does the instruction ever ask the model to recall "all the X so far" from the conversation? Use a `collect` list (2.6.1) so the engine holds them.
- Does every decision value the backend can return have a transition, and is there a fallback with a higher `priority` number?
- Does an operation that can exceed its tool's `timeoutMs`, or that a bot should not complete alone, have a submit-and-check, callback or hand-over path?
- Does every terminal phase have an `announce`, and does `closingTarget` name one of them?
- Does the closing phase tell the model to hang up on a decline?
- On a campaign flow: does every phase a classification moves into, and the call then closes in, declare its `lead` (2.7.1), rather than depend on the model calling `record_lead` in the same response?
- Is `maxTurnsWithoutTool` above the longest legitimate tool-less stretch?
- Does the flow avoid collecting secrets or digits through the model (6)?
- Are both languages written, with the same meaning?
