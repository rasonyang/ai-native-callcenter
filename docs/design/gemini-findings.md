# Gemini provider — findings (2026-09-19)

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
  scratch harness with no repository change, on a developer's machine over a
  domestic connection. Raw frames were captured in both directions. Sample
  counts are given with every figure because most of them are small.
- **LIVE TEST** — `internal/provider/gemini/live_test.go`, gated on
  `AICC_LIVE_PROVIDER_TEST=1`, run once against the real service after the
  client was written.

Each fact below is marked **DOC** (stated by one of those pages), **SDK**,
**MEASURED** (observed in the probe or the live test) or **NOT MEASURED**. Where
DOC and the probe disagree, the probe wins and the disagreement is the finding.

**Every figure in this document stays in this document.** The probe's timings
are one laptop against one region on one afternoon, with sample counts in single
figures. They are the record of what was verified and what it did, not a
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

The nine decisions the client was written under, as given:

- **The model is a constant in the client**, `gemini-3.8-live`, and never
  `gemini-3.8-live-extended-thinking`. **DOC**: on the extended-thinking model
  `turnComplete` "no longer indicates that the model is idle", blocking tool
  calls "return a hard error", and function scheduling is unsupported — three
  things this client is built on. `AICC_PROVIDER_MODEL` therefore moves nothing
  and the profile says so.
- **Server VAD only.** `automaticActivityDetection` stays enabled and
  `activityHandling` is sent as `START_OF_ACTIVITY_INTERRUPTS` — the documented
  default, sent anyway, because barge-in is not something to inherit quietly.
  Manual VAD (`activityStart`/`activityEnd`) is not used.
- **Every tool declaration carries `behavior: BLOCKING`** (§2.4).
- **The opening line and `SpeakText` are best effort**, asked for as words to
  repeat through `clientContent`, exactly as on the Realtime client and with the
  same shared demand (`provider.SayExactly`). `RequiresTerminalAnnounce` is
  therefore **false**: this engine speaks a line it is given.
- **No session resumption and no reconnect** (**G7**). The connection's lifetime
  running out is a fatal ending with a release reason and a metric of its own
  (§2.8), not something to paper over. `sessionResumption` and
  `contextWindowCompression` are not sent at all.
- **The pacer is shared only if it is the same mechanism**, which was the
  condition it had to pass and did, as a parameterised one (§2.5).

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
a field silently dropped. Whether the lowercase spelling every other provider
here takes is actually *rejected* was not established: see **W-G2**.

### 2.5 The uplink stalls, so the queue is deep and drains whole

**DOC**: 20–40 ms chunks, do not buffer a second of audio, nothing about pacing
in real time. **MEASURED**: bursts and a paced stream are **accepted and
transcribed identically** — this service has no opinion about cadence, which is
the exact opposite of the other non-Realtime client.

**MEASURED, and the finding that shaped the package**: this provider's sockets
**stall a single write**. In **3 of 4** Gemini sockets a write took between
**2.3 s and 3.6 s**, first occurring **23–42 s** into the session, with
cumulative lag of **5.4–18.1 s at the 90-second mark**. The same harness against
Qwen was clean on **4 of 4** sockets, maximum single write **17.9 ms**.

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
- **`outputTranscription` arrives even when it was not asked for.** The client
  asks for both transcriptions anyway — an empty object is how this protocol
  says "on, with your defaults" — because without them there is no record of
  what either side said.
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

## 3. Live verification (§3)

**NOT YET RUN.**

Nothing in this section has been measured over a telephone leg. The acceptance
gate, in the same form the doubao client was held to:

- **Barge-in**: at least **10 real-voice interruptions** over the dev trunk, with
  a cut **p50 ≤ 1.0 s**, a cut **p90 ≤ 1.5 s** and **zero misses**. A cut is the
  interval from speech onset on the caller channel to the bot channel falling to
  digital zero, read on a 20 ms envelope from a two-channel recording, and every
  `interrupted` in the log must match a cut measured that way.
- **Regressions**: qwen and doubao, same build, same flows, still answering
  calls with no new WARN or ERROR — the shared pacer and `SayExactly` moved
  under both of them.
- **The rest of the record**, in the shape of `doubao-findings.md` §10: stage
  timings relative to flow entry, the greeting and the farewell spoken exactly
  once each and not paraphrased, turn latency as the `turn latency` line records
  it, tool round trips, the uplink's sent/dropped counts, dead air, DTMF, and
  goroutine counts before and after.

Until that has run, this document's figures are a probe's and nothing more, and
no figure from here — measured or forthcoming — goes anywhere else in the
repository.

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
  repository ships is unsafe to repeat. A deployment whose tools are not should
  know this.
- **One session per call, capped by the provider's connection lifetime, with no
  resumption** (G7). A conversation that outlives the cap is released with
  `PROVIDER_SESSION_EXPIRED` and the caller is rescued; it is not continued.
  That path has never run against the real service (§2.8, W-G5).
- **The probe's sample counts are small** — single figures almost everywhere,
  one afternoon, one machine, one region. Every number here should be read as
  "this is what it did", not "this is what it does".

## Follow-ups

Five items, filed here because this document is what created them.

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

**W-G2 — is the lowercase schema actually refused?** The client rewrites every
JSON Schema `type` to this API's protobuf enum spelling because proto3 JSON
matches enum names case-sensitively, and the live test proves the **uppercase**
form is accepted. It does not prove the lowercase form is rejected — that probe
was never run. If the service in fact accepts both, the rewrite is still correct
but is insurance rather than a requirement, and saying which it is belongs in
this document. One live session with one lowercase declaration settles it.

**W-G3 — tune the detector's thresholds from real recordings.** `speechFloorRMS`,
`noiseFactor`, `noiseAdapt` and `framesToStart` (`detector.go`) are defaults
chosen to be defensible, not defaults chosen from data. §3's recordings are the
data: the same two-channel captures that measure the barge-in gate also say what
a telephone leg's noise floor is on this trunk and how much sustained energy a
real caller's first syllable carries. Retune from them, and record the numbers
here rather than in the code comment.

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
