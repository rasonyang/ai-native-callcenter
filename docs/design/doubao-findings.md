# Doubao provider — findings (2026-09-18)

The evidence record for `AICC_PROVIDER=doubao` and `internal/provider/doubao`,
in the shape of [m0-findings](m0-findings.md): what the client rests on, where
each fact came from, and which of them the documentation does not contain.
Design 02 §3–§4 and phase1-decisions A6 have been amended where this changed
them; this is the record behind those amendments.

**Sources.**

- **DOC-A** — 接入必读, <https://www.volcengine.com/docs/6561/2549732> (updated
  2026-09-04).
- **DOC-B** — 全双工版本 API, <https://www.volcengine.com/docs/6561/2549778>
  (updated 2026-09-11).
- **PROBE** — 28 live sessions against the real endpoint on 2026-09-18, raw
  frames captured in both directions, run from a scratch program with no
  repository change. Every session's `X-Tt-Logid` was recorded.

Each fact below is marked **DOC** (stated by one of the two documents) or
**MEASURED** (observed in the probe). Where they disagree, the probe wins and
the disagreement is the finding.

**Every figure in this document stays in this document.** The probe's timings
are one laptop against one region on one afternoon; §10's and §11's are an
afternoon each of telephone calls on a developer's FreeSWITCH. They are the
record of what was verified and what it did, not a statement about how fast or
how large this product is, and the repository still publishes no performance
claim before a benchmark that deserves the name (CLAUDE.md, owner directive
2026-08-16): no number from here goes into the README, the deployment guide, a
release note or a commit message.

## 1. Why this is a protocol and not a dialect

Not one of these is expressible as a value in a `Profile`, which is the whole of
the argument for a second client.

- **DOC** The session is bootstrapped: `session.create` and then a wait for
  `session.created` before anything else may be sent. **MEASURED** the
  confirmation echoes only the session id.
- **DOC, and by absence** There is no `response.create` in either document, and
  **MEASURED** no `response.created` in any of the 28 sessions. The engine
  answers audio. Nothing asks it for a turn.
- **MEASURED** No event announces the caller starting or stopping speaking.
  What arrives instead is the transcript of them — `transcription.started`,
  `.delta` (cumulative, not a fragment to append), `.completed` — and the
  client maps the first and last of those to `SPEECH_STARTED` and
  `SPEECH_STOPPED` because they are the only evidence there is.
- **DOC + MEASURED** A tool result is a conversation item with `role: "tool"`,
  not a function-call output, and parallel calls are answered **once**: the
  model continues only after every call in the set has a result, so the client
  holds them until the set is complete and then sends one message.
- **DOC + MEASURED** The session ends with a handshake — `session.close`, then
  `session.closed` — and a socket dropped without it is reported as an error
  against the account.
- **DOC** The uplink is a clock and a keepalive at once: frames are to be sent
  at real time, "too fast **or too slow**" is an error, and a silence nobody
  declared stops the engine answering at all (§7).

## 2. The audio format the client depends on is undocumented

**DOC-A** says PCM output is "24000Hz, mono, 16bit little-endian".
**MEASURED**: `type: "pcm"` is 32-bit float in [-1,1] — every sample finite and
in range across five sessions, and plainly not int16 when read as such. The
value that yields signed 16-bit is **`pcm_s16le`**, which appears in no
document: it is the vendor's own Go demo's default, and nothing else.

Every telephone leg in this application carries linear 16-bit, so `pcm_s16le`
is the one worth having and the client asks for it. That is a dependency on an
undocumented value, taken deliberately, with a detector rather than a hope: a
turn that produced no audio at all is a fatal error. **MEASURED** the same
guard catches the other silent failure this API has — an **invalid voice name
never errors**; `output_audio.started` and `.done` fire around zero bytes and
the caller hears nothing.

**DOC**, both ways, and **MEASURED** equivalent: the format can be set under
`session.audio.output.format` (DOC-B) or `extension.tts.audio_config` (DOC-A).
The client uses the first. With no format named at all the stream is OGG-Opus.

## 3. A turn ends on the audio, not on `response.done`

**MEASURED**: the wire's `response.done` trails `output_audio.done` by
**seconds** on a model turn, and carries usage and nothing else — no ids, no
status. Treating it as the end of the turn would hold every transfer and every
hangup behind a delay the caller can hear, because those fire on the turn after
the one that armed them.

So `RESPONSE_DONE` is raised on **`output_audio.done`**, and the wire's
`response.done` is logged for its usage figures and otherwise ignored. Its
field names are known (**MEASURED**: total, input and output tokens with a
breakdown of each) and they arrive after the turn they describe has already
ended above the boundary, which is why they are logged rather than carried on
the event.

## 4. Barge-in is the provider's, and the provider misses some

**MEASURED**: when the caller speaks over a reply, the old response's audio
deltas stop, and the response is ended by a **bare `response.done` with no
`output_audio.done`**, in the same millisecond as the new
`transcription.started`. No stale deltas follow it. That is the one shape in
this protocol where `response.done` means something, and the client reads it as
`INTERRUPTED{SPEECH}` when a turn is open and as usage when none is.

**MEASURED**: a client-initiated `response.cancel` is acknowledged with
`response.canceled` and is followed by a few stale audio deltas, which is why
the client fences deltas from the cancelled response until the next
`output_audio.started`.

**Detection is the provider's.** `Interrupt(SPEECH)` sends nothing upstream —
the server has already stopped — and only `Interrupt(DTMF)` and
`Interrupt(SYSTEM)` send `response.cancel`. There is no client-side voice
activity detection in this package.

**MEASURED, and the reason there was a gate at all**: in one probe of
three identical attempts, a caller talking over a reply was **never detected**
— no transcript, no `response.done`, the reply simply continued. One
observation on a laptop microphone is not a defect rate, and it is not the line
a real call runs on either.

**The gate has now run over a real telephone leg, and it failed.** The owner set
it at ≥10 human interruptions, a cut p50 ≤ 1.0 s, a p90 ≤ 1.5 s and **zero
misses**. Nine calls gave 17 conversational overlaps and 14 cuts: p50 **950 ms**,
p90 **1444 ms**, min 480 ms, max 2380 ms — the latency criteria passed, and
narrowly — and **3 misses**, which the gate does not allow. In the worst of them
a ~380 ms caller utterance 4.1 s into a 10.3 s bot turn was never noticed and the
bot talked on for 6.1 s; the other two ended within 380–480 ms only because the
turn was finishing anyway. §10 is the measurement.

So detection stays the provider's — nothing about the protocol changed — but the
provider is not reliable enough to be the only detector, and **W-D3 below is
required rather than contingent**.

## 5. There is no text cue, and that reaches the flows

**MEASURED**: a lone `conversation.item.create` with `role: "user"` is silently
dropped — no acknowledgement, no reply, with or without an audio-buffer commit.
Only user-and-assistant pairs are accepted, as history. There is no way to put
words in front of this engine and have it take a turn.

Consequences, in order of how far they travel:

- `SendUserText` returns `ErrTextCueUnsupported`, a typed error. Both callers
  above the boundary — the keypress report and the dead-air re-engagement cue —
  already log and continue, so nothing above changed. Those two features are
  simply **unavailable on this provider**. A consequence found later (issue #9,
  2026-09-23): the engine's NO_INPUT default for a flow with
  `global.closingTarget` (`noInput.count GTE 3 → closingTarget`) never fires
  here. Dead air is reported once per bot turn's playback; the refused cue
  produces no turn, so no second NO_INPUT follows and the count stops at 1
  until the caller speaks. An authored rule at `GTE 1` is the only silence
  rule that can end a doubao call (02-ai-voice §6).
- A phase the call does not leave must carry its own words. The profile says so
  with `RequiresTerminalAnnounce`, and a flow whose terminal phases have no
  `announce` is **refused at publish**, with a reason, rather than discovered by
  a caller listening to silence.
- An **entry** phase with no `announce` means the bot answers and waits for the
  caller to speak first. That is legal and sometimes wanted; it is written down
  because it is otherwise indistinguishable from the bug.

What *is* available is speaking a line outright. **MEASURED**:
`speech_text_buffer.commit` speaks promptly, cold and mid-call, and marks the
turn `chat_tts_text`. It **pre-empts** anything in flight — and the pre-empted
response gets no terminal event of any kind, which is why the client closes the
open turn as `INTERRUPTED` itself when the next one starts. It does **not
queue**: a second commit kills the first. And **MEASURED**, `replacement.*` is
silently ignored when nothing is in flight, so both the opening line and every
mid-call one go through `speech_text_buffer.commit`.

## 6. Failure ends the call; there is no reconnect

**DOC-A**'s error table advises reconnecting on 5xx and not retrying on 4xx.
This repository has no mid-call reconnect for any provider and gains none here:
the session's conversation state is unrecoverable, so **5xx and 4xx end the
same way** — a fatal `ERROR`, a call released as `FAILED` with
`MEDIA_OR_PROVIDER_FAILURE`, and the caller rescued to a queue.

**DOC-A** `45000003` "Abnormal silence audio" is a **ten-minute** idle release:
the server drops a connection that has gone that long without interaction. It
is handled as any other fatal error, and logged distinctly because it means the
uplink discipline in §7 failed rather than the network.

**MEASURED**: the `error` frame is `{type, event_id, error:{type, code, message}}`
— `code` is a string — and after a protocol error the server drops the socket
immediately. **MEASURED, and a trap**: a perfectly clean `session.close` can be
accompanied by spurious `error` frames of its own ("the stream is done", "no
session active"). An error arriving after the client has begun stopping is
logged and never raised, or every normal hangup would report a provider
failure.

## 7. The uplink is a clock, and silence has to be declared

**DOC**: 16 kHz mono int16 little-endian, 20 ms frames recommended; sending
faster or slower than real time is an error. **DOC-A/B**: when the client stops
sending frames it **must** send `input_audio_mute.commit`, and
`input_audio_unmute.commit` on resuming, or the engine stops answering. No
timeout is documented. The vendor's own demo sends neither.

Nothing above this package has a concept of muting, and the RTP path upstream is
arrival-driven rather than clocked, so the pacer is this client's own:

> **2026-09-19**: the *mechanism* is no longer this client's own — the queue,
> the ticker, the drop-oldest policy and the idle hook moved to
> `internal/provider/pacer` when the gemini client needed the same loop with the
> opposite policy (`gemini-findings.md` §2.5). The numbers below are unchanged
> and are now this client's parameters; the mute and unmute frames, being the
> only things in this list that are about what the frames *say*, stayed here.

- `SendAudio` never blocks; it pushes into a queue of **at most three frames**,
  dropping the oldest on overflow and counting the drop.
- One goroutine on a **20 ms ticker** writes at most one frame per tick, never
  two.
- **25 consecutive empty ticks** send `input_audio_mute.commit`; the tick that
  next finds a frame sends `input_audio_unmute.commit` and then that frame.
- **Nothing is buffered across a mute.** The mute is declared only on an empty
  queue, and the queue holds at most 60 ms otherwise, so no frame from before a
  silence can be delivered after it — audio that old is a different moment in
  the conversation.

**MEASURED**: mute and unmute are unacknowledged, five rapid cycles are
harmless, and a session held muted for a minute with nothing sent produced no
error and answered normally on unmute. **MEASURED**: the server pings on its own
and answers the client's pings, which is what lets `wsconn`'s keepalive be
shared with the Realtime client unchanged.

## 8. Close

**DOC**: send `session.close`, wait for `session.closed`, then close the socket;
a socket dropped without it is recorded against the account. No wait is
documented, so the client's bound is its own (two seconds — every caller above
passes a context with no deadline in it).

**MEASURED**: the handshake completes quickly and well inside that bound, and
the server does **not** close the socket afterwards in 27 of 28 sessions. The
client closes it. One `sync.Once` moves the session to stopping — which stops
the pacer and starts refusing audio — before `session.close` is written, so no
frame can follow it.

## 9. Quotas

**DOC-A**, vendor-documented limits and not our measurements: **60
`session.create` per minute** and **100k tokens per minute**, both per
application id. The server keeps 20 rounds of history, and instructions plus
context are capped at 12K tokens.

The first of those is why `aicc_provider_sessions_started_total` exists
(`internal/obs/callmetrics.go`). The limit is on *opening* sessions, and nothing
else this process measures would show it being approached: `aicc_calls_active`
counts how many are up, not how fast they were created, and a deployment can sit
well inside its concurrency and still be turned away at the door.

## 10. Live verification, 2026-09-18

**Method.** Nine calls with a human caller over the simulated PSTN trunk into the
development FreeSWITCH, DID 95002, flow `novanet_support`, Chinese — 435 s of
bot-phase audio in all. Every figure below is **MEASURED**. Barge-in was measured
in the two-channel recordings rather than in the log: a cut is the interval from
speech onset on the caller channel to the bot channel falling to digital zero,
read on a 20 ms envelope, and all 14 `the provider took the floor back` lines
were matched 1:1 to a cut measured that way.

The nine sessions' provider `X-Tt-Logid`s:
`20260918182805969D7FE8E79F856BAFCB`, `20260918183054D731BC4487E941E91FCE`,
`20260918183449A5731450751B3B6355AC`, `2026091818361989DA06FB5F5121362580`,
`2026091818364931F5382A6B1A133C9086`, `202609181837339B69074C06F3EB5896DD`,
`202609181839023DE652AA338844C287EE`, `2026091818415176395609B0B8603F6D56`,
`20260918184309BA30D30682355E6B8872`. Every one ended `CLOSED_CLEAN` — three
caller hangups, five transfers, one `hangup` tool — and none reported
`ContextCanceled`.

**Barge-in: the gate failed on misses.** 17 conversational overlaps, 14 cuts, **3
misses**; cut p50 950 ms, p90 1444 ms, min 480 ms, max 2380 ms. The gate's
latency criteria (p50 ≤ 1.0 s, p90 ≤ 1.5 s over ≥10 interruptions) passed
narrowly; its zero-miss criterion did not. Two more observations qualify the
count in both directions:

- In 7 of the 9 calls there is a burst on the caller channel 0.6–1.1 s into the
  greeting that never cut it. Whether it is speech or the transient of the call
  being answered cannot be decided from the recordings, so it is **not** counted
  as a conversational miss — but it is not dismissed either.
- Three further cases were the provider **starting** a turn while the caller was
  already speaking, then cutting it 1.3–1.5 s after the caller's onset. One of
  them appears in the application log as `the provider took the floor back`
  although the caller had the floor first: the attribution in that line is
  **unreliable on this provider**, which is worth knowing before anyone counts
  barge-ins from the log alone.

**Stages**, relative to flow entry: the provider session opens at +0.10–0.16 s,
the leg is bridged at +0.14–0.22 s, and the first greeting audio reaches the
caller at +0.42–1.00 s (median ≈0.6 s). The greeting — the entry phase's
`announce`, carried as `OpeningText` — was spoken verbatim **exactly once** in
all nine calls, and the farewell `announce` exactly once in the `hangup` call.
Neither was repeated, and neither was paraphrased by the model.

**Turn latency**, caller stopped → first bot frame, as the `turn latency` line
records it: n=17, median 645 ms, range 411–1067 ms. The turn that follows a tool
result is a different animal at 2797–3789 ms (n=6), which is the business API and
the model's second pass, not the transport.

**Tool round trip.** Executing the tool itself is 8–16 ms. Around it: the
caller's final transcript → `TOOL_CALL` is 0.51–2.39 s, and `TOOL_RESULT` → the
text of the bridging line is 0.53–1.83 s.

**Uplink pacer.** 15,619 frames sent and 181 dropped overall (1.16 %), 0.19–1.61 %
on an ordinary call — the three-frame queue doing what §7 says it does. Derived
rather than logged: 15,619 × 640 B is 9,996,160 B of PCM16 at 16 kHz, equivalent
to 2,499,040 B of G.711. The downlink converter's byte counts are logged nowhere
and are simply not available.

The DTMF call is the outlier: 435 frames sent, **77 dropped (17.7 %)**, 4 mutes
and 3 unmutes, ≈3.1 s of uplink that never arrived. That is consistent with RFC
2833 events taking the place of audio and a gap-filling burst being dropped on
resume, and it is **not confirmed** — no debug line existed that would have
settled it. All eight keypresses landed while the bot was silent, so "a keypress
cuts playback" was **not exercised**; each produced the expected WARN (`this
engine takes no text cue`, §5).

**Dead air** behaved as designed and the design is audibly thin: the
re-engagement cue is unavailable on this provider, so the caller heard **17.4 s**
of digital silence. Three short caller utterances inside that window produced no
transcript at all — the provider never acted on them. That is the residual gap
§5 could only call an assumption, now observed: an utterance the full-duplex
model does not act on re-arms nothing.

**Hold is NOT VERIFIED beyond the mute.** On hold the caller's RTP stops, the
pacer sent `input_audio_mute.commit` after ~500 ms (mutes=1), and the provider
raised no error — and then `internal/voice`'s dead-media watchdog ended the call
(`media went dead silentFor=5.45s` / `5.20s`), because `RTPDeadTimeout` is 5 s
(`internal/voice/uas.go`). That watchdog is provider-agnostic, so this is a hold
defect for every provider, filed as **W-D4**. Unmute, and "the first frame after
unmute is live", remain covered only by the package tests and the probe's
one-minute muted session.

**qwen regression**, same build, unattended loopback call, same flow: the
greeting was spoken exactly once with the same text, the caller was transcribed,
and there was no WARN or ERROR — the live qwen endpoint accepts the new
`OpeningText` frames. **Goroutines** (`go_goroutines`, ops port): 23 before and
22 after a doubao call; 22 → 23 around the qwen call.

**A defect this verification found, and it is not in the doubao client.** After
the bridging or farewell line finished playing, the caller heard 1.3–5.1 s of
silence before the transfer or the BYE. An instrumented build preserved the
timeline: `aicall.Session.watchPlayback` runs the drain watch and the dead-air
watch on one goroutine, and `awaitCallerOrDeadAir` (`internal/aicall/session.go`)
waits out the whole no-input timeout — 8 s — without listening for the next
playback marker. The marker sits in the buffered channel until that wait expires,
so an armed action fires on the timer rather than when the line's audio ended.
It is provider-agnostic and pre-existing: the same 8.000 s parks are there on
qwen. Doubao only makes it audible, because its tool turn carries no audio and
its speech arrives about five times faster than real time, so the line has long
finished playing before the wait is over. Every session test sets `NoInput: -1`,
which is exactly why the suite is blind to it. Filed as **W-D5**, and fixed in
this branch — the description above stays as the record of what was found.

## 11. Live regression, 2026-09-20

**Why.** The Gemini client landed after §10 and touched what every client
shares — `provider.Event.FailureCause`, `aicall.Session.fail`, the
orchestrator's hangup cause, `ProfileFor`, `cmd/aicc/wiring.go` — so the doubao
path was walked again on `main` at `667c1dc`.

**Method.** Twelve calls with a human caller over the simulated PSTN trunk into
the development FreeSWITCH, 531 s of bot-phase audio in all: nine on DID 95002
(`novanet_support`, Chinese), one on 95012 (`mobile_support`, Chinese), two on
95001 (`novanet_support`, English). Every figure below is **MEASURED**, from the
application log, the `transcripts` rows and the recordings. The recordings are
8 kHz two-channel — caller on one, what the caller hears on the other — read on
a 10 ms RMS envelope with silence at −40 dBFS; queue music was told from speech
by spectral flatness, and the separation is wide enough that "the bot stopped"
and "the queue began" are measured and not judged. File offsets map onto log
timestamps to about ±2 s, so durations below are exact and their wall-clock
positions are not.

The twelve sessions' `X-Tt-Logid`s:
`20260920142920D8EF0B4C1214E565FA95`, `2026092014301712D043F559680718311A`,
`2026092014323814DDDC87455577E84C99`, `202609201434135A85F1143F95EE1C3F89`,
`2026092014351545B0BF2A238500BE5A05`, `2026092014360915ADE18FE479902A5711`,
`20260920143712DDBF2EBD317451FF7883`, `202609201438105EBDD9446D8829BF9EF4`,
`2026092014393409D14794E7FE1C26D60F`, `20260920144121753C6C119F2879679DE2`,
`20260920144157C61F7A660CE153BF4B04`, `202609201442584B59F5FB640E39E9D48B`.
Every one ended `CLOSED_CLEAN` — six transfers, five `hangup` tools, one caller
hangup — with no provider error and no rescue.

**W-D5 holds.** Bot's last speech → queue music, on the six transfers: 0.34,
0.48, 0.51, 0.55, 0.56 and 0.69 s. Bot's last speech → end of recording, on the
five farewells: 0.54, 0.63, 0.72, 0.73 and 0.78 s. §10 measured 1.3–5.1 s at the
same two places. The last far-end segment is speech in every file, so none of
these is a line cut short.

**A keypress cuts playback — exercised this time.** Seventeen keypresses on one
call, three of them while the bot was speaking; each of the three stopped the
playback at once (`the caller took the floor back reason=DTMF`, `playedMs` 2800,
2340, 4300). The other half of §5 showed its cost on the same call: the caller
pressed eleven keys in ten seconds, the model never learned of any, the far
end stayed silent for 26.5 s, and the next thing in the transcript is the caller
asking whether anyone was listening.

**The DTMF pacer outlier did not return.** That call: 3,624 frames sent, 42
dropped (1.16 %), 16 mutes and 16 unmutes — the uplink did go empty under the
keypresses, sixteen times, and the pacer declared and ended each one. §10's
17.7 % stays unexplained and unreproduced. Over all twelve calls: 24,613 sent,
200 dropped (0.81 %), 0–2.31 % per call.

**Announces.** Both entry announces were spoken verbatim exactly once in all
twelve calls — `mobile_support`'s too, the first time it has been heard on this
provider — and the terminal announce verbatim exactly once in the five `hangup`
calls. English on the profile's default Chinese voice was intelligible enough
for the caller to hold both conversations; nothing more than that was measured.

**Turn latency.** Ordinary turns n=23, median 609 ms, range 376–858 ms. The turn
after a tool result n=8, 2127–4499 ms; the slowest was the English hand-over
line. The terminal announce after `hangup` n=5, 948–1131 ms, which is a
`speech_text_buffer.commit` and not a second pass of the model.

**Tools.** `repair_status` twice on one call and `transfer_to_agent` six times,
every result a `role:"tool"` item and every one picked up. The caller read the
second repair number as "IM1002" and the model asked for `RMA1002`.

**Dead air is longer than §10 found.** On the call where the caller said nothing,
the far end was silent for 27.8 s and then for 38.0 s, and both channels together
for 15.8 s, 8.5 s and 35.2 s. `dead air` fired twice and `could not prompt a
silent caller` followed it twice. §10's 17.4 s was not the ceiling; there is
none, short of the provider's ten-minute release (§6).

**A caller who is speaking can go unanswered.** New, and worse than dead air. On
the first English call the caller spoke in four stretches between 3.9 s and
13.7 s of the recording — about six seconds of speech — and the far end was
silent from 3.4 s to 22.0 s. One utterance was transcribed, at the end of that
window; nothing before it produced a transcript, a turn or an event. On the
caller-hangup call the caller spoke for the last 2.9 s and the far end was
digital zero throughout. §10 saw three short utterances lost inside a dead-air
window; this is the same gap with a caller who kept talking. Nothing in the
client can see it: with no speech events of our own (§1), an utterance the engine
ignores does not exist on this side of the socket.

**The hand-over line can be taken away, and the transfer waits for a line nobody
hears.** The same English call:

```
14:41:38.3  TOOL_CALL transfer_to_agent → ok, action armed
14:41:40.7  "I'm connecting you to our support team right now, please hold."
14:41:41.1  the provider took the floor back  playedMs=180
14:41:41.8  caller: "Hello."
14:41:42.3  "Go ahead, what can I do for you?"
14:41:43.8  "Yes, I can hear you clearly."
14:41:46.4  transferring the caller
```

The recording has no far-end audio where the hand-over line should be: the
caller was transferred having never been told. What the log supports is this
much: the armed action (`internal/aicall/actions.go`) runs on the
`PLAYBACK_DONE` of a turn later than the one that armed it, or on a barge-in
once that line's `TURN_DONE` has been seen; here the engine pre-empted its own
hand-over turn after 180 ms and opened another for the caller's "Hello", so the
line never finished, and the action ran after the playback of a later, unrelated
turn. Which of the two paths released it was **not confirmed** — no line in the
log says. It stayed inside the 10 s cap, which is why no WARN marks it. Filed as
**W-D7**.

**Two things the model did, recorded because a flow author will meet them.**
Neither is the client's.

- It said goodbye without calling `hangup`. The caller: "我说再见了，怎么还不挂电话？";
  the bot: "不好意思，再见啦。" — and no tool call. Eighteen seconds of silence
  later the caller asked again and `hangup` arrived with
  `isFarewellSpoken:"True"`, so the model knew. `novanet_support`'s rules tell it
  to call `hangup` first in so many words. On this provider the cost of that
  lapse is total, because nothing can prompt the turn that would repair it (§5).
- It transferred a caller who said "My internet is slow" with the reason "Caller
  has no internet service" and announced help with "your out-of-service
  internet". The summary an agent reads was wrong before the agent picked up.

**One server error, on a call that was not part of the twelve.** Five unattended
loopback calls preceded the human ones, silent callers on 95002. The first —
also the first session of that process — failed 4 s in: `code=55000000`, `sami
error: codes=42000999, desc=stream send timeout`, logid
`2026092014012344B12EDA9BF89AC0BCF9`, 202 frames sent and 3 dropped, no mute.
The caller was rescued to `support-zh` and the CDR reads `FAILED` /
`MEDIA_OR_PROVIDER_FAILURE`, which is §6 working. The identical call 32 s later
and the three after it closed clean, as did all twelve human calls, and the code
appears in no log from 2026-09-18. One occurrence; the cause is **not known**.

**What the shared-code changes did to this client.** Nothing a caller can hear.
Two things an operator can read, filed together as **W-D8**: this client names no
`FailureCause`, so every fatal ending — the ten-minute release included — is
`MEDIA_OR_PROVIDER_FAILURE` in the CDR and
`aicc_provider_sessions_expired_total` cannot move; and the one failed call above
counted **twice** in `aicc_provider_ws_errors_total`, because a provider error
frame and the session end that follows it are both fatal events and
`aicall.Session` counts at the one place both arrive.

**Not exercised:** hold (W-D4 is as §10 left it), a refused transfer and
`take_message`, and barge-in cut latency — 11 `reason=SPEECH` interruptions and
3 `the provider took the floor back` are in the log, and §10 is the reason
nobody should count misses from those lines. The recordings are kept; the
measurement was not made.

## Follow-ups

Eight items, filed here because this document is what created them.

**W-D1 — one provider registry.** A provider name is registered in two places
today: `provider.ProfileFor` for its profile and `cmd/aicc/wiring.go` for its
client. Two registration points for one concept is how a name comes to have a
profile and no client, or a client no `.env.example` line. The replacement is a
single registry of name → `Profile` + factory, which the composition root reads;
it is deferred rather than done because the right shape of it is clearer with a
third protocol in hand than with a second, and because doing it in the same
commit as the second client would have mixed a refactor into an addition.

> **2026-09-19 — the condition is met and this item moves.** The third protocol
> is in hand (`AICC_PROVIDER=gemini`), so the deferral's own reason is
> discharged. It is now tracked as **W-G1** in `gemini-findings.md`, with the
> requirements three clients made visible.

**W-D2 — `internal/provider/realtime.go` can lose the final `CLOSED`.** Found
while building this client, in the existing one: `emit` selects on the events
channel and on the session being done, so a `Close` racing the read loop's last
send can win the select and the terminal `CLOSED` event is dropped instead of
delivered. The Doubao client is written so that its read loop is the sole closer
of the channel and `CLOSED` is its last act, which is the shape the Realtime
client should have too. It needs its own test — one that loses the race
deterministically — and then the fix. Not done here: it is a bug in a different
client, and a fix smuggled into this commit would be a fix nobody reviewed.

**W-D3 — in-package voice activity detection. REQUIRED.** The gate it was
contingent on has run and failed on misses (§4, §10): 3 of 17 overlaps were never
detected, and in the worst of them the bot talked over the caller for 6.1 s. So
an energy-based detector inside `internal/provider/doubao` must raise
`SPEECH_STARTED` and send `response.cancel` rather than wait for the server to
notice. It is still a real cost — a detector is a thing to tune and to get
wrong — but it is no longer optional. It stays inside the package: where the
caller was heard is a property of the protocol and not of the seam.

> **2026-09-20 — §11 widens what this has to cover.** A caller spoke for about
> six seconds and the engine produced nothing — no transcript, no turn. A
> detector of our own is also the only way this client can know that happened:
> it is what would let the call path re-arm the dead-air watch on speech the
> engine ignored, and say in the log that a caller went unanswered.

**W-D4 — a held call must not be killed by the dead-media watchdog.** SIP hold —
a re-INVITE with `sendonly` or `inactive`, or simply a held call that stops
sending RTP — has to suspend `internal/voice`'s dead-media watch for as long as
the hold lasts. Today any hold longer than `RTPDeadTimeout` (5 s,
`internal/voice/uas.go`) ends the AI leg, on **every** provider; §10 watched it
happen twice. The doubao pacer's side of a hold is already correct — it mutes and
the provider is content — so this is the leg's problem, not the client's.

**W-D5 — the dead-air wait must be interruptible by the next playback marker.
DONE.** In `internal/aicall/session.go`, `awaitCallerOrDeadAir` now selects on
`s.playbackDone` as well as the timer and returns the marker it received, and
`watchPlayback` processes it on the next iteration of the loop instead of
letting it wait out the whole no-input timeout in the channel (§10). Pinned by
`TestASecondTurnsPlaybackIsNotHeldByTheDeadAirWatch`, with the no-input timeout
**enabled** — every pre-existing session test sets `NoInput: -1`, which is why
the suite never saw this — and by
`TestDeadAirIsReportedOnceAfterTheLastTurn`, which holds the other half: a turn
that interrupts the watch re-arms it, so the caller who then says nothing is
still told so, once and no earlier than the timeout.

**W-D6 — a startup line that reads as a bug (cosmetic).** `voice provider
selected … transcribesCaller=""` prints the profile's own transcription model. On
doubao there is none, so the line says `transcribesCaller=""` even when
`AICC_TRANSCRIBE_PROVIDER` is configured and the caller is in fact being
transcribed, which reads as "caller transcription off" to anyone checking a
deployment from its logs.

**W-D7 — an armed action must not wait on a line the engine took away.** §11: the
engine pre-empted its own hand-over line 180 ms in, the caller was never told of
the transfer, and the transfer ran several seconds late behind two turns of
small talk. The armed action waits for a closing line to be heard; on a provider
that decides interruptions itself, that line can be replaced without the call
path having interrupted anything. Two questions to settle before a fix: whether
a turn opened while an action is armed should be allowed to play at all, and
whether the hand-over line belongs in an `announce` on the `handoff` phase — said
as written through `SpeakText`, which pre-empts rather than is pre-empted (§5) —
instead of being left to the model. The second is a flow change and costs
nothing on the other providers. It needs a test that loses the line
deterministically first.

> **2026-09-20, later the same day — the flow half is done and measured.**
> Five of the six seeded flows already ended a transfer in a terminal
> `finish_transfer` with an `announce`; `novanet_support` was the one that left
> the line to the model, and every transfer in §11 was on it. It now has the
> same two phases as the rest (`need_transfer` as the fallback target,
> `finish_transfer` terminal and announced). Eleven transfers followed, all
> **MEASURED** in the log and, for the human calls, on the far-end channel of
> the recording: four on `mobile_support` before the change, four human calls on
> `novanet_support` after it (two Chinese, two English), and three unattended
> loopback calls replaying a recorded "转人工", two of them with the caller
> already talking when the line began.
>
> - The model's own turn after the tool result never appeared. Every call has
>   exactly one turn after `transfer_to_agent` — 131–183 tokens, the announce —
>   and no `the provider took the floor back`. Uninterrupted, the line played
>   whole (4.30–4.47 s) and queue music followed 0.47–0.61 s later. So the
>   orchestration change this item contemplated — withholding the tool result
>   while an action is armed — is **not needed**.
> - A caller who speaks over the line is transferred at once: `the caller took
>   the floor back` → `caller spoke after the closing line; running the armed
>   action now`, 0.7–1.2 s after the caller's onset, with the line cut where it
>   stood (`playedMs` 880–4440). No small talk, no deferred transfer, in seven
>   of seven. That is `onBargeIn` doing what it was written for, and it can,
>   because the announce's `TURN_DONE` arrives about 0.4 s after its first
>   frame.
> - **Still open, and smaller:** those first ~0.4 s. The earliest barge-in
>   recorded was 0.88 s into the line, so an interruption landing before
>   `TURN_DONE` — the 180 ms case of §11 — was **not reproduced** and not ruled
>   out. The `actions.go` half of this item stands for that window alone: a turn
>   later than the arming one that ends `INTERRUPTED` should run the action
>   rather than wait for a line that will not finish.

**W-D8 — what an operator reads about a doubao failure.** Two small things from
§11. The client sets no `provider.Event.FailureCause`; whether code `45000003`
(§6) should be named `PROVIDER_SESSION_EXPIRED` is a judgement — it is an idle
release and not a lifetime cap — but today the CDR cannot tell it from a fault.
And `aicc_provider_ws_errors_total` counts a failed doubao session twice, once
for the error frame and once for the session end it causes; one session that
ended badly should be one.
