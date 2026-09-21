# Qwen — live regression findings (2026-09-20)

**Qwen is not a client.** It is a `Profile` of the shared OpenAI-Realtime client
(`internal/provider/realtime.go`) — a name, an endpoint, a model and a dialect,
exactly as `docs/provider-extension.md` describes a profile. There is no
`internal/provider/qwen`, and `internal/provider/pacer` is **not** on this path:
it is imported by `internal/provider/doubao/pacer.go:10` and
`internal/provider/gemini/pacer.go:10` and by nothing else.

So this document is not a third peer to [doubao-findings](doubao-findings.md)
and [gemini-findings](gemini-findings.md). Those two record what a protocol
turned out to be. This one records what happened on one afternoon of telephone
calls against a profile that already existed, and nothing else.

**Every figure in this document stays in this document.** Eight calls, one
afternoon, one machine, one region, one network path. They are the record of
what was verified and what it did, not a statement about how fast or how large
this product is, and the repository still publishes no performance claim before
a benchmark that deserves the name (CLAUDE.md, owner directive 2026-08-16): no
number from here goes into the README, the deployment guide, `.env.example`, a
release note or a commit message.

## 1. Why the run happened

The last qwen live check was 2026-09-18, recorded in `doubao-findings.md` §10.
It was a single unattended loopback call, and it verified that the greeting was
spoken exactly once with the same text, that the caller was transcribed, that
no WARN or ERROR was logged, and that the goroutine count came back where it
started — enough to say the live endpoint accepts the new `OpeningText` frames.
That is the whole of what was known to work on this provider: one call that
never got past the greeting, on a caller who never spoke.

Since that check (`92227bc..667c1dc`) the product code qwen executes changed in
four files and no others:

- `internal/provider/realtime.go`, **+12 lines** — the rename and export of
  `SayExactly`, and nothing behavioural.
- `internal/aicall/orchestrator.go`, **+22 lines** — `FailureCause`.
- `internal/aicall/session.go`, **+65 lines** — the `243b6d0` playback fix, plus
  `FailureCause`.
- `internal/provider/profile.go` and `cmd/aicc/wiring.go` — new profiles and new
  clients for other engines, which qwen does not enter.

The `announce` feature set (`28e4215`, `cc8e7df`, `34467bd`, `ee878c3`,
`9149539`) predates the 2026-09-18 check, but only its **greeting** had ever
been exercised on this provider. Everything a phase change does with a line, and
everything the playback fix changed about how a call ends, was unverified here.

## 2. The run

Eight real handset calls on **2026-09-20, 12:47:02–12:55:30 +08**, into the
development FreeSWITCH on a developer's machine, human caller, ANI
`18688886666`. Build `667c1dc`, binary built from a clean tree.
`AICC_PROVIDER=qwen`, model `qwen-audio-3.0-realtime-plus`, endpoint
`wss://dashscope.aliyuncs.com/api-ws/v1/realtime`; transcription `qwen` /
`qwen-audio-3.0-asr-flash-streaming` at 16000. Process log
`logs/aicc-20260920-124541.log`. All eight recordings are two-channel 8 kHz
16-bit and were ingested to the dev SeaweedFS. **Every figure below is
MEASURED.**

**This box's DIDs differ from `internal/seed/`.** 95001 (en), 95002 (zh) and
95011 (en) reach `novanet_support`; 95012 (zh) reaches `mobile_support`. The
other four seeded flows have **no DID** here, and their published revisions date
from 2026-09-02 — before `announce` existed.

The eight calls:

| # | Start | DID | Flow | What it exercised | CDR |
|---|---|---|---|---|---|
| 1 | 12:47:02 | 95002 zh | `novanet_support` | hangup path | ANSWERED / NORMAL_CLEARING / bot_sec 20 / contained |
| 2 | 12:47:29 | 95001 en | `novanet_support` | hangup path | ANSWERED / 20 s / contained |
| 3 | 12:48:07 | 95001 en | `novanet_support` | keypad 0 → transfer to `support-en`, landing in `handoff` (non-terminal, no announce) | NO_ANSWER, rang out unanswered / 26 s |
| 4 | 12:48:41 | 95012 zh | `mobile_support` | nine `repair_status` calls, then transfer to `wt_queue` landing in `finish_transfer` (terminal, carries an announce) | NO_ANSWER / 108 s / bot_sec 76 |
| 5 | 12:50:35 | 95002 zh | `novanet_support` | silent caller, three dead-air rounds | ANSWERED / 41 s / contained |
| 6 | 12:51:19 | 95002 zh | `novanet_support` | hangup path | ANSWERED / 38 s / contained |
| 7 | 12:52:00 | 95002 zh | `novanet_support` | hangup path with three DTMF interrupts | ANSWERED / 37 s / contained |
| 8 | 12:54:49 | 95002 zh | `novanet_support` | transfer refused (`support-zh` disabled through the API for the test), then `take_message`, then hangup | ANSWERED / 40 s / contained |

## 3. Method for the audio measurements

The same method as `gemini-findings.md` §3.1, so that the two records can be
read against each other. Two-channel 8 kHz captures, channel 0 the caller and
channel 1 what the caller heard — exact digital zero when nothing is queued,
which is **71 %** of windows — read on **20 ms RMS envelopes**. A **cut** is
caller onset → channel 1 falling to zero. Bot-speech runs were merged across
gaps shorter than **100 ms** and matched to the log's `playedMs` within
**±400 ms**.

## 4. Measured results

**Entry announce (`SessionConfig.OpeningText`): 8 of 8 verbatim**, in both
languages, with no echo of the say-exactly wrapper and no provider error. That
settles the largest open question about this profile: qwen's Beta dialect
**accepts** a `response.create` carrying a `response.instructions` override, and
the doubled delivery — a fake `role:"user"` item *and* the override, because
qwen is `NeedsCueForFirstTurn` — yields the line itself rather than a model
paraphrasing a direction it can see.

**Phase-change announce (`VoiceSession.SpeakText`): 4 of 7 verbatim.** The three
misses:

- Call 1, zh farewell — the model said "再见！".
- Call 2, en farewell — the model said "Goodbye." where the line is "Thank you
  for calling NovaNet. Goodbye."
- Call 5, zh farewell — the model said "您好，您还在吗？请问有什么可以帮您？", a
  repeat of the previous check-in.

The four hits were calls 4 (`finish_transfer`), 6, 7 and 8. This is **W-Q1**.

**Barge-in.** 20 caller-side interruptions across 5 calls — **17 SPEECH, 3
DTMF** — plus 3 provider-side ones, qwen's own VAD reporting `INTERRUPTED` with
reason SPEECH at `playedMs` 460, 60 and 660. `aicc_bot_interruptions_total` read
SPEECH 17 and DTMF 3, matching the log line for line.

**Cut latency, n=13** — the interruptions with a measurable onset: **p50 360 ms,
p90 420 ms, min 240 ms, max 1460 ms**. Excluded are the 3 DTMF, because a
keypress puts no audio on channel 0, and 4 SPEECH cases whose onset preceded the
bot run. The 1460 ms outlier was the interruption of a greeting; on this client
speech detection is the provider's, so it is the provider's VAD that the figure
describes.

**Zero misses.** No bot run of ≥1 s containing ≥400 ms of sustained
above-threshold caller energy went uncut.

**The acceptance gate is MET** — the same gate the doubao and gemini clients
were held to, and the first time it has been met by anything: ≥10 interruptions
(20), cut p50 ≤ 1.0 s (360 ms), cut p90 ≤ 1.5 s (420 ms), zero misses.

**`playedMs` against the measured run length**: median **+100 ms**, range
**−80…+260 ms**. The sub-100 ms gap merge in §3 contributes error of its own, so
this is not a clean refutation of the 1–3 frame agreement `gemini-findings.md`
§3.1 reports for a different path; it is a looser agreement measured with a
looser instrument.

**Trailing silence**, last bot audio frame → end of recording, all eight calls:
**0.00, 0.00, 0.04, 0.20, 0.24, 0.28, 0.40, 0.42 s**.

**Metrics after the run.**
`aicc_provider_sessions_started_total{provider="qwen"}` 8;
`aicc_provider_ws_errors_total` absent;
`aicc_provider_sessions_expired_total` absent;
`aicc_provider_first_audio_ms` sum 44416 / count 31.

**Uplink: not one slow-write, dropped-frame or backlog warning.** Worth
recording because this box's DNS answers for `dashscope.aliyuncs.com` come from
a local mihomo proxy's fake-ip range (198.18.0.82), and the obvious reading of
that is a proxied path. The TLS handshake measured **65 ms**, which is a direct
domestic path. So the uplink confound that dominates `gemini-findings.md` §3.0
does **not** apply to this run.

**Total WARN and ERROR across the whole process log: 4.** Two provider errors —
the two in §6 — one expected `transfer refused: the queue is disabled`, and one
`queue not refreshed on the switch` caused by the tester's own API call.

## 5. Confirmed non-regressions

**`243b6d0` is confirmed live on qwen for the first time.** The trailing-silence
figures in §4 are the evidence: the ending is bounded by the drain, not by a
timer. Before the fix, doubao measured 1.3–5.1 s of silence there and the
pre-existing qwen behaviour was 8.000 s parks (`doubao-findings.md` §10, W-D5).

**The four pure-motion refactors show no observable effect.** `681913d`
(`wsconn`), `94e6b44` (`Watchdog`), `08795f9` (`MergeHint`) and `07911de`
(`SayExactly`): 8 of 8 calls bridged with `isPassthrough=false`,
`toProvider=PCM16@16000`, `fromProvider=PCM16@24000`; no socket fault and no
watchdog misfire. Those four are exactly what this client shares with the
gemini one, and `wsconn`, `Watchdog` and `MergeHint` are what it shares with
doubao as well, so each piece those commits moved has now been exercised over
real calls from both sides of the move. The pacer is the one shared component
qwen never takes, and it stays covered by doubao and gemini alone.

**`518130a` is unreachable on qwen by design — a confirmed negative.** No
Realtime-client code sets a `FailureCause`, so a failure on this profile would
still record `MEDIA_OR_PROVIDER_FAILURE`, and
`aicc_provider_sessions_expired_total` never appeared. That is what was expected
and it is recorded so that its absence is not later read as a gap in coverage.

**The highest-risk path held.** The feared sequence was `SpeakText` pre-empting a
turn, `handleResponseDone` emitting `INTERRUPTED{SYSTEM}`, and `Realtime.Interrupt`
then sending a `response.cancel` because qwen is the only Realtime profile with
`CancelsResponseItself: false` — cancelling the terminal announce on its way out.
It did not happen on call 4. No `reason=SYSTEM` floor-back was logged, so the
model's ad-lib ("正在为您转接人工") finished playing before the announce turn
began, the announce played in full, and the `uuid_transfer` fired after it.

## 6. Defects

**W-Q1 — a phase's `announce` is not reliably said as written on qwen: 4 of 7.**
The mechanism is visible in the code. `speakRequest`
(`internal/provider/realtime.go:339-344`) sends the direction **only** as
`response.instructions`. The opening turn (`realtime.go:205-227`) sends the same
direction **twice** — once as the override and once as a fake caller message,
because qwen is `NeedsCueForFirstTurn`. Single steering measured 4 of 7; double
steering measured 8 of 8. The worst observed consequence is call 5: the closing
line was replaced by a repeat of the previous check-in, and the caller was
released **0.2 s** after being asked "您好，您还在吗？" — hung up on in the middle
of a question. `CLAUDE.md` already calls this path best effort on the Realtime
client; this is the first number attached to it. **The fix is not decided.**
Injecting the direction as a conversation item would buy the opening path's
reliability at the cost the opening path already pays — the direction enters the
history as caller speech — and whether that trade is worth making mid-call is
open.

**W-Q2 — a no-input move into a phase that carries an announce asks for two
turns at once.** Provider-agnostic; qwen only makes it audible as an error.
`handleDeadAir` (`internal/aicall/orchestrator.go:505-527`) calls `afterMove`
first, which speaks the new phase's line, and then calls `SendUserText`
unconditionally with the re-engagement cue — two `response.create` frames
**38 ms** apart. qwen rejects the second: `invalid_value: Cannot create response
while another response is in progress` (12:51:12.870). Today the harm is
contained, because the rejected frame is always the later, redundant cue, and
that cue contradicts the farewell the flow has just decided on. But it is a WARN
on every such call, and the ordering is load-bearing rather than reasoned:
nothing states that the announce must win, it wins because it is written first.
After `9149539` every seeded flow's terminal phases carry an announce, so every
no-input path into them reaches this. Like W-D4 and W-D5, it is a defect one
engine surfaced that belongs to the shared path.

**W-Q3 — the barge-in cancel is a live race.** One of the six interruptions with
`wasGenerating=true` produced `invalid_value: Conversation has no active
response` **74 ms** after the flush (12:48:59.868). The local `isResponseOpen`
was still true when `Interrupt` ran; the provider's response had already ended.
The invariant in `CLAUDE.md` anticipates this class — "never send the provider a
cancel once generation ended; Qwen errors on it". What is new is that it is a
**race**, which cannot be closed from this side, is recoverable, and cost
nothing audible in this run.

## 7. Observations that are not defects

**The doubled goodbye.** Even when the announce is said correctly, the caller
hears two farewells: the flow's own rules tell the model to say goodbye after
the `hangup` tool returns, and the `farewell` phase then carries an announce
saying the same thing. That is a flow-authoring overlap introduced by `9149539`
adding announces to phases whose instructions already put the model in motion.
It is not a code defect.

**Call 7's model invented a keypad menu the flow does not have** — "您按了 1，请
问这是选择宽带安装服务吗？". Model behaviour, not a regression.

**`queue not refreshed on the switch`** (12:54:29) —
`callcenter_config queue reload support-zh: -ERR Invalid Queue not found!`,
raised when the tester disabled the queue through the API while this box's
mod_callcenter has no such queue loaded. A test-setup artefact, and an
API-to-switch synchronisation gap worth its own look. Nothing to do with qwen.

## 8. Not covered

`plan_change`, `early_collections`, `field_service_appointment` and
`lead_qualification` have no DID on this box, and their published revisions
predate `announce`. So multi-tool conversations, multi-hop phase graphs and the
tool-failure branches went untested. Also untested: two phase changes in one
session, and a no-input move into a phase whose announce contradicts the one
before it.

## Follow-ups

Three items, filed here because this document is what created them. W-Q2 is not
qwen's.

**W-Q1 — make a phase's `announce` as reliable mid-call as it is at the
greeting.** Measured at 4 of 7 against 8 of 8 (§4, §6). The difference between
the two paths is one frame: the opening turn puts the say-exactly direction in
front of the model twice, `speakRequest` puts it there once. The candidate
remedy is to make the mid-call path do what the opening path does, and its known
cost is the one the opening path already pays — the direction enters the
conversation history as caller speech. Whether that cost is acceptable on every
phase change, on a provider where the flow's closing line is the last thing the
caller hears, is a decision and not a patch. Until it is taken, a flow author on
qwen should assume a mid-call `announce` is a strong suggestion.

**W-Q2 — the no-input path must not ask for two turns at once.** In
`internal/aicall/orchestrator.go`, `handleDeadAir` speaks the new phase's
announce and then unconditionally sends the re-engagement cue, 38 ms apart
(§6). The cue is redundant exactly when a move happened — the flow has already
chosen the next words — and contradicts them when the new phase is terminal. The
fix is to send the cue only when no move carried words of its own, which makes
the ordering a stated rule instead of an accident of line order. It is
provider-agnostic: qwen returns an error, other engines take the second request
and talk over themselves.

*Fixed (2026-09-21), verified on one live call.* `afterMove` now reports whether it
asked for the new phase's line, and `handleDeadAir` returns without the cue when
it did; the comment there states the rule. A move into a phase without a line,
and silence that moves nothing, still send the cue. Covered by
`TestSilenceThatMovesIntoALineAsksForOneTurnOnly` and its two counterparts in
`internal/aicall/orchestrator_test.go`. On a 95002 call left silent
throughout, two no-input prompts moved nothing and sent the cue; the third moved
into `farewell`, whose line was the only turn asked for, and the call logged no
WARN before the BYE.

**W-Q3 — accept the cancel race rather than close it.** `Realtime.Interrupt`
sends `response.cancel` on a profile with `CancelsResponseItself: false` when
the local `isResponseOpen` is true, and the provider's response can end in the
window between that read and the frame arriving (§6). No local state can close
that window; the provider's view is the only authority and it is one round trip
away. The work is therefore to stop treating it as an error: recognise
`Conversation has no active response` as the benign outcome it is and log it at
debug, so that the WARN count of a healthy call means something.

*Fixed (2026-09-21), verified on one live call.* Every `response.cancel` the client
sends now goes through `sendCancel`, which records when. `handleError` drops an
error whose code is `invalid_value` and whose message contains `no active
response` if it arrives within five seconds of such a cancel, logging it at
debug; it emits no event, so nothing is logged at WARN and no failure is
recorded. The same words with no cancel behind them, and any other error after
one, are reported as before. A time bound, because the error event names
neither the frame it refuses nor the response it concerns. The invariant is
unchanged: `Interrupt` still sends no cancel once `response.done` has been
handled. Covered by `TestARefusedCancelThatLostTheRaceIsNotAnError` and
`TestOnlyTheRefusalOfOurOwnCancelIsSwallowed` in
`internal/provider/realtime_test.go`. On a 95002 call run at debug, one
interruption with `wasGenerating=true` lost the race: the refusal arrived 39 ms
after the flush and was logged only at debug, with no WARN and the call
continuing normally.
