# Design 02 — AI Voice Leg (SIP UAS, Audio Bridge, VoiceSession, Flow Engine)

## 1. Voice leg architecture

Per AI call, `internal/aicall` composes three parts around a per-call actor:

```
FreeSWITCH ──INVITE/RTP──► internal/voice (UAS + RTPSession)
                                │ 8k PCM16 frames (rx) / tx queue
                                ▼
                        aicall session actor ◄── internal/flow (engine, tools)
                                │ VoiceSession events/audio
                                ▼
                        internal/provider — one OpenAI-Realtime client × Profile
                        (openai | qwen | any endpoint speaking the protocol)
```

`internal/voice` is the golang-bot port (custom UAS; pion/rtp wire format). Port fixes applied during the port (T4): 200-OK retransmission until ACK (timer G-ish, 500ms×2 backoff, cap 3s), idempotent re-200 on INVITE retransmission, correct `487` CSeq method on CANCEL, no BYE after remote BYE, `a=ptime:20` in SDP answers, `MaxCalls` configurable (default 220), DTMF payload type taken from SDP offer (not hardcoded 101), RFC 3550 §5.1 checklist retained (random SSRC/seq/ts, SSRC collision regen, regen on remote addr change, unknown PT ignored).

**Ports**: SIP UDP `:6060` (takes over the `aicc_bot` gateway target). RTP pool `40000–40999` (even RTP / odd RTCP pairs → 500 concurrent pairs ≥ 220 MaxCalls with headroom), configurable; deliberately outside FS's 16384–32768 since dev co-locates both on one host.

**Kept from golang-bot verbatim**: 20ms ticker pacing with late-tick resync (no catch-up bursts), TX prebuffer 3 frames (60ms) + 2-tick underrun grace, reorder-only jitter buffer (4-frame window, ≥25-frame gap resync, codec-correct silence fill), RTP dead timeout (5s default), minimal RTCP SR/RR (one-way-dead detection), OPTIONS→200, INFO+RFC2833 DTMF merge, advertise-IP route-probe toward the peer's media address.

## 2. Codec & sample-rate paths (per direction, per provider)

| Leg codec | Direction | OpenAI (preferred: G.711 passthrough) | Qwen |
|---|---|---|---|
| PCMU | uplink | `audio/pcmu` passthrough — **M0-verified** (no resample, no decode) | LUT decode → 8k PCM16 → ×2 linear upsample → 16k PCM16 |
| PCMA | uplink | `audio/pcma` passthrough — **M0-verified** (no transmap needed) | LUT decode → resample as above |
| PCMU/PCMA | downlink | same-law `audio/pcmu` / `audio/pcma` deltas → RTP payload passthrough — **M0-verified** | 24k PCM16 deltas → **windowed-sinc** (32-tap Kaiser) ÷3 decimate → 8k → G.711 encode |
| contingency (OpenAI; retained, not needed per M0) | both | 24k `audio/pcm` both ways: ×3 sinc up / ÷3 sinc down | — |

Rules: all-or-nothing codec wiring per PR#3859's lesson (negotiation → SDP → PT → tables, both laws, with tests); one pre-encoded 20ms silence frame per law (no per-tick zero-encoding); `sync.Pool` for frame buffers, resampler scratch, and base64 buffers (golang-bot's ~5–8 allocs/frame/direction eliminated — the 8c16g requirement's main GC lever); resamplers are fixed-integer-factor (×2, ×3) — linear for upsampling, windowed-sinc for downsampling (java-bot's "muffled TTS" lesson).

## 3. `VoiceSession` — the seam between the call actor and the client

```go
type VoiceSession interface {
    Start(ctx context.Context, cfg SessionConfig) error
    SendAudio(f media.Frame) error            // caller audio, provider-native format (adapter converts)
    SendToolResult(callID string, output string, hint string) error // hint => steering (§6)
    UpdateInstructions(text string) error     // long-call fact pinning / node changes
    SpeakText(text string) error              // say this, now: pre-empts, never queues (§6)
    Interrupt(reason InterruptReason) error   // normalize barge-in (§5)
    Events() <-chan Event
    Close(ctx context.Context) error
}

type SessionConfig struct {
    Instructions string; Voice string
    Language string                            // "en"|"zh" (informs prompts, not routing)
    Turn TurnDetection                         // {Mode: Semantic|VAD, SilenceMs int}
    Tools []ToolSpec                           // JSON-schema tools from the flow
    OpeningText string                         // the call's first words, as written (§6)
    InputFormat, OutputFormat media.AudioFormat
}
```

**Words, not only subjects** (added 2026-09-18). Everything else here is a
brief the model writes from. `OpeningText` and `SpeakText` are the sentence
itself: *say this*. Both are documented as **verbatim where the provider can
manage it and best effort where it cannot** — the client in `internal/provider`
speaks one protocol, and the nearest that protocol comes to handing over words
is a per-response `instructions` telling the model to repeat one sentence and
add nothing. An engine that speaks text outright says it as written, and that
is a property of its profile rather than of this interface.

`SpeakText` is mid-call only, **pre-empts** whatever is being said, and **does
not queue**: a second call before the first has been spoken replaces it, because
both describe what should come next and the older one is by then out of date.
The opening line is `SessionConfig`'s instead, because the first turn is asked
for while the session is being started and there is no mid-call moment to catch
(§4, session bring-up). A turn stopped to make room for a line is reported as
`Interrupted{by: SYSTEM}` rather than `SPEECH`: the consumer must flush its
queue and report played-ms either way — the caller stopped hearing it, and the
provider's history has to be trimmed to what they heard — but it was not a
barge-in, and recording it as one writes an interruption the caller never made
into the log of the call.

Events (closed set): `SessionReady`, `AudioDelta{PCM/G711 bytes}`, `InputTranscript{delta|final}`, `OutputTranscript{delta|final}`, `SpeechStarted`, `SpeechStopped`, `ResponseStarted`, `Interrupted{by}`, `ToolCall{call_id,name,args}`, `ResponseDone{status,usage}`, `Error{fatal bool}`, `Closed`.

**What this interface is, and is not** (phase1-decisions A6). It is the seam between `aicall`'s per-call actor and whichever provider client answers, and the place a test stands a fake model. It is **not** the extension point for other kinds of engine. That job belongs one layer down, to the wire protocol: `internal/provider` speaks OpenAI Realtime, parameterised by a `Profile` per vendor of it. A new engine **on that protocol** is therefore a new **profile** — a name, an endpoint, a model, a session dialect — selected by `AICC_PROVIDER` at startup, never a second Go client. That is how the phase-2 **OpenAI Realtime Gateway** (ASR + LLM + TTS behind the same events, in its own service) attaches: as a third value carrying its own endpoint and dialect, impersonating no vendor. **Consequently no recognition, synthesis or cascade concept — type, interface, adapter, placeholder or TODO — ever enters this repo**, in phase 1 or after. The gateway profile itself shipped on 2026-09-02.

**Amended 2026-09-18 — a new protocol, not a new engine.** Only a different wire protocol earns a second client, and `AICC_PROVIDER=doubao` is the first: ByteDance's full-duplex dialogue API, in `internal/provider/doubao`. What makes it a protocol rather than a dialect is lifecycle — a session bootstrapped and confirmed before anything may be sent, no way to ask for a turn, no event for the caller starting or stopping speaking, tool results as items with a role, a close handshake, and an uplink that is a clock and a keepalive at once — none of which is expressible as values in a `Profile`. The seam is unchanged and stays the point: a second client is added **underneath** this interface, produces only events already listed above, and is written in AICC's vocabulary rather than its own. It shares `wsconn`, `provider.Watchdog` and `provider.MergeHint` with the Realtime client because ownership and runtime semantics are identical there, and shares no protocol event, decoder or dispatch, because that is where two protocols are supposed to differ. Which client a name selects is decided in `cmd/aicc/wiring.go` — a sub-package of `internal/provider` cannot be built from inside it — and `internal/aicall` still asks only for a `SessionFactory`. Findings and their evidence: `doubao-findings.md`.

## 4. Provider clients

One client per wire protocol, and today there are two: the **OpenAI-protocol client** described in the rest of this section, and `internal/provider/doubao` for ByteDance's full-duplex dialogue API (`AICC_PROVIDER=doubao`, added 2026-09-18 — §3's amendment for why, `doubao-findings.md` for what it rests on). Everything below is the first of them. The second differs in all of it: `session.create` and a confirmation instead of `session.update`/`session.updated`, no `response.create` because the engine answers audio and nothing else, a turn that ends on `output_audio.done` rather than the wire's own `response.done`, a paced uplink that must be muted when the caller's frames stop, and a `session.close` handshake. It keeps the WS hygiene stated below, in shared code where ownership is identical (`wsconn` for dial/write/keepalive/close, `provider.Watchdog` for the two deadlines) — and it keeps **no mid-call reconnect** for the same reason and with the same outcome, a fatal `Error` and a rescued caller.

The OpenAI-protocol client is parameterized by a `Profile` (endpoint, model, auth header, event-name dialect GA/beta, session-field dialect, audio formats) — java-bot's proven shape — plus a thin **Qwen** subtype for DashScope quirks. **Exactly one profile is live per deployment**, resolved once at startup by `provider.ProfileFor(AICC_PROVIDER, …)`: qwen inside mainland China, openai elsewhere (A1). Its endpoint and model are defaults, overridable with `AICC_PROVIDER_ENDPOINT` / `AICC_PROVIDER_MODEL` for a proxy, a regional host, or any protocol-compatible server. **Voice is not deployment configuration**: it belongs to the bot, so it is `global.voice` in the flow spec — published and versioned with the persona it goes with (A7). The profile's voice is only the fallback for a flow that names none. Both vendors offer voices that carry Chinese and English equally well, so one bot keeps one voice across a bilingual catalogue rather than switching timbre per call. Qwen quirks: strip-and-resend on rejected `session.update` fields, `turn_detection` immutable after first audio (config assembled fully before `Start` sends any frame), event-name folding into the internal enum. M0-verified: endpoint `wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model=…`, `Authorization: Bearer` (env `ALIYUN_API_KEY`), OpenAI-shaped `tools` accepted verbatim — but audio-format fields are never echoed in `session.updated`, so the client must not treat the echo as confirmation.

WS hygiene (mandatory, absent in golang-bot): ping/pong keepalive (15s), read deadlines (45s hard, reset on any frame), single-writer mutex, lazy nothing — connect at call start with a 3s deadline; **no mid-call reconnect** (provider session state is unrecoverable) — a fatal WS error surfaces as `Error{fatal}` → flow `on_error` route (transfer to queue / apology per flow config). Watchdogs: first-audio deadline per response (3s), delta-stall deadline (2s with audio already received → force-complete and play what arrived).

Session bring-up: `Start` = WS dial → `session.update` (instructions, voice, formats, turn detection, tools) → wait `session.updated` → **[Qwen only] synthetic greeting cue** → greeting `response.create` (flow's initial node). ⚠ **M3-verified**: Qwen rejects `response.create` on an empty conversation (`conversation has no messages or no user message`), so the opening turn is prompted with a `conversation.item.create` user text item — a stage direction, never recorded as caller speech. Carried as the profile trait `NeedsCueForFirstTurn` (false for OpenAI, which greets unprompted) with per-flow override `SessionConfig.GreetingCue`.

Where the entry phase names its own opening line (`SessionConfig.OpeningText`, from `announce` in §6), that last request carries it: `response.create` with `response.instructions` set to a say-exactly direction in the session's language, one frame on both dialects — the type discriminator the GA session object needs has no counterpart on a response object. On a `NeedsCueForFirstTurn` provider the direction *is* the cue item as well, because the conversation still may not be empty and a cue saying anything else would steer one turn two ways. With no opening line the request is the bare `{"type":"response.create"}` it always was, byte for byte. Preflight (java-bot pattern): on aicc startup and on config change, a background check dials each configured provider, round-trips one greeting + one tool call, and surfaces status on the admin health panel.

## 5. Barge-in — unified semantics (mandated comparison)

| Step | OpenAI | Qwen | Normalized behavior in aicall |
|---|---|---|---|
| Detect | `input_audio_buffer.speech_started` (server auto-cancels response, emits `response.cancelled`) | `input_audio_buffer.speech_started` (client must act) | emit `SpeechStarted` |
| Cancel | already cancelled server-side; client sends `conversation.item.truncate{audio_end_ms=played}` for history accuracy | client sends `response.cancel`; `response.done{status:cancelled}` follows | provider adapter does its dialect; actor sees one `Interrupted` |
| Flush local audio | — (client's job) | — (client's job) | **always ours**: `RTPSession.ClearTx()` drains the TX queue; ~2 frames remain in flight to FS → silence within ~40–60ms. Nothing needs sending to FreeSWITCH (we terminate RTP). Backstop: flush again on `Interrupted` even if `SpeechStarted` was missed (java-bot lesson). |
| Played-time tracking | needed for `truncate` | not needed | RTP send loop counts frames per response → `audio_end_ms` |

**Line echo — the failure mode both references hit on real calls (M3, borrowed per owner directive: golang-bot first, java-bot second).** The bot's own voice returns through the caller's handset or speakerphone, the provider's detector calls it speech, and the bot interrupts itself mid-greeting. Two graded mitigations exist in the references:

1. **Barge-in guard window** (golang-bot, `BARGE_GUARD_MS=800`): ignore speech detected within N ms of the bot starting to speak. Cheap, no signal processing, and it covers the common case where the echo arrives at the start of a turn. **Implemented** as `aicall.Config.BargeGuard`, default 800 ms; a keypress is never guarded, because it cannot be an echo of anything. golang-bot's companion heuristic — drop transcripts that are a substring of what the bot just said — does not transfer: in speech-to-speech the interruption fires before any transcript exists.
2. **True echo cancellation** (java-bot `EchoCanceller`, partitioned-block frequency-domain NLMS with Geigel double-talk freeze, 320 ms tail). Live-line evidence 2026-08-05: echo peaked at ~3700 RMS — *above* the floor of real user speech — and the echo is the bot's own words, so neither an energy threshold nor a semantic VAD can separate it from genuine barge-in. Only subtracting an adaptive estimate of the echo from the known playback signal works.

**Deliberately deferred to a later milestone, not overlooked**: (2) needs the far-end signal tapped at the RTP send tick, an FFT, and its own tuning pass, and it is only required where the echo return loss approaches 0 dB. Phase 1 ships (1) and the knob to tune it. The trigger to build (2) is a live call where the guard window proves insufficient — i.e. self-interruption occurring *after* the guard expires.

Turn-detection config mapping: `VAD` → `server_vad` with `SilenceMs` (**the default for both languages**: Qwen's own default 800 ms is lowered to 500 ms — M0-verified as accepted and echoed; OpenAI's GA default is already 500 ms). `Semantic` → Qwen `smart_turn` / OpenAI `semantic_vad`, **opt-in per flow**: M0 found `smart_turn` forces `silence_duration_ms = 2000` and ignores attempts to lower it, i.e. ≈+1.5 s of turn latency — worth it only where backchannel immunity (an "mm-hm" not interrupting) outweighs snappiness. Both modes are frozen after the first audio frame on Qwen, so the choice is made from flow config before `Start`. DTMF always interrupts immediately (policy from golang-bot field data).

## 6. Flow engine (DSL v1) over realtime sessions

Spec = **DSL v2**: ui-test's v1 re-keyed to lowerCamelCase per 07 §7 (`specVersion:"v2"`): `global{persona, rules, voice, fallback, maxTurns, tools, transitions}`, `nodes{instruction, announce, tools, transitions, isTerminal}`, `tools{description, params(JSON-schema), http{path, body template, success predicate, result slots}}` — all user-facing strings bilingual `{en,zh}`; the session uses the entry point's language. The five v1 reference flows are converted by a one-shot script; v1 files stay reference-only.

`announce` (added 2026-09-18) is optional and is the one field of a node that is not a brief: a line the bot says **as written** on entering the phase, rendered with `{slots.x}` exactly as `instruction` is. It exists for the two ends of the range an instruction cannot reach — a closing line a business has signed off, which should not be paraphrased, and an engine that takes no text cue at all, for which a phase whose only job is one sentence has no way to say it. A deployment whose provider is of the second kind sets `Profile.RequiresTerminalAnnounce`, and `flow.RequireTerminalAnnounce` then refuses to **publish** a flow whose terminal phases carry none (422 `TERMINAL_ANNOUNCE_REQUIRED`, `params.nodes` naming them). Publish and not load: the same document is perfectly good where the provider can be cued, and a spec that loads on one installation and not on another would make the dialect a property of the host.

Engine mechanics (java-bot's hint steering, verified live over realtime function calling): the model owns the conversation; the engine owns phase. Node entry → `UpdateInstructions(persona+rules+node instruction)`, then `SpeakText(node announce)` where the node has one — the entry node's goes into `SessionConfig.OpeningText` instead, being spoken before there is a call to speak into. On `ToolCall`: if the tool is not allowed in the current node → `SendToolResult(ok:0 + current-phase hint)`; else run it (HTTP runner: 5s timeout, success predicate, slot extraction) and reply with the result, **overwriting `hint` with the next node's instruction** when a transition fires. The `maxTurns` guard forces the fallback node on runaway tool loops.

⚠ On a **terminal** node the order within node entry is load-bearing: the ending is armed *before* the line is asked for. Arming records the turn it happened in and fires on the playback of a later one (§6, `PLAYBACK_DONE`), and the line is spoken in a turn of its own — ask for it first and that turn can be the one recorded, after which no playback ever counts and the call ends on the 10 s grace cap with the caller sitting in silence.

**Built-in tools** (present in every flow's allowed set; not HTTP):

```jsonc
transfer_to_agent: { "queue": "enum(flow's queues)", "reason": "enum(flow-defined categories)",
                     "summary": "string (≤600 chars, caller language)",
                     "slots": "object (flow-collected fields)" }        // → F2 in design 01; all args → userData
take_message:      { "message": "string", "callbackNumber": "string?" }  // → callbacks table + CDR flag
hangup:            { "isFarewellSpoken": "boolean" }                     // ends call gracefully
```

`transfer_to_agent` sequencing: tool call → engine replies with a result whose hint says "tell the caller you're connecting them" → on that response's **`PLAYBACK_DONE`** (or 3 s cap) → execute the transfer. ⚠ **Amended in M3**: waiting on `ResponseDone` is wrong — the model finishing generation and the caller finishing hearing are separated by everything still in the RTP send queue (up to seconds), so a transfer on `ResponseDone` cuts the bridge line off mid-word. `aicall` therefore reports the two separately: `TURN_DONE` when the model stops producing, `PLAYBACK_DONE` when the queue has drained and one further frame interval has passed. Anything that must not interrupt the bot — transfer, hangup, goodbye — waits for the second. (golang-bot does the same thing, waiting on `sink.Pending()` before declaring a prompt finished.)

**A transfer can be refused, and the bot handles it in conversation.** Before executing, the engine checks the target queue's business hours and staffing; when the queue is closed or unstaffed the tool returns `ok:0` with a hint naming the reason (`QUEUE_CLOSED`, `NO_AGENTS_STAFFED`) and the next node's instruction, so the model says something like "we're closed right now — I can take a message" and continues with `take_message`. This is why business hours live on the queue and not on the DID: an AI-native call center has no separate out-of-hours branch in the dialplan, because the bot has already answered and can simply explain (owner decision 2026-08-13).

## 7. Long-call policy (Qwen 50-turn/300s-audio; OpenAI 60-min)

Tracked per session from usage/turn counts: at **80% of budget** the engine injects a wrap-up steer (via next tool hint or `UpdateInstructions`): summarize, resolve, or offer transfer. At **hard limit** (Qwen: history silently truncates — session survives but memory fades; OpenAI: session ends at 60min): if not terminal, execute `transfer_to_agent(queue=flow.fallback, reason="SESSION_LIMIT")` with the running summary. Fact durability: flow slots live in aicc (not provider history) and are re-pinned via `UpdateInstructions` at each node change, so Qwen's silent truncation never loses structured state. Per-flow `maxDurationSec` (default 900) as product-level cap → same wrap-up path.

## 8. Latency budget (target ≤1.2s p50 / ≤2s p95, caller-stop → caller-hears)

| Hop | p50 est. | Knob |
|---|---|---|
| Turn detection hold | 400–600ms | `SilenceMs=500` — M0-verified accepted on both providers. **`smart_turn` is 2000 ms fixed** (M0) and therefore excluded from the budget: flows selecting it target ≈2.7 s p50 instead |
| Provider first audio delta | 400–700ms | model tier; region proximity (Qwen Beijing; OpenAI intl route) |
| aicc pipeline (decode+resample+b64+queue) | <5ms | pooled, measured per-frame |
| RTP prebuffer | 60ms | 3 frames (jitter absorb) |
| Network (WSS RTT + RTP LAN) | 30–150ms | deployment region |

p50 realistic total ≈ 0.9–1.4s → the budget holds only with `SilenceMs≈500` and healthy provider routes; **measured, not assumed**: OTel span per turn (`speech_stopped → first AudioDelta → first RTP out`) exported as histogram `aicc_turn_latency_ms` from day one; the M3 gate re-tunes knobs against the target (A3).
