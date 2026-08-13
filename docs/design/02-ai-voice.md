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
                        internal/provider (openai | qwen | mock)
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

## 3. `VoiceSession` — the provider abstraction (phase-1 surface, cascade-proof)

```go
type VoiceSession interface {
    Start(ctx context.Context, cfg SessionConfig) error
    SendAudio(f media.Frame) error            // caller audio, provider-native format (adapter converts)
    SendToolResult(callID string, output string, hint string) error // hint => steering (§6)
    UpdateInstructions(text string) error     // long-call fact pinning / node changes
    Interrupt(reason InterruptReason) error   // normalize barge-in (§5)
    Events() <-chan Event
    Close(ctx context.Context) error
}

type SessionConfig struct {
    Instructions string; Voice string
    Language string                            // "en"|"zh" (informs prompts, not routing)
    Turn TurnDetection                         // {Mode: Semantic|VAD|None, SilenceMs int}
    Tools []ToolSpec                           // JSON-schema tools from the flow
    InputFormat, OutputFormat media.AudioFormat
}
```

Events (closed set): `AudioDelta{PCM/G711 bytes}`, `InputTranscript{delta|final}`, `OutputTranscript{delta|final}`, `SpeechStarted`, `SpeechStopped{reason}`, `Interrupted{by}`, `ToolCall{call_id,name,args}`, `ResponseDone{usage}`, `SessionWarning{budget}`, `Error{fatal bool}`, `Closed`.

**Why cascade fits later without interface change** (mandated argument): the surface speaks only in audio frames, transcripts, turn boundaries, tool calls, interruption, and instructions — no ASR/TTS concepts leak (no phoneme/voice-clone/partial-hypothesis types; transcripts are already deltas+finals; `TurnDetection` is declarative). A cascade implementation composes VAD→ASR→LLM→TTS behind the same channel: `SendAudio` feeds VAD/ASR, `ToolCall` comes from the LLM, `AudioDelta` from TTS, `Interrupt` cancels TTS + LLM. The one asymmetry — cascade emits `InputTranscript` before the LLM turn rather than after — is already permitted by the event ordering contract (transcript events are unordered relative to `AudioDelta`). **No cascade code, interfaces, or stubs ship in phase 1.**

## 4. Provider clients

One shared **OpenAI-protocol client** parameterized by a `Profile` (endpoint, model, auth header, event-name dialect GA/beta, session-field dialect, audio formats) — java-bot's proven shape — plus a thin **Qwen** subtype for DashScope quirks: strip-and-resend on rejected `session.update` fields, `turn_detection` immutable after first audio (config assembled fully before `Start` sends any frame), event-name folding into the internal enum. M0-verified: endpoint `wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model=…`, `Authorization: Bearer` (env `ALIYUN_API_KEY`), OpenAI-shaped `tools` accepted verbatim — but audio-format fields are never echoed in `session.updated`, so the client must not treat the echo as confirmation.

WS hygiene (mandatory, absent in golang-bot): ping/pong keepalive (15s), read deadlines (45s hard, reset on any frame), single-writer mutex, lazy nothing — connect at call start with a 3s deadline; **no mid-call reconnect** (provider session state is unrecoverable) — a fatal WS error surfaces as `Error{fatal}` → flow `on_error` route (transfer to queue / apology per flow config). Watchdogs: first-audio deadline per response (3s), delta-stall deadline (2s with audio already received → force-complete and play what arrived).

Session bring-up: `Start` = WS dial → `session.update` (instructions, voice, formats, turn detection, tools) → wait `session.updated` → greeting `response.create` (flow's initial node). Preflight (java-bot pattern): on aicc startup and on config change, a background check dials each configured provider, round-trips one greeting + one tool call, and surfaces status on the admin health panel.

## 5. Barge-in — unified semantics (mandated comparison)

| Step | OpenAI | Qwen | Normalized behavior in aicall |
|---|---|---|---|
| Detect | `input_audio_buffer.speech_started` (server auto-cancels response, emits `response.cancelled`) | `input_audio_buffer.speech_started` (client must act) | emit `SpeechStarted` |
| Cancel | already cancelled server-side; client sends `conversation.item.truncate{audio_end_ms=played}` for history accuracy | client sends `response.cancel`; `response.done{status:cancelled}` follows | provider adapter does its dialect; actor sees one `Interrupted` |
| Flush local audio | — (client's job) | — (client's job) | **always ours**: `RTPSession.ClearTx()` drains the TX queue; ~2 frames remain in flight to FS → silence within ~40–60ms. Nothing needs sending to FreeSWITCH (we terminate RTP). Backstop: flush again on `Interrupted` even if `SpeechStarted` was missed (java-bot lesson). |
| Played-time tracking | needed for `truncate` | not needed | RTP send loop counts frames per response → `audio_end_ms` |

Turn-detection config mapping: `VAD` → `server_vad` with `SilenceMs` (**the default for both languages**: Qwen's own default 800 ms is lowered to 500 ms — M0-verified as accepted and echoed; OpenAI's GA default is already 500 ms). `Semantic` → Qwen `smart_turn` / OpenAI `semantic_vad`, **opt-in per flow**: M0 found `smart_turn` forces `silence_duration_ms = 2000` and ignores attempts to lower it, i.e. ≈+1.5 s of turn latency — worth it only where backchannel immunity (嗯/啊 not interrupting) outweighs snappiness. Both modes are frozen after the first audio frame on Qwen, so the choice is made from flow config before `Start`. DTMF always interrupts immediately (policy from golang-bot field data).

## 6. Flow engine (DSL v1) over realtime sessions

Spec = **DSL v2**: ui-test's v1 re-keyed to lowerCamelCase per 07 §7 (`specVersion:"v2"`): `global{persona, rules, fallback, maxTurns, tools, transitions}`, `nodes{instruction, tools, transitions, isTerminal}`, `tools{description, params(JSON-schema), http{path, body template, success predicate, result slots}}` — all user-facing strings bilingual `{en,zh}`; the session uses the entry point's language. The five v1 reference flows are converted by a one-shot script; v1 files stay reference-only.

Engine mechanics (java-bot's hint steering, verified live over realtime function calling): the model owns the conversation; the engine owns phase. Node entry → `UpdateInstructions(persona+rules+node instruction)`. On `ToolCall`: if the tool is not allowed in the current node → `SendToolResult(ok:0 + current-phase hint)`; else run it (HTTP runner: 5s timeout, success predicate, slot extraction) and reply with the result, **overwriting `hint` with the next node's instruction** when a transition fires. The `maxTurns` guard forces the fallback node on runaway tool loops.

**Built-in tools** (present in every flow's allowed set; not HTTP):

```jsonc
transfer_to_agent: { "queue": "enum(flow's queues)", "reason": "enum(flow-defined categories)",
                     "summary": "string (≤600 chars, caller language)",
                     "slots": "object (flow-collected fields)" }        // → F2 in design 01; all args → userData
take_message:      { "message": "string", "callbackNumber": "string?" }  // → callbacks table + CDR flag
hangup:            { "isFarewellSpoken": "boolean" }                     // ends call gracefully
```

`transfer_to_agent` sequencing: tool call → engine replies with a result whose hint says "tell the caller you're connecting them" → on that response's `ResponseDone` (or 3s cap) → execute the transfer. Guarantees the bridge line is spoken before the BYE.

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
