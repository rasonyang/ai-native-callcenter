# Realtime Transcription — Gap Analysis

Date: 2026-08-16, revised 2026-08-17 · Status: **investigation complete; prerequisites
verified by experiment on the dev switch. Every decision needing the owner is settled.
The `PROVISIONAL`s that remain are mine to settle at implementation (§17's definition),
and of those only D15 is genuinely undecided. No application code changed.** ·
Author: agent session

> ## What is settled, and what the experiments found
>
> **Owner decisions (2026-08-16).** A6 constrains the conversational path only, so an
> ASR client may live in this repository (D0). The bot phase keeps the model's own
> transcripts; the human phase streams audio out (D1). The stream is attached to the
> **human-agent leg**, not the caller's (D4 — owner correction; it is the better
> anchor, see §8.1). The agent's voice may be sent to OpenAI/Alibaba. Cost is the
> deployer's call, so transcription is switchable **in system administration**, not
> only by env var. `AICC_TRANSCRIBE_PROVIDER` is independent of `AICC_PROVIDER`. One
> migration, so the `Speaker` enum rename lands with it (D13). Click-to-dial outbound
> is in scope. Supervisor live view is out of scope this milestone.
>
> **Owner decisions (2026-08-17), closing the remaining `PROVISIONAL`s.** The Qwen model
> named in the brief is correct and reachable — on a workspace-scoped host, over a
> *different wire protocol*, which is why `internal/transcribe` is one interface over
> **two** protocol clients (D14, D20). **No TLS on the ingest**: the switch→app link is
> LAN-local, so plaintext `ws://` with a single-use HMAC token, a listener bound to the
> private interface, and the scheme always from config (D5); the module's TLS build stays
> unmeasured and is not an M6.1 blocker. This is **M6**, split M6.1–M6.4 with **M6.4 as
> its own slice** rather than an appendix, and the file becomes
> `docs/design/08-transcription.md` on promotion (D7). **No PII masking this milestone**,
> recorded as a deferred item whose sequencing is fixed: policy first, then redaction
> across recording *and* transcript together (D11). **`BOT_TRANSCRIPT` is removed**, with
> `make api-breaking` left to flag it as an intentional breaking change (D12).
>
> **D15, settled by measurement (2026-08-17) — and it was a false dilemma.** The choice
> was framed as 16 kHz + a new ×3/2 resampler *versus* 8 kHz + the existing
> `Upsample(…, 3)`. It is neither: `mod_audio_stream`'s README documents `8k`/`16k`, but
> its **source accepts any multiple of 8000**, and the module was measured genuinely
> emitting **24 kHz** — verified by tone, not by the command returning `+OK`. So the tap
> is asked for the client's native rate (`24000` for OpenAI, `16000` for Qwen), the
> switch resamples with speex, and **no Go-side resampler is written at all**. One trap:
> the literal is `24000`; `24k` is rejected. **Every decision in this document is now
> settled.**
>
> **Verified on the dev switch (FreeSWITCH 1.11.1) and against both vendors:**
>
> | # | Question | Answer |
> |---|---|---|
> | 1 | Does `mod_audio_stream` build and load on this switch? | **Yes** — arm64/macOS, three portability fixes needed (§B.1) |
> | 2 | What is `stereo`? | **left = READ (far end), right = WRITE (what this party hears)** — measured with a two-tone call |
> | 3 | Does the stream stop when the channel ends? | **Yes** — clean WS close 1000, from source and observed |
> | 3b | Does it survive `uuid_transfer` of its own channel? | **Yes**, and the write stream follows the new bridge |
> | 4 | Does OpenAI's transcription session accept 16 kHz? | **No — rejects anything < 24000** |
> | 4f | Can the tap emit 24 kHz itself, removing the need for a Go resampler? | **Yes** — `24000` (not `24k`) is accepted and **measured emitting true 24 kHz** by tone; 1920-byte frames. Closes D15 with no new code |
> | 4b | Does `gpt-live-transcribe` accept server VAD? | **No — `turn_detection` must be null**, so *we* own endpointing |
> | 4c | Does Qwen accept 16 kHz with server VAD? | **Yes**, and it emits finals with no client commit |
> | 4d | Is `qwen-audio-3.0-asr-flash-streaming` reachable? | **Yes** — on the workspace-scoped host, over DashScope's **native `run-task` protocol**, not the OpenAI dialect (§17 D14) |
> | 4e | Do the two engines share a wire protocol? | **No.** Two protocols ⇒ **two clients** behind one interface (§8.3, §17 D20) |
>
> The two engines are **structurally asymmetric** — protocol, rate, audio transport,
> turn detection, partial semantics and who owns the utterance boundary all differ.
> That is the single biggest change to the design since the first draft, and it is why
> `internal/transcribe` is one interface over two clients rather than one client over
> two profiles.

Goal of the capability under study: a continuous, correctly ordered transcript of a
**whole** call — the bot phase and the human-agent phase that follows a
`transfer_to_agent` handoff — visible live in the agent cockpit and durable in the
ledger.

Every statement below is tagged:

- `[FACT]` — supported by a `path:line` citation in this repository.
- `[INFERENCE]` — derived from code that was read, but not directly observed running.
- `[ASSUMPTION]` — depends on something outside this repository (FreeSWITCH module
  behaviour, provider protocol) and is unverified here. Where a vendor document was
  read during this pass it is cited in Appendix B and the tag says so.

Recommendations that are mine rather than the owner's are marked `PROVISIONAL` and
are collected in §17.

> **Naming note on this file.** This was drafted as `06-realtime-transcription-gap-analysis.md`,
> which collided with `docs/design/06-capacity.md`. It landed as
> **`docs/design/08-transcription.md`** per §17 D7 (owner directive, 2026-08-17: take the
> right number now rather than correct it later). ✗ Still outstanding: `00-overview.md`'s
> reading order and its §6 milestone list do not yet mention this document or M6.

---

## 1. Workspace state

`git status --porcelain` — raw output:

```
```

(empty: the working tree is clean, nothing staged, nothing untracked)

`git log --oneline -5` — raw output:

```
75438f4 m5: the project stops claiming a performance it has not measured
44a2095 m5: the readme says where the project stands
e85b979 m5.6: a plan for measuring, and the evidence record for the milestone
8b467c9 m5.5: a healthy turn is no longer reported as an abandoned one
5884275 m5.4: a harness that can put two hundred calls through the process
```

`[FACT]` There is no uncommitted work to protect. Branch `main`, HEAD `75438f4`, the
last commit of M5. Nothing in this pass wrote to any file other than this one.

Verification gates run during this pass (both permitted, both read-only):

| Gate | Result |
|---|---|
| `go build ./...` | pass (exit 0) |
| `go vet ./...` | pass (exit 0) |

---

## 2. Scope & method

Read in full: `docs/design/00`–`07`, `m0-findings.md`, `m4-cleanup-findings.md`,
`m5-findings.md`, `docs/phase1-decisions.md` (structure + all decision rows),
`docs/phase0-research.md` §1–3, `docs/openapi.json` (programmatically enumerated),
all eight files in `internal/store/migrations/`, and the Go/TS files cited below.

Search commands actually issued (verbatim, the ones that produced findings):

```sh
rg -n -i "transcript" --type go -l
rg -n -i "transcript" web/src -t ts -l
rg -n "TranscribeModel" --type go
rg -n "mod_audio_stream|audio_stream|audio_fork|mod_audio_fork|uuid_audio_stream" .
rg -n "live_calls" .
rg -n "TypeBotTranscript|TypeBotSessionStarted|TypeBotInterrupted|TypeBotSessionEnded|TypeQueueJoined|TypeQueueCount" internal/ --type go
rg -n "default_extension_id" --type go --type sql
rg -n "AgentIDForUser|QueuesForAgent|AgentAtExtension|AgentByCallcenterName" internal/ --type go
rg -n "Transcript" internal/store/ledgerstore.go internal/httpapi/ledger_handlers.go internal/store/sql/ledger.sql
rg -n "input_audio_transcription|audio_transcript" internal/provider/realtime.go
rg -n "func Test" internal/events/hub_test.go internal/httpapi/events_handler_test.go
rg -n "^func |^type |^const |^var " internal/voice/uas.go internal/flow/*.go internal/obs/callmetrics.go
python3 …json.load('docs/openapi.json')…   # enumerate paths, operationIds, enums
```

External documentation fetched during this pass (network was available; results in
Appendix B): the `mod_audio_stream` README, OpenAI's realtime-transcription and
realtime-conversation guides, and Alibaba Model Studio's Qwen-ASR-Realtime client
events / server events / model list pages.

Not done, by instruction: no code, migration, contract or config file was modified;
no `go generate`, `sqlc generate`, migration run, server start, or connection to
FreeSWITCH, PostgreSQL or any provider API.

---

## 3. Component inventory — Table 1

| Concern | Exists? | Path | Evidence | Notes |
|---|---|---|---|---|
| ESL client + event dispatch | **YES** | `internal/esl/link.go:71`, `internal/telephony/switchevent.go:173` | `Link.Run` reconnect loop; `Normalize` is the single FS→domain boundary | Subscriptions list at `switchevent.go:163` |
| Actor-per-call model | **YES** | `internal/telephony/registry.go:70` | `actor` with bounded `mailbox chan command`; `post`/`postSync` at `:258`,`:274` | One goroutine is the sole mutator; snapshots read through the mailbox |
| Call/party FSM | **YES** | `internal/telephony/call.go:65` (`partyTransitions`), `:131` (`apply`) | Table-driven; illegal edges logged at `registry.go:380` | |
| Flow engine | **YES** | `internal/flow/engine.go:22`, `internal/flow/runtime.go:25` | Phase machine + tool dispatch + hint steering | DSL v2, `internal/flow/spec.go:15` |
| Realtime provider client (s2s) | **YES** | `internal/provider/realtime.go:40` | One OpenAI-Realtime client × `Profile` | `internal/provider/profile.go:24` |
| Provider abstraction above OpenAI/Qwen | **YES, but narrow** | `internal/provider/session.go:37` | `VoiceSession` is documented as a *seam*, not a plug-in point | `session.go:31-36`; A6 binds it |
| **Transcription provider (ASR)** | **ABSENT** | — | `rg "mod_audio_stream\|audio_fork\|ASR"` → only docs; no `internal/transcribe`, no ASR type anywhere | Deliberate under `phase1-decisions.md` A6 as written; A6 is since scoped to the conversational path (§17 D0), so the absence is now a gap rather than a rule |
| Queue / transfer / mod_callcenter mirror | **YES** | `internal/telephony/adapter.go:59-128`, `coordinator.go:337` | `callcenter_config` vocabulary confined to the adapter | |
| SSE hub | **YES** | `internal/events/hub.go:52` | Ring 65 536 (`:14`), per-subscriber buffer 64 (`:18`), `Last-Event-ID` resume `Subscribe():117` | |
| SSE event type registry | **YES** | `internal/events/event.go:25-70` + `docs/openapi.json` `SseEventType` | 32 values; contract is the source of truth | `BOT_TRANSCRIPT` declared at `event.go:59` |
| `BOT_TRANSCRIPT` **published** | **ABSENT** | — | `rg TypeBotTranscript internal/ --type go` → only the declaration | Recorded as an implementation gap in `m4-cleanup-findings.md:218-226` |
| Bot-phase transcript capture | **YES (partial)** | `internal/aicall/ledger.go:33-90` | In-memory `callRecorder`, finals only | See §4 |
| Bot-phase transcript persistence | **YES (batch, at call end)** | `internal/aicall/ledger.go:136`, `internal/store/ledgerstore.go:276` | `InsertTranscript` is a sqlc `:copyfrom` (`internal/store/sql/ledger.sql:47`) | One write, at teardown |
| **Human-leg transcript** | **ABSENT** | — | `transcripts.role CHECK IN ('BOT','CALLER')` — `internal/store/migrations/00005_call_ledger.sql:61` | No third speaker is representable |
| Transcript REST read | **YES, supervisor-only, finished calls only** | `internal/httpapi/ledger_handlers.go:45-72` | `GET /cdrs/{callId}` returns `{cdr, transcript, recordings}`; route mounted under `requireSupervisorRole` at `internal/httpapi/server.go:207-210` | No live/agent-readable transcript route |
| Recording | **YES** | `internal/recording/storage.go:22`, ingestion at `internal/telephony/cdr.go:107` | FS/S3, key `recordings/YYYY/MM/DD/<call_id>.wav` | Audio only; no ASR is run on it |
| Agent ↔ extension binding | **YES** | `internal/store/migrations/00002_telephony.sql:36`, `00007_agent_extension_binding.sql:8` | `agents.default_extension_id` + partial unique index | Resolution: `internal/agents/service.go:622` |
| Frontend SSE consumer | **YES** | `web/src/lib/events.ts:26`, `web/src/lib/use-event-stream.ts:46` | One app-level `EventSource`; per-type listeners | Types come from `web/src/generated/api.ts` (`events.ts:10-16`) |
| Frontend transcript display (live) | **ABSENT** | — | `web/src/routes/_app.agent.index.tsx:26-35` states the omission in a comment | "the platform publishes no queue-depth or transcript events … deliberately absent rather than faked" |
| Frontend transcript display (finished call) | **YES** | `web/src/routes/_app.admin.cdr.$callId.tsx:105-118`, `TranscriptLine():161` | Offset + role + body; SUPERVISOR-gated route (`:14`) | The rendering pattern to reuse |
| Quality-review UI | **ABSENT** | — | `web/src/lib/nav.ts:49` — `/supervisor/quality`, `isReady: false`; no route file exists | Backend exists: `server.go:212-214` |
| `live_calls` recovery table | **ABSENT** | — | `rg live_calls .` → only `docs/design/03-data.md` | Designed, never migrated |
| `mod_audio_stream` / `mod_audio_fork` | **ABSENT from both switches** | — | `docs/phase0-research.md:17,144` (dev switch is a slim build **without** audio_fork); demo image enables only `mod_callcenter`, `mod_lua`, `mod_pgsql` — `deploy/demo/freeswitch/entrypoint.d/10-aicc.sh:59` | Also an explicit "no fork" design decision: `docs/design/06-capacity.md:32` |

---

## 4. Current transcript pipeline (bot phase, end to end)

### 4.1 Where it is produced

`[FACT]` Two provider wire events become transcript text, in the one client:

```go
case "response.audio_transcript.delta", "response.output_audio_transcript.delta":
    r.emit(Event{Type: EventTypeOutputTranscript, Text: event.Delta})
case "response.audio_transcript.done", "response.output_audio_transcript.done":
    r.emit(Event{Type: EventTypeOutputTranscript, Text: event.Transcript, IsFinal: true})
case "conversation.item.input_audio_transcription.delta":
    r.emit(Event{Type: EventTypeInputTranscript, Text: event.Delta})
case "conversation.item.input_audio_transcription.completed":
    r.emit(Event{Type: EventTypeInputTranscript, Text: event.Transcript, IsFinal: true})
```
— `internal/provider/realtime.go:511-521`

### 4.2 The caller is very likely not transcribed at all today

`[FACT]` Input transcription is only requested when the profile carries a
`TranscribeModel`, and only in the GA dialect:

```go
if r.profile.TranscribeModel != "" && !isReduced {
    input["transcription"] = map[string]any{"model": r.profile.TranscribeModel}
}
```
— `internal/provider/realtime.go:367-369`

`[FACT]` Neither shipped profile sets it: `OpenAIProfile()` (`profile.go:70-84`) and
`QwenProfile()` (`profile.go:92-115`) both leave `TranscribeModel` at `""`. The only
assignment in the tree is a test (`internal/provider/realtime_test.go:367`).

`[FACT]` The Beta-dialect branch never sends any transcription field at all —
`buildSessionUpdate` styleBeta writes only `modalities`, `input_audio_format`,
`output_audio_format`, `turn_detection`, `voice` (`realtime.go:380-389`).

`[ASSUMPTION — vendor doc read, Appendix B.2]` OpenAI's realtime guide does not
describe input-audio transcription as on by default; it is configured under
`audio.input.transcription`. Therefore on the OpenAI profile no
`conversation.item.input_audio_transcription.*` event is expected, and
`store.TranscriptRoleCaller` rows are never written.

`[ASSUMPTION]` Qwen-Audio-Realtime's default behaviour for input transcription on the
`qwen-audio-3.0-realtime-plus` s2s model is unverified.

**Consequence:** what is called "the transcript" today is, on the OpenAI path, a
record of the **bot's own words plus tool traces** and nothing the caller said,
except keypresses. That is a defect in the existing feature, independent of this
capability, and it is the cheapest thing on this whole list to fix (§7 G-06).

### 4.3 How it travels

`[FACT]` `internal/aicall/session.go:439-443` folds provider events into the bridge's
own vocabulary, preserving `IsFinal`:

```go
case provider.EventTypeInputTranscript:
    s.emit(Event{Type: EventTypeCallerSaid, Text: event.Text, IsFinal: event.IsFinal})
case provider.EventTypeOutputTranscript:
    s.emit(Event{Type: EventTypeBotSaid, Text: event.Text, IsFinal: event.IsFinal})
```

`[FACT]` The orchestrator's `drive` loop consumes them and **drops every partial**:

```go
case EventTypeCallerSaid:
    if event.IsFinal { recorder.say(store.TranscriptRoleCaller, event.Text) }
case EventTypeBotSaid:
    if event.IsFinal { recorder.say(store.TranscriptRoleBot, event.Text) }
case EventTypeDigit:
    recorder.say(store.TranscriptRoleCaller, "[keypad] "+event.Text)
```
— `internal/aicall/orchestrator.go:319-330`

`[FACT]` Tool traces are recorded around dispatch: `recorder.toolCall(...)` /
`recorder.toolResult(...)` at `orchestrator.go:333,335`.

### 4.4 Where it is persisted

`[FACT]` Nowhere until the call ends. `callRecorder.add` appends to an in-memory
slice under a mutex and increments a per-recorder counter
(`internal/aicall/ledger.go:79-90`). The single write happens in `finish`, deferred at
`orchestrator.go:256`:

```go
if err := ledger.InsertTranscript(ctx, r.callID, entries); err != nil {
    log.Error("could not write the transcript", "error", err)
}
if isTransferred { return }
```
— `internal/aicall/ledger.go:136-141`

`[FACT]` `InsertTranscript` is a sqlc `:copyfrom` batch
(`internal/store/sql/ledger.sql:47-49`, `internal/store/ledgerstore.go:276-297`).
`[FACT]` On a transfer the transcript is still written; only the CDR is skipped
(`ledger.go:139-141`), because the switch-side assembler owns that row
(`internal/telephony/cdr.go:79`).

`[INFERENCE]` If the process dies mid-call, the entire bot transcript is lost — there
is no incremental write and no `live_calls` snapshot table (that table is designed in
`docs/design/03-data.md:66` but exists in no migration).

### 4.5 Where it is displayed

`[FACT]` Only after the call, only to SUPERVISOR and above:
`GET /cdrs/{callId}` → `internal/httpapi/ledger_handlers.go:57` → rendered by
`web/src/routes/_app.admin.cdr.$callId.tsx:112-117`, one `<li>` per entry keyed by
`entry.seq` (`:114`), with a call-relative offset computed client-side from
`cdr.startedAt` (`offsetLabel():156`).

`[FACT]` **Nothing is pushed to SSE.** `events.TypeBotTranscript` is declared
(`internal/events/event.go:59`) and enumerated in the contract
(`docs/openapi.json` `SseEventType`), and the browser already registers a listener for
it (`web/src/lib/events.ts:61`) — but no Go code publishes it.

### 4.6 Summary of §4 as a table

| Stage | Bot phase status | Human phase status |
|---|---|---|
| Produced | `[FACT]` bot's own words: yes. `[INFERENCE]` caller's words: **no** (§4.2) | **ABSENT** — no audio tap exists |
| Propagated in-process | `[FACT]` yes, `aicall.Event` | ABSENT |
| Pushed to SSE | **ABSENT** | ABSENT |
| Persisted | `[FACT]` yes, once, at bot-session end | ABSENT |
| Displayed live | **ABSENT** | ABSENT |
| Displayed after the call | `[FACT]` yes, CDR detail, SUPERVISOR | ABSENT |

---

## 5. Bot → Human handoff lifecycle (event sequence table)

`[FACT]` unless marked. Read in order; "internal event" is what the domain publishes.

| # | Trigger | ESL / wire event | Code | Internal event | State change | DB write |
|---|---|---|---|---|---|---|
| 1 | Model decides to hand off | `response.function_call_arguments.done` | `internal/provider/realtime.go:523` | `provider.EventTypeToolCall` → `aicall.EventTypeToolCall` (`session.go:445`) | — | — |
| 2 | Orchestrator dispatches | — | `internal/aicall/orchestrator.go:332-341` | — | `recorder.toolCall` / `toolResult` appended in memory | — |
| 3 | Queue admissibility check | — | `internal/aicall/actions.go:56-64` | — | a refusal returns `flow.Failed("QUEUE_CLOSED"…)` and the call continues | — |
| 4 | Handoff context stamped on the **caller's** channel | `uuid_setvar` ×N | `actions.go:71-77` + `stampBotShare` `ledger.go:208-216` via `adapter.go:254` | — | `aicc_bot_summary`, `aicc_bot_reason`, `aicc_bot_slots`, `aicc_bot_sec`, `aicc_language`, `aicc_flow_id`, `aicc_did` | — |
| 5 | Transfer armed, not executed | — | `actions.go:87-95`, `arm():139` | — | `recorder.markTransferred(queue.ID)` (`ledger.go:92`) | — |
| 6 | Bridge line generated | `response.done` | `realtime.go:544` | `TURN_DONE` (`session.go:436`) | `actions.onTurnDone` sets `isLineSpoken` (`actions.go:179`) | — |
| 7 | Bridge line **heard** | RTP queue drained + one frame interval | `session.go:589-637` | `PLAYBACK_DONE` (`session.go:603`) | `actions.onPlaybackDone` fires the armed action if `turn > armedInTurn` (`actions.go:166`) | — |
| 7a | (fallback) caller talks over the goodbye | — | `actions.onBargeIn():195` | `BARGE_IN` | armed action fires immediately | — |
| 7b | (fallback) 10 s cap | — | `orchestrator.go:42`, `actions.go:148` | — | armed action fires anyway | — |
| 8 | Caller moved to the queue extension | `uuid_transfer <caller> <7xxx> XML default` | `adapter.go:185-190` via `actions.go:89` | — | caller's channel re-enters the dialplan at `freeswitch/conf/dialplan/default/05_aicc.xml` | — |
| 9 | Bot leg torn down | SIP `BYE` to our UAS | `internal/voice/uas.go:642` (`handleBye`) | `aicall` session closes; `EventTypeEnded` | `onCallEnded` (`orchestrator.go:183`) | — |
| 10 | **Transcript written** | — | deferred `recorder.finish` `orchestrator.go:256` → `ledger.go:136` | — | — | **`INSERT` into `transcripts` (batch)**; CDR skipped (`ledger.go:139`) |
| 11 | Bot leg hangup observed | `CHANNEL_HANGUP_COMPLETE` | `switchevent.go:229-236` | `PARTY_RELEASED` (`registry.go:329`) | party → `RELEASED`; `BotShare` captured from channel vars (`registry.go:326`) | — |
| 12 | Caller enters the queue | `CUSTOM callcenter::info` `CC-Action=member-queue-start` | `switchevent.go:315` → `KindQueueMemberJoined` | — | `call.Queue.JoinedAt` (`registry.go:350`) | `queue_events` `JOINED` (`cdr.go:349`) |
| 13 | An agent is chosen | `agent-offering` | `switchevent.go:322` | **`QUEUE_AGENT_OFFERED`**, scoped to that agent (`coordinator.go:359-369`) | — | `queue_events` `OFFERED` |
| 14 | Agent's phone dialled | `CHANNEL_CREATE` (outbound, `dialed_user`) | `coordinator.adopt():170`, `agentForLeg():593` | **`PARTY_RINGING`** with screen-pop payload (`coordinator.go:246-258`) | **a *separate, provisional* call is created for the agent leg** (`coordinator.go:196-217`); `SetOnCall(true)` | — |
| 15 | Agent clicks Answer | `POST /calls/{id}/answer` → `uuid_phone_event <agent_leg> talk` | `coordinator.Answer():393`, `adapter.go:142` | — | — | — |
| 16 | Phone picks up | `CHANNEL_ANSWER` | `registry.go:315` | `PARTY_ESTABLISHED` | agent party → `TALKING` | — |
| 17 | Legs bridged | `CHANNEL_BRIDGE` | `coordinator.join():266` | — | **the agent-only call is absorbed into the caller's minted call** (`coordinator.go:280-289`, `merge():294`); channels rebind; the absorbed actor retires | — |
| 18 | Queue confirms the bridge | `bridge-agent-start` | `switchevent.go:326` | — | `call.Queue.BridgedAt` (`registry.go:358`) | `queue_events` `BRIDGED` |
| 19 | Conversation ends | `CHANNEL_HANGUP_COMPLETE` ×2 | `registry.go:321-341` | `PARTY_RELEASED` ×2, then **`CALL_CDR`** | `Call.Finish` (`call.go:287`) | `CDRAssembler.CallFinished` writes the one CDR because the call has a bot leg **and** a non-zero bot share (`cdr.go:79`); recording ingested (`cdr.go:107`) |

**Two consequences that shape everything downstream:**

`[FACT]` **The callId the agent sees changes at step 17.** Between steps 14 and 17 the
agent's leg lives on its own provisional call (`coordinator.go:196-217`), so
`PARTY_RINGING` and `GET /calls/mine` carry the provisional id; after the merge the
same conversation answers under the minted id. Any UI keyed on `callId` must survive
that flip.

`[FACT]` **The bot's transcript is committed at step 10, before the agent exists.** So
the agent can never have received it live over a scoped stream — a backfill read is
structurally required, not a convenience.

---

## 6. Leg model & identity

`[FACT]` A leg is a first-class object with **two** identities:

```go
type Party struct {
    PartyID   uuid.UUID   // ours, UUIDv7, minted in AddParty
    ChannelID string      // the FreeSWITCH channel UUID
    …
```
— `internal/telephony/call.go:100-103`; minted at `call.go:210-217`.

`[FACT]` Both are exposed on the wire: `PartySnapshot.partyId` and
`PartySnapshot.channelId` (`call.go:312-327`), consumed by the cockpit
(`web/src/routes/_app.agent.index.tsx:487`).

`[FACT]` Legs are **not persisted**. `transcripts` has no leg column
(`00005_call_ledger.sql:56-65`); `cdrs.legs` is a jsonb journey summary with kinds and
durations only (`00005_call_ledger.sql:45`, built at `internal/telephony/cdr.go:280`).
There is no `live_calls` table (`rg live_calls .` matches only `03-data.md`).

**Which leg is which:**

| Leg | How it is identified | Lifetime |
|---|---|---|
| Caller (user) | `Party.Role == RoleOriginator` (`call.go:236`); after a merge exactly one survives (`normalizeOriginator()` `coordinator.go:621`) | Whole call |
| Bot | `Party.IsBotLeg`, stamped at adoption: outbound channel whose destination equals its `aicc_did` (`coordinator.go:129-132`) | Bot phase only |
| Human agent | `Party.AgentID != nil`, set when `agentForLeg` matches `dialed_user` / `aicc_extension` / destination against a signed-in agent (`coordinator.go:593-610`, `internal/agents/service.go:622`) | One delivery attempt |

**Does the caller's a-leg UUID survive the bridge?**

`[FACT]` The caller's channel UUID is captured once, in Lua, before anything is
bridged: `session:setVariable("sip_h_X-AICC-Channel-ID", session:getVariable("uuid"))`
— `freeswitch/scripts/aicc_inbound.lua:80`. `[FACT]` That same value is the target of
the handoff: `TransferToExtension(a.callerChannel, queue.ExtNumber, "default")`
(`internal/aicall/actions.go:89`) → `uuid_transfer <uuid> <ext> XML <ctx>`
(`adapter.go:189`).

`[INFERENCE]` Therefore the caller's channel UUID is stable across the bot bridge,
the transfer into the queue, and the bridge to the agent — `uuid_transfer` re-routes
an existing channel rather than creating one. Corroborating in-repo evidence:
`recording_follow_transfer` is set once on that channel and one continuous stereo file
is expected to span the whole call (`aicc_inbound.lua:90-93`,
`docs/design/03-data.md:124`). `[ASSUMPTION]` Not observed live in this pass.

`[FACT]` The **agent** leg's UUID does not survive: RONA, a re-offer, or a second
transfer each produce a new channel, adopted as a new `Party` (`coordinator.adopt()`
`:170`).

That is the fact §8 is built on, but not in the direction it first suggests. A new
channel per delivery is also a new `Party` with a **known** `AgentID`, stamped at
adoption (`coordinator.addParty():231-233`) and resolvable at any time by
`Coordinator.agentChannel(callID, agentID)` (`coordinator.go:567-584`). One agent leg
is one person for its whole lifetime, and its lifetime is exactly the human phase.
The caller's channel has the opposite property: stable, but its bridge partner changes
identity silently underneath it.

---

## 7. Gap list — Table 2

| # | Gap | Severity | Affected modules | Proposed change |
|---|---|---|---|---|
| G-01 | No way to obtain human-leg audio. The Go process terminates RTP only for the bot leg; agent audio is FreeSWITCH↔browser and never enters this process (`m4-cleanup-findings.md:42` invariant #7, PASS by design) | **BLOCKER** | `freeswitch/`, `deploy/demo/`, `internal/telephony/adapter.go` | Install `mod_audio_stream`; add exactly one command to the adapter's vocabulary (§8.1) |
| G-02 | No transcription provider client exists. ~~And none may exist under A6~~ — **governance resolved**: A6 constrains the conversational path only (owner directive 2026-08-16, §17 D0), so the remaining gap is code, not permission | **BLOCKER** (code) | new `internal/transcribe` | One client × `Profile`, in its own package — never inside `internal/provider` (§8.3) |
| G-03 | No WebSocket **server** anywhere in the process; the only HTTP surface is chi with cookie sessions + CSRF (`internal/httpapi/server.go:112-250`) | **BLOCKER** | `cmd/aicc`, new `internal/streamin` | Separate listener + token auth (§8.2, D5) |
| G-04 | The transcript is an in-memory slice written once at teardown (`internal/aicall/ledger.go:79-90,136`) — nothing can read it during the call, and a crash loses it | **BLOCKER** | `internal/aicall`, `internal/store` | Incremental append through a per-call transcript sequencer (§8.4) |
| G-05 | `BOT_TRANSCRIPT` is declared and never published (`internal/events/event.go:59`; `rg` finds no publisher) | **MAJOR** | `internal/aicall`, `internal/events`, contract | Publish a call-scoped `CALL_TRANSCRIPT` and retire `BOT_TRANSCRIPT` (§11, D-SSE) |
| G-06 | The caller's speech is almost certainly not transcribed in the bot phase (`internal/provider/profile.go:70-115` leave `TranscribeModel` empty; `realtime.go:367`) | **MAJOR** | `internal/provider/profile.go`, `realtime.go:380-389` | Set a transcription model on each profile; add the field to the Beta dialect |
| G-07 | No REST route returns a transcript for a **live** call, and the only transcript route is SUPERVISOR-gated (`server.go:207-210`) | **MAJOR** | `docs/openapi.json`, `internal/httpapi` | `GET /calls/{callId}/transcript` under a call-involvement check (§10, D8) |
| G-08 | `transcripts` cannot represent a third speaker, a leg, a partial, an offset, a language or a producer (`00005_call_ledger.sql:56-65`) | **MAJOR** | migrations, `internal/store`, contract, TS, i18n | §9 Table 3 |
| G-09 | SSE **replay ignores the original scope**: `who.wants(ev, Scope{})` at `internal/events/hub.go:132` passes an empty scope, so on reconnect an agent is replayed events they were never entitled to receive live | **MAJOR** (privacy; pre-existing, becomes serious once transcripts are on the stream) | `internal/events/hub.go` | Store the scope alongside each ring entry and apply it on replay |
| G-10 | The browser applies events in arrival order; there is no per-call ordered buffer and no `seq` sort (`web/src/lib/use-event-stream.ts:67-71`, `web/src/lib/events.ts:42-51`) | **MAJOR** | `web/src/lib` | Seq-ordered per-call buffer (§12) |
| G-11 | No live transcript panel; its absence is documented as deliberate (`web/src/routes/_app.agent.index.tsx:26-35`) | **MAJOR** | `web/src/components`, `web/src/routes` | §12 |
| G-12 | The callId flips from provisional to minted at `CHANNEL_BRIDGE` (`coordinator.go:196-217` then `:280-289`) | **MAJOR** | `web`, backend event scoping | Re-run backfill when `callId` changes; §14 |
| G-13 | A second writer would collide with `uq_transcripts_call_id_seq` (`00005_call_ledger.sql:64`), since the bot allocates `seq` in its own process-local counter (`ledger.go:82`) | **MAJOR** | `internal/aicall`, new sequencer | One per-call allocator owns `seq` for both producers (§8.4, D2) |
| G-14 | A listener that throws stops the remaining listeners for that event (`web/src/lib/use-event-stream.ts:69-70`) | **MAJOR** (failure isolation, Q22) | `web/src/lib/use-event-stream.ts` | Wrap each listener call in try/catch |
| G-15 | Neither switch has the module: the dev build is slim without audio_fork (`docs/phase0-research.md:17,144`); the demo entrypoint enables only `mod_callcenter`, `mod_lua`, `mod_pgsql` (`10-aicc.sh:59`) | **BLOCKER** (environment) | `freeswitch/README.md`, `deploy/demo/` | Build/install step + a demo image that carries it |
| G-16 | `docs/design/06-capacity.md:32` records "SIP/RTP in-process (**no mod_audio_fork/stream**) … Confirmed choice" | **MAJOR** (design conflict) | `docs/design/06-capacity.md` | Amend: the decision was about the *bot* leg's media path and stays; the human leg has no in-process alternative |
| G-17 | No `live_calls` table, so nothing survives a restart mid-call (`rg live_calls .`) | **MINOR** | — | Accept; a restart loses the in-flight tail, exactly as it loses the bot transcript today |
| G-18 | Quality-review UI absent (`web/src/lib/nav.ts:49` `isReady:false`) though the API exists (`server.go:212-214`) | **MINOR** | `web` | Out of scope; §12.9 |
| G-19 | `role` is overloaded three ways in the contract: `Role`(AGENT/SUPERVISOR/ADMIN), `PartyRole`(ORIGINATOR/TARGET), `TranscriptRole`(CALLER/BOT) | **MINOR** (naming, 07 §5/§6) | contract, DB, TS | Rename the transcript one to `speaker` (§9, D-ENUM) |

---

## 8. Proposed architecture

**The rule that governs the whole picture (owner directive, 2026-08-16):
`uuid_audio_stream` runs *only* while a human agent is on the call, and it runs on the
*human agent's own leg* — never on the caller's leg, never on the bot's.** There is no
media bug during the bot phase and none while the caller waits in the queue. The bot
phase is transcribed by the model that is already having the conversation (D1); the
switch is asked for audio only for the one thing this process cannot otherwise hear.

Anchoring on the agent leg makes the timing rule structural instead of procedural: the
agent leg does not exist during the bot phase or the queue, so there is no window in
which a mistimed start could tap the bot, an announcement or music-on-hold. The leg's
lifetime *is* the window.

```
   bot phase              queue (MOH)          human phase
   ──────────────────────────────────────────────────────────────────────
   caller channel   ·············································  never bugged
   (record_session, recording_follow_transfer — one continuous file)

   agent channel        (does not exist)   ┌ created ─ ringing ─ bridged ═══ hangup
   (a new one on every delivery attempt)                          ▲          ▲
                                                            stream START  stream STOP
   ═══  the only window in which any media bug exists

  ┌──────────────── FreeSWITCH ────────────────┐
  │  agent channel (the WebRTC leg to Chrome)  │
  │    read stream  = the human agent          │  ← the agent's own microphone
  │    write stream = the customer             │  ← what is played to the agent
  │            │                               │
  │   mod_audio_stream media bug               │    exists only between the bridge
  │   uuid_audio_stream <agent_leg> start      │    and its end, so it can never
  │       <stream-url>?t=<hmac>                │    hear the bot, an announcement
  │       stereo <24000|16000> '<metadata>'    │    or music-on-hold
  └────────────┬───────────────────────────────┘    rate = the transcribe client's
               │ WS: 1 text metadata frame,         native rate (D15); the switch
               │     then L16 stereo binary         resamples, we never do
               ▼
   internal/streamin  (separate listener :8090, token-authenticated)
               │  de-interleave (internal/media PCM16 helpers)
      ┌────────┴────────┐
      ▼                 ▼
  left = HUMAN_AGENT  right = CUSTOMER
  (agentId is known from the leg, not inferred from who is bridged)
      │                 │
   internal/transcribe.Session  ×2   (one WS each, OpenAI- or Qwen-ASR profile)
      │                 │
      └────────┬────────┘
               ▼
   transcript coordinator  (one actor per call — sole allocator of seq)
               │
      ┌────────┴────────────────────────────┐
      ▼                                     ▼
  events.Hub  → SSE CALL_TRANSCRIPT   store: INSERT INTO transcripts
  (finals + partials)                  (finals only)
               ▲
               │  BOT-phase lines, from the existing path
   internal/aicall  (provider.EventTypeInput/OutputTranscript)
```

### 8.1 mod_audio_stream interaction (question 8)

**Who issues the command.** `internal/telephony/Adapter` — it is by construction the
only place a switch command string is built (`internal/telephony/adapter.go:21-23`:
"Nothing outside this file builds a switch command string"). Two new methods:

```go
func (a *Adapter) StartAudioStream(channelID, url, mixType, rate, metadata string) error
func (a *Adapter) StopAudioStream(channelID string) error
func (a *Adapter) PauseAudioStream(channelID string) error   // uuid_audio_stream … pause
func (a *Adapter) ResumeAudioStream(channelID string) error  // … resume
```

In every one of them `channelID` is the **agent leg's** channel — see below.

`[FACT]` The vocabulary is declared complete in `docs/design/01-telephony.md:9`
("anything else needs a design change"), so this is a documented amendment, not a
casual addition.

**Which channel — the hard rule.** **The bug goes on the human agent's own leg** (owner
directive, 2026-08-16). Not the caller's leg, not the bot's leg. And **a stream exists
only while that agent leg is bridged to the caller**: never during the bot phase, never
in the queue.

Why the agent leg and not the caller's:

1. **Lifetime.** `[FACT]` The agent leg is created at the delivery attempt and dies when
   the agent hangs up or the call is transferred on (`coordinator.adopt():170`). It does
   not exist during the bot phase or the queue. Anchoring there makes "no bug outside the
   human phase" a property of the object rather than a rule the code has to keep.
2. **Attribution.** `[FACT]` The agent leg carries `Party.AgentID`, stamped at adoption
   (`coordinator.addParty():231-233`). Every line from that stream has a known
   `agent_id` (§9) with no lookup and no ambiguity. A bug on the caller's channel would
   have to answer "who is bridged *right now*" for every utterance, and answer it again
   after each re-offer or second transfer — an inference where the agent leg gives a
   fact.
3. **Two agents, two transcripts.** On a transfer from agent A to agent B, or a second
   delivery after RONA, each agent leg carries its own bug and its own `agentId`. One
   stream on the caller's channel would silently change the person behind its right
   channel mid-file, which the transcript has no way to represent.
4. **The caller's channel stays untouched.** `[FACT]` It already carries `record_session`
   with `recording_follow_transfer` for the whole call (`aicc_inbound.lua:90-93`) and is
   the channel every transfer re-routes (`actions.go:89`). It is the one leg that must
   never break; it gains no second media consumer.
5. **The bot's leg is never bugged either.** Its audio is already in this process and the
   model returns its own words verbatim (D1). Forking it back through the switch would
   contradict `docs/design/06-capacity.md:32` for no gain.

The price is bookkeeping — a start and a stop per delivery instead of one per call — and
it is smaller than it looks: between two agent legs the caller is in the queue on MOH,
so there is no audio to lose during the gap. Nothing is dropped that anyone would want.

| Boundary | Normalized event | Where it already arrives | Action |
|---|---|---|---|
| Start | `KindChannelBridge` on a party with `AgentID != nil` | `switchevent.go:217`, handled at `coordinator.join():266` — both channel ids are in hand (`ev.ChannelID`, `ev.OtherChannelID`) | `StartAudioStream(agentLegChannelID, …)` |
| Start (queue confirmation) | `KindQueueBridgeStart` (`bridge-agent-start`) | `switchevent.go:326`, `coordinator.go:83`, `registry.go:357` | Idempotent re-assert only. `bridge-agent-start` names the **member** channel (`CC-Member-Session-UUID`, `switchevent.go:307`), not the agent's, so it is a confirmation, not the trigger — and it never fires for a non-queue agent call (click-to-dial, direct extension), which `CHANNEL_BRIDGE` covers. |
| Stop | `KindChannelHangup` on the agent's channel | `switchevent.go:229`, `registry.go:321` | **Optional.** `[FACT — measured, B.1]` the module stops itself: `SWITCH_ABC_TYPE_CLOSE` → `stream_session_cleanup` (`mod_audio_stream.c:32-38`), observed as a clean WS close 1000 on `uuid_kill`. An explicit `StopAudioStream` is kept only as a belt-and-braces for the case where our state and the switch's disagree; it is not needed to avoid a socket leak |
| Stop | `KindChannelUnbridge` / `KindQueueBridgeEnd` | `switchevent.go:218,327` | same, for the agent leg that survives its bridge (a transfer away) |
| Suspend / resume | `KindChannelHold` / `KindChannelUnhold` | `switchevent.go:225-227`, `registry.go:317-320` | `pause` / `resume` — on hold the write stream is not the customer, and the read stream may be a private side-conversation. Neither belongs in a transcript. |

Three consequences of the rule, each worth stating because each removes a problem the
alternative would have created:

1. **Music-on-hold, announcements and the bot are never transcribed.**
   `[FACT — measured, B.1]` "stereo" is the channel's read stream in the **left** channel
   and its write stream in the **right**: `mono → SMBF_READ_STREAM`,
   `stereo → SMBF_READ_STREAM|SMBF_WRITE_STREAM|SMBF_STEREO`
   (`mod_audio_stream.c:190-203`), confirmed on this switch with a two-tone call — the
   leg playing 440 Hz toward a 1000 Hz peer captured left=1004 Hz, right=440 Hz, and its
   mirror captured left=440 Hz, right=1004 Hz. On the agent's leg those two are the
   agent's microphone and the customer
   — and only ever those, because the leg exists only for the human phase. An ASR fed
   MOH emits plausible nonsense attributed to whoever the design says owns that channel;
   here it cannot be fed MOH, not because of a filter but because the leg carrying the
   bug did not exist while the music was playing.
2. **Nothing is spent on a call that never reaches a person.** `[FACT]` the seeded demo
   mix is ~55 % bot-contained and ~10 % abandoned in queue
   (`docs/design/03-data.md:139`), so on that shape roughly two thirds of calls open no
   stream, no WebSocket and no ASR session at all. A RONA leg that rings and is never
   answered opens nothing either: the trigger is the bridge, not the ring. The cost of
   this feature is per *answered* delivery, bounded by agent headcount, not by call volume.
3. **The bot phase's audio never leaves this process.** The invariant that the Go app
   terminates RTP only for the bot leg and stays out of the agent media path
   (`m4-cleanup-findings.md:42`, invariant #7) is untouched in one direction, and in the
   other the switch is asked for exactly the audio this process has no other way to hear.

**How it aligns with actor-per-call.** The command is issued from the coordinator's
`Handle` path, which already runs before `registry.Dispatch` (`coordinator.go:57-115`)
and already owns `join()` (`coordinator.go:266`), where both legs of the bridge and their
call are resolved. The agent leg's channel and `AgentID` are read the same way
`agentChannel` reads them (`coordinator.go:567-584`). The resulting stream state lives on
a new per-call transcript actor (§8.4), reached through its own mailbox, exactly like
`telephony.actor` (`registry.go:70`). No shared mutable state, no lock on call state.

**Failure and rollback.** `uuid_audio_stream` returning `-ERR` is turned into a Go
error by `Adapter.exec` (`adapter.go:263-274`). On failure: log at Warn, publish
`CALL_TRANSCRIPTION_STATE {state: ERROR}`, and **do nothing else** — the call is
untouched, the softphone is untouched, and the panel degrades (§12.8). There is
nothing to roll back because nothing about the call was changed.

### 8.2 The WebSocket server (question 9) — D5

**A separate listener inside the Go monolith**, not a chi route under `/api/v1`.
~~`PROVISIONAL`~~ → SETTLED with D5 (owner, 2026-08-17), together with the transport
ruling below.

Reasons, each evidenced:

1. `[FACT]` Every `/api/v1` route sits behind `s.requireSession` and, for mutations,
   a CSRF header — `internal/httpapi/server.go:119-121`, `docs/design/04-api-sse.md:16`.
   FreeSWITCH can present neither.
2. `[FACT]` Request-scoped routes carry `middleware.Timeout(30 * time.Second)`
   (`server.go:115`). A media stream must not. The existing exception for `/events`
   (`server.go:247`) shows the shape of the workaround, and adding a second one for a
   binary path is worse than a listener of its own.
3. `[FACT]` The project already separates an unauthenticated operational listener by
   address (`AICC_METRICS_ADDR`, `internal/httpapi/server.go:261`,
   `internal/config/config.go:100`). This is the same pattern: a listener that binds to
   a private interface and never faces a browser.
4. `[FACT]` The API surface is wrapped in `otelhttp` and an audit trail
   (`server.go:95`, `:121`); a 50-frames-per-second-per-call path should not be.

**Authentication.** `[ASSUMPTION, Appendix B.1]` The module offers exactly two carriers:
the URL, and a `STREAM_EXTRA_HEADERS` channel variable holding a JSON object of extra
HTTP headers. Metadata is sent as the *first WebSocket text frame*, i.e. **after** the
handshake, so it cannot authenticate the connection.

Recommendation: a short-lived HMAC token minted by the Go process at the moment it
issues `uuid_audio_stream`, over `callId|agentLegChannelId|agentId|expiry`, keyed by
`AICC_STREAM_SECRET`, carried **both** in the query (`?t=`) — which always works — and
in `Authorization: Bearer` via `STREAM_EXTRA_HEADERS` where the build supports it. The
server verifies the token at upgrade, then verifies that the identity in the first
metadata frame — `{callId, partyId, agentId, channelId}` — matches the token's claims. A
token is valid for 60 s and for one connection. Because the token is minted per agent
leg, the ingest session knows which agent's microphone is on its left channel before the
first audio byte arrives; nothing downstream has to infer attribution.

**Transport: plaintext `ws://`, and the listener is bound narrowly to earn it.**
~~`PROVISIONAL`~~ → SETTLED by owner directive (2026-08-17): FreeSWITCH → Go is a LAN
link; no TLS on the ingest.

`[FACT]` This agrees with the standing mandate — "The repo itself never handles TLS"
(`docs/design/00-overview.md:46`). The four rules that make plaintext defensible, rather
than merely convenient:

1. **Bind the ingest listener to the private interface, not `0.0.0.0`.** The confidential
   thing on this socket is call audio, and the mitigation for not encrypting it is that it
   is not reachable. `[FACT]` `AICC_METRICS_ADDR` is the precedent for an unauthenticated
   listener separated by address (`config.go:100`), and this listener must be bound more
   tightly than that one, not less. `[INFERENCE]` A default of `:8090` binds every
   interface, so the default itself is the risk — see §13.
2. **The scheme comes from config, never a hardcoded `ws://`.** `AICC_STREAM_PUBLIC_URL`
   carries the full URL including scheme; nothing in the code composes one. This is what
   keeps "no TLS" a *deployment* choice rather than a property baked into the binary, so a
   later cross-host deployment needs a config change and a module rebuild — not a patch.
   `[INFERENCE]` Validation should accept `ws://` **or** `wss://` and reject anything else,
   so a typo fails at startup rather than at the first call.
3. **The HMAC token stays, and stays in the URL.** It travels in the clear — accepted on a
   LAN. What it still buys with no TLS is real: it is single-use, expires in 60 s, and
   binds the connection to one call and one agent leg, so a replayed URL is dead and a
   guessed one was never alive. `[INFERENCE]` It authenticates; it was never confidentiality.
4. **`wss://` stays unbuilt and unmeasured.** ✗ — `-DUSE_TLS=ON` is deliberately *not*
   verified (B.4). It is deferred, not a blocker for M6.1, and it becomes a real question
   only if a deployment ever puts the switch and the app on different hosts.

The demo stack is unaffected either way: both containers share a compose network, so
plain `ws://` never leaves the host — the same argument
`deploy/demo/docker-compose.yml:8-11` already makes for RTP.

### 8.3 Provider abstraction (question 10, D6)

**Recommendation `PROVISIONAL`: a new package `internal/transcribe`, following
`internal/provider`'s rule — *one client per wire protocol*, each parameterised by a
`Profile` — and never merged into it. Unlike `internal/provider`, that rule yields
**two** clients here, because its two engines speak two protocols (D20).**

Permission to build it at all is settled: A6 constrains the conversational path only
(owner directive 2026-08-16, §17 D0). What A6 still forbids is putting it *here*, in the
package that owns the conversation.

Why a new package rather than an extension of `internal/provider`:

`[FACT]` The existing package's contract is explicit that it must not grow this:
"no recognition or synthesis concept ever enters this one"
(`internal/provider/session.go:14-15`), and `VoiceSession` "is not a plug-in point for
other kinds of engine" (`session.go:35-36`). Widening it would break the compile-time
story that M4's audit verified (invariant #11, `m4-cleanup-findings.md:46`).
`[FACT]` `internal/transcribe` must therefore not import `internal/provider`, and
`internal/provider` must not import it — the dependency rule of `00-overview.md:68`
("dependencies point downward only") makes them siblings above `internal/media`, joined
only by `internal/aicall` and the transcript actor.

**Why it is *two clients*, not one client × profile — corrected 2026-08-17, and this is
the single biggest structural change in this document.**

The first draft asserted `[ASSUMPTION]` that both vendors spoke the same event grammar
(`session.update` / `input_audio_buffer.append` / `…transcription.completed`), making the
difference "field placement and one event name — precisely what a `Profile` is for."
`[MEASURED, B.3b]` **That assumption is false for the model this deployment must ship.**
`qwen-audio-3.0-asr-flash-streaming` speaks DashScope's **native duplex protocol**:

```jsonc
// client → server, then raw binary PCM frames, then finish-task
{"header":{"action":"run-task","task_id":"<32 hex>","streaming":"duplex"},
 "payload":{"task_group":"audio","task":"asr","function":"recognition",
            "model":"qwen-audio-3.0-asr-flash-streaming",
            "parameters":{"format":"pcm","sample_rate":16000},"input":{}}}
```

Server replies are `task-started` / `result-generated` / `task-finished` / `task-failed`
in a `header`+`payload` envelope. There is no `session.update`, no
`input_audio_buffer.*`, no `conversation.item.*`. No amount of `Profile` fields bridges
that; a profile chooses *values*, and this is a different *grammar*.

**This is not a violation of the codebase's provider rule — it is that rule applied
correctly.** `CLAUDE.md` says "the provider extension point is **the wire protocol**, not
Go": a new engine on the *same* protocol is a new profile, never a second client. The
converse is the same rule read forward — a genuinely different protocol *is* where a
second client is legitimate. `internal/provider` stays one client because every engine it
supports speaks OpenAI-Realtime. `internal/transcribe` needs two because its two engines
do not.

Structure, therefore: **one `Session` interface, two implementations**, each owning one
protocol, each with its own profile for the values that vary within it:

| Client | Protocol | Engines |
|---|---|---|
| `transcribe/openairt` | OpenAI-Realtime transcription session | `gpt-live-transcribe`; also `qwen3-asr-flash-realtime`, which speaks this dialect `[MEASURED]` |
| `transcribe/dashscope` | DashScope native duplex (`run-task`) | `qwen-audio-3.0-asr-flash-streaming`, `fun-asr-realtime*` `[MEASURED]` |

`AICC_TRANSCRIBE_PROVIDER` selects the client at startup, exactly as `AICC_PROVIDER`
selects the voice profile — one transcription engine per deployment, resolved once in
`main`. Recorded as **D20**.

Interface boundary (deliberately smaller than `VoiceSession` — there is no synthesis,
no tools, no interruption):

```go
type Session interface {
    Start(ctx context.Context, cfg Config) error
    SendAudio(pcm16 []byte) error   // mono, the profile's rate
    Events() <-chan Event           // PARTIAL | FINAL | SPEECH_STARTED | ERROR | CLOSED
    Close(ctx context.Context) error
}
```

**Protocol/data differences to adapt.** Every row below marked `[MEASURED]` was
exercised live against the vendor on 2026-08-16 with audio captured through the real
media path; the transcripts both engines returned are quoted in Appendix B.

The two columns are **the two clients**, not two configurations of one.

| Concern | `openairt` — OpenAI `gpt-live-transcribe` | `dashscope` — Qwen `qwen-audio-3.0-asr-flash-streaming` |
|---|---|---|
| Endpoint | `wss://api.openai.com/v1/realtime?intent=transcription` `[MEASURED]` — note `intent=`, not `model=` | `wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference` `[MEASURED]` — **fixed path, no query**; the workspace ID *is* the hostname |
| Model name | `gpt-live-transcribe`, in `audio.input.transcription.model` `[MEASURED]` | `qwen-audio-3.0-asr-flash-streaming`, in `payload.model` `[MEASURED]` |
| Session shape | nested: `session.type:"transcription"`, `session.audio.input.*` `[MEASURED]` | `run-task` envelope: `header{action, task_id, streaming:"duplex"}` + `payload{task_group:"audio", task:"asr", function:"recognition", model, parameters, input:{}}` `[MEASURED]` |
| Auth | `Authorization: Bearer` `[MEASURED]` | `Authorization: Bearer` `[MEASURED]`; optional `X-DashScope-WorkSpace` (not needed when the workspace is in the host) |
| Audio transport | base64 inside `input_audio_buffer.append` JSON | **raw binary WebSocket frames** `[MEASURED]` — no JSON wrapper, no base64 |
| Encoding | `audio/pcm` | `parameters.format:"pcm"` (also wav/mp3/opus/speex/aac/amr); **mono only** `[MEASURED]` |
| **Sample rate** | **≥ 24000 enforced.** 16000 → `integer_below_min_value`, "Expected a value >= 24000" `[MEASURED]`. Supplied by asking the tap for `24000` — no Go resampling (D15) | **16000 accepted natively** `[MEASURED]`; the tap is asked for `16000`. 8 k needs a different model (`fun-asr-flash-8k-realtime`, exactly 8000) |
| **Turn detection** | **must be `null`.** `server_vad` → "Turn detection is not supported for this transcription model" `[MEASURED]` | server-side, tuned by `semantic_punctuation_enabled`, `max_sentence_silence` (default 1300 ms, 200–6000), `speech_noise_threshold` |
| **Who ends an utterance** | **We do.** 6 s of trailing silence produced *no* final; only `input_audio_buffer.commit` did `[MEASURED]` | **The server does** `[MEASURED, B.3b]` — in a two-utterance test both finals arrived *while audio was still streaming*, 8 s and 3 s before `finish-task` |
| Partial event | `…transcription.delta` carries an **incremental fragment** — the client accumulates `[MEASURED]` | `result-generated` with `sentence_end:false`; `sentence.text` is the **cumulative sentence so far** (`"您"` → `"您好"` → `"您好，我这边"`), so the client *replaces* rather than appends `[MEASURED]` |
| Final event | `…completed`; `transcript` is a **corrected rewrite**, not the concatenation of the deltas `[MEASURED]` | `result-generated` with `sentence_end:true`, punctuated and corrected (`"，妈"` → `"，麻烦"`) `[MEASURED]` |
| **Utterance identity** | `item_id` | `sentence.sentence_id`, **monotonic from 1** `[MEASURED]` — a ready-made ordering key |
| **Timestamps** | **none** `[MEASURED]` | **`begin_time` / `end_time`, ms from stream start** `[MEASURED]`, plus a `words[]` array with per-word times and punctuation |
| Keep-alive | — | `heartbeat:true` results with `sentence_id:0`, to be skipped; `parameters.heartbeat` keeps a silent connection open |
| Language hint | `audio.input.transcription.languages[]`, plus `prompt`, `keywords[]` | `language_hints[]` (up to 4), `vocabulary{term:weight}` hot words |
| Speaker labels / confidence | **none** | **none** (diarization explicitly unsupported) |
| Errors | `error` with `error.{type,code,message,param}` `[MEASURED]` | `task-failed` with `header.{error_code,error_message}`; **the connection is then closed and not reusable** |
| Ordering | "Ordering between completion events from different speech turns isn't guaranteed" — reconcile on `item_id` | sequential by `sentence_id` `[MEASURED]` |

`qwen3-asr-flash-realtime` is a third engine that speaks the **`openairt`** dialect (its
measured behaviour is in B.3). It is not what this deployment ships, but it is why the
`openairt` client must not hard-code "OpenAI" into its profile — endpoint, model and the
`text`/`stash` partial spelling all vary within that one protocol.

**The asymmetry is structural, not cosmetic**, and it lands in four places:

1. **The tap's sample rate is chosen per client — on the switch, not in Go.**
   `[MEASURED, B.1]` `mod_audio_stream` accepts any multiple of 8000 and genuinely emits
   what it is asked for, so the attach command carries **`24000`** for `openairt` and
   **`16000`** for `dashscope`, and no resampling happens in our process at all. The rate
   is therefore an attach-time parameter derived from the selected client — one integer in
   the command string, not a code path. Note the literal must be `24000`; `24k` is
   rejected (B.1). Settled as D15.

   > **Superseded (2026-08-17), kept per the B.3/D6 convention.** This item previously read:
   > *"The ingest resamples per profile. `mod_audio_stream` emits 8 k or 16 k only, so the
   > OpenAI path needs 16 k → 24 k (×3/2) … `internal/media` has no rational resampler …
   > Recommendation: take 16 k and write the ×3/2 resampler with its benchmark."* The
   > premise — 8 k or 16 k only — came from the module's README and is false in its source.
   > `[FACT]` `internal/media`'s inventory is unchanged and still has no rational resampler
   > (`resample.go:28,75`); it simply no longer matters here.
2. **The profile carries an endpointing trait, not a constant.** `OwnsEndpointing bool`
   — false for Qwen (server VAD), true for OpenAI. Where true, the ingest runs
   energy-based silence detection on the PCM it already holds and sends
   `input_audio_buffer.commit`; that commit is also the natural moment to allocate a
   `seq` (§8.4). This is new work the first draft did not have, and it exists only
   because the measurement contradicted the documentation's implication.
3. **Three different partial semantics are folded inside the clients.** OpenAI's `delta`
   is an *increment* to accumulate; DashScope's `sentence.text` is the *cumulative
   sentence* to replace; `qwen3-asr-flash-realtime`'s is `text`+`stash` to concatenate.
   `[FACT]` This is the same move `internal/provider/realtime.go:502` already makes when
   it folds two spellings of the audio-delta event into one internal type. The seam above
   sees `PARTIAL{text}` / `FINAL{text}` — always the full current text, never a fragment —
   and never learns which vendor produced it. **Getting this wrong is silent:** treating a
   cumulative text as a delta yields `您您好您好，我这边…` on screen, with no error
   anywhere.
4. **The audio send path differs at the transport layer, not just in framing.** OpenAI
   takes base64 inside a JSON envelope; DashScope takes **raw binary WebSocket frames**
   `[MEASURED]`. The binary path is the cheaper one — no base64 expansion, no JSON
   marshal per 20 ms frame — and the `Session.SendAudio(pcm16 []byte)` signature already
   hides the difference. `[INFERENCE]` Only the `openairt` client needs a per-frame
   encode, so only it needs the allocation scrutiny the frame paths are held to.

One further consequence, and a correction to the first draft:

- **~~Neither engine gives us time.~~** `[MEASURED, B.3b]` The DashScope client **does**:
  `begin_time` / `end_time` in milliseconds from stream start, per sentence, plus per-word
  times. OpenAI still gives nothing.
  This does **not** change D2 — `seq` is still allocated by our single-threaded transcript
  actor, because a vendor timestamp is per-stream and we are merging *two* streams whose
  clocks share no origin. What it does give is a **within-speaker** ordering key that
  survives out-of-order delivery, and a real `startedAtMs`/`endedAtMs` for the agent's
  half. `[INFERENCE]` Treat it as optional enrichment carried on the `Event`, present on
  one client and absent on the other — never as the ordering mechanism, or the two clients
  stop being substitutable.

### 8.4 Ordering, and the one thing that must be single-threaded (question 11, D2)

`[FACT]` The problem is concrete: `callRecorder` allocates `seq` from a counter local
to one bot session (`internal/aicall/ledger.go:82`), and the table enforces
`uq_transcripts_call_id_seq` (`00005_call_ledger.sql:64`). Two producers in one call
would collide.

**Recommendation `PROVISIONAL`: one transcript actor per call, created on first use,
sole allocator of `seq`, sole writer to `transcripts` and sole publisher of
`CALL_TRANSCRIPT`.** Both producers — the bot session and the ASR sessions — post
lines to its mailbox. This is the same actor pattern the registry already uses
(`internal/telephony/registry.go:70-76,258-287`), and it makes the ordering property a
structural fact rather than a runtime hope.

Three time-shaped fields, with different jobs:

| Field | Type | Generated where | Meaning | Used for |
|---|---|---|---|---|
| `seq` | `int`, dense, per call, from 1 | the transcript actor, at the moment a **final** line is accepted | the **only** total order | client sort key; resume cursor; `uq_transcripts_call_id_seq` |
| `occurredAt` | `timestamptz` | the transcript actor, `time.Now().UTC()` at ingest | server receipt | audit, CDR export, cross-call correlation |
| `offsetMs` | `int` | the transcript actor, `occurredAt − call.answeredAt` | position in the conversation | alignment with the recording; display |

Rationale for `seq` being per call and dense rather than reusing the global SSE `seq`:
`[FACT]` the SSE sequence is globally shared and its gaps are explicitly meaningless
(`internal/events/seq.go:11-14` — hi/lo blocks, "a crash burns the remainder of the
block"). A resume cursor over a transcript needs density; the SSE seq cannot provide it.
Both numbers appear on a streamed line: the envelope's `seq` (transport) and the
payload's `seq` (transcript order).

Rationale for anchoring `offsetMs` at answer rather than at call creation:
`[FACT]` `Call.AnsweredAt()` already exists and reports the first answer by anybody,
including the bot (`internal/telephony/call.go:258-269`), which is also when
`record_session` begins (`aicc_inbound.lua:82,93`) — so the offset lines up with the
recording's timeline, which is the point.

---

## 9. Data model delta — Table 3

**Recommendation `PROVISIONAL`: extend `transcripts` rather than add a second table.**
Two tables would force every reader to union two orderings for one conversation, and
the existing unique constraint is already the right invariant.

Migration `00009_transcript_speakers.sql` (goose, in `internal/store/migrations/`):

| Change | DDL | Rationale |
|---|---|---|
| Rename `role` → `speaker` | `ALTER TABLE transcripts RENAME COLUMN role TO speaker;` | `role` means three different things in this contract (G-19); 07 §5 exists to kill exactly that |
| New enum values | drop/recreate the CHECK: `speaker IN ('CUSTOMER','BOT','HUMAN_AGENT')` | the fixed enum from the brief |
| Data migration | `UPDATE transcripts SET speaker='CUSTOMER' WHERE speaker='CALLER';` | 07 §6 forbids synonym drift; `CALLER` and `CUSTOMER` cannot both survive |
| `party_id uuid` | `ADD COLUMN party_id uuid` | which leg said it; nullable because the bot phase's `Party` is gone by write time |
| `agent_id uuid` | `ADD COLUMN agent_id uuid` | who, when `speaker='HUMAN_AGENT'`; the data-layer input to D10 |
| `offset_ms int NOT NULL DEFAULT 0` | | §8.4 |
| `language varchar(8) NOT NULL DEFAULT ''` | | as reported by the engine; lowercase BCP 47 (07 §7) |
| `source varchar(16) NOT NULL DEFAULT 'MODEL'` | `CHECK (source IN ('MODEL','ASR'))` | which producer wrote it — the D1 hybrid is only auditable if the line says so |
| `provider varchar(32) NOT NULL DEFAULT ''` | | profile name (`openai`/`qwen`), for quality comparison |
| `utterance_id text NOT NULL DEFAULT ''` | | idempotency + partial→final identity (D3); the engine's `item_id` where one exists, else minted at ingest |
| Idempotency index | `CREATE UNIQUE INDEX uq_transcripts_call_id_utterance_id ON transcripts (call_id, utterance_id) WHERE utterance_id <> '';` | a redelivered final must not duplicate |
| Backfill index | none needed | `uq_transcripts_call_id_seq` already serves `WHERE call_id=$1 AND seq>$2 ORDER BY seq` |

`kind` is unchanged (`TEXT | TOOL_CALL | TOOL_RESULT`) — an ASR line is always `TEXT`.

**Not added, deliberately:** no `live_calls`, no `transcription_sessions` table. Session
state is in-memory and re-derivable; persisting it would promise a mid-call recovery
this system does not offer for anything else (G-17).

**Lua contract:** `[FACT]` `docs/design/03-data.md:120` requires every migration
touching base tables to re-assert the `luacc.*` view shapes. `transcripts` is not in
any view (`00002_telephony.sql:157-202`), so this migration touches the contract not at
all — but the reviewer checklist still applies.

**Retention:** `[FACT]` `03-data.md:124` says transcripts are retained independently of
recordings; no retention job exists in the tree for either. Out of scope here; noted.

---

## 10. API & OpenAPI delta

Order is fixed by `CLAUDE.md`: edit `docs/openapi.json` → `make api-generate` →
implement → test. `make api-check` and `make api-breaking BASE=main` are the gates.

**New operation**

```
GET /calls/{callId}/transcript
  operationId: getCallTranscript
  query: sinceSeq (integer, optional, default 0), limit (integer, default 500)
  200: CallTranscript { items: TranscriptLine[], nextSinceSeq: integer,
                        isLive: boolean, state: TranscriptionState }
  403 / 404 per the standard error envelope
```

Authorization: **not** `requireSupervisorRole`. `[FACT]` The existing transcript read
is supervisor-only (`internal/httpapi/server.go:207-210`), which an agent on the call
cannot use. Mount it in the call-control group (`server.go:156-167`, already
`requireAgentRole`) and add the same involvement check the other call operations rely
on: `Coordinator.agentChannel(callID, agentID)` returns `ErrNoAgentLeg` for a
non-party (`coordinator.go:567-584`). Supervisors and admins pass unconditionally, as
they do everywhere else (`internal/events/hub.go:203`).

**Changed schemas**

| Schema | Change | Breaking? |
|---|---|---|
| `TranscriptRole` → `Speaker` | rename; values `CUSTOMER \| BOT \| HUMAN_AGENT` (was `CALLER \| BOT`) | **yes** — declare it |
| `TranscriptEntry` → `TranscriptLine` | `role` → `speaker`; add `partyId?`, `agentId?`, `offsetMs`, `language?`, `source`, `provider?`, `utteranceId`, `isFinal` | **yes** |
| `CDRDetail.transcript` | items become `TranscriptLine` | no (additive at the array level, breaking at the item level) |
| `SseEventType` | add `CALL_TRANSCRIPT`, `CALL_TRANSCRIPTION_STATE`; remove `BOT_TRANSCRIPT` | removal is **breaking** |
| new `SseTranscriptPayload` | see §11 | additive; register it alongside the other `Sse*Payload` components and keep the lint-ignore convention (`CLAUDE.md`, `.redocly.lint-ignore.yaml`) |
| new `SseTranscriptionStatePayload` | see §11 | additive |
| new `TranscriptionState` | `IDLE \| CONNECTING \| LIVE \| DEGRADED \| ERROR \| STOPPED \| ENDED` | additive |

`[FACT]` Go initialisms come from `oapi-codegen.yaml`'s `name-normalizer` +
`additional-initialisms` (`CLAUDE.md`); nothing here introduces a new initialism, so
that list is untouched.

`[FACT]` The ledger group still writes `store.*` types straight out
(`internal/httpapi/ledger_handlers.go:67-71`), a documented exception to be settled
"when the ledger group next changes" (`m4-cleanup-findings.md:232-236`). **This is that
change.** The new endpoint and the reshaped `GetCDR` response should be built on
`api.*` types, and `TestLedgerTypesMarshalPerTheNamingSpec`
(`internal/store/naming_test.go:14`) updated accordingly.

---

## 11. SSE event delta

**`CALL_TRANSCRIPT`** — call-scoped. `[FACT]` The scoping rule in
`docs/design/04-api-sse.md:44` is that party-lifecycle facts are party-scoped and
aggregate facts are call-scoped; a transcript is a fact about the conversation, which
is why it should replace the party-flavoured `BOT_TRANSCRIPT` rather than sit beside it.

```jsonc
// SseTranscriptPayload — envelope carries callId, callType, occurredAt, seq (transport)
{
  "seq": 41,                       // transcript order within the call — NOT the envelope seq
  "utteranceId": "item_A7…",
  "speaker": "HUMAN_AGENT",        // CUSTOMER | BOT | HUMAN_AGENT
  "agentId": "…uuid…",             // present only for HUMAN_AGENT
  "partyId": "…uuid…",             // present when the leg is still known
  "kind": "TEXT",                  // TEXT | TOOL_CALL | TOOL_RESULT
  "isFinal": true,
  "text": "I can see the order here.",
  "offsetMs": 184320,
  "language": "en",
  "source": "ASR"                  // MODEL | ASR
}
```

Partial lines carry `isFinal:false` and **no** `seq` (they have no place in the order
until they are final) — see D3/D9.

**`CALL_TRANSCRIPTION_STATE`** — call-scoped:

```jsonc
{ "state": "DEGRADED", "reason": "AGENT_STREAM_LOST", "degradedSpeakers": ["HUMAN_AGENT"] }
```

**Scope.** `events.Scope{AgentIDs: <every agent party on the call>, QueueID: call.QueueID}`
— the same construction the registry already uses (`internal/telephony/registry.go:407-418`).
`[FACT]` During the bot phase there is no agent party, so these events reach
supervisors and admins only (`hub.go:203`). That is correct and it is *why* the REST
backfill in D8 is mandatory: the agent's first sight of the bot phase is the snapshot
they fetch when they join.

**Frontend type source.** `[FACT]` `web/src/lib/events.ts:10-16` re-exports
`components['schemas']['SseEvent']` from `web/src/generated/api.ts`; no handwritten DTO
is permitted (`CLAUDE.md`). The two new names must also be appended to the literal
listener array at `web/src/lib/events.ts:54-64`, because `EventSource` dispatches by
event name and an unlisted type is silently dropped.

**Prerequisite fix (G-09).** Replay must stop passing an empty scope
(`internal/events/hub.go:132`). Today a reconnecting agent is replayed every event in
the ring that matches their `?types=` filter, regardless of who it was for. With
transcripts on the stream that becomes a disclosure bug, not an untidiness. The ring
entry should carry its `Scope` and `Subscribe` should evaluate it.

---

## 12. Frontend delta

### 12.1 Where the panel goes (question 13)

`[FACT]` The cockpit is three columns:

```tsx
<div className="flex w-[320px] shrink-0 flex-col gap-4"> …CallPanel / DialCard, CallbacksCard
<div className="flex min-w-0 flex-1 flex-col gap-4">     …CallerCard, JourneyCard
<div className="flex w-[280px] shrink-0 flex-col gap-4"> …WrapUpCard, PresenceCard
```
— `web/src/routes/_app.agent.index.tsx:45-61`

**Recommendation: the centre column**, as a third card between `CallerCard` and
`JourneyCard`, taking the growth (`min-h-0 flex-1`) that `JourneyCard` holds today
(`:479-480`).

Why not the left column: `[FACT]` it holds `CallPanel`, which is the in-call control
grid — mute/hold/transfer/DTMF/hangup (`:118-159`). Growing anything there squeezes the
one part of the screen that must never be squeezed, and Q22's isolation requirement is
easiest to honour when the transcript is not a sibling of the controls at all. Why not
the right column: 280 px is far too narrow for a two-column speaker/text layout at
13 px with a fixed label column.

`JourneyCard` keeps its content and stops growing. `CallPanel`, `CallbacksCard`,
`WrapUpCard`, `PresenceCard` and the topbar `SoftphoneBar` are not touched.

### 12.2 Files — new vs locally modified

| File | New / modified | What |
|---|---|---|
| `web/src/components/live-transcript.tsx` | **new** | the panel: header + status dot, scroller, rows |
| `web/src/lib/transcript.ts` | **new** | contract type re-exports, `useCallTranscript(callId)` (backfill + live merge), the status machine |
| `web/src/components/live-transcript.test.tsx` | **new** | §16 layer 3 |
| `web/src/routes/_app.agent.index.tsx` | **modified,局部** | insert `<LiveTranscript …/>` at line 52–53; drop `flex-1` from `JourneyCard` (`:479`); rewrite the file-header comment at `:26-35`, which currently states the panel is deliberately absent. **No existing component, handler or mutation is removed.** |
| `web/src/lib/events.ts` | **modified, 局部** | append two names to the `types` array (`:54-64`) |
| `web/src/lib/use-event-stream.ts` | **modified, 局部** | wrap listener invocation in try/catch (`:69-70`) — G-14 |
| `web/src/generated/api.ts` | regenerated | `make api-generate` output; never hand-edited |
| `web/src/lib/ledger.ts` | **modified, 局部** | `TranscriptEntry` → `TranscriptLine` re-export (`:19`) |
| `web/src/routes/_app.admin.cdr.$callId.tsx` | **modified, 局部** | `entry.role` → `entry.speaker`; `cdr.roles.CALLER` → `cdr.roles.CUSTOMER`; add `HUMAN_AGENT` (`:161-198`) |
| `web/src/locales/en/translation.json`, `…/zh/translation.json` | **modified** | new `transcript.*` block; rename `cdr.roles.CALLER` |

`[FACT]` A whole-file rewrite of `_app.agent.index.tsx` is forbidden by the standing
constraint in the brief and by history; the change above is three edits inside a
645-line file. The regression assertions in §16 layer 3 exist to prove it.

### 12.3 Backfill and resume (questions 14, 15 — D8)

`[FACT]` There is no way to read a live call's transcript today: the only route is
`GET /cdrs/{callId}`, SUPERVISOR-gated, and it reads the `cdrs` row first
(`internal/httpapi/ledger_handlers.go:46-54`) — a live call has no such row.

`[FACT]` The stream *does* support resume: `Last-Event-ID` (header or `?lastEventId`)
is parsed at `internal/httpapi/events_handler.go:139-149`, resolved against the ring in
`Hub.Subscribe` (`internal/events/hub.go:117-137`), and a resume point older than the
ring produces `SYSTEM_RESET` (`events_handler.go:79-89`). It is covered by
`TestEventStreamReplaysAfterLastEventID`
(`internal/httpapi/events_handler_test.go:126`) and
`TestResumeBeyondRingRequestsReset` (`internal/events/hub_test.go:186`).
`[FACT]` The browser's own `EventSource` handles reconnect and the header
(`web/src/lib/events.ts:5-8`).

**The no-loss / no-duplicate contract:**

```
1. subscribe first   — the panel registers its listener and starts BUFFERING
                       CALL_TRANSCRIPT payloads for this callId. Nothing is rendered
                       from the buffer yet.
2. snapshot second   — GET /calls/{callId}/transcript?sinceSeq=0
3. merge             — render snapshot.items, then every buffered line whose
                       payload.seq > max(snapshot.items.seq); sort by payload.seq
4. tail              — subsequent lines append if seq == last+1, or insert in order
                       if they arrive out of order
```

Order matters: subscribing after the snapshot leaves a window in which a final line is
neither in the snapshot nor in the buffer. Subscribing first can only produce
duplicates, which step 3 removes deterministically because `seq` is dense (§8.4).

On SSE reconnect the same four steps run again with `sinceSeq = lastRenderedSeq`; the
snapshot closes whatever the ring could not replay. On `SYSTEM_RESET` the same, from
`sinceSeq = lastRenderedSeq` — no full reload is needed, because the transcript's own
cursor is independent of the transport's.

`[FACT]` `useEventStream` already invalidates every query on open and on reset
(`web/src/lib/use-event-stream.ts:58-66`), so a TanStack-Query-backed snapshot refetches
automatically on reconnect; the panel only has to keep its buffer across the gap.

### 12.4 partial → final rendering (question 16 — D3/D9)

**Recommendation `PROVISIONAL`: partials are pushed but never stored; a partial is
rendered in place and replaced by its final.**

- React key: `utteranceId`. **Not** `seq` — a partial has no `seq` (§11) — and not the
  array index. `[FACT]` The finished-call view keys on `entry.seq`
  (`_app.admin.cdr.$callId.tsx:114`), which is correct there because everything is final;
  the live panel cannot reuse it.
- A partial with no `seq` sorts **after** every line that has one, at the position of
  its speaker's last known activity. In practice at most two partials exist at once
  (one per speaker), so this is a two-element tail, not a sort problem.
- Height change on replacement: measure stickiness **before** the mutation, in a
  `useLayoutEffect`, as `scrollHeight − scrollTop − clientHeight < 24`; if it was
  sticky, `scrollTop = scrollHeight` after paint. This is the standard fix and it makes
  a growing final invisible to a user at the bottom, while a user who has scrolled up
  is never yanked.
- Deduplication is by `utteranceId`, not by text: repeated text is legitimate ("yes",
  "yes").

### 12.5 Ordering on the client (question 17)

`[FACT]` There is no ordering tolerance today. `connectEvents` parses and dispatches
in arrival order (`web/src/lib/events.ts:42-51`); `useEventStream` invokes listeners
immediately (`use-event-stream.ts:67-71`); no consumer sorts on `seq`.

That is fine for the events that exist — `applyToCache` only invalidates queries
(`use-event-stream.ts:15-33`), so order does not matter — but it is not fine for a
transcript. The panel must hold its own array and insert by `payload.seq`
(binary search; the common case is append). Global out-of-order handling is not needed
and should not be added: `Hub.Publish` stamps and fans out under one lock
(`internal/events/hub.go:84-101`), so the stream itself is ordered; the risk is the
*merge* of a snapshot and a buffer (§12.3), which is exactly what the sort protects.

### 12.6 Merge strategy (question 18)

**Recommendation `PROVISIONAL`: one row per final line. No merging of consecutive
lines from the same speaker.**

Merging would destroy the one-to-one map between a rendered row and a `seq`, an
`occurredAt` and an `offsetMs` — and every downstream consumer needs that map: the CDR
detail view aligns rows against the recording by offset
(`_app.admin.cdr.$callId.tsx:156-158`), and a merged block's timestamp would be a
fiction. The reading benefit of merging is obtained for free by *visual* grouping:
suppress the repeated speaker label on a run of same-speaker rows and tighten the top
margin. The data stays honest, the page reads as paragraphs.

### 12.7 Auto-scroll (question 19)

`[FACT]` No reusable scroll-follow container exists. The two scrolling containers in
the tree are plain overflow panes with no follow behaviour:
`bodyClassName="min-h-0 flex-1 overflow-y-auto"` on `CallbacksCard` and `JourneyCard`
(`_app.agent.index.tsx:382`, `:480`).

Behaviour: stick to bottom by default; the moment `scrollHeight − scrollTop −
clientHeight > 24` after a user-initiated scroll, stop following and show a "jump to
latest" affordance in the card's `aside` slot (the pattern the `Card` component already
provides, `_app.agent.index.tsx:601-627`); clicking it resumes following.

Additional rule for question 27: pause following while a non-collapsed selection is
anchored inside the container (`document.getSelection()`), so copying a line is not
interrupted by the next arrival.

### 12.8 Failure isolation (question 22)

Hard boundary, stated as rules a reviewer can check:

1. The panel's query key is its own (`['transcript', callId]`) and shares nothing with
   `CALLS_KEY`, `PRESENCE_KEY` or `ROSTER_KEY` (`web/src/lib/agent.ts:13-14,101`), so no
   transcript failure can invalidate or block a call snapshot.
2. `[FACT]` A throwing listener currently skips the remaining listeners for that event
   (`use-event-stream.ts:69-70`). Wrap each invocation in try/catch; the transcript
   listener must not be able to starve the softphone's cache updates.
3. The panel is wrapped in an error boundary that renders `transcript.unavailable` and
   nothing else.
4. A 4xx/5xx from the transcript endpoint renders the empty state. It never triggers a
   sign-out path and never surfaces through `describeError` into a call control.
5. No transcript code path calls any endpoint under `/agent/*` or `/calls/{id}/*`.
6. `AICC_TRANSCRIPTION_ENABLED=false` must leave the cockpit byte-identical to today
   apart from an absent card — asserted in §16 layer 3.

### 12.9 Speaker labels, other viewpoints, and status (questions 20, 21, 25)

**D10 — label mapping belongs in the component layer.** `[FACT]` The decisive evidence
is the hub's ring: `appendRing` stores **one** copy of an envelope and `Subscribe`
replays that same copy to every subscriber (`internal/events/hub.go:172-184`,
`:131-135`). A payload whose text depends on who is reading it cannot exist there. The
payload therefore carries `speaker` plus `agentId`; the component resolves:

| Viewpoint | `HUMAN_AGENT` renders as | Source |
|---|---|---|
| `/agent` cockpit, own call | `You` (bold) | `agentId === myAgentId` — the cockpit already resolves its own agent id from the call's parties (`_app.agent.index.tsx:70`) |
| `/agent` cockpit, someone else's leg | agent display name | `useRoster()` → `RosterEntry.displayName` (`internal/agents/service.go:67`) |
| CDR detail, quality, supervisor | agent display name, else `Agent` | same |

`Bot` is the only labelled speaker that carries an icon; `Customer` and `You` carry
none. Stated in the brief as fixed, and it survives review: the label column is fixed
width, and icon scarcity is what makes the one icon mean something. If `You` and
`Customer` later need more separation, the escalation is a left rule or a very faint
row tint drawn from the token layer (`web/src/index.css`), never a second icon.

**Status machine (question 21).** `Transcribing…` alone is not enough; the panel needs
to be honest about a degraded call.

| State | Rendered | Trigger source | i18n key |
|---|---|---|---|
| `IDLE` | dot muted, "Not transcribing" | no active call, or `AICC_TRANSCRIPTION_ENABLED=false` (endpoint 404/feature off) | `transcript.status.IDLE` |
| `CONNECTING` | dot amber, "Connecting…" | `CALL_TRANSCRIPTION_STATE {CONNECTING}` — emitted when `uuid_audio_stream` is issued | `transcript.status.CONNECTING` |
| `LIVE` | dot green, "Transcribing…" | `CALL_TRANSCRIPTION_STATE {LIVE}` — both ASR sessions ready | `transcript.status.LIVE` |
| `DEGRADED` | dot amber, "Transcribing one side" | `{DEGRADED, degradedSpeakers:[…]}` — one leg's session failed | `transcript.status.DEGRADED` |
| `ERROR` | dot red, "Transcription unavailable" | `{ERROR}`, or the SSE stream is `offline`/`reconnecting` (`useEventStream` already reports this, `use-event-stream.ts:35,48`) | `transcript.status.ERROR` |
| `STOPPED` | dot muted, "Transcription ended" | `{STOPPED}` — stream stopped while the call is still up | `transcript.status.STOPPED` |
| `ENDED` | dot muted, "Call ended" | the call left `GET /calls/mine`, or `PARTY_RELEASED` for the last leg | `transcript.status.ENDED` |

**After the call (question 25): freeze, do not clear.** `[FACT]` The cockpit's call
disappears from `useMyCalls` when the call ends (`web/src/lib/agent.ts:104-119`), and
wrap-up begins (`WrapUpCard`, `_app.agent.index.tsx:555`). Clearing the panel at exactly
the moment the agent has to write a disposition would throw away what they need. Keep
the last call's transcript rendered, in `ENDED` state, until a new call arrives or
wrap-up is released.

Reuse elsewhere: the same `<LiveTranscript>` renders the finished-call transcript on
`/admin/cdr/$callId` with `isLive=false` and the supervisor label mapping. **The quality
UI is not built in this milestone** — `[FACT]` the route does not exist
(`web/src/lib/nav.ts:49`, `isReady:false`) and building it is a separate feature; the
component is designed so that page is a consumer, not a rewrite.

### 12.10 Long calls (question 23)

`[INFERENCE]` Rough scale: two speakers, a final utterance every ~4–6 s each while
talking, ≈50 % talk duty → **roughly 600–900 rows** for a 30-minute call, plus the bot
phase's tool traces.

**Recommendation: no virtualization in v1.** 900 memoized rows of two spans each is
well inside what React handles at 60 fps, and virtualization would fight the
partial-in-place replacement and the selection-preserving scroll rules above. Instead:
render at most the newest 400 rows, with "load earlier" pulling from the REST endpoint
(which already pages on `sinceSeq`/`limit`); memoize the row component on
`utteranceId + isFinal + text`; keep the whole ordered array in a ref, not in state, and
publish only a windowed slice. Revisit only if a real call proves otherwise — the
project's standing rule against unmeasured performance claims cuts both ways.

### 12.11 i18n (question 24)

`[FACT]` en/zh, flat namespaced keys, zero hardcoded user-visible strings
(`docs/design/05-frontend.md:21`; the key inventory is in
`web/src/locales/en/translation.json`).

New keys: `transcript.title`, `transcript.empty`, `transcript.loading`,
`transcript.unavailable`, `transcript.jumpToLatest`, `transcript.loadEarlier`,
`transcript.speakers.{BOT,CUSTOMER,HUMAN_AGENT,YOU}`,
`transcript.status.{IDLE,CONNECTING,LIVE,DEGRADED,ERROR,STOPPED,ENDED}`.
Renamed: `cdr.roles.CALLER` → `cdr.roles.CUSTOMER` (both locales); `cdr.roles.AGENT`
already exists and is reused for `HUMAN_AGENT` in non-agent viewpoints.

**Transcript body is never translated.** It is what was said. A bilingual call renders
mixed lines in reading order; the per-line `language` field (§9) is available if a
future view wants to mark them, but the panel shows no language badge — a badge on
every line in a bilingual call is noise, and the text speaks for itself.

### 12.12 Accessibility (question 27)

`role="log"` on the scroller. `aria-live="polite"` on a region that receives **only
final lines**; partials render with `aria-hidden="true"`. Announcing every partial
would make a screen reader unusable — it is the same word three times a second. Each
row is a `<li>` with the speaker label as visible text (not `aria-label`), so copying a
selection copies "You: …". Selection-aware scroll pause per §12.7.

---

## 13. Config delta

`[FACT]` `.env.example` is the registry and must be kept in step with
`internal/config/config.go` (`CLAUDE.md`); the loader treats an empty value as unset
and has no inline-comment syntax.

| Variable | Default | Meaning |
|---|---|---|
| `AICC_TRANSCRIPTION_ENABLED` | `false` | Master switch. **Off by default**: it is a new external dependency, a new listener and new spend, and M5's behaviour must be reachable by doing nothing. |
| `AICC_STREAM_ADDR` | `127.0.0.1:8090` | The ingest listener (D5). **Loopback by default, never `:8090`** — `[FACT]` this copies `AICC_METRICS_ADDR`'s actual default, which is `127.0.0.1:9090` and not `:9090` (`config.go:100`). Since the traffic is unencrypted call audio (D5), a default that binds every interface would be the one mistake nobody notices. The dev setup has FreeSWITCH on the same host, so loopback just works; the demo overrides it to the container address, exactly as it already must for `AICC_ESL_ADDR`. |
| `AICC_STREAM_PUBLIC_URL` | `""` | What the **switch** dials, e.g. `ws://aicc:8090/stream`. **Carries the scheme** — nothing in the code composes `ws://` (D5). Must be resolvable from the switch's network namespace — in the demo that is the compose service name, exactly as `AICC_ESL_ADDR: freeswitch:18021` is today (`deploy/demo/docker-compose.yml:55`). Empty disables. |
| `AICC_STREAM_SECRET` | `""` | HMAC key for the ingest token. Empty + enabled = startup error. |
| `AICC_TRANSCRIBE_PROVIDER` | falls back to `AICC_PROVIDER` | `openai` \| `qwen`. **Selects the client, not just a profile** (D20). Defaulting to the deployment's provider honours A1: one vendor is reachable per deployment. |
| `AICC_TRANSCRIBE_ENDPOINT` | `""` | Override, same semantics as `AICC_PROVIDER_ENDPOINT` (`config.go:116`). **On `qwen` this is effectively required**, not optional: `[MEASURED, B.3b]` the DashScope host embeds the workspace ID (`llm-….cn-beijing.maas.aliyuncs.com`), so it is deployment-specific and cannot ship as a profile constant. `[INFERENCE]` Validation should therefore refuse to boot with `qwen` + transcription enabled + an empty endpoint, rather than dial a placeholder host. |
| `AICC_TRANSCRIBE_MODEL` | `""` | Override; client default is `gpt-live-transcribe` / `qwen-audio-3.0-asr-flash-streaming` `[MEASURED, B.3b]`. Note the value must match the *client*: `qwen3-asr-flash-realtime` is an `openai`-dialect model and will not run on the `qwen` client. |
| `AICC_TRANSCRIBE_PARTIALS` | `true` | Push partials over SSE (D9). Storage is unaffected (D3). |

No new credential slot: the ASR client reads the **same** `OPENAI_API_KEY` /
`ALIYUN_API_KEY` the voice profile names (`internal/provider/profile.go:74,96` →
`realtime.go:104`).

**No `AICC_STREAM_RATE`, deliberately.** The tap's sample rate is *derived* from the
selected transcription client (D15: `24000` for `openairt`, `16000` for `dashscope`), not
configured. `[INFERENCE]` A knob here could only ever be set to disagree with the client
that consumes the audio, and the failure it would produce is the quiet kind — chipmunk or
slowed speech that still transcribes into plausible-looking wrong words, rather than an
error. The rate is a property of the client; let it stay one.

Validation to add in `Config.validate()` (`config.go:140`), each one failing at startup
rather than at the first call:

- enabled ⇒ non-empty `AICC_STREAM_PUBLIC_URL` and `AICC_STREAM_SECRET`;
- `AICC_STREAM_PUBLIC_URL`'s scheme is `ws` or `wss` and nothing else (D5 rule 2) — this
  is what catches an `http://` paste, which would otherwise fail as a mystery at the
  switch;
- `AICC_TRANSCRIBE_PROVIDER=qwen` + enabled ⇒ non-empty `AICC_TRANSCRIBE_ENDPOINT`, since
  the workspace ID is the hostname and has no sane default (D14);
- unknown `AICC_TRANSCRIBE_PROVIDER` is a startup failure, matching how an unknown
  `AICC_PROVIDER` already refuses to boot (`cmd/aicc/main.go:211-213`).

---

## 14. Edge cases (question 12, one by one)

| Case | Current state | Risk | Handling |
|---|---|---|---|
| **Leg UUID change around a transfer** | `[FACT]` the caller's channel UUID is minted once (`aicc_inbound.lua:80`) and is the transfer target (`actions.go:89`); `[FACT]` the agent's channel is new on every delivery (`coordinator.adopt():170`) | A stop/start per delivery — the cost of anchoring on the agent leg | Accepted, and it costs no audio: between two agent legs the caller is in the queue on MOH, so the gap contains nothing anyone would want transcribed. What it buys is that each stream has exactly one `agentId` for its whole life (§8.1). |
| **Bridge set-up and tear-down; second transfer; consult/hold** | `[FACT]` `CHANNEL_BRIDGE`/`UNBRIDGE`/`HOLD`/`UNHOLD` are all normalized (`switchevent.go:217-228`); `[FACT]` `CONSULT` is reserved in the contract but never emitted (`docs/design/01-telephony.md:19`) | On hold the agent's two streams are a private side-conversation and MOH, neither of which belongs in the transcript | `uuid_audio_stream … pause` on `KindChannelHold`, `resume` on `KindChannelUnhold` — both already reach the actor (`registry.go:317-320`). A second transfer needs no special handling: the first agent's leg unbridges and its bug stops with it; the next agent's leg brings its own bug and its own `agentId`. The window in between (queue, MOH, a second bot leg) has no stream at all, because no agent leg exists to carry one. |
| **Agent reconnects (extension re-registers)** | `[FACT]` `sofia::register` → `DEVICE_REGISTERED` (`switchevent.go:273`), agent identity is bound to the extension, not the session (`00007_agent_extension_binding.sql:8`) | None for audio: the media path is the switch's | The panel's SSE stream reconnects on its own; §12.3 closes the gap by cursor. |
| **Customer hangs up first** | `[FACT]` `CHANNEL_HANGUP_COMPLETE` on the caller → `PARTY_RELEASED`; the call finishes when no leg remains (`call.go:287`, `registry.go:330`) | The agent's leg is torn down a moment later, and the last final may still be in flight at the ASR | The caller's hangup unbridges the agent leg, which stops the stream; hold the ASR sessions open for a bounded flush window (recommend 2 s, the same order as `recordingFlushWait`, `cdr.go:35`), then close. Any final arriving inside the window is written; after it, dropped and counted. |
| **Agent hangs up first** | `[FACT]` same path, on the agent's own channel | The bug dies **with** its channel — this is the leg that carries it | `KindChannelHangup` on the agent's channel stops the stream explicitly (§8.1, backstop) and the same flush window applies. `[ASSUMPTION, Appendix B.1]` the module may also tear the bug down itself; the explicit stop is what makes that not matter. |
| **Bot session close vs the last bot final** | `[FACT]` today the whole transcript is written in a `defer` after the session ends (`orchestrator.go:256`), so nothing is lost *and* nothing is live. After §8.4 the bot posts each final to the transcript actor as it happens, and `Close` races the last one | Dropped or duplicated word at the exact handoff moment — the case the brief calls out | The bot's flush is ordered before the actor is told the bot phase ended: `recorder.finish` becomes "post remaining finals, then post `BOT_PHASE_ENDED`", and the actor allocates `seq` in receipt order. Duplicates are impossible because `utterance_id` is unique per call (§9). |
| **WS drops mid-call** | `[FACT]` no reconnect exists anywhere for a provider socket, by design: "no mid-call reconnect (provider session state is unrecoverable)" (`docs/design/02-ai-voice.md:65`) | Silence in the transcript for the outage, misattributed if a session is reused | Different from the voice path: an **ASR** session holds no conversation state worth preserving, so reconnect **is** correct here. Recommend: reconnect with 250 ms→4 s backoff, a new `utteranceId` namespace per connection, `state: DEGRADED` while down, and a visible gap marker rather than a silent one. The audio that arrived during the outage is lost — say so in the UI, do not paper over it. |
| **Provider timeout / rate limit / one-sided close** | `[FACT]` the existing client models all three: read deadline 45 s reset by any frame (`transport.go:26,159`), 15 s ping (`:173`), and a watchdog that closes out an abandoned response from *state* rather than from signals (`realtime.go:637-709`, the M5 bug at `m5-findings.md:9-35`) | Repeating that bug in a second client | Reuse the shape, not the code: the `transcribe` client gets the same deadlines and the same "read it from state" rule. A 429 at dial time is `state: ERROR` and no retry storm — one call, one attempt per backoff step, capped. |
| **Backpressure: the switch pushes faster than the ASR consumes** | `[ASSUMPTION, Appendix B.1]` `mod_audio_stream` documents no backpressure behaviour; buffer size is a duration (`STREAM_BUFFER_SIZE`, default 20 ms) | Unbounded growth in our process, or the switch blocking on a media bug | Bound it in our ingest: a fixed-capacity per-session ring (recommend 2 s of audio ≈ 100 frames); on overflow **drop the oldest and count it**, never block the reader. This is the same rule the RTP send queue already follows (`internal/aicall/session.go:365-368`, "dropping the overflow beats growing without bound"). Expose the count as a metric. |
| **Concurrency: connections, memory, cost** | `[FACT]` per AI call today: 2 UDP + 1 WSS (`docs/design/06-capacity.md:14`) | The human phase adds, per bridged call: 1 inbound WS + 2 outbound WS + 2 ASR sessions — i.e. it roughly **triples** the socket count of a human-agent call, which currently costs this process nothing but ESL events | Sizing is deferred with the rest of the benchmark campaign. **No capacity figure is stated here** (owner directive 2026-08-16, `CLAUDE.md`). What *can* be said without measuring: the cost is per *bridged* call, not per call; it is bounded by the number of agents (50 in this design, `docs/design/00-overview.md:13`), not by 200 AI calls; and the natural throttle is a per-process cap on concurrent transcription sessions that degrades to `state: STOPPED` rather than to a failed call. |

---

## 15. Reuse map

| Need | Reuse | Path | Boundary |
|---|---|---|---|
| WebSocket **client** with ping/deadlines/single-writer | `provider.transport` | `internal/provider/transport.go:93-205` | Not exported. **Copy the shape, do not export it** — exporting would put a transport type on `internal/provider`'s public surface, which A6 argues against. ~110 lines. |
| Watchdog written against state, not signals | `Realtime.watchdog` | `internal/provider/realtime.go:637-709` | Pattern, with the `m5-findings.md:9-35` lesson attached. |
| Reconnect with exponential backoff | `esl.Link.Run` | `internal/esl/link.go:71-121` | 500 ms→30 s pattern; the ASR reconnect wants a much shorter ceiling. |
| Actor with a bounded mailbox, sole-mutator discipline | `telephony.actor` | `internal/telephony/registry.go:70-76,258-304` | Direct structural reuse for the transcript actor. |
| PCM16 ↔ bytes, resampling, pooled buffers | `internal/media` | `format.go:172-181` (`BytesToPCM16`/`PCM16ToBytes`), `resample.go:28,75`, `pool.go:15-25` | Direct. De-interleaving stereo L16 belongs here as a new zero-alloc helper — and must carry a benchmark, per `CLAUDE.md`. |
| SSE fan-out, scoping, ring, resume | `events.Hub` | `internal/events/hub.go:76-137` | Direct, after the G-09 replay-scope fix. |
| Ledger write path + sqlc batch insert | `LedgerStore.InsertTranscript` | `internal/store/ledgerstore.go:276-297`, `internal/store/sql/ledger.sql:47` | Direct; the batch becomes a small periodic flush instead of one call-end write. |
| Transcript row rendering (offset + speaker + body, tool kinds) | `TranscriptLine` | `web/src/routes/_app.admin.cdr.$callId.tsx:161-198` | Extract into the shared component; the CDR page becomes its first consumer. |
| A scrolling card with an `aside` slot | `Card` | `web/src/routes/_app.agent.index.tsx:601-627` | Direct; the status indicator goes in `aside`. |
| Frontend test harness: fetch stub, recorded requests, fixtures, router+i18n providers | `web/src/test/harness.tsx` | `installBackend():98`, `renderPage():139`, `callFixture():66` | Direct; extend `installBackend` with the transcript route and an SSE stub. |
| Load harness: a real Realtime **server** and a UAC generator | `internal/mockprovider`, `internal/loadgen` | `internal/mockprovider/server.go:81`, `internal/loadgen/uac.go` | The mock provider is a WS server reached by endpoint override (`docs/design/06-capacity.md:71`) — the same trick works for a mock ASR endpoint and a mock `mod_audio_stream` client. |
| Config loader, `.env` semantics, validation | `internal/config` | `config.go:94-160` | Direct. |
| Metrics registration in one place | `internal/obs/callmetrics.go` | `:22-114` | Direct; new instruments go here and nowhere else (`CLAUDE.md`). |

---

## 16. Test plan

### Layer 1 — unit (Go, always `-race`)

- `internal/transcribe`: table-driven wire-event decoding for **both** dialects,
  including Qwen's `…input_audio_transcription.text` with `text`+`stash` folding —
  mirroring `TestEventMapping`-style tables at `internal/provider/realtime_test.go:475`.
- `internal/transcribe`: `session.update` payload assembly per profile, and the
  one-shot reduced retry on a rejected field (the existing test at
  `realtime_test.go:356-392` is the template).
- Transcript actor: `seq` is dense and monotonic across two producers; a duplicate
  `utteranceId` is idempotent; partials never reach the store; the ordering survives a
  producer that posts out of order.
- `internal/media`: stereo L16 de-interleave — correctness **and** `0 allocs/op`
  (`go test -run XXX -bench . -benchmem ./internal/media/`, the gate in `CLAUDE.md`).
- `internal/telephony/adapter`: the two new command strings, asserted verbatim
  (`adapter_test.go` pattern) — **including the rate literal**: `openairt` must produce
  `… stereo 24000 …` and `dashscope` `… stereo 16000 …`. `[MEASURED, B.1]` `24k` is
  rejected by the module with a bare `-ERR`, so the assertion is on the exact integer, and
  the command is built from an int rather than a rate string.
- `internal/telephony/coordinator`: the stream is started with the **agent leg's**
  channel id and never the caller's or the bot's — assert on a bridge whose two channels
  are distinguishable, and again after a RONA re-offer (second agent leg → second start,
  carrying the second `agentId`).
- Ingest token: mint/verify, expiry, tamper, replay after use.
- `internal/transcribe` — **one table-driven suite run against both clients** (D20), so
  the substitutability the seam claims is asserted rather than assumed: the same recorded
  frame script in, the same `PARTIAL`/`FINAL` sequence out.
- `internal/transcribe/dashscope`: a `result-generated` whose `sentence.text` is
  cumulative yields `PARTIAL` **replacing** the previous text, never appending — the
  failure mode that produces `您您好您好…` on screen and no error anywhere (§8.3
  consequence 3). The mirror test on `openairt` asserts deltas *do* accumulate.
- `internal/transcribe/dashscope`: `heartbeat:true` / `sentence_id:0` results are dropped
  before the seam; `task-failed` surfaces as `ERROR` **and** marks the connection unusable
  rather than retrying on it.
- `internal/transcribe/dashscope`: **an empty final is dropped at the client, and the
  assertion is that no `seq` was allocated** — not merely that no row was written.
  `[MEASURED, B.3c]` leading silence produces a real `sentence_end:true` carrying
  `text:""`. **Why the stronger assertion:** `seq` is the ordering base (D2) *and* the
  cursor the `?sinceSeq=` backfill contract keys off (D8). A `seq` burned on silence
  leaves a permanent hole between the snapshot and the tail, and **nothing errors** — the
  panel just loses or duplicates a line at the seam, which is the exact failure §12.3 is
  built to prevent. A test that only checks the row count would pass while the defect
  ships. So: assert the actor's counter is unchanged, and place the drop **before** the
  transcript actor, since the actor is what allocates.
- `internal/events`: **G-09 regression** — an event published with
  `Scope{AgentIDs:[a]}` is not replayed to agent `b` on resume. This test would fail
  today.

### Layer 2 — integration (Go, mock provider + mock WS, no external services)

- A mock ASR server (`internal/mockprovider`'s shape) plus a mock `mod_audio_stream`
  client that dials our ingest listener, sends metadata then stereo L16.
- End to end in-process: bot finals + two ASR streams → one ordered transcript →
  `CALL_TRANSCRIPT` on the hub → rows in a scratch database.
- Handoff race: bot posts its last final while `BOT_PHASE_ENDED` is in flight; assert
  no loss, no duplicate, correct order.
- ASR socket dropped and reconnected mid-utterance: assert `DEGRADED` then `LIVE`, a
  gap marker, and no duplicated final.
- Backpressure: feed audio at 4× real time with a stalled ASR; assert the ring drops
  oldest, the counter rises, and the reader never blocks.
- `AICC_TRANSCRIPTION_ENABLED=false`: no listener bound, no adapter command issued, no
  new event type published.

### Layer 3 — frontend component tests (Vitest + RTL, SIP layer mocked)

Using `web/src/test/harness.tsx` (`installBackend():98`, `renderPage():139`):

- Out-of-order arrival: deliver `seq` 3, 1, 2 → rendered order is 1, 2, 3.
- A partial is replaced in place by its final (same `utteranceId`), and the row count
  does not grow.
- Reconnect backfill: buffer live lines, then resolve the snapshot, assert **no
  duplicate and no missing** line across the seam (the §12.3 contract).
- Agent joins mid-call: snapshot contains the whole bot phase; it renders above the
  first live line, in order.
- Speaker labels: `You` only when `agentId === myAgentId`; the same fixture rendered in
  the supervisor viewpoint shows the display name.
- Status machine: each of the seven states renders its own key; `ERROR` when the stream
  status is `offline`.
- Auto-scroll: sticky at the bottom; a user scroll stops following and reveals "jump to
  latest"; clicking it resumes.
- Long list: 1 000 lines render and the "load earlier" control appears.
- **Softphone regression assertions (mandatory):** re-run the existing suites unchanged
  — `web/src/components/softphone-bar.test.tsx` (23 cases: sign in/ready/not-ready/sign
  out, answer, hold, retrieve, transfer, hang up, mute/unmute, dial, DTMF) and
  `web/src/routes/_app.agent.index.test.tsx` (the control grid must still contain
  exactly six buttons with only `Conference` disabled, `:37-58`). Add one new case: with
  the transcript endpoint returning 500 and the transcript listener throwing, **every**
  softphone assertion still passes.

### Layer 4 — live-call verification (Chrome with the web-sip-phone extension)

Script, run against the dev switch with a real provider:

1. Dial 95001. Bot answers. Say three things; confirm each appears as `Customer`, and
   each bot reply as `Bot`, live in the supervisor view.
2. Ask for a person; the bot calls `transfer_to_agent`.
3. Agent (signed in on extension 1000, Chrome + web-sip-phone) answers the offered call.
4. **Check A — backfill:** the whole bot phase is already on the agent's screen the
   moment the call connects, in order, with no duplicates.
5. Both parties speak four turns each, alternating, with one deliberate overlap.
6. **Check B — attribution, and the stereo assignment D4 rests on:** run the first minute
   with each party saying only its own distinctive phrase, so a swap is unmissable. The
   customer's lines must be `Customer`, the agent's own `You`, none swapped, and the
   overlap must produce two lines rather than one merged line. `[ASSUMPTION]` This is the
   check that confirms left = the agent's microphone and right = the customer on a bug
   attached to the agent's leg (Appendix B.1). If it fails, the fix is one constant, but
   nothing else in the transcript can be trusted until it is made.
7. **Check C — ordering:** `seq` is dense and increasing; the rendered order matches
   the order heard on the recording.
8. Agent presses F5 to reload the page mid-conversation.
9. **Check D — resume:** after reload the panel shows exactly the same lines as before,
   in the same order, with nothing lost and nothing repeated; the conversation continues
   and new lines append.
10. Speak two more turns; hang up from the customer side.
11. **Check E — freeze and durability:** the panel freezes in `ENDED` state; after the
    CDR is written, `/admin/cdr/{callId}` shows the identical transcript, including the
    bot phase and both human speakers.
12. **Check F — no regression:** during the whole call, mute, hold, retrieve, DTMF and
    transfer all behave as they did before this feature existed.

Also run once with `AICC_TRANSCRIPTION_ENABLED=false` to confirm the product is exactly
M5 with one card absent.

---

## 17. Open Decisions (every `PROVISIONAL` in one place)

Each row: the recommendation, why, and what it would cost to overturn it.

**D0 — the reach of A6. ~~`PROVISIONAL`~~ → SETTLED by owner directive (2026-08-16):
A6 constrains the conversational path only.**

`[FACT]` A6 as written binds the whole repository: the cascade "never enters this
application", "no ASR/LLM/TTS types, interfaces, adapters, placeholders or TODOs", and
"a 'cascade-shaped' symbol anywhere in the tree is drift by definition"
(`docs/phase1-decisions.md:36`).

**The ruling.** A6's subject is the *conversational* path — the engine that answers the
phone and talks to a caller. Composing ASR + LLM + TTS into a synthetic agent stays out
of this application permanently and arrives, if ever, as a separate OpenAI Realtime
Gateway reached through one more value of `AICC_PROVIDER`. Standalone transcription of a
human-to-human conversation is not that: it synthesizes nothing, drives no conversation,
holds no dialogue state, and has no LLM and no TTS anywhere near it. It is therefore
outside A6's reach and may live in this repository.

**What still stands, unchanged.** Every other clause of A6, and in particular:

- `internal/provider` remains one client for one wire protocol and never gains a
  recognition concept — its own doc comment says so (`internal/provider/session.go:14-15`)
  and M4 invariant #11 verified it (`m4-cleanup-findings.md:46`). This is exactly why
  §8.3 puts the ASR client in a **separate** package rather than widening that one.
- `VoiceSession` stays a seam for the call actor and its test double, not a plug-in
  point (`session.go:35-36`).
- No cascade is assembled anywhere: `internal/transcribe` is recognition and nothing
  else — no language model, no synthesis, no chaining of the three.
- The phase-2 gateway strategy is untouched.

**A test a reviewer can apply**, so the boundary does not erode: *does the component
produce speech, or decide what to say?* If yes, it belongs behind the Realtime protocol
in a gateway and A6 forbids it here. If it only turns audio into text that a person
reads, A6 does not reach it.

**One neighbouring decision this ruling does not settle.** `[FACT]` R2 says "**No
human-leg ASR in phase 1**" (`phase1-decisions.md:53`). That is a scope statement about
phase 1's feature set, not a prohibition — and this capability is proposed as **M6**,
after the phase-1 milestones close (D7). So R2 needs no amendment; it needs the
milestone framing to say plainly that M6 crosses the line R2 drew, which D7 now does.
Worth recording rather than assuming, because R2 and A6 read as one prohibition when
skimmed and are two different kinds of statement.

**Consequences now firm.** G-02 loses its governance half (the code half remains real
work); §8.3's package boundary is no longer a hedge against a ruling that might have gone
the other way but the direct expression of it; and the alternative design — a sidecar
service owning the ingest, the ASR clients and its own store — is off the table, which
matters because it was the one option that split the transcript's ordering across two
processes, the single property §8.4 shows must stay single-threaded.

**D1 — single source vs dual source. ~~`PROVISIONAL`~~ → SETTLED by owner directive
(2026-08-16): dual (hybrid), and `uuid_audio_stream` runs *only* when a human agent is
on the call.**
Bot phase keeps the realtime model's own transcripts; human phase uses
`mod_audio_stream` + ASR **on the agent's own leg** (D4), started when that leg bridges
and stopped when it unbridges or hangs up (§8.1).
*Why:* the bot's audio is already in this process and the model's output transcript is
its own words verbatim — no ASR error is possible — at zero extra socket, zero extra
spend and zero switch cost. Forking the bot leg's audio back through FreeSWITCH would
contradict the still-valid part of `docs/design/06-capacity.md:32` and add a second WS
hop per AI call, the exact cost that decision avoided.
*What the directive removes from the design:* the unified single-source option (one
stream from answer to hangup) is no longer a live alternative, so its migration path is
not carried as an open branch. The `source` column in §9 stays anyway — it is what makes
a line's provenance auditable when two engines write into one ordered stream, which is
the property the hybrid actually needs.
*Consequences elsewhere in this document, now firm rather than recommended:* MOH is
never transcribed (§8.1); a bot-contained or abandoned call opens no WebSocket and no
ASR session at all; the per-call cost of this feature is bounded by agent headcount, not
by call volume (§14); and the bot-phase transcript reaches the agent only through the
REST backfill of D8, because during that phase there is no stream and no agent-scoped
recipient.

**D2 — ordering and time base. `PROVISIONAL`.**
`seq` = per-call, dense, from 1, allocated by the transcript actor at the moment a final
is accepted; `occurredAt` = server receipt; `offsetMs` = `occurredAt − Call.AnsweredAt()`.
*Why:* the global SSE `seq` cannot serve as the cursor because its gaps are meaningless
by design (`internal/events/seq.go:11-14`), and no engine gives us a **shared** clock.
*Corrected 2026-08-17:* the first draft justified this with "neither engine returns
timestamps", which `[MEASURED, B.3b]` is false — the DashScope client returns
`begin_time`/`end_time` per sentence and per word. **The decision is unchanged, but the
reason is a better one:** vendor timestamps are per-stream, measured from that stream's
own start, and we are merging *two* streams (agent, customer) plus a *third* source (the
bot phase, which has no ASR stream at all). Three clocks with no common origin cannot
order a transcript; our receipt order can. OpenAI still returns nothing `[MEASURED, B.2]`,
so a design resting on vendor time would also stop working the moment the deployment
switched provider.
*Where the vendor timestamps do earn their place:* as optional per-line enrichment
(`startedAtMs`/`endedAtMs`), present on one client and absent on the other — never as the
ordering mechanism, or the two clients stop being substitutable (D20).
*Cost to overturn:* an engine emitting timestamps on **both** providers, plus a way to
align two independent stream clocks to the call's answer time — a different product on one
side and an unsolved problem on the other.

**D3 — partials are **not** persisted. `PROVISIONAL`.**
*Why:* a partial is a guess that will be contradicted within a second; storing it makes
the ledger the only record in the system that contains text nobody said. Idempotency key
is `(call_id, utterance_id)`; a final is an insert, never an update.
*Cost to overturn:* a `revision int` column and an upsert path; the unique index in §9
already anticipates it.

**D4 — one stereo stream on the *human agent's* leg, de-interleaved in Go.
~~`PROVISIONAL`~~ → SETTLED by owner directive (2026-08-16): the media bug goes on the
human agent leg — not the caller's leg, not the bot's.**
*Why:* the agent leg's lifetime is exactly the human phase, so "no bug outside the human
phase" stops being a timing rule and becomes a property of the object; and the leg
carries `Party.AgentID` (`coordinator.addParty():231-233`), so every line has a known
speaker instead of an inferred one. On a re-offer or an agent-to-agent transfer each leg
brings its own bug and its own `agentId`, which one stream on the caller's channel could
not represent — its right channel would change person silently. The caller's channel also
stays free of a second media consumer, and it is the leg that already carries the
recording and every transfer.
`[MEASURED, Appendix B.3]` Qwen's realtime ASR accepts mono only, so *some* channel split
is required regardless — doing it in Go costs one pass over a PCM16 buffer and removes a
second media bug from the switch.
*Channel assignment on the agent's leg:* left = read stream = **the agent's microphone**;
right = write stream = **the customer**. `[MEASURED, Appendix B.1]` — no longer an
assumption: a two-tone loopback call captured left = the peer's tone and right = the
channel's own, on both legs of the pair, and the source shows `stereo` as
`SMBF_READ_STREAM|SMBF_WRITE_STREAM|SMBF_STEREO` (`mod_audio_stream.c:190-203`).
*What this costs:* a start and a stop per delivery attempt — and `[MEASURED]` the stop is
optional, because the module tears the bug down itself when the channel closes. It loses
no audio — between two agent legs the caller is in the queue on MOH — and it opens
nothing at all for a RONA leg that is never answered, because the trigger is the bridge,
not the ring.
*Residual risk, still unverified:* that a bug attached at `CHANNEL_BRIDGE` survives the
bridge's own set-up rather than being torn down with it. `[MEASURED]` mitigates this
considerably — a bug *does* survive a `uuid_transfer` of its own channel and its write
stream follows the new bridge — but attaching during bridge set-up is a different moment
and stays on the Layer 4 checklist. If it turns out not to survive, attach on the agent
leg's `CHANNEL_ANSWER` and discard output until the bridge.
*Cost to overturn:* moving to the caller's leg buys UUID stability the design no longer
needs (D1 already bounds the stream to the bridge) and pays for it in attribution — every
utterance would need a "who is bridged now" lookup, and an agent-to-agent transfer would
produce one stream with two speakers on one channel.

**D5 — WS server in the Go monolith, on its own listener, HMAC-token auth, **plaintext
`ws://`**. ~~`PROVISIONAL`~~ → SETTLED by owner directive (2026-08-17): no TLS on the
ingest; FreeSWITCH → Go is a LAN link.**
*Why the listener:* four evidenced reasons in §8.2 (cookie+CSRF, the 30 s timeout, the
existing separate-listener precedent, and keeping a binary path off the audited/otel'd
surface). Identity rides the metadata frame; authentication must ride the URL/headers
because the frame arrives after the handshake.
*The transport ruling, and its four conditions* (§8.2): bind the listener to the private
interface — **default `127.0.0.1:8090`, never `:8090`**, matching what `AICC_METRICS_ADDR`
actually does (`config.go:100`); take the scheme from `AICC_STREAM_PUBLIC_URL` and never
compose `ws://` in code; keep the HMAC token, in the URL, in the clear — it is single-use
and 60 s-lived, so it authenticates even where it cannot hide; and leave `-DUSE_TLS=ON`
unbuilt and unmeasured (B.4), **deferred, not a blocker for M6.1**.
*What this deliberately accepts:* anyone already on the LAN segment can read call audio off
this socket. That is a bounded, stated risk on a trusted link, and the narrow bind is the
control that bounds it — which is why rule 1 is not a nicety. `[INFERENCE]` The trigger to
revisit is precise and worth writing down: **the first deployment that puts the switch and
the app on different hosts.** At that point the module needs a TLS build and B.4 stops
being deferrable.
*Cost to overturn (a separate process):* a second deployable, a second config surface,
and the transcript ordering split across processes — see D0.

**D6 — new package `internal/transcribe`, separate from `internal/provider`.
`PROVISIONAL`.** *(Its "one client × `Profile`" half is superseded by D20 — see there.)*
*Why the separate package:* `internal/provider`'s own doc comment forbids the concept
(`session.go:14-15,35-36`), so the ASR client cannot live there whatever its internal
shape.
*What changed:* the first draft justified the *internal* shape with "the two ASR protocols
share one event grammar". `[MEASURED, B.3b]` They do not, and D20 replaces that half with
one interface over two protocol clients. The package boundary — the part D6 is actually
about — is unaffected, and if anything is now better supported: two protocol clients are
even less welcome inside a package whose contract is one client for one protocol.
*Cost to overturn (folding it into `internal/provider`):* it breaks M4 invariant #11 and
the A6 argument in one move; not recommended under any reading.

**D7 — milestone and document number. ~~`PROVISIONAL`~~ → SETTLED by owner directive
(2026-08-17), as recommended, with M6.4 kept as its own slice.**
This is **M6**, after M5, not part of it. `[FACT]` M5 is packaging and is complete
(`docs/design/00-overview.md:95`), and R2 explicitly deferred human-leg ASR out of
phase 1 (`phase1-decisions.md:53`); a capability that needs a new FreeSWITCH module, a
new listener, a new provider client, a migration and a contract break is not a
packaging task. Split: **M6.1** module + ingest + ASR clients (no UI); **M6.2** transcript
actor + persistence + SSE + REST; **M6.3** the cockpit panel and the CDR-view rename;
**M6.4** live verification + findings record.

**M6.4 is a slice, not an appendix to M6.3** (owner directive). `[FACT]` This is how the
milestones that produced trustworthy results were run: `m0-findings.md` and
`m4-cleanup-findings.md` exist as their own records and are authoritative enough that
`CLAUDE.md` instructs readers to check them *before trusting a doc's original claim*. A
verification pass folded into the slice that wrote the code has an obvious conflict —
it is scoped by what was built rather than by what was claimed — and it is the first
thing dropped when the slice runs long. `[INFERENCE]` It is also where this feature's
remaining `[ASSUMPTION]`s go to be settled: the `CHANNEL_BRIDGE` attach-survival question
(D4), recognition quality on real G.711-originated audio (B.3b), and the module's
backpressure behaviour (B.4). None of those can be answered by the slice that implements
them.
On numbering: ~~this file's `06-` collides with `06-capacity.md`~~ → **done at first
landing** (owner directive, 2026-08-17). The file is `docs/design/08-transcription.md`; it
was never committed under the colliding number, so no rename has to propagate. ✗ Still
outstanding: `00-overview.md`'s reading order and §6 milestone list should gain both this
document and M6 — a separate edit, not folded into the commit that adds this file.

**D8 — backfill and resume contract. `PROVISIONAL`.**
`GET /calls/{callId}/transcript?sinceSeq=&limit=`, mounted in the agent call-control
group under the existing involvement check, plus the four-step subscribe→snapshot→
merge→tail rule in §12.3.
*Why:* subscribing first can only duplicate, and duplicates are removable because `seq`
is dense; snapshotting first can lose, and loss is not.
*Cost to overturn:* none available — every alternative that fetches before subscribing
has a hole.

**D9 — partials **are** pushed to SSE (D3 says they are not stored). `PROVISIONAL`,
default-on via `AICC_TRANSCRIBE_PARTIALS`.**
*Why:* the difference between a panel that feels live and one that lurches once every
five seconds is entirely partials. The asymmetry with D3 is deliberate and is the whole
point of separating the two questions: the wire is allowed to carry a guess, the ledger
is not. Consequence, stated plainly: refreshing mid-utterance loses the in-flight
partial until the next one — self-healing within a second.
*Cost to overturn:* one flag.

**D10 — speaker→label mapping lives in the component layer. `PROVISIONAL`.**
*Why:* `[FACT]` the hub stores one copy of an envelope in its ring and replays that same
copy to every subscriber (`internal/events/hub.go:172-184,131-135`). A payload whose text
depends on the reader cannot exist. The payload carries `speaker` + `agentId`; the
component decides `You` vs a name.
*Cost to overturn:* per-subscriber payload rendering, which means abandoning the shared
ring — a rewrite of the SSE core for a label.

**D11 — sensitive content: no redaction in this milestone. ~~`PROVISIONAL`~~ → SETTLED by
owner directive (2026-08-17): no masking now; no deployment target in scope carries a
regulatory requirement that makes it blocking. Recorded as a deferred item with its
sequencing fixed, so it is not rediscovered as a surprise.**
The transcript will contain order numbers, amounts and possibly card or ID numbers.
*The ruling:* role-gate it (already the case), rely on the existing retention
setting, log nothing of the text at Info, and **do not** add display- or storage-layer
masking now.

**The deferred item, written down so the sequencing survives this document:**

> **Deferred — PII redaction across recording *and* transcript.** Not scheduled. When it
> is taken up, the order is fixed and is not an implementation preference: **(1)** a PII
> policy that names what is sensitive, who may see it and how long it is kept; **(2)**
> redaction applied to the call **recording** and the **transcript** together. Doing (2)
> for the transcript alone is the failure mode this decision exists to prevent — the
> recording holds the same words unredacted (`internal/recording/storage.go`,
> `aicc_inbound.lua:93`), so a masked transcript beside an unmasked recording is a
> **downgrade**: it reads as compliance to anyone auditing the screen while the audio sits
> untouched. `[INFERENCE]` Anyone proposing transcript-only masking as "a quick win" is
> proposing that downgrade, whether or not they know it.
*Why:* a regex-based masker on a partial stream is a false promise in both directions —
it leaks what it misses and corrupts what it hits mid-token — and the call recording
already holds the same words unredacted (`internal/recording/storage.go`,
`aicc_inbound.lua:93`), so masking the transcript alone would buy the appearance of
protection rather than protection. The honest sequence is: a PII policy first, then
redaction across recording *and* transcript together.
*Cost to overturn:* a redaction pass in the transcript actor before both the store and
the hub — mechanically easy, and it should stay easy, which is another reason the actor
is a single choke point.

**D12 — retire `BOT_TRANSCRIPT` rather than keep it beside `CALL_TRANSCRIPT`.
~~`PROVISIONAL`~~ → SETTLED by owner directive (2026-08-17): remove it. Let
`make api-breaking` flag it, and record it in the release notes as an intentional
breaking change.**
*Why:* `[FACT]` it is declared, never emitted (`internal/events/event.go:59`, no
publisher), and its `BOT_` scope contradicts `04-api-sse.md:44`'s rule once the same
stream carries human speech. M4 kept it on the principle "do not delete the contract to
match an incomplete implementation" (`m4-cleanup-findings.md:218-226`) — that principle
protects a contract from being trimmed to fit lazy code, not from being superseded by a
better one.
*Owner's reasoning, recorded because it generalises:* keeping both names costs a dead enum
value **plus a note explaining why there are two** — residue that is deleted rather than
softened. Removal is cheapest now, while there is provably no consumer.
*Mechanics, in the spec-first order (`CLAUDE.md`):* remove it from `docs/openapi.json`
first, then `make api-generate`, then delete the Go constant — never the reverse. Landed
that way in M6.1a.

*Correction, `[MEASURED 2026-08-17]`:* this entry predicted that
`make api-breaking BASE=main` would flag the removal. **It does not.** The run reports
`0 error, 2 warning`, and both warnings are for the *added* values
(`response-property-enum-value-added` for `CALL_TRANSCRIPT` and
`CALL_TRANSCRIPTION_STATE`); the removal is not mentioned and the gate exits 0.
`[INFERENCE]` oasdiff models a **response** enum by what a client can *receive*: dropping a
value only means the server sends it no longer, which cannot break a reader, while adding
one can surprise an exhaustive reader. That reasoning holds for a tolerant JSON client and
**not** for this project's own frontend, where the generated TypeScript union is exhaustive
and referencing a removed member is a compile error — which is exactly what
`web/src/lib/events.ts` did, and why that file changed in the same commit.
*The lesson worth keeping:* `make api-breaking` is a floor, not a verdict. A removal that
it passes in silence can still break a typed consumer, so a removal is declared in the
release notes on its own merits rather than because a tool demanded it.
*Cost to overturn:* keep both names, emit only the new one, and carry a dead enum value
forever.

**D13 — rename `TranscriptRole{CALLER,BOT}` to `Speaker{CUSTOMER,BOT,HUMAN_AGENT}`.
~~`PROVISIONAL`~~ → SETTLED by owner directive (2026-08-16): one migration.**
*Why:* the brief fixes `CUSTOMER`, and 07 §6 forbids synonym drift — `CALLER` and
`CUSTOMER` cannot both mean the same person. `role` is additionally overloaded three
ways in this contract (G-19).
*What the ruling buys:* the rename, the new columns of §9 Table 3 and the enum change all
land in **migration 00009 together**, so the breaking contract change is declared once to
`make api-breaking` and the `transcripts` table is rewritten once. Deferring the rename
would have meant two migrations and two breaking changes.

---

## New decisions the owner's answers and the experiments created

**D14 — ~~the Qwen model name in the brief is not reachable~~ → SETTLED (owner, 2026-08-17):
the model is reachable, on a workspace-scoped host and over a *different wire protocol*.
The model name in the brief stands.**

The owner supplied the missing coordinates: the reference is Model Studio's
**Fun-ASR-Realtime WebSocket API**
(`help.aliyun.com/zh/model-studio/fun-asr-realtime-websocket-api`) and a host of the form
`{WorkspaceId}.cn-beijing.maas.aliyuncs.com` — a **workspace-scoped** MaaS host, one
of the three explanations D14 could not distinguish. `[MEASURED, B.3b]` Against
`wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference`,
`qwen-audio-3.0-asr-flash-streaming` connects and transcribes correctly at 16 kHz.
*(The deployment's actual workspace ID is a config value, not a document constant — it
belongs in `AICC_TRANSCRIBE_ENDPOINT`, §13.)*

The correction that matters is not the hostname. **`qwen-audio-3.0-asr-flash-streaming`
does not speak the OpenAI-Realtime dialect at all.** It speaks DashScope's native duplex
protocol — `run-task` / `task-started` / `result-generated` / `finish-task`, a `header` +
`payload` envelope with `task_id`, and raw binary audio frames. The earlier
`close 1007: "Model not found"` was the right answer to the wrong question: the probe was
speaking OpenAI-Realtime at a path that model was never served on.

`qwen3-asr-flash-realtime` — the fallback D14 recommended — *is* an OpenAI-Realtime-dialect
model and does work. So the two are not interchangeable spellings of one engine; they are
two engines on two protocols, and the brief named the one this deployment is provisioned
for. **Ship `qwen-audio-3.0-asr-flash-streaming` on the native protocol**, per the brief.
`AICC_TRANSCRIBE_MODEL` stays as an override; `AICC_TRANSCRIBE_ENDPOINT` must carry the
workspace host, because the workspace ID *is* the hostname here and cannot be a build
constant (§13).

*This falsifies the "one client × profile" premise of §8.3 — see D20.*

**D15 — ~~the OpenAI path needs a 3:2 resampler~~ → CLOSED by measurement (2026-08-17):
there is no Go-side resampler. The tap emits 24 kHz itself.**

> **Superseded text, kept per the B.3/D6 convention.** The first version read: *"`[MEASURED,
> B.1]` `mod_audio_stream` emits 8 k or 16 k only … take **16 k** from the module and write
> a ×3/2 resampler with its benchmark. The cheap alternative — take 8 k and reuse
> `Upsample(…, 3)` — is one line … but on the agent leg the **left** channel is the agent's
> WebRTC microphone, which may genuinely be wideband … Do not spend the agent's audio
> quality to save a resampler."*
>
> **Both options were wrong, because the premise was.** "Emits 8 k or 16 k only" came from
> the module's README. `[FACT]` The source accepts **any multiple of 8000**
> (`mod_audio_stream.c:210-221`), and `[MEASURED, B.1]` the module genuinely emits 24 kHz —
> proven by tone, not by the command succeeding: a 1004 Hz and a 440 Hz tone decoded as
> 24000 Hz read back at exactly 1004.0 and 440.0 Hz, where a mislabelled 16 kHz stream would
> have read ≈1506 and ≈660. Frames are 1920 bytes = 20 ms at 24 kHz stereo.

*The ruling:* **the tap's rate is an attach-time parameter derived from the selected
transcription client** — `24000` for `openairt`, `16000` for `dashscope` — and no
resampling happens in Go at all. FreeSWITCH does it on the switch box with speex at
`SWITCH_RESAMPLE_QUALITY` (`audio_streamer_glue.cpp:484-486`).

*Why this is better than either option it replaces, beyond being less code:*

- `[FACT]` It deletes the whole question from the hot path. No new DSP, no new 0 allocs/op
  benchmark, no `internal/media` change — the frame paths this project guards
  (`CLAUDE.md`) are simply not touched.
- `[INFERENCE]` It resamples **once, from the leg's own native rate**, rather than twice.
  The rejected 8 k option would have decimated a wideband agent mic to 8 k and interpolated
  it back to 24 k; the rejected 16 k option would have resampled 8 k→16 k on the switch and
  16 k→24 k again in Go. Doing it in one step from the source rate is strictly less
  destructive than both, which is the point the superseded text was reaching for and
  solving the expensive way.
- `[INFERENCE]` It moves the cost to the switch, where the audio already lives, instead of
  adding per-frame work to the process that also runs the call actors.

*The one trap, and it is a real one:* `[MEASURED, B.1]` the token must be **`24000`**, not
`24k`. Only `8k` and `16k` have word forms; anything else goes through `atoi` and fails
`% 8000`. `24k` returns a bare `-ERR Operation Failed` and the real reason appears only in
the switch log. `[INFERENCE]` The attach command should therefore be built from an integer,
never from a string like `"24k"`, and the adapter test should assert the literal command
including `24000` — the pattern `internal/telephony/adapter` already uses for verbatim
command strings.

*Residual, on the M6.4 checklist:* ✗ 16000→24000 specifically is not yet measured (8000→24000
and 8000→16000 are). `[INFERENCE]` The module passes the rate pair straight to
`speex_resampler_init` with no special-casing, so the ratio is data rather than code; but it
wants confirming on a real wideband agent leg, which is also where FACT 2 below gets its
answer.

*What was never established, and no longer needs to be:* whether the agent's uplink is
genuinely wideband. That question only mattered as a tiebreak between the two rejected
options — how much of the agent's mic we could afford to spend. With the switch resampling
from whatever the leg natively is, **we spend none of it either way**, so the question
dissolves rather than being answered. It was not put to a live agent call, and no human
action was requested for it.

**D16 — endpointing is a client trait, and on OpenAI it is ours. `PROVISIONAL`.**
`[MEASURED, B.2]` `gpt-live-transcribe` refuses `turn_detection`, and six seconds of
silence produced no final — only `input_audio_buffer.commit` did. `[MEASURED, B.3b]` The
DashScope client segments server-side and needed no client involvement: in a two-utterance
test both finals arrived while audio was still streaming, the first ~8 s before
`finish-task`.
*Recommendation:* the trait moves up one level — with D20 it is a property of the
**client**, not a `Profile` field, since it no longer varies within either protocol.
`openairt` owns endpointing: the ingest runs energy-based silence detection over the PCM16
it already holds (~600 ms of silence → `commit`), and that commit is also where a `seq` is
allocated (§8.4). `dashscope` takes the boundary from `sentence_end:true` and tunes it with
`max_sentence_silence` (default 1300 ms — `[INFERENCE]` worth lowering toward 600–800 ms so
the two clients feel alike on screen, and cheap to change since it is a request parameter).
This is genuinely new work that the documentation did not imply; it is small, but it must
not be discovered during implementation.

**D20 — `internal/transcribe` is one interface with two protocol clients, not one client
× profile. `PROVISIONAL` — but forced by measurement, not by taste.**
`[MEASURED, B.3b]` The shipped Qwen model speaks DashScope's native `run-task` duplex
protocol; OpenAI speaks the Realtime transcription dialect. These are different grammars,
not different values (§8.3).
*Recommendation:* `transcribe.Session` (4 methods) with `transcribe/openairt` and
`transcribe/dashscope` behind it, selected once in `main` by `AICC_TRANSCRIBE_PROVIDER`.
`[FACT]` This does not weaken `CLAUDE.md`'s provider rule — that rule keys the extension
point to **the wire protocol**, so one protocol per client is the rule honoured, and
`internal/provider` stays a single client precisely because all its engines share one
protocol. *The rule to write down so it is not eroded later:* a new **engine** is a new
profile; only a new **protocol** earns a new client, and it needs a measurement like B.3b
to prove it is one.
*Cost of being wrong in the other direction:* if the two were forced into one client, the
union type would carry `task_id`, `sentence_id`, `item_id`, two audio transports and three
partial semantics — the shape `internal/provider` was explicitly designed not to become
(`session.go:35-36`).

**D17 — transcription is switchable in system administration, not only by env var.
~~`PROVISIONAL`~~ → SETTLED by owner directive (2026-08-16): cost is the deployer's
decision, so an administrator must be able to turn it off without a restart.**
*Consequence for §13:* `AICC_TRANSCRIPTION_ENABLED` becomes the *deployment ceiling*
(false = the feature does not exist, no listener bound), and a row in the existing
`settings` table (`00001_foundation.sql:38-42`) is the *operational switch*, editable
from an admin screen and readable per call at stream-start time.
*Two things this drags in that the first draft did not have:* `settings` is currently
inert — `[FACT]` M4 removed `GetSetting`/`UpsertSetting`/`PutSetting` as unused
(`m4-cleanup-findings.md:186-190`), so the accessors must come back — and there is no
settings screen in the SPA, so `/admin` grows one control. Both are small; neither is
free, and neither was in the milestone plan before this ruling.

**D18 — click-to-dial outbound is in scope, which is why the trigger is `CHANNEL_BRIDGE`
and not `bridge-agent-start`. ~~`PROVISIONAL`~~ → SETTLED by owner directive
(2026-08-16).**
`[FACT]` `Service.Dial` originates the agent leg and bridges it straight to the trunk
(`internal/outbound/outbound.go:149-190`) — mod_callcenter is never involved, so
`bridge-agent-start` never fires. Anchoring on `CHANNEL_BRIDGE` where one party carries
`AgentID` covers inbound queue delivery, click-to-dial and direct extension calls with one
rule (§8.1). `bridge-agent-start` is kept only as an idempotent confirmation.

**D19 — no supervisor live transcript this milestone. ~~`PROVISIONAL`~~ → SETTLED by
owner directive (2026-08-16): supervisors read the transcript from the CDR after
hangup.**
*Consequence:* the `CALL_TRANSCRIPT` scope stays as §11 defines it (agents on the call,
plus supervisors, who receive everything by identity — `hub.go:203`), but **no supervisor
screen consumes it**, and none is built. `/supervisor/quality` stays `isReady:false`
(`web/src/lib/nav.ts:49`). The live panel ships for `/agent` only; the same component
renders the finished transcript on `/admin/cdr/$callId`, which supervisors already reach.

---

## Appendix A — module map

| Package | Responsibility | Key types / entry points |
|---|---|---|
| `cmd/aicc` | composition root: config → store+migrate → hub → ESL → registry → agents → coordinator → CDR assembler → outbound → orchestrator → HTTP | `run()` `main.go:110-289`; provider resolved once at `:207` |
| `internal/config` | `AICC_*` env loading, `.env`, validation | `Config` `:20`, `Load()` `:94`, `validate()` `:140` |
| `internal/obs` | slog, OTel, `/metrics`, call metrics | `callmetrics.go:22-114` |
| `internal/store` | pgx pool, goose migrations (embedded), sqlc queries, repositories, advisory lock | `Store` `store.go:29`, `LedgerStore` `ledgerstore.go`, `queries/` |
| `internal/events` | SSE envelope, type registry, global seq (hi/lo), ring, fan-out hub, scoping | `Event` `event.go:86`, `Type` `:22`, `Hub` `hub.go:52`, `Sequence` `seq.go:28` |
| `internal/esl` | ESL inbound client: auth, `event plain`, FIFO api replies, reconnect | `Link` `link.go:23`, `Client`, `Event` |
| `internal/telephony` | the FS boundary: normalization, actor-per-call registry, call/party FSM, command adapter, CDR assembly, registrations | `Normalize` `switchevent.go:173`, `Registry`/`actor` `registry.go:39,70`, `Coordinator` `coordinator.go:37`, `Adapter` `adapter.go:24`, `CDRAssembler` `cdr.go:52`, `Call`/`Party` `call.go:151,100` |
| `internal/agents` | presence FSM, ACW, RONA mirroring, device observation, roster, agent configuration | `Service` `service.go`, `RosterEntry` `:63`, `Presence`, `AgentAtExtension` `:622` |
| `internal/media` | PCM16 frames, G.711 LUTs, resamplers, pools, format conversion (zero-alloc hot path) | `Converter` `format.go:73`, `Downsampler` `resample.go:59`, `GetBytes`/`PutBytes` `pool.go:15` |
| `internal/voice` | SIP UAS on :6060, SDP, RTP/RTCP, jitter buffer, DTMF | `UAS` `uas.go:219`, `Dialog` `uas.go:68`, `Config` `uas.go:19` |
| `internal/provider` | one OpenAI-Realtime client × `Profile`; the s2s seam | `VoiceSession` `session.go:37`, `Realtime` `realtime.go:40`, `Profile` `profile.go:24`, `ProfileFor` `:143`, `transport` `transport.go:93` |
| `internal/flow` | DSL v2: spec, validation, phase engine, tool runtime, built-ins | `Spec` `spec.go`, `Engine` `engine.go:22`, `Runtime` `runtime.go:25`, `Actions` `builtin.go:115` |
| `internal/aicall` | one AI call: leg + model + flow; barge-in, playback gating, transfers, the ledger recorder | `Orchestrator` `orchestrator.go:94`, `Session` `session.go:121`, `Leg` `leg.go:16`, `callActions` `actions.go:25`, `callRecorder` `ledger.go:33` |
| `internal/catalog` | DIDs, queues, extensions; switch sync on write | `Service` `service.go` |
| `internal/outbound` | click-to-dial and AI outbound origination, idempotency, rate limit | `outbound.go`, `limiter.go` |
| `internal/recording` | storage abstraction (FS / S3), key naming, ingest | `Storage` `storage.go:22`, `Key` `:44` |
| `internal/httpapi` | chi router, generated-wrapper handlers, cookie sessions, CSRF, SSE endpoint, SPA | `Server` `server.go:39`, `router()` `:100`, `StreamEvents` `events_handler.go:34` |
| `internal/api` | oapi-codegen output (`DO NOT EDIT`) | `api.gen.go` |
| `internal/seed` | demo dataset and its exact removal | `seed.go`, `fresh.go`, `history.go`, `flows/novanet_support.json` |
| `internal/mockprovider` | a Realtime **server** for load tests, reached via endpoint override | `Server` `server.go:81` |
| `internal/loadgen` | SIP UAC generator carrying the dialplan's `X-AICC-*` headers | `uac.go`, `run.go` |
| `web/` | React 19 SPA; routes under `src/routes`, contract types in `src/generated/api.ts` | `_app.tsx`, `_app.agent.index.tsx`, `lib/events.ts`, `lib/use-event-stream.ts` |
| `freeswitch/` | Lua (`aicc_xml`, `aicc_inbound`, `aicc_queue`) + dialplan XML | `scripts/`, `conf/dialplan/{public,default}/05_aicc.xml` |
| `deploy/` | dev compose, demo compose (PG + stock FS turned into ours at boot + app), Lua role SQL | `demo/docker-compose.yml`, `demo/freeswitch/entrypoint.d/10-aicc.sh` |

---

## Appendix B — external dependencies: the experiment record

**Run on 2026-08-16 against the development switch (FreeSWITCH 1.11.1-release
git c2c5964, macOS/arm64) and against both vendors' live APIs.** No application source
was modified. Scratch programs used: a WebSocket receiver that writes each stereo
channel to its own WAV, and one probe per vendor. Rows below are `[MEASURED]` unless
marked otherwise.

### B.1 `mod_audio_stream` — https://github.com/amigniter/mod_audio_stream

**Build and install — done, with three portability fixes.** The repository at
`~/workspaces/github/mod_audio_stream` builds on this machine after:

1. `git submodule update --init` (`libs/libwsc` ships empty).
2. **`-DCMAKE_BUILD_TYPE=RelWithDebInfo`, not `Release`.** libwsc's Release path runs
   `strip --strip-unneeded` (`libs/libwsc/CMakeLists.txt:100-105`), a GNU flag macOS's
   `strip` rejects, which fails the link.
3. **SpeexDSP must be added by hand.** `CMakeLists.txt:38-43` links
   `PkgConfig::FreeSWITCH pthread libwsc` and *never* links SpeexDSP, though
   `mod_audio_stream.h:5` includes `speex/speex_resampler.h`. On Debian this works by
   accident (default include path, symbols pulled in transitively); on Homebrew it fails
   twice — missing header, then five undefined `speex_resampler_*` symbols. Fixed with
   `-DCMAKE_C_FLAGS=-I/opt/homebrew/include -DCMAKE_CXX_FLAGS=-I/opt/homebrew/include`
   and `-DCMAKE_SHARED_LINKER_FLAGS="-L/opt/homebrew/lib -lspeexdsp"`.

The artifact is `mod_audio_stream.dylib`; FreeSWITCH's module directory holds `.so`
names, so it is installed as `/usr/local/freeswitch/mod/mod_audio_stream.so`.
`<load module="mod_audio_stream"/>` was added to `modules.conf.xml` after
`mod_callcenter` (backup written alongside), and **a full `fsctl shutdown restart
elegant` confirmed it auto-loads**: `module_exists` → `true`, the API registers, and both
softphones re-registered (1001 over WSS, 1007 over UDP).

| Question | Result |
|---|---|
| API syntax | `<uuid> [start\|stop\|send_text\|pause\|resume\|graceful-shutdown] [wss-url\|path] [mono\|mixed\|stereo] [<rate>] [metadata]` — note `graceful-shutdown`, which the README omits |
| **Accepted sample rates — the README is wrong** | The README documents only `8k` and `16k`, and §8.3's first draft repeated it. `[FACT]` `mod_audio_stream.c:210-216` matches the literals `"16k"` and `"8k"`, then falls through to `atoi(argv[4])`, and `:221` accepts **any multiple of 8000**. So the module takes `8000`, `16000`, `24000`, `48000` — as *numbers*. See the next row for the trap. |
| **`24k` fails, `24000` succeeds** `[MEASURED 2026-08-17]` | `uuid_audio_stream <uuid> start <ws-url> stereo 24k` → **`-ERR Operation Failed`**, switch log `mod_audio_stream.c:222 invalid sample rate: 24k`. The same command with **`24000`** → `+OK Success`. `[FACT]` The cause is in the source above: `atoi("24k")` is 24, and `24 % 8000 != 0`. Only `8k`/`16k` have word forms; every other rate must be spelled in full. **This is a silent-looking failure that costs an afternoon** — the command reports a generic error, and the real message is only in the switch log. |
| **24 kHz is genuinely emitted, not merely accepted** `[MEASURED 2026-08-17]` | The decisive test, because a mislabelled stream would still be "accepted": with the two-tone loopback (this leg playing 440 Hz, peer on `9197` milliwatt at 1004 Hz) and the capture **decoded as 24000 Hz**, the tones read back **left = 1004.0 Hz, right = 440.0 Hz** — exact. Had the module labelled the stream 24k while still emitting 16 kHz samples, the same tones would have read ≈1506 Hz and ≈660 Hz. Frame size **1920 bytes** = 480 samples/channel = exactly 20 ms at 24 kHz stereo. Switch log confirms the work is done on the switch: `audio_streamer_glue.cpp:485 resampling from 8000 to 24000`. |
| 16 kHz control, same run | 1280 bytes per frame, tones **1004.0 / 440.0 Hz** decoded as 16000 Hz — so the 24 kHz result is not an artefact of the measurement method |
| **Mix types, from source** | `mono → SMBF_READ_STREAM`; `mixed → +SMBF_WRITE_STREAM`; `stereo → +SMBF_WRITE_STREAM|SMBF_STEREO` (`mod_audio_stream.c:190-203`) |
| **Stereo channel order, measured** | **left = READ, right = WRITE.** A loopback pair was built where one leg played a 440 Hz tone and its peer ran `9197` (milliwatt, 1000 Hz), and both legs were bugged at once. The 440 Hz leg captured **left = 1004 Hz, right = 440 Hz**; its mirror captured **left = 440 Hz, right = 1004 Hz**. Unambiguous. |
| **Auto-stop at channel end** | **Yes.** From source: `SWITCH_ABC_TYPE_CLOSE → stream_session_cleanup(…, channel_closing=1)` (`mod_audio_stream.c:32-38`). Observed: `uuid_kill` produced a clean WebSocket close 1000 on both open streams within milliseconds. **No socket leak, and no explicit stop is required.** |
| **Survives `uuid_transfer` of its own channel** | **Yes.** A bugged channel was transferred mid-stream to `9664` (hold music). The WebSocket stayed open across the transfer (11.1 s total, closing only on `uuid_kill`), and the **right** channel changed from a 442 Hz pure tone to complex audio (zero-crossing spread 6 → 338), while **left** stayed at 1003 Hz. So the bug survives, and the write stream follows the new bridge. |
| Frame shape at `16k stereo` | 1280 bytes per WebSocket binary frame = 320 samples/channel = **exactly 20 ms**, from an 8 kHz L16 channel — the module's internal speex resampler works |
| **Resampler wiring** | `[FACT]` `audio_streamer_glue.cpp:484-486` calls `speex_resampler_init(channels, sampling, desiredSampling, SWITCH_RESAMPLE_QUALITY)` with the channel's own `actual_samples_per_second` as the source (`mod_audio_stream.c:83`), and stereo goes through `speex_resampler_process_interleaved_int` (`:823`). Nothing special-cases a particular rate pair, so the ratio is data, not code. `[MEASURED]` 8000→16000 and 8000→24000 both verified. ✗ **16000→24000 is not yet measured** — a wideband source leg is needed and `absolute_codec_string` does not take on a `loopback/` channel (it stayed L16/8000 with the variable set). On the M6.4 checklist |
| Metadata as the first text frame | Delivered in 5 of 6 attaches, including all three of a deliberately simultaneous 3-way attach. The single miss was the very first connection ever made to a freshly started server and did not reproduce. **Treated as advisory:** identity rides the URL (which authentication needs anyway), and the metadata frame confirms it. |
| Upgrade request | Sends `Origin`, `Upgrade`, `Connection`, `Sec-WebSocket-Key`, `Sec-WebSocket-Version` — **no `Authorization` unless `STREAM_EXTRA_HEADERS` is set**, and no subprotocol |
| One bug per channel | Enforced: "bug already attached!" (`mod_audio_stream.c:70-73`). A second consumer of the same leg is impossible |
| Requires pre-answer | `switch_channel_pre_answer` must succeed (`mod_audio_stream.c:74-78`) — satisfied at bridge time |
| TLS / wss | Build-time `-DUSE_TLS=ON` (not enabled here; dev is HTTP-only by mandate) `[not tested]` |
| Backpressure | Still undocumented and untested; §14 bounds it on our side regardless `[not tested]` |

Environment facts unchanged: `[FACT]` the demo image is
`dheaps/freeswitch@sha256:06798d…` (1.10.12), enables only `mod_callcenter`, `mod_lua`,
`mod_pgsql` (`deploy/demo/docker-compose.yml:115`,
`deploy/demo/freeswitch/entrypoint.d/10-aicc.sh:52-63`), and is amd64-only
(`m5-findings.md:180`). **The demo stack still needs this module added** (G-15); only the
development switch is done.

**Switch-side changes made for these experiments, so they reproduce.** Exactly one, and it
predates this pass: `autoload_configs/modules.conf.xml` gained
`<load module="mod_audio_stream"/>` after the `mod_callcenter` line, with a timestamped
`.bak-` backup beside it, plus the built `mod_audio_stream.so` in the module directory.
**The D15 measurements required no further configuration** — no dialplan edit, no profile
change: the rate is a runtime argument to `uuid_audio_stream`, and the two-tone rig reuses
the stock `9197` (milliwatt, `tone_stream://%(251,0,1004)`) already present in
`dialplan/default.xml`. `[INFERENCE]` For the demo image that means G-15 is still exactly
two things — ship the `.so`, add the one `<load>` line — and **nothing about 24 kHz
changes the packaging**, because the module needs no rate configuration at build or load
time. What the demo build must not lose is the SpeexDSP link (B.1's third fix): the
resampler is what makes 24 kHz work at all, and on Debian it links by accident rather than
by declaration.

### B.2 OpenAI Realtime transcription

Read: `developers.openai.com/api/docs/guides/realtime-transcription` and
`…/guides/realtime-conversations`.

- Dedicated transcription session: `session.update` with `session.type:"transcription"`,
  `audio.input.format {type:"audio/pcm", rate:24000}`,
  `audio.input.transcription.model` — recommended model `gpt-live-transcribe`.
- Events: `conversation.item.input_audio_transcription.delta` (field `delta`) and
  `.completed` (field `transcript`). **"Ordering between completion events from
  different speech turns isn't guaranteed"** — reconcile on `item_id`.
- Turn taking: `turn_detection: null` + explicit `input_audio_buffer.commit`, or server
  VAD.
- Hints: `prompt`, `keywords[]`, `languages[]` (plural — `gpt-live-transcribe` does not
  take the singular `language`). Latency knob `delay` ∈
  `minimal|low|medium|high|xhigh`.
- **Checked as the brief asked:** the guide states explicitly that `gpt-live-transcribe`
  **does not return word-level timestamps, speaker labels, or transcription confidence
  scores**. This confirms the brief's premise and is the basis of D2.
- G.711 for a *transcription* session is **not documented** (`audio/pcm` only). ✗ —
  must be tested. Note this differs from the *conversation* session, where this project
  has M0-verified `audio/pcmu` and `audio/pcma` in both directions
  (`docs/design/00-overview.md:101`).
- Input transcription in a *conversation* session is configured, not automatic — the
  basis of §4.2 and G-06.

**Measured on 2026-08-16**, feeding 5.82 s of English speech captured through
`mod_audio_stream` at 16 kHz from FreeSWITCH's own `ivr-*` prompts:

| Attempt | Result |
|---|---|
| `audio/pcm` @ **16000**, `server_vad` | **Rejected:** `integer_below_min_value` — *"Invalid 'session.audio.input.format.rate': … Expected a value >= 24000, but got 16000 instead."* And because the update failed, `transcription` stayed `null`: VAD events arrived (`speech_started/stopped`, `committed`, `conversation.item.added/done`) but **not one transcript** — which independently confirms §4.2/G-06, that input transcription is off unless configured |
| `audio/pcm` @ **24000**, `server_vad` | **Rejected:** *"Turn detection is not supported for this transcription model."* |
| `audio/pcm` @ **24000**, `turn_detection: null` | **Accepted.** `session.updated` echoed `{model: gpt-live-transcribe, language: null, languages: null, prompt: null}`, `turn_detection: null`, `rate: 24000`. Deltas arrived word by word under one stable `item_id`: `" And"`, `" the"`, `" reason"`, … |
| …then **6 s of trailing silence, no commit** | **No final.** Silence alone does not close an utterance |
| …then `input_audio_buffer.commit` | **Final arrives:** `"And the reason for your call, speak to a customer service representative, please hold while your party is being contacted."` — a punctuated, corrected rewrite, not the concatenation of the deltas (the deltas ended mid-phrase at `" party is"`) |

The audio came from `ivr-please_state_your_name_and_reason_for_calling`,
`ivr-speak_to_a_customer_service_representative` and
`ivr-please_hold_while_party_contacted`, so the transcript is verifiably correct.

### B.3 Qwen / Alibaba Model Studio realtime ASR — the **OpenAI-dialect** interface

> **Read B.3b first.** This section documents Model Studio's *OpenAI-Realtime-compatible*
> ASR interface, which is what this pass found unaided. It is **not** the interface the
> shipped model uses — that one is in B.3b. B.3 stays because
> `qwen3-asr-flash-realtime` genuinely lives here, and because the D14 correction is only
> legible next to the result it corrects.

Read: Model Studio's `qwen-asr-realtime-interaction-process`,
`qwen-asr-realtime-client-events`, `qwen-asr-realtime-server-events`, and the ASR model
list.

- Endpoint `wss://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime?model=…`
  (also an `ap-southeast-1` host); `Authorization: Bearer <key>`; auth at handshake,
  401/403 on failure.
- `session.update` is **flat**: `session.{input_audio_format, sample_rate,
  input_audio_transcription{language, corpus.text}, turn_detection}`.
  `input_audio_format` ∈ {`pcm`, `opus`}; `sample_rate` ∈ {16000, 8000} (8 k is
  upsampled server-side). `turn_detection` = `{type:"server_vad", threshold (default
  0.2, range −1..1), silence_duration_ms (default 800, range 200..6000)}`, or `null` for
  manual commit.
- Server events: `session.created`, `session.updated`,
  `input_audio_buffer.speech_started` (`audio_start_ms`, `item_id`),
  `…speech_stopped` (`audio_end_ms`, `item_id`), `input_audio_buffer.committed`,
  `conversation.item.created`,
  **`conversation.item.input_audio_transcription.text`** (interim; `text` = confirmed
  prefix, `stash` = revisable suffix, concatenate for the preview),
  `…completed` (`transcript`, `language`, `emotion`), `…failed`, `error`,
  `session.finished`.
- Model `qwen-audio-3.0-asr-flash-streaming`: WebSocket, **mono only**, hot words +
  prompt context supported, speaker diarization and emotion **unsupported**, unlimited
  duration. ✗ — the model-list page describes the *model*; it does not say which
  interface serves it, which is the trap this pass fell into. `qwen3-asr-flash-realtime`,
  the one that does live here, documents no timestamps and returned none `[MEASURED]`.
- **Which of the known Qwen s2s quirks carry over to the ASR interface** — the brief
  asks this explicitly:
  - *Empty conversation rejects `response.create`*: **does not apply.** There is no
    `response.create` in the ASR protocol; the transcription session generates nothing.
  - *`server_vad@500ms` default*: the ASR interface documents `silence_duration_ms`
    default **800**, range 200–6000, with a low-latency preset of 400 + threshold 0.0.
    So the same knob exists with a different default. `[ASSUMPTION]`
  - *`smart_turn` forces 2000 ms and ignores `silence_duration_ms`*: **not applicable** —
    the ASR interface documents only `server_vad`.
  - *`turn_detection` freezes after the first audio frame*: **not documented either
    way** for the ASR interface. Treat as if it does (assemble the full config before
    the first `append`), which is what `internal/provider` already does
    (`docs/design/02-ai-voice.md:63`). ✗
  - *Audio-format fields are never echoed in `session.updated`*: the ASR interface's
    `session.created`/`session.updated` **do** echo `input_audio_format`. So the
    "cannot confirm acceptance from the handshake" rule may not apply here. ✗ — verify
    before relying on it.
- G.711 / mulaw is **not listed** as an accepted encoding for any realtime model. ✗ —
  moot for this design: `mod_audio_stream` delivers L16 regardless.

**Measured on 2026-08-16**, same 5.82 s of speech, at **16 kHz — no resampling**:

| Attempt | Result |
|---|---|
| model `qwen-audio-3.0-asr-flash-streaming` | **`close 1007: "Model not found"`** on `wss://dashscope.aliyuncs.com/api-ws/v1/realtime` with this account's `ALIYUN_API_KEY`. Note `session.created` arrives *first*, carrying s2s defaults, and the failure follows — so a naive client would think it had connected. **✗ SUPERSEDED by B.3b:** the model exists; it is simply not served on this path or in this dialect. The result was real, the inference drawn from it was wrong |
| model `qwen-asr-realtime` | "Model not found" |
| model `paraformer-realtime-v2` | "Model not found" |
| model `fun-asr-realtime` | reached the model but rejected the payload: "format is empty" |
| **model `qwen3-asr-flash-realtime`** | **Works.** `session.updated` echoed `{model: qwen3-asr-flash-realtime, language: "en", sample_rate: 16000, input_audio_format: "pcm", turn_detection: {type: server_vad, threshold: 0.2, silence_duration_ms: 800}}` |
| …with `server_vad`, **no client commit** | **Full server-side segmentation:** `input_audio_buffer.speech_started` → `conversation.item.created` → `speech_stopped` → `committed` → `completed`, unprompted. Final: `"Name and the reason for your call. Speak to a customer service representative. Please hold while your party is being contacted."` — slightly more complete than OpenAI's on the same audio |
| Partial shape | Confirmed as documented: `text` = confirmed prefix, `stash` = revisable suffix — observed `text="" stash="And"`, then `stash="And."`, then `stash="And the"` |

- **The s2s quirk that does *not* carry over:** the ASR interface's `session.updated`
  **echoes the audio configuration truthfully** (rate and language both came back), unlike
  the Qwen s2s path where the echo confirms nothing (`docs/design/02-ai-voice.md:63`). The
  handshake can be trusted here.

### B.3b `qwen-audio-3.0-asr-flash-streaming` — the native DashScope protocol

**Measured 2026-08-17**, after the owner supplied the reference the first pass was
missing: `help.aliyun.com/zh/model-studio/fun-asr-realtime-websocket-api` (Fun-ASR-Realtime
/ Qwen-Audio-3.0-ASR-Flash-Streaming) and this deployment's workspace-scoped host, of the
form `{WorkspaceId}.cn-beijing.maas.aliyuncs.com` (the ID itself is configuration, §13).

**This is a different protocol from B.3, not a different endpoint for the same one.**
That is why B.3's probe got "Model not found": it spoke OpenAI-Realtime at
`/api-ws/v1/realtime`, and this model is served at `/api-ws/v1/inference` speaking
DashScope's native duplex protocol. The negative result in B.3 was correct and its
conclusion was wrong.

Contract, from the client-events and server-events pages:

- `wss://{WorkspaceId}.{cn-beijing|ap-southeast-1}.maas.aliyuncs.com/api-ws/v1/inference`;
  `Authorization: Bearer`; optional `X-DashScope-WorkSpace`, `user-agent`,
  `X-DashScope-DataInspection`. Legacy `dashscope.aliyuncs.com` still works but the doc
  recommends migrating off it.
- Flow: `run-task` → `task-started` → **binary audio frames** → `result-generated`* →
  `finish-task` → (more `result-generated`) → `task-finished`.
- `parameters`: `format` (pcm/wav/mp3/opus/speex/aac/amr), `sample_rate`,
  `language_hints[]` (≤4 on this model), `vocabulary{term:weight}` instant hot words
  (weights 1–5, or 50 for "super" hot words, ≤50 entries — **flash-streaming only**),
  `semantic_punctuation_enabled`, `max_sentence_silence` (default 1300 ms, 200–6000),
  `multi_threshold_mode_enabled`, `speech_noise_threshold` (−1.0..1.0), `heartbeat`,
  `special_word_filter`.
- `result-generated` carries `payload.output.sentence`: `begin_time`, `end_time`, `text`,
  `sentence_begin`, `sentence_end`, `sentence_id`, `heartbeat`, `words[]`
  (`begin_time`, `end_time`, `text`, `punctuation`). **`sentence_end` is the partial/final
  discriminator**; `usage` is null until it is true.
- `task-failed` puts `error_code`/`error_message` in the **header**, and the connection is
  closed and not reusable.

| Attempt | Result |
|---|---|
| Dial the workspace host, `model: qwen-audio-3.0-asr-flash-streaming`, `format: pcm`, `sample_rate: 16000`, `semantic_punctuation_enabled: true` | **HTTP 101, `task-started` in ~200 ms.** The model name from the brief is correct — D14 resolved |
| 6.89 s of Mandarin speech, 20 ms binary frames paced in real time | 12 `PARTIAL`s then, after `finish-task`, `FINAL id=1 begin=120 end=6760` — `"您好，我这边的订单还没有收到，麻烦帮我查一下物流信息，谢谢。"` — verbatim correct including punctuation |
| **Partial semantics** | **Cumulative, not incremental:** `"您"` → `"您好"` → `"您好，我这边"` → … Each partial is the whole sentence so far. Also **self-correcting**: an intermediate `"，妈"` became `"，麻烦"` in a later partial. A client that appends deltas here would render garbage |
| **Who ends an utterance** — two utterances, 2.5 s gap, 4 s trailing silence, `finish-task` withheld until all audio was sent | **The server does.** `FINAL id=1` arrived **~8 s before** `finish-task`, `FINAL id=2` ~3 s before it. `sentence_id` incremented 1 → 2 unprompted. Server-side segmentation confirmed on this protocol |
| **Timestamps** | **Present and stream-relative:** `id=1 begin=120 end=7120`, `id=2 begin=9500 end=13060` ms. B.2/B.3's "neither engine gives us time" does not hold for this client |
| Audio transport | **Raw binary WebSocket frames** — no base64, no JSON envelope per frame |

The test audio was macOS `say -v Tingting` at 16 kHz mono LE16 — synthetic, not captured
through the media path, so it tests the **protocol**, not recognition quality on telephony
audio. ~~✗ recognition quality on 8 kHz-originated, G.711-transcoded speech is unmeasured
for this model.~~ → **measured, B.3c.**

### B.3c Telephony-path recognition quality (M6.0.3)

**Measured 2026-08-17.** The last value risk in the plan: the protocol was proven, the
accuracy on real telephony audio was not. `[FACT]` The 5.82 s sample from B.2/B.3 no longer
existed — the scratch directory is cleared between sessions — so an equivalent was captured
fresh rather than substituted with something weaker.

*The rig, so it reproduces.* A SIP call **to the switch itself** over PCMU, so the audio is
genuinely G.711 companded rather than merely 8 kHz: `originate
{absolute_codec_string=PCMU}sofia/internal/<ext>@<lan-ip> &playback(silence_stream://30000)`,
with `mod_audio_stream` attached to that leg as `stereo 16000`. The **left/READ** channel is
then audio that has crossed a real RTP leg and been decoded from μ-law. `[FACT]` The switch
confirms it: `Channel-Read-Codec-Name: PCMU`, `Channel-Read-Codec-Rate: 8000`.

Two rig details cost time and are worth writing down:

- `[MEASURED]` **The leg must be running an app that pumps media.** With `&park()` the tap
  received **zero** frames; with `&echo()` it received 6.84 s and then stopped mid-playback,
  silently and with no module error. `&playback(silence_stream://…)` streamed the whole
  call. A truncated capture looks exactly like a model that stopped transcribing — the
  envelope plot is what distinguished them.
- `[MEASURED]` **The module does not cap a stream.** A 30 s control capture on a
  continuously-pumping call delivered **1487 frames = 29.74 s**, unbroken. This is what
  proved the truncation above was the rig and not the tap.

| | |
|---|---|
| **Ground truth** (three stock Callie prompts, 6.66 s total) | "Please state your name and the reason for your call. Speak to a customer service representative. Please hold while your party is being contacted." |
| **Qwen returned** | "Please state your name and the reason for your call. Speak to a customer service representative. Please hold while your party is being contacted." |
| **Verdict** | **Exact**, word for word, including sentence punctuation |

Because this deployment is mainland and the model is Chinese-first, English alone would have
left the primary case untested. The same rig, same PCMU leg, with a Mandarin utterance:

| | |
|---|---|
| **Ground truth** | 您好，我这边的订单还没有收到，麻烦帮我查一下物流信息，谢谢。 |
| **Qwen returned** | 您好，我这边的订单还没有收到，麻烦帮我查一下物流信息，谢谢。 |
| **Verdict** | **Exact**, character for character |

`[INFERENCE]` G.711 companding costs this model nothing measurable on clean speech. What
this does **not** establish, and should not be read as establishing: accuracy on real
callers — accents, overlap, background noise, and the agent's WebRTC uplink rather than a
played file. Those belong to M6.4 with live calls. What it does close is the question that
was blocking: the engine understands telephony-band audio arriving through this exact path.

**One implementation consequence, and it would have been a defect.** `[MEASURED]` Both
captures opened with a spurious final over the leading silence —
`FINAL id=1 begin=0 end=780 text=""`. An empty final is still `sentence_end:true`, so a
client that trusts the flag writes an **empty transcript row** and allocates a `seq` for
it. The `dashscope` client must drop finals whose text is empty after trimming, before the
seam (§8.3). Add it to the unit suite in §16.

### B.4 Things nobody has measured

**Deliberately unmeasured, by owner directive (2026-08-17): the `wss://` build.**
`-DUSE_TLS=ON` was not attempted and the module's TLS path is untested here. This is a
decision, not an omission — the ingest link is LAN-local and plaintext by ruling (D5), so
verifying TLS would be work spent on a configuration nothing in scope uses. It stays
unmeasured until the trigger in D5 fires: **the first deployment that separates the switch
from the app.** Do not quietly cite the module's TLS support as available on the strength
of its README; nobody here has run it.

~~Recognition quality on real G.711-originated audio for
`qwen-audio-3.0-asr-flash-streaming`~~ → **measured, B.3c: exact in both English and
Mandarin.** A related one is also gone: the module was seen streaming 29.74 s unbroken, so
"does the tap survive a normal call length" is no longer open — only true *backpressure*,
under a consumer that stalls, still is.

Also unmeasured, and each already noted where it matters: the module's
backpressure behaviour under a stalled consumer (§14), whether a bug attached at
`CHANNEL_BRIDGE` survives the bridge's own set-up (D4), **16000→24000 resampling
specifically** (D15 — 8000→16000 and 8000→24000 are measured; a `loopback/` channel cannot
be forced wideband, so this needs a real agent leg), and **whether the agent's uplink is
wideband at all** — which D15 no longer depends on, but which is free to record once a
live agent call is on the switch. `[FACT]` All belong to **M6.4**, which D7 keeps as its
own slice precisely so they are answered rather than assumed.

Per the standing owner directive of 2026-08-16, **no capacity or latency figure appears
anywhere in this document**, including for the new path. What this feature adds per
bridged call — one inbound WS, two outbound WS, two ASR sessions, one media bug on the
switch — is a *shape*, not a number. Sizing it belongs to the deferred benchmark
campaign in `docs/load-tests.md`, and until that runs the estimate in
`docs/design/06-capacity.md` stays an internal design aid.

---

**End of gap analysis. No source file, migration, contract or configuration was modified
in producing it.**
