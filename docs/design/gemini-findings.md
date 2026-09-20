# Gemini provider — findings (2026-09-19, live verification 2026-09-20)

The evidence record for `AICC_PROVIDER=gemini` and `internal/provider/gemini`,
in the shape of [m0-findings](m0-findings.md) and
[doubao-findings](doubao-findings.md): what the client rests on, where each fact
came from, and which of them the documentation does not contain. Design 02 §3–§4
and phase1-decisions A6 have been amended where this changed them; this is the
record behind those amendments.

**Sources.**

- **DOC** — the Live API pages at <https://ai.google.dev/gemini-api/docs/live-api>
  (overview, capabilities, tools, session-management, thinking, best-practices,
  the raw-WebSocket tutorial), the normative reference at
  <https://ai.google.dev/api/live>, the model pages for `gemini-3.8-live` and
  `gemini-3.8-live-extended-thinking`, and the voice list on the
  speech-generation page. All of them are rendered by JavaScript and return
  nothing useful to a fetch; **appending `.md.txt` to the URL returns the raw
  markdown**, which is how they were read.
- **SDK** — `googleapis/go-genai` and `python-genai`, read only where DOC is
  silent. They are cited as evidence of what the service accepts, never as a
  statement of what it guarantees.
- **PROBE** — live sessions against the real endpoint on 2026-09-19, run from a
  scratch harness with no repository change, on one development machine whose
  network path to the endpoint was **not a controlled condition** (see the next
  bullet and **W-G10**). Raw frames were captured in both directions. Sample
  counts are given with every figure because most of them are small.
- **LIVE** — two rounds of real telephone calls on 2026-09-20, eighteen calls in
  all, human caller, development FreeSWITCH on the same development machine
  (§3). **That machine's network path to the endpoint was not a controlled
  condition either**, and the socket errors both rounds produced came out of it.
  Every uplink figure in §3 is therefore a figure about this path and
  not necessarily about this provider; §3 says so before it says anything else,
  and the run is to be repeated from another host.
- **LIVE TEST** — `internal/provider/gemini/live_test.go`, gated on
  `AICC_LIVE_PROVIDER_TEST=1`, run against the real service after the client was
  written and again on 2026-09-20, when the schema-casing control answered
  **W-G2** (§2.4).

Each fact below is marked **DOC** (stated by one of those pages), **SDK**,
**MEASURED** (observed in the probe, the live test or the live calls) or
**NOT MEASURED**. Where DOC and a measurement disagree, the measurement wins and
the disagreement is the finding.

**Every figure in this document stays in this document.** The probe's timings
are one laptop against one region on one afternoon, and §3's are eighteen phone
calls on the same laptop over the same uncontrolled network path, with sample
counts in single and low double figures. They are the record of what was
verified and what it did, not a
statement about how fast or how large this product is, and the repository still
publishes no performance claim before a benchmark that deserves the name
(CLAUDE.md, owner directive 2026-08-16): no number from here goes into the
README, the deployment guide, `.env.example`, a release note or a commit
message.

## 1. Why this is a protocol and not a dialect

Not one of these is expressible as a value in a `Profile`, which is the whole of
the argument for a third client.

- **DOC + MEASURED** The session is configured **once**. `setup` is "to be sent
  in the first (and only in the first)" client message, nothing else may be sent
  until `setupComplete` answers it, and "you cannot update the configuration
  while the connection is open". Not the instructions, not the tools, not the
  voice. §2.6 is what that costs and what this client does about it.
- **DOC + MEASURED** A turn ends **twice**. `generationComplete` is the model
  having stopped producing; `turnComplete` is the playback it presumes happened,
  and DOC says so outright — "there will be delay between generation_complete
  and turn_complete that is caused by model waiting for playback to finish".
  They are seconds apart on a real turn (§2.2). One wire, two of this
  repository's own distinctions on it.
- **DOC + MEASURED** There is **no cancel primitive**. A turn is stopped by
  starting another one: a `clientContent` with `turnComplete: true`
  "unconditionally interrupts active model generation". Nothing in the protocol
  says "stop", which is the opposite of both other clients.
- **MEASURED** **Errors are not frames.** Every rejection observed arrived as a
  WebSocket close code and a reason sentence, truncated by the protocol at 123
  bytes. There is no error envelope to parse and no way to continue afterwards
  (§2.8).
- **MEASURED** The downlink arrives **several times faster than the caller can
  hear it**, several audio parts to a frame, interleaved with the transcript of
  itself. Playback accounting is entirely the client's.
- **DOC** Function calls default to `NON_BLOCKING` on this model, which lets the
  model keep talking while a tool runs. Every tool this application has is a
  decision the conversation cannot run ahead of, so every declaration says
  `BLOCKING` (§2.4).

## 2. The decisions, and what they rest on

### 2.1 The brief: G1–G9 (owner, 2026-09-19)

The nine decisions the client was written under, numbered as they were given:

- **G1 — the model is a constant in the client**, `gemini-3.8-live`, and never
  `gemini-3.8-live-extended-thinking`: background reasoning and asynchronous tool
  execution break "turn complete = idle", which the watchdog and the transfer
  logic depend on. `thinking_level` is omitted. **DOC** says the same three
  things from the other side: on the extended-thinking model `turnComplete` "no
  longer indicates that the model is idle", blocking tool calls "return a hard
  error", and function scheduling is unsupported. `AICC_PROVIDER_MODEL` therefore
  moves nothing and the profile says so.
- **G2 — a separate client, in `internal/provider/gemini`.** The credential is
  `GEMINI_API_KEY` and `AICC_PROVIDER_ENDPOINT` overrides the Live WebSocket URL,
  as on every other provider. §1 is the argument that this had to be a client and
  could not be a profile.
- **G3 — audio is PCM in and out at the documented rates**, with
  `media.Converter` on both sides: 16 kHz up, 24 kHz down (§2.2). The response
  modality is **audio only**. Output transcription is for the log and the
  transcript, and was to be taken only if it cost nothing measurable in
  first-audio latency — it costs nothing at all, because it arrives whether or
  not it is asked for (§2.9).
- **G4 — turn-taking is the server's.** `automaticActivityDetection` stays
  enabled and `activityHandling` is sent as `START_OF_ACTIVITY_INTERRUPTS` — the
  documented default, sent anyway, because barge-in is not something to inherit
  quietly. Manual VAD (`activityStart`/`activityEnd`) is not used, there is no
  client VAD in the turn decision, the barge-in signal is
  `serverContent.interrupted`, and the local playout is flushed on it (§2.3).
  **Narrowed later by the owner**: an energy detector inside the package, armed
  only while the model is idle and never while the bot is speaking, for the two
  things above the boundary that cannot wait for a transcript (§2.7).
- **G5 — tools are synchronous.** Every declaration carries
  `behavior: BLOCKING`, nothing is scheduled asynchronously, and a transfer halts
  generation until its result has been returned (§2.4).
- **G6 — the opening line and `SpeakText` are best effort**, asked for as words to
  repeat through `clientContent` with a say-exactly instruction, exactly as on
  the Realtime client and with the same shared demand (`provider.SayExactly`).
  `RequiresTerminalAnnounce` is therefore **false**: this engine speaks a line it
  is given.
- **G7 — no session resumption and no reconnect.** The connection's lifetime
  running out is a `goAway`, and a `goAway` is a graceful end of the bot leg
  through the fatal path, with a release reason and a metric of its own (§2.8),
  not something to paper over. `sessionResumption` and
  `contextWindowCompression` are not sent at all.
- **G8 — affective dialogue and video are off and unsent**, and the language is
  the model's own business. `enableAffectiveDialog` is not in the setup (the API
  no longer carries it), nothing video-shaped is ever sent, and **A1 is
  unchanged**: a call's `language` selects greeting, prompt language and voice
  and never a provider. What the model speaks is steered by the flow's
  instructions alone, because this model refuses to be told a language (§2.10).
- **G9 — the pacer is shared only if it is the same mechanism**, which was the
  condition it had to pass and did, as a parameterised one; and if this engine
  tolerates bursty input, pacing is not to be forced on it, with a measurement
  saying so. It does, the measurement is in §2.5, and what the queue there
  absorbs is a stalled socket rather than a cadence.

### 2.2 A turn ends when the model stops, not when the server says the caller heard it

**MEASURED**: `generationComplete` arrives when the audio stops arriving.
`turnComplete` arrives at **first audio + the duration of that audio** — across
5 turns the gap between the two landed between **−244 ms and +50 ms** of that
prediction, which is the server timing a playback it cannot observe.

So `RESPONSE_DONE` is raised on **`generationComplete`**. Raising it on
`turnComplete` would attribute the end of a turn to a moment the call has
already moved past, and would make every armed transfer and hangup wait out a
playback the switch is doing its own accounting for. `turnComplete` is used for
one thing only: it is the moment nothing else is coming, which is when the
caller can be listened to again (§2.7).

**MEASURED**: nothing follows `turnComplete` on this model — **0 frames in
5 × 10 s of waiting after one**. DOC says the same for `gemini-3.8-live` and the
opposite for the extended-thinking variant, which is one of the reasons the
model is a constant.

**MEASURED**: audio is s16le at 24 kHz, as documented, and is delivered at
**≈3.3× real time**. A single server frame carries several `inlineData` parts
and may carry a transcript beside them; every part of every frame is played, and
a frame with parts this client does not understand is not a reason to drop the
ones it does.

### 2.3 Barge-in is the server's, and it is clean

**MEASURED**: when the caller talks over a reply, the server sends
`serverContent.interrupted` and **stops**. Over 12 clean samples the interval
from speech onset to that frame had a median of **≈640 ms**, in a range of
**441–2970 ms**. In **14 of 14** attempts **not one byte of audio arrived after
it**.

**MEASURED, and the number that matters most for the caller's experience**: at
the moment `interrupted` arrives, **7.3–8.2 s of unplayed audio** is already in
this client's hands. That is the downlink running at 3.3× real time, and it is
why the client flushes locally and immediately: a client that let the queue
drain would talk over the caller for the better part of eight seconds having
been told not to.

**MEASURED**: the server's detector is not trigger-happy. **100 ms of line noise
did not interrupt in 3 of 3 attempts**; a **400 ms speech fragment interrupted
in 2 of 2**.

**MEASURED**: an interrupted turn gets **no `generationComplete` at all** — DOC
says so and the probe confirms it. The client's hard invariant is that every
`RESPONSE_STARTED` is closed by exactly one of `RESPONSE_DONE` and
`INTERRUPTED`, never both and never neither, and this is the case that makes it
necessary rather than tidy.

### 2.4 Tools are blocking because they were made blocking

**DOC**: `NON_BLOCKING` is the default on `gemini-3.8-live`; `BLOCKING` is
available "for backwards compatibility". **MEASURED**: with `BLOCKING`
declared, **0 bytes of audio arrive while a call is pending** — the model waits,
which is what every tool in this application needs.

**MEASURED**: `toolResponse` → first audio of the answer was **828 ms** and
**1382 ms** on the two measured round trips. Parallel calls arrive together and
are answered **in one frame** once the whole set has a result.

**MEASURED, and a documented mechanism that does not fire**:
`toolCallCancellation` was **never sent in 6 sessions**, including sessions
where the caller interrupted while a call was outstanding. What happens instead
is that the server **discards the call silently**, ignores a `toolResponse`
carrying the old id, and **re-issues the same call under a new id ~2.6–3.6 s
later**. The client therefore forgets outstanding calls on an interruption and
answers a withdrawn one with nothing at all; the cancellation handler exists,
and is covered by synthetic frames only.

**LIVE TEST**: a real flow's tool schemas are accepted, with the type names
rewritten to this API's protobuf enum spelling (`OBJECT`, `STRING`, …). Nothing
else about a schema is touched — a keyword this API may not know is left in
place to be refused with a message that names it, which is a better failure than
a field silently dropped.

**MEASURED, 2026-09-20, and not what the rewrite was written for**: the service
**accepts the lowercase spelling too**. `TestLiveGeminiRefusesASchemaInTheCasingTheFlowsUse`
sent one `setup` carrying `transfer_to_agent` with `{"type":"object", …
"type":"string"}` exactly as a flow writes it, and in **3 runs of 3** the service
answered rather than closing the socket:
`the service ACCEPTED the flows' own casing and answered {…}`. So the rewrite in
`geminiSchema` is **not required for a setup to be accepted**. It stays, and why
it stays is **W-G2**.

### 2.5 The uplink stalls, so the queue is deep and drains whole

**DOC**: 20–40 ms chunks, do not buffer a second of audio, nothing about pacing
in real time. **MEASURED**: bursts and a paced stream are **accepted and
transcribed identically** — this service has no opinion about cadence, which is
the exact opposite of the other non-Realtime client.

**MEASURED, and the finding that shaped the package**: a write to this endpoint
**blocks for seconds at a time**. In **3 of 4** Gemini sockets a write took
between **2.3 s and 3.6 s**, first occurring **23–42 s** into the session, with
cumulative lag of **5.4–18.1 s at the 90-second mark**. The same harness against
Qwen was clean on **4 of 4** sockets, maximum single write **17.9 ms**. §3's two
live rounds saw the same thing on the telephone leg, worse: single writes of
**3.6–5.0 s**, and two calls out of seven ended on `write tcp … i/o timeout`.

**Where that happens is NOT established, and this document used to say it was.**
The probe and both live rounds ran from one development machine whose network
path to the endpoint was not a controlled condition. The Qwen comparison was
made with the same harness on the same machine and showed no such stalls, which
on its own isolates nothing: it is a different endpoint, and a network path that
buffers, rate-limits or re-establishes a TCP connection produces exactly this
signature. Two of §3's seven calls also had **zero** slow writes under the same
conditions, which is not what a provider-side limit looks like. The attribution
is open and is **W-G10**; the queue below is right either way,
because a client that cannot write for five seconds has to do something with the
caller's audio whoever is to blame.

That is why `internal/provider/pacer` exists as a package and why the Realtime
client is **not** paced. The cadence mechanism is shared and its policy is a
parameter: doubao keeps a three-frame queue and one frame per tick, because its
engine reads the uplink as a clock and calls a burst an error; gemini keeps
**five seconds** of queue and `DrainEverything`, because there is no cadence to
preserve and what dropping the caller's words would buy is a sentence with a
hole in it. A second of empty ticks sends `audioStreamEnd` once.

**NOT MEASURED**: what `audioStreamEnd` actually changes. DOC says it flushes
audio the server was holding and bypasses the silence timer; the probe never
isolated its effect, and the client sends it on DOC's word. **W-G4**.

### 2.6 Nothing can update the instructions, and the phase rides the next thing said

This is the one place this client does something the others do not have to, and
it is the most heavily measured fact in the document.

**MEASURED**, four ways:

| What was tried | Obeyed |
|---|---|
| A user-role `clientContent` carrying new standing instructions, sent before / after / while idle | **0 of 3** — and with `turnComplete: true` the model *explicitly refused*, saying it could not change its instructions |
| The same words inside a **tool result's output** | **3 of 3**, and **9 of 9** for persistence across later turns |
| The phase's own paragraph **prefixed to a text cue** in one user turn | **18 of 18**, against **0 of 6** for the control with no prefix |
| A `role: "model"` turn framing the new instructions as the model's own | **0 of 3** |

So `UpdateInstructions` **writes no frame**. It keeps the part of the new
instructions the session was not started with — the phase's own words, found by
taking off the prefix the two share, by rune rather than by byte because half
the instructions in this repository are Chinese — and the next thing this client
says to the model carries it: a tool result through its hint, a keypress or a
dead-air cue through the turn that reports it.

The shared prefix counts only when it is the **whole** of the original. A flow
moves between phases by appending to its persona; instructions that diverge part
way through a sentence were rewritten rather than extended, and half a sentence
is not a phase.

**NOT MEASURED**: two phase changes in one session, and a phase change that
conflicts head-on with the one before it on the no-input path. §4.

### 2.7 The caller is listened to in the one window where nobody else is

**MEASURED, by absence**: this protocol says nothing about the caller until it
has recognised what they said, which on a real call is seconds later. There is
no speech-started and no speech-stopped; `inputTranscription` has, in DOC's own
words, "no guaranteed ordering" relative to anything. Two things above the
boundary need to know sooner: the call's dead-air timer gives up on a silent
caller after eight seconds and cannot otherwise tell silence from someone
talking, and the turn-latency measurement has no start point.

So there is an energy detector inside the package — and it is armed **only**
when the server has said the turn is over, or when a turn produced nothing to
play, and disarmed the instant the model produces anything. While the model is
speaking, or while what it said is still reaching the caller's ear, barge-in
belongs to the server's `interrupted` and to nothing else. This detector never
sends a frame, never interrupts a turn and never takes part in deciding whose
turn it is. It runs inline in `SendAudio` on the call's own goroutine, with no
goroutine of its own, and allocates nothing per frame.

**NOT MEASURED**: its thresholds. They are unmeasured defaults, named and
gathered in one place precisely so that moving them is a one-line change.
**W-G3**.

### 2.8 Failure is a close code, and one of them has a name

**MEASURED**: every rejection arrives as a **WebSocket close code and a reason**,
and nothing else. `1007` for a field the setup may not carry (the reason names
it — that is how `thinkingConfig` and a top-level `responseModalities` were
ruled out), `1008` for a model or a credential. The reason is **truncated at
123 bytes** by the protocol itself, so the client's own bound repeats that
number rather than inventing one. **MEASURED**: a client-initiated close with
code 1000 is echoed back in **≈220 ms**.

There is **no reconnect**, for the reason this repository has never reconnected
any provider mid-call: the conversation state is unrecoverable. A lost socket is
a fatal `ERROR`, a call released as `FAILED`, and the caller rescued to the
DID's fallback queue.

**One ending is different enough to name.** DOC: the connection's lifetime is
capped (documented at around ten minutes) and `goAway` precedes it with a
`timeLeft`. Nothing is broken there — not the network, not the credential, not
the engine — and a deployment seeing it has conversations outliving a cap, which
is answered by what the flow asks of a caller rather than by fixing anything.
Read as `MEDIA_OR_PROVIDER_FAILURE` in a CDR it is indistinguishable from a
socket that died. So:

- `provider.Event.FailureCause` carries **`PROVIDER_SESSION_EXPIRED`**, in this
  repository's terms rather than a vendor's, and `internal/aicall` uses it as
  the hangup cause. `hangupCause` is a free string, so there is no contract
  change and no migration.
- **`aicc_provider_sessions_expired_total{provider}`**
  (`internal/obs/callmetrics.go`) counts it **beside**
  `aicc_provider_ws_errors_total` and not instead of it: it is still a session
  that ended on an error, and an operator watching that total should not have to
  know which engines cap a session to read it.
- `goAway` is treated as fatal **at once** rather than at the deadline: the call
  has to be routed somewhere a person can take it, and every second spent
  waiting for a connection that is going to close is a second of that transfer
  not happening.

**MEASURED, and the gap in all of the above**: `goAway` was **never observed**,
including in a session held open for **17.4 minutes** — past both documented
limits. The whole path is therefore exercised by **synthetic frames only**.
**W-G5**.

### 2.9 What the service sends that no document mentions

All **MEASURED**, all of it handled rather than assumed away:

- Server frames arrive with the **binary** opcode carrying pretty-printed JSON.
  The client reads the bytes and does not look at the opcode, which is what both
  official SDKs also do.
- **Bare `{}` frames** arrive. They mean nothing and are ignored.
- **`sessionResumptionUpdate` arrives unsolicited**, although
  `setup.sessionResumption` was never sent. It is ignored: this client does not
  resume.
- **`outputTranscription` arrives even when it was not asked for.** That is what
  was measured, and it is measured about the **output** side only: this client
  has asked for input transcription in every session it has ever opened, so
  whether the caller's words would arrive unrequested too is **NOT MEASURED**.
  Both configs are sent regardless — an empty object is how this protocol says
  "on, with your defaults" — because without them there is no record of what
  either side said, and because the input one is also where the call's language
  is declared (`languageCodes`, §3).
- **`interactionStatus` and `waitingForInput` were never seen.** Both are
  documented; neither is depended on.

### 2.10 The bot has to be told to speak first, and it says lines as written

**MEASURED**: this model **never greets unprompted**. DOC says the same and
gives the remedy as a system-instruction directive; what actually works is
simpler and is what the client does — a `clientContent` with an **empty turn
list** and `turnComplete: true`, which produced a greeting from the instructions
in **971 ms**, in **3 of 3** English sessions and **3 of 3** Chinese ones with a
cue variant.

**MEASURED**: a line asked for verbatim comes back verbatim — **10 of 10**,
including Chinese and including an alphanumeric reference id, which is the case
a model paraphrases if it is going to. First audio ≈**800 ms**. That is what
makes `RequiresTerminalAnnounce` false here: an `announce` is best effort on
this engine in exactly the way it is best effort on the Realtime client, and a
flow's closing line arrives intact.

**DOC**: the native-audio models "don't support explicitly setting the language
code", so `speechConfig.languageCode` is not sent and **the flow's language
reaches the model only through its instructions**. That is a property a
deployment has to know before it writes a bilingual flow for this provider.

## 3. Live verification, 2026-09-20

### 3.0 The confound, before anything else

**The network path from this machine to the endpoint was not a controlled
condition.** The socket errors both rounds produced came out of it, and nothing
about it was held fixed or characterised. Everything below about the
**uplink** — the blocked writes, the dropped caller audio, the calls lost to a
write timeout, and the turn latencies measured while any of that was happening —
is a measurement of this client **on this path**, and the path is part of the
result. Nothing here establishes that the provider is the cause; §2.5 used to
claim it did, and no longer does (**W-G10**).

**Per owner decision (2026-09-20) the run is to be repeated from another host,
and the cause is to be established before any remedy is chosen.** §3.5 lists the
numbers that re-run has to produce. What is **not** contaminated by the network
path is the part of the record that does not depend on the uplink: the greeting,
whether lines are spoken as written, phase changes, the
transcription language, the dead-air path, and — where the uplink was
demonstrably healthy for the ten seconds before it — barge-in.

### 3.1 Method

Two rounds of real telephone calls on 2026-09-20, into the development
FreeSWITCH on a developer's machine, with a human caller and the seeded bilingual
flows behind **95002 / 95012** (Chinese) and **95001** (English). Every figure
below is **MEASURED**.

**The gate**, unchanged from the one this section used to state as forthcoming and
the same one the doubao client was held to: at least **10 real-voice
interruptions** over the dev trunk, cut **p50 ≤ 1.0 s**, cut **p90 ≤ 1.5 s** and
**zero misses**; qwen and doubao still answering calls on the same build with no
new WARN or ERROR; and the rest of the record in the shape of
`doubao-findings.md` §10.

Barge-in and playback were measured in the recordings and not in the log:
two-channel 8 kHz captures, channel 0 the caller and channel 1 what the caller
heard (digital zero when nothing is queued), read on **20 ms RMS envelopes**. A
**cut** is caller onset → channel 1 falling to zero. Every
`the provider took the floor back` line was matched to a cut measured that way,
and the `playedMs` the client reports agrees with the measured run of audio to
within **1–3 frames**, which is what makes the log usable as evidence for the
rest.

### 3.2 Round 1 — eleven calls, the client as first committed

Build `d9665d2`, the tip of `abeab35..d9665d2`: the client and its record, with no
uplink instrumentation in it at all. That is the first thing this round changed.

**Greeting**, conversation started → first bot audio reaching the caller:
**779–1044 ms** over 11 calls, median **913 ms**.

**Barge-in: the gate FAILED on misses.** Real-voice cuts **n=13** (8 of them from
one call): **p50 420 ms**, **p90 1296 ms**, **max 2940 ms** — the latency
criteria pass comfortably. Against them:

- **2 misses.** The caller spoke for **1520 ms** and for **620 ms** and the bot
  ran on for **2180 ms** and **5900 ms** respectively, with no `interrupted` at
  all.
- **3 `interrupted` frames with the caller in digital silence** — the server
  deciding it had been interrupted when the caller channel shows nothing. One of
  them destroyed a reply **80 ms** after it started.

**The flush is clean.** Residual audio after an `interrupted` was **0–140 ms**,
and in no case did a sentence resume after the cut. That half of §2.3 holds over
a telephone leg.

**The uplink, unseen.** One call dropped **1877 of 8409 caller frames** — 37.5 s,
**22 %** of everything the caller said — and the only trace of it was the closing
stats line after the call had ended. Another died on `write tcp … i/o timeout`;
the caller was rescued to the DID's fallback queue and the CDR reads **FAILED /
MEDIA_OR_PROVIDER_FAILURE**, which is the designed behaviour working on an
undiagnosable failure.

**A 64.6 s stretch of silence toward the caller**, during which the model issued
four tool calls and produced no speech at all. Nothing was wrong with the socket
and nothing in the log said anything: a blocking tool call that the model does
not narrate is silence the caller has no explanation for.

**`transfer_to_agent` was dispatched three times** for one request, each dispatch
re-arming the 10 s action cap, so the transfer happened about **19 s** after the
caller asked for it.

**Input transcription was in the wrong language on ≈27 % of caller lines** —
Spanish, Italian and Hindi tokens for callers speaking Chinese and English.
Nothing was telling the recogniser what to expect. The bot's own language was
never wrong: English held on 95001 across the call, steered by the instructions
alone as §2.10 says it must be.

**Everything else behaved.** `turn latency` lines agree with the recordings
(recording − log ≈ **+482 ms**, which is the in-package detector's 500 ms hold,
§2.7). Dead air fired once and correctly. No watchdog stall, no stale tool id, no
`RESPONSE_STARTED` left unclosed.

### 3.3 What was changed between the rounds

Three commits, all from round 1's record: `083069c` made a stalled uplink say so
while the call is happening, `b67eb62` turned a tool answer the model never picks
up into a reported stall rather than silence, and `a68b00e` told the transcription
which language the caller speaks (`inputAudioTranscription.languageCodes`).
Round 2 ran on `a68b00e`.

Round 2's own logs then produced one more change: the uplink instrumentation
reported every drop episode **twice**, and in **31 of 31** episodes the pair
landed in the same millisecond carrying the same count, because frames are
dropped while a write is blocked and are discovered only when it returns — by
which time the "stall" a recovery line announced the end of was over. `e5ed1a8`
made it one line per episode, said by the write that lived through it, carrying
`blockedMs`, `framesDropped` and `backlogFrames`.

### 3.4 Round 2 — seven calls, build `a68b00e`

**The uplink, now visible — and in trouble.** **5 of 7** calls stalled:
`slowWrites` **7–40** per call, worst single write **3564–5001 ms**, backlog up to
**440 frames** (8.8 s of audio; the figure spans both the queue and the batch a
blocked tick is still writing, so it can exceed the 250-frame queue), frames
dropped **45–2454** — the worst call lost **49.1 s**, **33 %** of what the caller
said. **2 of 7** calls ended on a write timeout. Stall onset, measured from the
session opening: **29.7 / 32.9 / 37.1 / 44.0 / 52.1 s**. And **2 of 7 calls, of
69 s and 120 s, had zero slow writes** under the same conditions on the same
machine — which is why §2.5's attribution is now open rather than settled.

**Barge-in: the gate FAILED on misses again**, and this time on a healthy
uplink. Every cut was classified by what the uplink had been doing in the
preceding 10 s, because a cut measured behind a stall measures the stall. On a
**healthy** uplink: **n=10** (6 of them from one call), **p50 420 ms**, **p90
560 ms**, **max 740 ms** — the same picture as round 1 and tighter. Against that:

- **1 miss with the uplink demonstrably healthy.** The caller spoke for
  **2480 ms** at RMS **4800–9900** — not a fragment and not quiet — and the bot
  ran on for **4280 ms**. The first slow write of that call came **20.8 s
  later**. Nothing about the transport explains it.
- **1 false `interrupted` with a silent caller**, **7.6 s** after the caller's
  last speech. The measured lag on that call at that moment was **≤ 1.34 s**, so
  it is not explained by audio arriving late through our own send path either.
  Whatever the server was reacting to, buffering beyond that path is invisible
  from here.

**Turn latency**, as the `turn latency` line's `totalMs` records it. The one call
with a clean uplink throughout (120 s, 8 turns) ran **663–1615 ms**. Over the
healthy-uplink set, **n=15, p50 1615 ms**, with a tail of **3972 / 5575 /
9134 ms** that **no WARN explains** — no slow write, no drop, no tool stall. The
degraded set (n=3) reaches **47075 ms**, which is the transport and says nothing
about the engine.

**The tool-answer stall fires, and names a silence it cannot fill.** `turn
abandoned reason="the provider never answered a tool result"` **6 times across 4
calls**, one of them on a healthy uplink. It is doing its job — the silence is in
the log where round 1 had nothing — but **nothing speaks in its place**: on one
call the farewell never played, the caller heard **21.2 s** of silence, and the
line then dropped. Filed as **W-G6**.

**Input transcription, after `languageCodes`.** Wrong-language lines: **0 of 25**
on the Chinese DIDs. On English, **1 of 4** came back as Korean — n is too small
to say anything, and §3.5 asks for fifteen.

**Dead air** fired **3 times**, every time with a genuinely silent caller. The
check-in line reached the caller **1.56 s** later on the clean call and **7.0 /
9.4 s** later on stalled ones.

**Phase lines after a transition were spoken** on both transfer calls — round 1's
failure of the same check passed here.

**Two defects that are not this client's.** Both filed as **W-G7**:

- An **armed hangup fired although the conversation had moved on**: the caller
  asked for more service, the bot answered with a question, and the call was cut
  anyway.
- A **duplicate `take_message`** created **two OPEN callbacks** for one caller,
  the same duplicate-dispatch shape as round 1's triple `transfer_to_agent`.

**And one that is the platform's**, filed as **W-G8**: single 20 ms frames of bot
audio in which every sample is `−1`, at **≈6 per 1000 frames** on clean-uplink
gemini calls. The same signature is in earlier **qwen (1.90 per 1000)** and
**doubao (5.20 per 1000)** recordings, so it is the shared RTP or recorder path
and not this client.

### 3.5 Verdict, and what the re-run has to measure

**The acceptance gate is NOT met.** Its latency criteria (cut p50 ≤ 1.0 s, p90 ≤
1.5 s over ≥10 interruptions) passed in **both** rounds, comfortably. Its
**zero-miss** criterion failed in both: 2 misses in round 1, 1 in round 2 — and
the round 2 miss was on an uplink that was demonstrably healthy for the following
twenty seconds, so "the audio arrived late" does not account for it. The **qwen
regression has been run** — 2026-09-20, recorded in
[qwen-findings](qwen-findings.md): the same gate was **met** there, and
`SayExactly` measured **4 of 7** on mid-call lines against 8 of 8 on the
greeting (its **W-Q1**). The **doubao regression has still NOT BEEN RUN**. What
that run does and does not cover follows from what is actually shared: qwen and
this client have **four** pieces in common — `wsconn`, `Watchdog`, `MergeHint`
and `SayExactly` — and all four were exercised over real telephone calls on
2026-09-20, so the refactors that moved them out are now covered from both
sides. The **pacer is the exception**: it sits under doubao and gemini and has
never sat under qwen, which takes the Realtime client's unpaced uplink, so it
remains covered by those two alone and by doubao only once that run happens.

**Per owner decision (2026-09-20), the cause comes before the remedy.** The run
is to be repeated from another host, and until that has happened:

- **G7 stands.** No reconnect and no resumption, despite two calls lost to write
  timeouts. A client that reconnects around a broken path hides the path.
- **Detection during bot speech stays the server's alone** (§2.7, G4). The misses
  argue for a client-side detector the way doubao's did (W-D3), and that argument
  is not answered until it is known whether this client was hearing the caller at
  all.

What the re-run must produce, per call unless stated:

- `slowWrites`, `maxWriteMs`, `maxBacklogFrames`, `framesDropped` — the four
  figures the closing line now carries.
- **Stall onset**, seconds from the session opening, if there is one.
- The **count of calls ending on a write timeout**.
- **Barge-in gate statistics over ≥ 20 cuts across ≥ 4 calls**, classified by
  uplink state as in §3.4.
- The **healthy-uplink turn-latency tail** — whether 3972 / 5575 / 9134 ms
  survive a clean path.
- The **count of tool-answer stalls**, and whether any remain on a healthy
  uplink.
- **Dead-air check-in latency**, line entry → audio at the caller.
- The **dropout rate** (`−1`-sample frames per 1000), to confirm W-G8 is
  path-independent.
- **≥ 15 English caller lines**, for the transcription-language check that n=4
  could not settle.

No figure from this section goes anywhere else in the repository — not the
README, not the deployment guide, not `.env.example`, not a release note, not a
commit message.

## 4. Known gaps, stated plainly

- **`aicc_turn_latency_ms` on gemini starts from the in-package detector's
  `SPEECH_STOPPED`**, not from anything the provider said, because the provider
  says nothing. The number is therefore as good as the detector's thresholds,
  and those thresholds are unmeasured defaults (§2.7, W-G3). Comparing this
  provider's turn latency against another's is comparing two different
  measurements until that is settled.
- **Two phase changes in one session are untested**, and so is a phase change
  that conflicts head-on with the one before it on the no-input path. The
  mechanism carries the most recent phase and nothing else, which is believed
  right and is not demonstrated.
- **`audioStreamEnd`'s effect is unmeasured.** It is sent on DOC's word (W-G4).
- **A tool can be dispatched twice.** When the caller interrupts while a call is
  outstanding, the server discards it silently and re-issues it under a new id
  (§2.4) — so a tool with a side effect runs a second time. Nothing in the
  protocol distinguishes the re-issue from a fresh call, and no tool this
  repository ships is unsafe to repeat. §3 watched it happen twice with
  consequences a caller can see — a transfer 19 s late and two OPEN callbacks for
  one request — so it is no longer only a caution: **W-G7**.
- **A tool-answer stall names the silence and does not fill it** (§3.4, **W-G6**).
- **The barge-in gate is not met** (§3.5): three misses over the two live rounds,
  one of them on a healthy uplink, with the latency criteria passing in both.
  Whether the answer is a client-side detector is deliberately not decided yet.
- **Whether the blocked writes are the provider's or the network path's is
  unknown** (§2.5, §3.0, **W-G10**), and every uplink figure in §3 inherits that.
- **One session per call, capped by the provider's connection lifetime, with no
  resumption** (G7). A conversation that outlives the cap is released with
  `PROVIDER_SESSION_EXPIRED` and the caller is rescued; it is not continued.
  That path has never run against the real service (§2.8, W-G5).
- **The sample counts are small** — single figures almost everywhere in the
  probe, eighteen phone calls in §3, one afternoon, one machine, one region, one
  network path. Every number here should be read as "this is what it did", not
  "this is what it does".

## Follow-ups

Eleven items, filed here because this document is what created them. W-G6 to
W-G10 are §3's, and two of them are not this client's; W-G2 is answered and its
remainder is W-G11.

**W-G1 — one provider registry.** This is `doubao-findings.md`'s W-D1, now due:
it was deferred because "the right shape of it is clearer with a third protocol
in hand than with a second", and the third protocol is in hand. A provider name
is still registered in two places — `provider.ProfileFor` for its profile and
`cmd/aicc/wiring.go` for its client — and two registration points for one
concept is how a name comes to have a profile and no client, or a client and no
`.env.example` line. The replacement is a single registry of name → `Profile` +
factory, read by the composition root. With three clients the requirements are
now visible: the registry has to carry a name, a profile constructor and a
factory; it cannot live in `internal/provider`, because two of the three
factories are in sub-packages of it; and the composition root is the only place
that can populate it. Not done in the same commit as the third client, for the
same reason it was not done with the second: a refactor mixed into an addition
is a refactor nobody reviewed.

**W-G2 — the lowercase schema is NOT refused; what is unmeasured is now
narrower. ANSWERED 2026-09-20, and re-filed as W-G11.** The question was whether
the service rejects the JSON Schema casing every flow in this repository is
written in, which is what `geminiSchema`'s rewrite was written for. It does not:
3 runs of 3 of `TestLiveGeminiRefusesASchemaInTheCasingTheFlowsUse` — despite its
name — logged `live_test.go:169: the service ACCEPTED the flows' own casing and
answered {…}` (§2.4). Proto3 JSON matching enum names case-sensitively is a true
statement about proto3 and was the wrong prediction about this endpoint.

Two things that answer does **not** say, which is why the rewrite stays and why
this item has a successor rather than a grave:

- What was measured is **acceptance of a setup frame**. Nothing has been measured
  about whether the model *calls* a tool the same way, with the same argument
  types and the same coercions, when its declaration went up in lowercase. A
  schema the service parses leniently at setup is not a schema it necessarily
  reasons over identically.
- Every real call in §3, and every measurement in this document, was made with
  the rewrite in place. Removing it would put this client's whole live record
  behind a configuration nothing has ever run a conversation on.

**W-G11 — settle whether the lowercase declaration behaves identically at tool
call time, then delete the rewrite or say why it stays.** The measurement is a
live session per spelling, on the same flow, each driven to an actual
`toolCall`: same tool chosen, same argument names, and argument values of the
same JSON types (a `string` that arrives as a number is exactly the failure this
is looking for). If they match, `geminiSchema` becomes a no-op worth deleting and
the type-name table in it goes with it; if they do not, the difference is the
reason it exists, and it belongs in §2.4 in place of the prediction that was
wrong.

**W-G3 — tune the detector's thresholds from real recordings.** `speechFloorRMS`,
`noiseFactor`, `noiseAdapt` and `framesToStart` (`detector.go`) are defaults
chosen to be defensible, not defaults chosen from data. §3's recordings are the
data, and they now exist: the same two-channel captures that measured the
barge-in gate also say what a telephone leg's noise floor is on this trunk and
how much sustained energy a real caller's first syllable carries. Retune from
them — the misses in §3.2 and §3.4 are the reason this is worth doing before
anything else in the detector is changed — and record the numbers here rather
than in the code comment.

**W-G4 — measure what `audioStreamEnd` does.** DOC says it flushes cached audio
and bypasses the server's silence timer; the probe never isolated it. Two
sessions — one sending it after a pause, one not — and the difference in time to
the first byte of the answer is the measurement.

**W-G5 — the connection-lifetime path has never run for real.** `goAway` was
never observed in 17.4 minutes, past both documented limits, so
`PROVIDER_SESSION_EXPIRED`, its metric and the rescue that follows it are
exercised by synthetic frames only. A session deliberately held open until the
provider ends it — and a check that the caller does reach the fallback queue —
is what turns that path from tested to verified. It belongs with §3.

**W-G6 — a turn abandoned after a tool answer leaves the caller in silence.**
`b67eb62` made the stall visible and §3.4 watched it fire six times on four
calls; on one of them the farewell never played and the caller heard 21.2 s of
nothing before the line dropped. Naming the silence is not filling it. Two parts:
the watchdog has to hand the phase's own words to the caller when the model
answers a tool result with nothing — the flow already has a line for the case,
and `SpeakText` already pre-empts — and a **content-free `generationComplete`
arriving after a tool answer** is not treated as a stall at all today, so the one
shape that ends the turn cleanly with no audio in it slips past the check. Both
belong to this client.

**W-G7 — an armed terminal action fires after the conversation has moved on, and
a duplicate dispatch re-arms it.** Not this client's, and not gemini's: it is
`internal/aicall` and it applies to every provider. §3.4 saw a hangup fire
although the caller had asked for more service and the bot had answered with a
question — the action was armed by an earlier tool result and nothing disarmed it
when the conversation continued. The second half is the same defect from the
other end: a tool dispatched twice (§2.4, §4) re-arms the action cap each time,
which is how one request became a transfer ~19 s late in §3.2 and two OPEN
callbacks for one caller in §3.4. An armed action needs an owner — the turn that
armed it — and a dispatch of a tool already outstanding needs to be recognised as
the same request rather than a new one.

**W-G8 — 20 ms of `−1` samples in the bot's audio, on the shared path.** Single
frames in which every sample is `−1` appear in the recordings at ≈6 per 1000
frames on clean-uplink gemini calls, and with the same signature on earlier qwen
(1.90) and doubao (5.20) captures. Three clients on three protocols do not share
a bug; what they share is the RTP send path, the framer and the recorder. This is
a platform follow-up and explicitly **not** a gemini defect — where the frames
are minted is the first thing to establish, because a recorder artefact is
audible to nobody and a TX artefact is audible to every caller.

**W-G9 — `cdrs.user_data` carries no bot summary on a call that was not
transferred.** Observed across §3's calls: where a transfer happened the
business data the flow gathered is on the CDR, and where the call ended with the
bot — a `hangup` tool, a caller hanging up — it is not. Whether that is intended
is genuinely unclear: the summary exists to brief the agent who takes the call,
and there is no agent on a call that never left the bot. It is filed rather than
fixed because the answer is a product decision, not a defect report, and because
a supervisor reading a bot-only CDR today learns nothing about what the caller
wanted.

**W-G10 — establish where the blocked writes happen, before anything is done
about them.** This is the owner's decision of 2026-09-20 and it gates the
remedies. Every Gemini session this repository has ever opened — the §2.5 probe
and both §3 rounds — ran from the same development machine over the same
uncontrolled network path; the Qwen comparison, clean on the same harness and
the same machine, is a different endpoint and settles nothing by itself. A
network path that buffers or re-establishes a TCP connection produces the
signature exactly: multi-second writes, a full queue, `write tcp … i/o timeout`,
and two calls out of seven with none of it. The measurement is §3.5's list,
taken from another host.
Until it exists: G7 stands (no reconnect), the queue stays as it is, and no
figure from §3's uplink section is quoted as a property of this provider.
