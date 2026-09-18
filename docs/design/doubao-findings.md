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

**No latency or capacity figure appears in this document.** The timings the
probe recorded are real but they are one laptop against one region on one
afternoon, and the repository publishes no performance claim before a benchmark
that deserves the name (CLAUDE.md, owner directive 2026-08-16). The numbers that
gate barge-in are recorded after the live verification, in their own commit.

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

## 4. Barge-in is the provider's, and the one open question

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

**MEASURED, and the reason this section is not finished**: in one probe of
three identical attempts, a caller talking over a reply was **never detected**
— no transcript, no `response.done`, the reply simply continued. One
observation on a laptop microphone is not a defect rate, and it is not the line
a real call runs on either. The acceptance gate is a live verification over a
real telephone leg, and **its numbers are recorded when it has run**, here, in
its own commit. If that gate fails, the follow-up below is the answer.

## 5. There is no text cue, and that reaches the flows

**MEASURED**: a lone `conversation.item.create` with `role: "user"` is silently
dropped — no acknowledgement, no reply, with or without an audio-buffer commit.
Only user-and-assistant pairs are accepted, as history. There is no way to put
words in front of this engine and have it take a turn.

Consequences, in order of how far they travel:

- `SendUserText` returns `ErrTextCueUnsupported`, a typed error. Both callers
  above the boundary — the keypress report and the dead-air re-engagement cue —
  already log and continue, so nothing above changed. Those two features are
  simply **unavailable on this provider**.
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

## Follow-ups

Three items, filed here because this document is what created them.

**W-D1 — one provider registry.** A provider name is registered in two places
today: `provider.ProfileFor` for its profile and `cmd/aicc/wiring.go` for its
client. Two registration points for one concept is how a name comes to have a
profile and no client, or a client no `.env.example` line. The replacement is a
single registry of name → `Profile` + factory, which the composition root reads;
it is deferred rather than done because the right shape of it is clearer with a
third protocol in hand than with a second, and because doing it in the same
commit as the second client would have mixed a refactor into an addition.

**W-D2 — `internal/provider/realtime.go` can lose the final `CLOSED`.** Found
while building this client, in the existing one: `emit` selects on the events
channel and on the session being done, so a `Close` racing the read loop's last
send can win the select and the terminal `CLOSED` event is dropped instead of
delivered. The Doubao client is written so that its read loop is the sole closer
of the channel and `CLOSED` is its last act, which is the shape the Realtime
client should have too. It needs its own test — one that loses the race
deterministically — and then the fix. Not done here: it is a bug in a different
client, and a fix smuggled into this commit would be a fix nobody reviewed.

**W-D3 — in-package voice activity detection, if the gate fails.** Only if the
live barge-in verification in §4 does not pass: an energy-based detector inside
`internal/provider/doubao` raising `SPEECH_STARTED` and sending
`response.cancel`, rather than waiting for the server to notice. It is a real
cost — a detector is a thing to tune and to get wrong — so it is contingent on
measurement and not on preference, and it stays inside the package, because
where the caller was heard is a property of the protocol and not of the seam.
