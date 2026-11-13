# Qwen — live regression findings (2026-09-20)

**Qwen is not a client.** It is a `Profile` of the shared OpenAI-Realtime client
(`internal/provider/realtime.go`) — a name, an endpoint, a model and a dialect,
exactly as `docs/provider-extension.md` describes a profile. There is no
`internal/provider/qwen`, and `internal/provider/pacer` is **not** on this path:
it is imported by `internal/provider/doubao/pacer.go` and by nothing else.

So this document is not a peer to [doubao-findings](doubao-findings.md),
which records what a protocol turned out to be (as did a record for a Google
Live client, since removed). This one records what happened on one afternoon of telephone
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
`AICC_PROVIDER=qwen`, model `qwen-audio-3.1-realtime-plus`, endpoint
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

The same method as the removed Google Live client's record used, so that the
records could be read against each other. Two-channel 8 kHz captures, channel 0 the caller and
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

**The acceptance gate is MET** — the same gate the doubao and the removed Google Live clients
were held to, and the first time it has been met by anything: ≥10 interruptions
(20), cut p50 ≤ 1.0 s (360 ms), cut p90 ≤ 1.5 s (420 ms), zero misses.

**`playedMs` against the measured run length**: median **+100 ms**, range
**−80…+260 ms**. The sub-100 ms gap merge in §3 contributes error of its own, so
this is not a clean refutation of the 1–3 frame agreement the Google Live
record reported for a different path; it is a looser agreement measured with a
looser instrument.

**Trailing silence**, last bot audio frame → end of recording, all eight calls:
**0.00, 0.00, 0.04, 0.20, 0.24, 0.28, 0.40, 0.42 s**.

**Metrics after the run.**
`aicc_provider_sessions_started_total{provider="qwen"}` 8;
`aicc_provider_ws_errors_total` absent;
`aicc_provider_first_audio_ms` sum 44416 / count 31.

**Uplink: not one slow-write, dropped-frame or backlog warning.** Worth
recording because this box's DNS answers for `dashscope.aliyuncs.com` come from
a local mihomo proxy's fake-ip range (198.18.0.82), and the obvious reading of
that is a proxied path. The TLS handshake measured **65 ms**, which is a direct
domestic path. So the uplink confound that dominated the Google Live measurements
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
watchdog misfire. Those four are exactly what this client shared with the
Google Live one, and `wsconn`, `Watchdog` and `MergeHint` are what it shares with
doubao as well, so each piece those commits moved has now been exercised over
real calls from both sides of the move. The pacer is the one shared component
qwen never takes, and it stays covered by doubao alone.

**`518130a` is unreachable on qwen by design — a confirmed negative.** No
Realtime-client code sets a `FailureCause`, so a failure on this profile would
still record `MEDIA_OR_PROVIDER_FAILURE`, and
The Google Live client's lifetime-cap failure never appeared, as expected on a profile that cannot produce it.

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

*Update 2026-09-23.* The tool-result half was since fixed by carrying the line
in the result (A5c, then the profile capability `PutsTerminalAnnounceInToolResult`, since collapsed into the negation of `RequiresTerminalAnnounce`, #225). The half with no tool
result to carry it — a dead-air move into a terminal phase, and the
`maxTurnsWithoutTool` wall — still went through `SpeakText` with the override
alone, and failed again on live call `01a0cbb9-3f93-702d-8c62-7536378c1b93`
(`mobile_support`, DID 95012, `logs/aicc-20260923-084400.log`): after the third
silence the flow moved to `finish` and armed the ending, `SpeakText` asked for
"感谢来电,再见。", and the turn it produced was a third "您好，请问您还在线吗？…" —
the two dead-air check-ins already in the conversation as caller messages won
over the override. The ending fired on that turn's playback, so the caller was
released without a goodbye. `novanet_support`'s declared `NO_INPUT → farewell`
rule takes the same path and has the same exposure. The trade is now taken, for
lines that end the call only: the qwen profile sets
`NeedsDirectedLineInConversation`, and `SpeakText(text, isClosing=true)` — the
orchestrator sets `isClosing` for a terminal phase's `announce` — sends the same
`SayExactly` direction as a user `conversation.item.create` strictly before the
`response.create` that still carries it as the override, both when the floor is
free and after a pre-empted turn ends: the opening turn's double steering,
applied to a closing line. A line in a phase the call goes on from keeps the
override alone, because the item stays in the history and what it does to the
turns after it is unmeasured; a call that is ending has none. Pre-emption is
unchanged; openai and gateway stay on the override alone; doubao is
another client. **Confirmed on two live calls**, 2026-09-23, both
`mobile_support` on DID 95012 — three silences, the move to `finish`, then
"感谢来电,再见。" said and the BYE: `01a0cbc9-ad0f-7797-b98b-6edab0eec8fe`
(before the item was scoped to closing lines) and
`01a0cc44-a4d4-73ec-9906-869749760eb6` (after it, `isClosing=true`,
`logs/aicc-20260923-110845.log`). That is two calls, not a reliability figure;
the 8 of 8 the approach borrows from was measured on the opening turn, not
mid-call.

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

**W-Q4 — a tool result answered while the response that called the tool is
still open asks for a turn the provider refuses.** Found after the run, on
2026-09-21, on the Realtime client. qwen sometimes goes on generating in the
same response after emitting a function call: `response.output_item.done` for
the call, then a new `conversation.item.created` and audio ("好的，正在为您转接人工客服。")
inside that response. `SendToolResult` sent the `function_call_output` and, at
once, a `response.create`, and qwen refused the second: `invalid_value: Cannot
create response while another response is in progress` (call `d31ad143-…`, DID
95002, 13:33:38.588 in `logs/aicc-20260921-130714.log`, 86 ms after
`transfer_to_agent` ran). OpenAI refuses the same request while a response is
active. The same refusal is in `logs/aicc-20260921-104426.log` at 10:45:25.548,
on the path before A5c, where it cost nothing audible: the phase's line was
asked for by `SpeakText`, which cancelled the ad-libbed response and requested
the line when that response ended ("the provider took the floor back",
`reason=SYSTEM`). Under A5c it is not harmless. The tool result carries the
closing line and the turn it produces *is* the line; no `SpeakText` follows. The
refused request was the only request for that turn, so the line was never said:
the caller heard only the ad-libbed fragment, and the armed transfer waited for a
later turn's playback. On `d31ad143` the caller spoke twice and the transfer ran
on the second, 13:33:45.039, 6.5 s after the tool; a caller who stays silent
waits for the 10 s cap.

**W-Q6 — N tool results asked N times for a turn, and qwen speaks a closing line
without calling the tool (issue #147, 2026-10-13).** Two findings from the dev
logs of 2026-10-12, both about a call that should have ended on a `hangup` and
idled until dead air instead.

*The race.* A response that called N tools produced N-1 `invalid_value: Cannot
create response while another response is in progress` WARNs, about 100 ms after
the batch (for example `lead_submit` moving the call to its next phase). By the
time the orchestrator sends the results, `response.done` has been handled, so
`isResponseOpen` is false and W-Q4's owed-turn rule does not apply. The first
`SendToolResult` asks for the turn (`isResponseRequested` is set); until the
provider reports `response.created` nothing is open, and the second result asked
again. The provider refused the duplicate, harmlessly for the turn itself, but
the WARN count of a healthy call stopped meaning anything. Fixed in
`Realtime.SendToolResult`: a turn that is already asked for and not yet started
covers the results that follow it, so one batch asks for exactly one response
(`TestSeveralToolResultsAfterTheResponseEndedAskForOneTurn`). The flag is
cleared when the response is created (as before) and now also when the provider
refuses a request for any reason but a response already running, so a refused
request never swallows a later legitimate turn. What this does not prove: a
later result in the batch reaches the provider after the `response.create` that
the first result asked for, so that response may be made without it. It was not
measured whether qwen reads the later outputs; the live calls to check it are
the ones that answer a multi-tool batch.

*The closing line without the tool.* In a phase whose instruction said "say the
closing line, then call `hangup`" (lead_qualification `done`, early_collections
`promised`), qwen spoke the line, ended the turn with no tool call, and the call
idled until the 8 s dead-air cue or the caller speaking; only then did it call
`hangup`. The unbacked-claim check saw it (`claim=FAREWELL phrase=再见`). The
model treats the spoken goodbye as having done the job. The proven shapes are
tool-first (`hangup` first, the goodbye after it, as `novanet_support`'s
`welcome` and the global `hangup` -> `closingTarget` rule) and a terminal phase
with an `announce`, where the platform arms the ending before the line is spoken
and does not depend on the model. The seeded closing phases were moved to those
shapes and the authoring rule written down (`docs/flow-authoring.md` 5.3).
Backstops added: the dead-air cue now tells a model that has closed the
conversation to call `hangup` silently instead of repeating the question, the
farewell detector reads the closing phrases the flows use, and a non-terminal
phase move now sends the new phase's instructions before the tool answer asks
for a turn, so that turn is not made under the previous phase.

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

Four items, filed here because this document is what created them. W-Q2 is not
qwen's, and W-Q4 applies to every Realtime profile.

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

*Probe (2026-09-21), outside the call path.* A script against the same endpoint
and model — no audio, no tools, no cancels — asked for a mid-call line two ways.
With the direction added as a `role:"system"` conversation item on top of the
`response.instructions` override, en said the line 3 of 3 and zh 0 of 3, and in
two of the zh attempts the model spoke as though it were the caller. With the
override alone — exactly the frame `speakRequest` sends today — zh said the line
3 of 3. Samples of three in a clean context, not the live call path, so this is
no rate. What it does settle is the premise above: a lone override is honoured
on a quiet conversation, so single against double steering is not by itself the
difference, and a second steering frame cost more in zh than it bought. The
system-role approach is dropped.

*Cause, as far as the logs reach (2026-09-21) — analysis, not a fix.* Rebuilt
from the process log (INFO only), the stored transcripts and the recordings, for
the seven announces of §2 and for the four qwen calls made on 2026-09-21 with
`dd040be` and `dab56cc` in, one of them logged at debug.

| Call | Into the phase by | The model's last words before the request | The line's request | Said | |
|---|---|---|---|---|---|
| 1 | `hangup` → `farewell` | tool-result turn "再见！", stopped unheard | waited, sent when that turn ended | "再见！" | miss |
| 2 | `hangup` → `farewell` | "Good", stopped unheard | waited | "Goodbye." | miss |
| 4 | `transfer_to_agent` → `finish_transfer` | "正在为您转接人工", stopped unheard | waited | the line | hit |
| 5 | third no-input → `farewell` | the check-in "您好，您还在吗？请问有什么可以帮您？" | at once; the cue 38 ms later was the frame refused | the check-in again | miss |
| 6 | `hangup` → `farewell` | "再见", stopped unheard | waited | the line | hit |
| 7 | `hangup` → `farewell` | "再见！", stopped unheard | waited | the line | hit |
| 8 | `hangup` → `farewell` | "再见！", stopped unheard | waited | the line | hit |
| 09-21 10:40 | `hangup` → `farewell` | "再见！", stopped unheard | waited | "再见！" | miss |
| 09-21 10:41 | third no-input → `farewell`, no cue (W-Q2 fix) | the check-in | at once, the only request | the check-in again | miss |
| 09-21 10:43 | `hangup` → `farewell` | nothing transcribed | waited | the line | hit |
| 09-21 10:45 | `transfer_to_agent` → `finish_transfer` | the tool-call turn's own speech, cut at 440 ms | behind a cancel of the tool-call turn; the tool result's request refused | the line, cut by qwen's own VAD at 120 ms | hit |

**The line's request is never lost, and the words the caller heard in a miss
were the line's own turn.** On the tool path every hit and every miss of the run
went through the same sequence: the tool result asks for a turn, `SpeakText`
finds a request outstanding and waits, the tool-result turn is cancelled the
moment it is created, and the line is asked for when that turn ends. The build
under test logged both kinds of refusal at WARN, and this log carries each of
them elsewhere (12:48:59.868, 12:51:12.870); none appears near any of the six
tool-path announces. No `reason=SYSTEM` floor-back was logged for any of them,
so the cancelled turn never put a frame in front of the caller, and the
recordings agree: after the caller's last words each call has exactly one bot
run in a miss and the line's two clauses in a hit. The hang-up fired after that
turn's playback — 2.2 s after the move in the misses, 3.8–4.3 s in the hits. So
the ad-lib playing in the line's place, and the line being dropped, cancelled or
refused, are both excluded for the run. §5's reading that call 4's ad-lib "finished playing" and
§7's that the caller "hears two farewells" do not survive this: the second
goodbye is in the model's history, not in the caller's ear. The one ordering
that does refuse a frame — the tool-call turn still open when the result is
answered — appeared only on 2026-09-21 at 10:45, and there it was the tool
result's request that qwen refused while the line was said.

**Call 5 is not the cue winning.** One response ran after the move; the refused
frame was the cue's; the words were neither the cue's goodbye nor the line. The
10:41 call on 2026-09-21, with the W-Q2 fix in and no cue sent at all, missed
the same way. The fix removed the refused frame and did not change the words.

**What the misses share: the line's turn reproduced the model's own last
words.** It repeated them (calls 1 and 5, both 2026-09-21 misses) or finished
them ("Good" → "Goodbye."). On the tool path those words are the goodbye the
flow's own rule asks for after `hangup` returns (§7): stopped before anyone
heard them, but left whole in the conversation, because history is trimmed only
when `playedMs > 0`. On the no-input path they are the model's previous
check-ins, and the line's request adds no item of its own — nothing but the
override tells it apart from "go on" — and it missed 2 of 2. The clean probe had
neither. It is a tendency, not a rule: the tool-path hits carried the same kind
of stopped ad-lib and said the line, 6 of 9 on that path across both days. The
INFO log cannot say whether in a miss qwen ignored the override or received it
and was outweighed by the history.

**What would decide it.** At debug: each outbound `response.create` with
whether it carries an override; `response.created` with its id and the raw
`response` object, in case qwen echoes the instructions it applied; the status
and final transcript per response id; and every `conversation.item.created` with
id, role and status, so the history at the moment of the line's request is on
record. And a probe that replays the live shape rather than a quiet one: the
override after a cancelled assistant goodbye, and after two check-ins with no
new item — each again with the stopped item removed (`conversation.item.delete`),
and as an out-of-band response (`conversation: "none"` with an explicit
`input`), if qwen accepts one.

**Directions, none taken.** On the tool path, not making the ad-lib at all: when
the move a tool result causes carries a line, answer the tool without asking for
a turn and let the line be that turn — W-Q2's rule applied to the other path. It
removes the cancel, the 10:45 race and the stopped goodbye in one step, and it
needs a way to answer a tool without asking for a turn, which the provider seam
does not offer today. Short of that, remove a stopped, unheard turn from the
history before asking for the line. On the no-input path neither applies; the
request needs something that makes it differ from "go on", and the cheapest
candidate, the out-of-band response, is unverified on qwen.

*Probe of the live shapes (2026-09-21), outside the call path.* A script against
the same endpoint and model, text only, replayed the two shapes above with the
real `novanet_support` text: the session configuration `buildSessionUpdate`
builds from the rendered welcome instructions and the three built-in tools, the
opening turn as `Start` sends it, and the farewell instructions, hint and
announce as the flow renders them. Tool path (A): greeting; a caller text item
"拜拜。"/"Bye."; the model called `hangup` itself in every session; the
`function_call_output` and its `response.create`, then the farewell
`session.update`, then `response.cancel` on the tool-result turn's
`response.created`; the line asked for on that turn's `response.done`. No-input
path (B): greeting; two check-ins, each the real cue as a caller item and a
bare `response.create`; the farewell `session.update`; the line. The line was
always `speakRequest`'s frame unless the row says otherwise. Forty-four
sessions.

| Path | Treatment | zh | en | What was said instead |
|---|---|---|---|---|
| A | none (today) | 0 / 5 | 0 / 2 | "再见！" 5 of 5; "Goodbye." |
| A | the cancelled turn deleted first | 0 / 5 | 0 / 2 | "再见。"; "Goodbye." |
| A | out-of-band (`conversation: "none"`, explicit `input`) | 0 / 3 | 0 / 1 | "再见！"; "Goodbye." |
| A | no tool-result turn: the result answered without `response.create` | 0 / 3 | 0 / 1 | "再见。"; "Goodbye." |
| B | none (today) | 0 / 5 | 0 / 2 | the check-in again, e.g. "您好，您还在吗？感谢致电 NovaNet，请问有什么可以帮您？"; "Are you still there? How can I help you today?" |
| B | the two cue items deleted first | 0 / 3 | — | the greeting and both check-ins recited back to back, or the greeting alone |
| B | out-of-band | 0 / 3 | 0 / 1 | the check-in again |
| A | the direction also as a caller item | 3 / 3 | 1 / 1 | — |
| B | the direction also as a caller item | 3 / 3 | 1 / 1 | — |

**Both misses reproduce, every time, outside a call.** Unlike the quiet probe
above, these contexts are the live ones, so the baselines are the finding: a
lone override loses to the conversation on both paths. The cancelled turn's
`response.done` carried `status: "cancelled"` and its full transcript
("再见！"), so the stopped goodbye is in history whole, as §6's analysis read.
But deleting it — `conversation.item.delete` is accepted, `conversation.item.deleted`
comes back — changes the punctuation and not the answer: the model says goodbye
because the last thing on the caller's side is a tool result whose hint says
the phase is farewell, not because it is copying its own words. On the no-input
path, removing the cues left the opening direction as the last caller item, and
the model answered that instead.

**qwen does not support out-of-band responses, and says nothing.** A
`response.create` carrying `conversation: "none"` and an `input` array is
accepted without an error; its `response.created` is the ordinary object, its
item is announced by `conversation.item.created` with a `previous_item_id` in
the default conversation, and its `input_tokens` equal the in-band request's to
the token (763 against 763 on A). Both fields are ignored.

**The override is applied, and it replaces the standing instructions.** Neither
`response.created` nor `response.done` echoes the instructions a response ran
under; the only witness is `usage`. The opening turn, with the override, used
663 input tokens; the next turn, without one and with more history, used 901; the
line turns on both paths used 759–771. The difference is the size of the
session instructions, so the override was received and stood in for them. The
misses are the model following the conversation's last caller-side item over a
system prompt that tells it otherwise — not the override being dropped.

**What worked is the opening path's shape.** Adding the same direction as a
caller item before the same request said the line 8 of 8, on both paths and in
both languages. That is the remedy §6 named and whose cost it named: the
direction enters the history as caller speech. It is the only shape in the
table that changed the answer. Of the directions above, deleting the unheard turn and
answering the tool without asking for a turn are both measured and neither
works — with the tool result still the last caller-side item the model says its
own goodbye — and the out-of-band response does not exist on qwen.

*Probe of the direction in the last caller-side item (2026-09-21), outside the
call path.* The same script, the same session setup and the same rendered
`novanet_support` text, with one constraint: no caller item that the caller did
not say. The direction goes into the item that is already last on the caller's
side. On the tool path that is the `hangup` result: its `hint` became
`SayExactly(line)` in place of the farewell phase's instruction, `ok` kept, and
the model called `hangup` itself in every session. On the no-input path it is
the cue item a silence sends, with `SayExactly(line)` as its text; that is the
shape of the row above, run again to widen the sample. Since the W-Q2 fix no cue
is sent on a silence that moves into a line, so in the call path this is the cue
restored in that case with the direction as its words, not an item that is
there today with different words.
Twenty-four sessions, no error in any of them.

| Path | Treatment | zh | en | What was said instead |
|---|---|---|---|---|
| A | direction in the tool result; today's sequence (turn asked, cancelled on `response.created`, farewell `session.update`, the line on `response.done`) | 4 / 5 | 2 / 2 | "再见。" |
| A | direction in the tool result; answered without `response.create`, farewell `session.update`, the line | 5 / 5 | 2 / 2 | — |
| A | direction in the tool result; farewell `session.update`, then a plain `response.create` left to finish — no cancel, no override, no line request | 5 / 5 | 2 / 2 | — |
| B | the cue item's text is the direction, then the line | 2 / 2 | 1 / 1 | — |

**The tool result carries the direction on its own.** The third row is the
decisive one: the tool-result turn, asked for with no override and run to
completion, said the line in 7 of 7 — it produced one message and called no
tool. The same happened in the first row before the cancel landed: all seven
cancelled turns had already started the line ("感谢您致电", "Thank you for"). The
one miss there is that turn continuing: the stopped turn had got as far as
"感谢您致电 NovaNet，", and the line's request produced the rest, "再见。". A cut
turn still sits in the history, and the model finishes it rather than starting
over — the same pull as the stopped goodbye above, now in the line's favour
except when the cut falls mid-sentence. Answering the tool without asking for
a turn (second row) removes that turn and said the line 7 of 7. On the no-input
path the rewritten cue said the line 3 of 3, which with the earlier row makes 5
of 5 in zh and 2 of 2 in en for that shape.

Samples of seven per treatment on a text-only session, so no rate. What they do
not test is the history afterwards: every session ended at the line, so whether
a direction left in a tool result or a cue item changes what the model says on a
later turn is not measured. On the paths probed it does not arise — the phase is
terminal and the call ends after the line — but it would on a phase with a line
that is not terminal.

*A5c on the other providers (2026-09-21), outside the call path.* The third row
of the table above — the `hangup` result's `hint` replaced by `SayExactly(line)`,
the farewell instructions applied, the tool-result turn left to run, no line
requested — against today's sequence, with each client's own session setup and
frames and the same rendered `novanet_support` text. openai: the Realtime client
under `ProfileFor("openai")` (GA, `gpt-realtime-2.1`), `buildSessionUpdate` with
`output_modalities: ["text"]` added, and one audio session per treatment; the
caller's "拜拜。"/"Bye." a text item, as on qwen; today's sequence the qwen probe's
(turn asked, cancelled on `response.created`, `speakRequest` on `response.done`).
The Google Live client (since removed) and doubao: each package's own `Session`, opened by `Start` with the
welcome line, fed room noise and a synthesised "拜拜。"/"Bye." as caller audio,
since doubao takes no text turn; the tool answered through `SendToolResult` and
then `UpdateInstructions`, and today's sequence adds `SpeakText(line)` right
after, as `afterMove` does. Doubao's today is `speech_text_buffer.commit` and
exact by construction, so it has no baseline row. Thirty-nine sessions: openai 13,
Google Live 11, doubao 15.

| Provider | Treatment | zh | en | What was said instead |
|---|---|---|---|---|
| openai | today | 3 / 3, audio 1 / 1 | 1 / 1 | — |
| openai | A5c | 5 / 5, audio 1 / 1 | 2 / 2 | — |
| Google Live | today | 3 / 3 | 1 / 1 | — |
| Google Live | A5c | 5 / 5 | 2 / 2 | — |
| doubao | A5c | 5 / 5 | 2 / 2 | — |

**Nothing missed, on either path, on any of the three.** Every model called
`hangup` itself wherever the caller's goodbye reached it as one, and the turn
after the result was the line, word for word, with nothing after it. qwen's
miss has no counterpart on openai: in three of the text-only zh sessions the
tool-result turn finished before the cancel reached it, so a whole ad-lib
goodbye ("好的，感谢您的来电，祝您一天顺利。") sat in the history, and the lone
override that follows still said the line. The history that outweighs the
override on qwen does not outweigh it on `gpt-realtime-2.1`.

**openai.** Those three cancels were refused with `response_cancel_not_active`,
"Cancellation failed: no active response found". A text turn ends inside the
round trip; in the audio session and the en one the cancel landed ("好的，",
"Goodbye, and"). W-Q3's filter matches qwen's code, `invalid_value`, so on
openai the same race would still be logged at WARN. In 2 of 13 sessions the
model spoke before calling `hangup`, in the same response ("好的，我处理一下结束通话的流程。",
"好的，我先按流程结束通话。").

**Google Live.** Today's `SpeakText` reaches the server microseconds after the tool
response, and one turn followed, the line — no interrupted turn, no audio from a
continuation of the tool response. So on this client today's path and A5c
produce the same single turn; A5c differs only in not adding the direction to
the history as caller speech. On both paths the farewell phase text never
reaches the model: `UpdateInstructions` writes no frame, and only
`SendUserText` carries the pending phase.

**doubao.** The turn after the result was the model's own (`tts_type`
`default`, not `chat_tts_text`) and said the line in 7 of 7, so the engine
lets the model call `hangup` and follows a `SayExactly` hint in a `role:"tool"`
item. Eight further sessions do not count. In seven of them the goodbye was
transcribed 25–30 s after it was sent, and in none of the seven did the model
call `hangup`: it answered as if to a keypress ("如果您需要人工服务，可以按0…",
"It looks like you pressed 0" followed by `transfer_to_agent`) or asked what the
caller needed. The eighth ended in `55000000: rpc error: code = 13 desc = the
stream is done`. With the caller's audio started 3 s after the greeting's last
frame arrived rather than 0.8 s, 4 of 4 zh goodbyes were transcribed within
1.5 s; the en "Bye." was still late in both sessions, one of which is counted
above because the model then called `hangup` and said the line, and "Okay,
that's all. Bye." was transcribed in 3 s. No cause was established. In both
counted en sessions the model's
`response.output_text.done` arrived 8 ms before its `response.output_audio.started`,
so the client reports the line's final transcript with no turn open.

Samples of seven per client, so no rate. For a later implementation: on openai
and Google Live, like qwen, the line is a direction to a model whichever path carries
it, and A5c said it as often as today's path while sending one request fewer and
no cancel. On doubao, today's `speech_text_buffer.commit` is the only path that
is verbatim by construction, and 7 of 7 does not make the model's continuation
equivalent to it.

*Implemented (2026-09-21), verified on live calls on qwen.* A5c, on the tool
path only. When a tool result moves the call into a terminal phase that carries
an `announce`, `answerToolCall` (`internal/aicall/orchestrator.go`) replaces the
result's hint — the new phase's preamble and instruction — with
`provider.SayExactly(line, language)`, re-pins the instructions and arms the
ending, and only then answers the tool; no `SpeakText` follows, so the turn the
result produces is the line. The gate is a new profile capability,
`PutsTerminalAnnounceInToolResult` (since collapsed into the negation of `RequiresTerminalAnnounce`, #225): true on the Realtime profiles only — openai,
qwen and gateway. Doubao and Google Live (then a client) kept today's path exactly (the phase's
instruction as the hint, the answer, the instructions, then `SpeakText`), for
different reasons. Doubao's `SpeakText` is exact by construction, which a
direction to a model is not. Google Live was excluded pending its own W-G6: its
model sometimes never answers a tool result (6 times on 4 live calls), today's
`SpeakText` right after the tool response still produces the line then, and under
A5c such a silent continuation would complete with no audio and the armed ending
would release the caller without the line. A
phase with a line that is not terminal, a phase with no line, the opening line
and the no-input path (`handleDeadAir`, as of `dd040be`) are unchanged on every
client. The armed ending still waits for the playback of a turn later than the
one the tool call arrived in, and arming before the answer guarantees the line's
turn is such a turn. One gap in that binding is closed with it: a turn reported
done because it was cut short no longer counts as the closing line, so a caller
speaking can no longer fire the ending while the line is still to come; that
applies on every client. Covered by
`TestAToolThatEndsTheCallCarriesTheClosingLineOnARealtimeProfile`,
`TestAClosingLineCutShortEndsTheCallOnlyAtTheCap`,
`TestAToolThatEndsTheCallOnDoubaoSpeaksTheLineAsBefore` (since renamed),
`TestAToolThatMovesIntoALineThatIsNotTheEndIsUnchanged` and
`TestACutShortTurnIsNotTheClosingLine` in `internal/aicall/orchestrator_test.go`,
and `TestOnlyTheRealtimeProfilesPutTheClosingLineInTheToolResult` in
`internal/provider/profile_test.go`. On eleven zh calls to 95002 on qwen that
ended through a tool — six `hangup` and five `transfer_to_agent`, over two runs
(logs `aicc-20260921-130714` and `aicc-20260921-134821`) — the line was said as
written ten times, and every ending ran after the line had played, none on the
cap. The one miss is W-Q4: the result's request for the line's turn was refused
while the tool call's own response was still speaking.

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

**W-Q4 — a tool result's turn waits for the response that holds the floor.**
`Realtime.SendToolResult` must not ask for a response while one is open (§6).

*Fixed (2026-09-21), not yet exercised on a live call.* In
`internal/provider/realtime.go` only; `VoiceSession` is unchanged.
`SendToolResult` sends the `function_call_output` item at once and then, if a
response is open (`isResponseOpen`) or a tool turn is already owed, records the
turn as owed (`isTurnOwed`) instead of sending `response.create`; the
decision is taken after the item is sent, so a released request can never
precede the output it answers. `handleResponseDone` (and the watchdog's
`onResponseStalled`) calls `releaseOwedTurn` after `dispatchPendingSpeak`, which
asks for the owed turn. Several results in one response owe one turn. Any
request the client makes discharges the owed turn (`requestResponse`), and so
does any `response.created`, whoever asked for it, because every later response
is made with the output in the conversation. Three consequences follow. A line
that was waiting for the floor takes the owed turn: one request, for the line.
While the turn is owed, `SpeakText` treats the floor as taken, as it does for a
request in flight. And a caller who barges in on the open response is answered
by the provider alone: when that response ends while the provider has reported
`speech_started` and neither `speech_stopped` nor a new response since
(`isCallerSpeaking`), the turn is held rather than requested, because the
provider's own turn detection answers the caller with a response that reads
the output, and a request on top of it would be refused. After `speech_stopped`
the turn is no longer held: a provider that heard speech while a response was
open may never answer it, and a refused request costs a WARN where a stranded
turn costs the call its line. Deferral, hold and release are logged at debug.
No cancel is added, and W-Q3's handling is untouched. Covered by
`TestAToolResultSentWhileTheResponseIsOpenAsksForItsTurnWhenItEnds`,
`TestAToolResultWithTheFloorFreeAsksAtOnce`,
`TestSeveralToolResultsForOneResponseAskForOneTurn`,
`TestACallerWhoBargesInIsAnsweredByTheProviderAloneAfterTheDone`,
`TestAResponseTheProviderStartsBeforeTheDoneDischargesTheOwedTurn`,
`TestALineAskedForWhileAToolTurnWaitsIsTheOneTurnAskedFor` and
`TestALineAskedForWhileTheToolTurnIsHeldWaitsForTheProvidersReply` in
`internal/provider/realtime_test.go`; all but the second fail against the
previous client. Six qwen calls on the fixed build (log
`aicc-20260921-134821`, three transfers and three hangups) logged no WARN and
said every line as written, but in none of them did the model keep speaking
after its tool call, so the deferral was never taken: they show the ordinary path
unharmed, not the fix at work.

## Model: qwen-audio-3.1-realtime-plus (2026-09-24)

The profile's model is `qwen-audio-3.1-realtime-plus`, with the voice
`longanqian_v3.1`.

**What the vendor documents.** Sources: the Model Studio guide for
Qwen-Audio-Realtime,
<https://www.alibabacloud.com/help/en/model-studio/qwen-audio-realtime-user-guides>
and <https://help.aliyun.com/en/model-studio/qwen-audio-realtime-user-guides>,
read 2026-09-24.

- Model id: `qwen-audio-3.1-realtime-plus`. There is no 3.1 flash.
- The model is selected by the `model` query parameter.
- Voices: eight `_v3.1` voices (`longanqian_v3.1`, `longanhuan_v3.1`,
  `longanlingxin_v3.1`, `longanfengyue_v3.1`, `xunanchuan_v3.1`, `beth_v3.1`,
  `betty_v3.1`, `cally_v3.1`) alongside the earlier set. The documented default
  is `longanqian_v3.1`. The voice is honoured only in the first `session.update`.
- Turn detection: `server_vad` (`threshold`, `silence_duration_ms` 200–6000,
  default 800), `smart_turn`, or `null` for push-to-talk; settable only before
  the first audio frame.
- Audio: PCM 16 kHz 16-bit mono in, PCM 24 kHz 16-bit mono out.
- Tools, `response.create`, `response.cancel`: the Realtime events this client
  already sends. `conversation.item.truncate` is not mentioned in the guide.

**What was measured.** A standalone probe (raw WebSocket, not the product
client) against `wss://dashscope.aliyuncs.com/api-ws/v1/realtime`, this
repository's `ALIYUN_API_KEY`, sending exactly the `session.update` the qwen
profile sends (`modalities [text, audio]`, `input_audio_format` and
`output_audio_format` `pcm`, `turn_detection {server_vad,
silence_duration_ms 500}`, `voice longanqian` — the profile's voice before
the decision below — one function tool). **MEASURED:**

| Item | Result |
|---|---|
| Connect on the non-workspace host | HTTP 101, `session.created` naming `qwen-audio-3.1-realtime-plus` |
| `session.update` as the profile sends it | `session.updated` echoing `voice longanqian`, `server_vad` at `silence_duration_ms 500` |
| `response.create` on an empty conversation | `error invalid_value "Cannot create response: conversation has no messages or no user message."` — `NeedsCueForFirstTurn` applies |
| Cue item + `response.create` | greeting spoken, `response.done status=completed` |
| Output format | audio bytes consistent with 24 kHz PCM16 for the transcript's length |
| Mid-call `session.update` with `instructions` only | `session.updated`, turn detection unchanged |
| Tool call | `response.output_item.added` (`function_call`) → `response.function_call_arguments.done` → `response.done`; `function_call_output` + `response.create` → spoken turn |
| `response.cancel` on an open response | `response.done status=cancelled`, `status_details.reason client_cancelled` |
| `conversation.item.truncate` after the cancel | no acknowledgement and no error within 3 s |
| `conversation.item.truncate` with `audio_end_ms` 0 or 300 after a completed response (2026-10-01, `TestLiveQwenTruncate`, 3 runs each) | no acknowledgement and no error; asked to repeat its last sentence, the model recites it in full every time, as it does with no truncate. The truncate does not trim the text the model reads back, so on qwen it cannot tell the model a line went unheard (#53) |
| `response.cancel` with nothing open | `error invalid_value "Conversation has no active response."` — the text `isAnsweredCancel` matches |
| `smart_turn` with `silence_duration_ms 500` | echoed back as 2000 — `SemanticTurnSilenceMs` true |
| `voice longanqian_v3.1` / `beth_v3.1` | accepted |
| `voice nosuchvoice` | `error "Unsupported voice"`, listing the accepted names |
| `model qwen-audio-3.1-realtime-flash` | `session.created`, then `close 1007 "Model not found (qwen-audio-3.1-realtime-flash)!"` |

**Consequences.**

- Every qwen trait in `QwenProfile` holds on this model: `NeedsCueForFirstTurn`,
  instructions-only mid-call updates, `server_vad` at 500 ms, the `smart_turn`
  hold of 2000 ms, `CancelsResponseItself false`, 16 kHz in / 24 kHz out.
- **Owner decision, 2026-09-24: the profile's fallback voice is the vendor's
  default, `longanqian_v3.1`** (was `longanqian`). The shipped flows name no
  voice, so they get `longanqian_v3.1`. `global.voice` (A7) is a free string:
  no check at load or publish catches a name the model refuses, and such a
  session is rejected with `Unsupported voice` and the call is rescued to its
  fallback.
- `session.created` reports `input_audio_transcription.model
  gummy-realtime-v1` as a session default; the caller's words are transcribed
  unprompted. The separate transcription service
  (`internal/transcribe`, `qwen-audio-3.0-asr-flash-streaming`) was not changed
  and not re-probed.

**Not covered.** This probe placed no telephone call: barge-in over real
speech, the drain and playback gating, a mid-call `announce` (W-Q1) and voice
quality on the 8 kHz leg are not verified by it.

## W-Q5: a cancelled response leaves its half-made function call in the conversation (2026-10-03)

**W-Q5 — a response cancelled while it writes a function call's arguments leaves
the call in the conversation, and the model then copies it instead of calling
the tool.** Found 2026-10-03 with a standalone probe (a logging WebSocket proxy
in front of `wss://dashscope.aliyuncs.com/api-ws/v1/realtime`, model
`qwen-audio-3.1-realtime-plus`, the production session of the mobile-support
flow for DID 95012, a keypad cue standing for the caller pressing 0).

**The shape.** The first cue starts a response that adds a
`transfer_to_agent` item (`response.output_item.added`, `function_call`,
`arguments:""`). A second keypress cancels it (`response.cancel`, which a
keypress always sends) before the arguments finish. qwen answers
`response.done` with `status=cancelled` whose `output` still carries that
function call, with the arguments as far as they got (`""` or `{"queue": `), and
**never sends `response.function_call_arguments.done`**. The item stays in the
conversation with no output. The next response, asked for by the second cue,
often only *says* "正在为您转接人工客服" and never calls the tool, and keeps
copying that on every later turn.

**Counts.** With the cancel landing on a call in flight and the second cue sent
straight after (the product's behaviour since a second keypress started
cancelling the first keypress's response), the second response called the tool
in 4 of 9 cases and only said it in 5 of 9. The same cue sequence with no
dangling call (the first response completed, or never started a call): 0 of 10
misses.

**The gate.** The same sequence with a `conversation.item.delete` for the
function call's item id sent when `response.done(cancelled)` arrived and before
the second cue's `response.create`: 20 sessions, 10 of which had a dangling
call. All 10 deletes were answered with `conversation.item.deleted` and no
`error`; in all 10 the second response called `transfer_to_agent`. The other 10
sessions' first response had completed before the cancel could land; a delete
sent for those items (not something the product does) was acknowledged too. So
qwen **does** honour `conversation.item.delete`, and the history without the
call is one the model acts on.

**The fix (`provider.Realtime`).** `output_item.added` of a `function_call` opens
the call and `response.function_call_arguments.done` closes it (matched by item
id or by call id, since a dialect may carry only one). When a response ends with
any status other than `completed`, every call that is open, or is listed in the
response's `output` and was never closed, is deleted with
`conversation.item.delete`, before the `response.create` that the same
`response.done` releases (`dispatchPendingSpeak`, `releaseOwedTurn`), so the next
turn is made on the cleaned history. A call whose arguments completed was handed
out as a tool call and gets its output; it is never deleted. The stall path
(`onResponseStalled`) does not delete: the provider has not said that response is
over and may still finish the call, and a deleted item that then receives its
arguments and its output breaks a call the consumer is answering. A refused
delete (an item already gone, a provider without the event) is logged at debug
and not reported, recognised like a refused cancel by a time window after our own
delete. All Realtime profiles send it, as they all send `conversation.item.truncate`;
OpenAI documents the event. Covered by
`TestACancelledResponseTakesItsUnfinishedCallOutBeforeTheOwedTurn`,
`TestACancelledResponseKeepsACallThatFinished`,
`TestACallFinishedByCallIDAloneIsKept`, `TestACompletedResponseDeletesNothing`
and `TestARefusedDeleteIsNotAnError` in `internal/provider/realtime_test.go`.

## W-Q7: a mid-call instructions-only update is obeyed (2026-10-15)

**W-Q7 — `Realtime.UpdateInstructions` on qwen is obeyed from the next reply,
including when sent while a reply is being spoken, and the tools survive.
MEASURED, 2026-10-15, for an appended style directive.** The row in the
measurement table of the first section ("Mid-call `session.update` with
`instructions` only → `session.updated`") recorded only that the server answers;
this measures that the model acts on it.

**Method.** `TestLiveQwenMidCallInstructions`
(`internal/provider/live_instructions_test.go`), through the product client
(`provider.New(QwenProfile())`, `Start`, `SendAudio`, `UpdateInstructions`,
`SendToolResult`), model `qwen-audio-3.1-realtime-plus`, default endpoint,
`ALIYUN_API_KEY`. The standing instructions are a persona; the update is the
same persona plus one appended paragraph, the way a flow changes phase,
requiring every answer to begin with "好的，王先生" (a phrase the model never
says unprompted). The caller's questions are synthesized speech (qwen3-tts-flash,
voice Cherry; `testdata/q_*.pcm`, 16 kHz mono PCM16, which is this profile's
`LinearInput`, so no conversion) streamed at real-time pace in 20 ms frames
between stretches of room tone. Three sessions per arm, 15 sessions in all, the
same arms and clips as doubao W-D9 so the two compare directly. Replies are the
final `EventTypeOutputTranscript`s of each turn.

| Arm | Obeyed |
|---|---|
| **Control**: no update, three questions | marker in **0 of 9** replies |
| Update while idle, after the first answer; then two more questions | **6 of 6**: the first reply after the update and the next one; the answer before the update was plain in 3 of 3 |
| Update sent while the first answer was still being spoken; then two more questions | **6 of 6**; the answer being spoken was not cut and carried no marker (3 of 3); every later one did |
| **Tool control**: `lookup_order` declared at start, no update, the order question | tool called **3 of 3** with no instruction at all |
| **Tools**: tool declared at start; the update adds the marker rule and "call `lookup_order` on an order question" | tool called **3 of 3**; the reply after the tool result carries the marker **3 of 3** |

Every `session.update` was answered with `session.updated`; no error frame, no
close, no turn-detection complaint in any of the 15 sessions. The server
returned no session id that the client surfaces (`session.created` is dropped by
`handle`), so there are none to quote.

Quoted. Control, third question: `资费会根据您选择的套餐带宽和签约时长有所不同，
通常每月在几十到几百元不等。` Idle #1, before the update: `我们的客服工作时间是早上
9点到晚上6点。` after it: `好的，王先生，您可以登录我们的官网在线办理，或者携带身份证
前往附近的营业厅办理。` then `好的，王先生，资费根据您的具体需求和套餐类型有所不同…`.
Speaking #3, the answer spoken during the update: `我们的客服全天 24 小时在线，随时
为您提供服务。` the next: `好的，王先生，您可以通过我们的官方网站或前往线下营业厅办理
宽带业务。` Tools #2: before the call `好的，王先生，我马上为您查询订单 12345 的状态。`,
tool call `lookup_order {"orderId": "12345"}`, after the result `好的，王先生，您的订单
12345 已发货，预计明天送达。` The tool control's post-result reply was plain
(`您的订单 12345 已发货，预计明天送达。`).

**Consequences.**

- An instructions-only update is a sufficient phase change on qwen: it takes
  effect at the next reply and keeps over the following turn, mid-reply
  included. No full `session.update` is needed, which matters because a full one
  would re-assert turn detection.
- **Tools survive an instructions-only update**: the tool declared at start was
  still called afterwards. The tool control shows the order question calls it
  without any instruction, so the tools arm proves survival, not that the phase
  paragraph caused the call.
- The line before a tool call also follows the new instructions, as on doubao.

**NOT MEASURED**: a long or conflicting instruction (a second update that
contradicts the first), many updates in one session, an update carrying a slot
value the model must recall verbatim (the marker is a style rule, the easiest
kind of instruction to check), an update late in a long call when the provider
trims its own history, and a phase paragraph that competes with a tool result's
hint.

## W-Q8: a line asked for over an unanswered call repeats the call (2026-10-22)

**W-Q8 — a `response.create` asking for a line (`SpeakText`, a per-response
`instructions` override) while a tool's call is still unanswered is answered
with a second function call to the same tool with the same arguments, and
nothing on the request stops it.** Found in the live round of #286 (the wait
line), 4 calls on DID 95012, 2026-10-22 13:07–13:10, model
`qwen-audio-3.1-realtime-plus`.

**On the calls.** In every call the orchestrator said the wait line at the 1 s
mark, and the transcript shows `TOOL_CALL kb_search_slow`, then straight after
the line's request a second `TOOL_CALL kb_search_slow` with the same query. The
second call was dispatched like the first: the backend was hit twice, the two
results made two answer turns, and the answer was said twice. On the recordings:
4.6 s of silence and no wait line (call 1); the wait line only ~5.4 s after the
tool call, together with the answer (call 2); an apology after ~4 s instead of
2 s for a tool with `timeoutMs` 2000 (calls 3 and 4).

**Measured.** `TestLiveQwenLineCallsNoTool`
(`internal/provider/live_line_tools_test.go`), through the product client:
`lookup_order` declared at Start, the caller's order question (`q_order.pcm`),
the model calls the tool, and with that call unanswered the test asks for
"请稍等，我查一下。" with `SpeakText`. One session per arm:

| Line request | Line's turn |
|---|---|
| `response.instructions` only (as shipped) | said the line, then called `lookup_order {"orderId": "12345"}` again |
| plus `response.tool_choice: "none"` | the same: no error, the field ignored |
| plus `response.tools: []` | the same: no error, the field ignored |

The vendor's guide documents no field of `response.create`'s `response` at all
(the `instructions` override included, which is obeyed), so neither result
contradicts it. OpenAI documents a response-level `tool_choice` with `"none"`;
xAI documents the `instructions` override alone; the Realtime gateway refuses
`response.tool_choice` and `response.tools` as "Unsupported parameter"
(cascade-realtime-gateway, `internal/protocol/decode.go`).

**The fix (`provider.Realtime`).** A function call made in a line's turn
(`onLineCall`: the response created for a `responseKindLine` request) is never
handed out as a tool call, on any profile, and its item is deleted with
`conversation.item.delete` when that turn ends, however it ends, through the
same path that removes a cancelled turn's half-made call (W-Q5), so no later
turn reads it as a call still waiting for its output. The call the line was said
over belongs to the earlier turn and is answered as usual. On a profile that
obeys a response-level bar (`AcceptsResponseToolChoice`, openai only, on its
documentation), every line request also carries `tool_choice: "none"`, so the
model says the line instead of spending the turn on a call; qwen, grok and the
gateway do not get the field. With the fix the same session passed: the line was
said (first audio 1.1 s after the request), the provider's second call
(`call_bc1159eb…`) was kept back and its item deleted (`conversation.item.deleted`
acknowledged), the result made exactly one answer turn
(`您的订单已发货，预计明天送达。`), and nothing else started in the 6 s after it.
Covered by `TestACallMadeInALinesTurnIsNeverHandedOut`,
`TestALineRequestBarsToolsWhereTheProfileObeysIt` and
`TestOnlyALineRequestBarsTools` (`internal/provider/line_tools_test.go`).

**The line said nothing (live call on 320b827, the guard alone).** Call
01a1622a-950a-766a-af17-47812eae7178 (13:52:19): the wait line was asked for at
+1 s, the guard kept back the line's turn's call at 13:52:20.054, and the bot
channel of the recording is silent from the question (17.2 s) to the answer
(22.6 s). One backend hit, one answer, but no wait line: 2 of 5 live calls had no
line audio at all, the others said it late or together with the call.

**Why.** The two-sentence persona of the first test hides it: there qwen said the
line and called the tool as well. The flow's session carries the platform's
action-claim rule (`flow.Runtime.actionClaimRule`, #44): never say you are
looking something up unless you call the tool in the same reply, with
"我帮您查询一下" as its example. The default wait line, "请稍等，我查一下。", is that
sentence. Asked to say it word for word, the model obeys the standing rule
instead: it calls the tool, and often says nothing.

**Measured, on that session.** `TestLiveQwenWaitLine`
(`internal/provider/live_line_tools_test.go`) replays the call:
`testdata/line_session_zh.json` is the mobile_support session the orchestrator
built (instructions, the six tools, the opening line), `testdata/q_contract.pcm`
is the call's question ("我的合约机想提前解约，要交多少违约金？", qwen3-tts-flash,
voice Cherry), the wait line is asked for 1 s after the model's call and the
result is sent 3 s after it, as on the call. One session per run:

| Arm | Line said | Call made in the line's turn | Exactly one answer |
|---|---|---|---|
| `plain`: SayExactly, as on the call | **0 of 3** | 3 of 3 (kept back) | 3 of 3 |
| `direction`, then `shipped`: the direction below | **6 of 6** (3 + 3), first audio 1.6–2.0 s after the call | **0 of 6** | 6 of 6 |

The direction (`sayExactlyWithoutTools`): "这一轮只做一件事：一字不差地说出下面这句
话，不要添加任何其它内容，也不要调用任何工具。这句话提到的工具已经在运行或已经运行
过，结果会另外交给你，所以说这句话不违反关于工具调用的规则：" and the line; the
English twin says the same. It is the bar in words, and the second sentence is
what answers the action-claim rule. Arms B (a session-level `tools: []` around
the line) and C (asking for the line again after a silent turn) were not needed
and not measured.

Two checks on the same direction: the first test's two-sentence persona said
the line with no call at all (1 of 1, against a call in every earlier run), and
a closing line ("感谢来电,再见。", `SpeakText(isClosing)`, which on qwen also goes
into the conversation) after the answer was said as written with no call, 3 of
3 (`TestLiveQwenClosingLine`).

**What ships.** `Profile.NeedsToolFreeLineDirection`, true on qwen only: every
mid-call line (`SpeakText`) carries that direction in place of SayExactly, in
the override and in the conversation item where the line goes there too. The
opening line and a tool result's hint keep SayExactly. The guard stays on every
profile.

**NOT MEASURED**: how often qwen makes the call with the new direction over many
calls (6 sessions here), a phase announce mid-call with it, the English
direction, and the openai, grok and gateway behaviour (the openai bar rests on
its documentation; grok and the gateway keep SayExactly and the guard).

## W-Q9: an answer cut before its audio stays in the history as said (2026-10-22)

**W-Q9 — when the caller starts to speak just after a tool's answer was asked
for, qwen cancels the answer's response itself, with its transcript written and
none of its audio sent, and the model's next reply takes the answer as given.**
Found in the live round of #286, call 01a16256-ddd5-73b7-9335-13322c315bfa
(DID 95012, 14:40). The caller spoke over the end of the wait line ("哎喂你在吗？",
onset about 43.4 s); the result came back at 43.600 and its turn was asked for;
the transcript holds that turn's partial answer at 44.025
("合约期内变更套餐需支付200元违约金，由"); the next reply was "在的，请问还有什么
可以帮您？", and the 200 yuan was first heard 12 s later, in a dead-air prompt
that said "刚才提到". The recording's bot channel is silent from the end of the
line to the reply: none of the answer was played. The INFO log had no
interruption line at all: no audio of the answer had been queued, so the
session had nothing to flush or count and reported nothing, and the client
sent no truncate (one would not have helped: qwen ignores it, see the
measurement table above).

**Measured.** `TestLiveQwenSpeechOverTheAnswer`
(`internal/provider/live_unheard_test.go`) replays the call on the session of
`TestLiveQwenWaitLine`, with the caller's words (`testdata/q_are_you_there.pcm`,
"哎，喂，你在吗？", qwen3-tts-flash) starting 50 ms after the result. On the wire,
every run: `response.created` for the answer, 180–630 ms later (runs with the fix)
`input_audio_buffer.speech_started`, and in the same instant
`response.audio_transcript.done` (the answer's text, whole or nearly) and
`response.done` with `status: cancelled`, no audio delta in between. This
client sent no `response.cancel`: on a response that has sent no audio qwen
cancels on its own speech detection, although `CancelsResponseItself` stays
false (that trait was measured on a response already speaking). A first probe
with the caller's own audio cut from the call's recording reproduced the live
reply word for word.

| Client | Answer cut before audio | Reply gives the result |
|---|---|---|
| before the fix | 3 / 3 | 0 / 3 ("在的，请问有什么可以帮您？" every time) |
| with the fix | 3 / 3 | 3 / 3 ("在的。合约期内变更套餐需支付200元违约金，由人工客服办理。") |

**The fix (`provider.Realtime.removeUnheardTurn`).** A response that ends with
any status but `completed` before a single audio delta of it reached the
consumer was heard by nobody, so every spoken item it began
(`response.output_item.added` of a message, or a message in the response's
`output`) is deleted with `conversation.item.delete` when its `response.done`
arrives, before the requests that `response.done` releases, as W-Q5 does for a
half-made call. A response some audio of which reached the consumer stays the
consumer's to account for: it alone knows how much was played. The same holds on
every Realtime profile; the delete is the one W-Q5 sends everywhere, and a
refusal of it is not an error. The deletion is logged at INFO ("took a turn the
caller heard none of out of the conversation"), the trace this call lacked.

The other shape of an unheard answer is covered with it, from the unit tests
only: a turn whose audio was queued behind an earlier turn's tail and flushed
before a frame of it played (the consumer reports `Interrupt(…, 0)`, #53).
Its truncate to 0 is still sent, for an engine that honours it, and the item is
now deleted as well, at once if its response has ended and when it ends if not,
because qwen ignores the truncate. Covered by
`TestAnAnswerCutShortBeforeAnyAudioIsTakenOutOfTheConversation`,
`TestAnUnheardItemNamedOnlyInTheOutputIsTakenOut`,
`TestTheUnheardTurnIsTakenOutBeforeTheNextTurnIsAskedFor`,
`TestATurnWithAudioOrCompletedIsNotTakenOutHere` and
`TestATurnReportedUnheardIsDeleted` (`internal/provider/unheard_test.go`).

**NOT MEASURED**: the flushed-behind-a-tail shape on a live call; a turn heard in
part (qwen still keeps its whole text, #53); openai, grok and the gateway live.

**Deletion alone was not enough on a live call.** On 06b465d, call
01a1627d-7eae-77c2-98f7-e997fca088da (15:22): the caller spoke over the wait
line's tail about 450 ms before the result ("余伟你在吗？"), the session's
barge-in flushed the line's tail (`playedMs=1300`), the answer was asked for,
cut by qwen before its audio and deleted (the INFO line fired, status
`cancelled`), and the reply was still "在的，请问有什么可以帮您？"; the result came
12 s later in the dead-air prompt. The deleted item was the answer's: the line's
response had completed with audio, so it is never deleted on this path, and
`Interrupt(SPEECH, 1300)` aims at the client's latest spoken item, which from the
answer's `response.created` on is the answer's or none, and on qwen a truncate
changes nothing anyway. The INFO log cannot show whether qwen honoured the
delete or what else the history held.

**Measured, outside timing.** `TestLiveQwenUnheardResultNote` builds the history
directly (the model's call, the wait line, the result with no request for its
turn), then the caller's words, answered by qwen's own turn detection. Counts
are runs where the reply gave the result; "real" is the caller's audio cut from
the call's recording (not in the repository), "tts" is `q_are_you_there.pcm`:

| History after the result | real | tts |
|---|---|---|
| the cut answer's words kept (assistant item "合约期内变更套餐需支付200元违约金，由") | 0 / 4 | 1 / 4 |
| kept, plus the note | 5 / 5 | 4 / 4 |
| clean (no answer item) | 6 / 6 | 6 / 6 |
| clean, plus the note | 4 / 4 | — |

A follow-up question after the reply ("好的，那还有别的费用吗？") restated the
200 yuan in 2 of 4 with the note and 3 of 4 without: the note does not make the
result be said again. So the live reply is the one a history still holding the
answer's words draws, and the note overrides it.

**The shape, replayed.** `TestLiveQwenSpeechOverTheLineTail`: the caller's words
start 2550 ms after the model's call, over the line's tail, and the first
`speech_started` after the line gets the session's `Interrupt(SPEECH, 1300)`.
Without `removeUnheardTurn`: the answer cut before its audio 4 of 4, the result
given in the reply 3 of 4 (the miss was the run whose cut answer had written its
whole text). With it and the note: cut 3 of 4, given 3 of 3; the fourth run's
answer was not cut and was heard in full.

**The note (`provider.Realtime.noteUnheardResult`).** When the turn that answers
a tool result is deleted unheard (cut short before its audio, or reported
flushed with 0 ms played), the client puts one caller-side note in the
conversation, after the delete and before any request: "（系统提示：刚才的查询结果
还没有说给来电者听，来电者一个字也没有听到。下一次回复来电者时，先简短回应他，然后
把查询结果告诉他。）" (English for an English session). An answer turn is any
turn other than a line's made while a result waits that no turn has yet
delivered (run to its end with its audio handed out). Once per result: a
second unheard turn does not repeat it, a new result arms it again. Not for a
turn stopped to make room for a line, whose turn must say the line alone.
Covered by `TestAnUnheardAnswerIsNotedOnceForTheReply`,
`TestOnlyAnUnheardAnswerIsNoted`, `TestAnAnswerStoppedForALineIsNotNoted`,
`TestADeliveredAnswerIsNotNotedAndANewResultIs` and
`TestAnAnswerFlushedUnplayedIsNoted`.

**NOT MEASURED**: the note on a live call; the English note; openai, grok and
the gateway.

## W-Q10: a mid-call line's turn calls the tool its phase invites (2026-10-27)

**W-Q10 — a phase's `announce` asked for mid-call in a phase the conversation
goes on from (`SpeakText`, not closing, override alone) is answered by a turn
that calls a tool and says nothing, when the phase's instruction tells the model
to call that tool on the caller's next words.** Found on live call
01a17c7d-b4d4-7870-883e-b82a9553f3c6 (#351, `AI_OUTBOUND`, build c6586c8,
`qwen-audio-3.1-realtime-plus`). The flow's entry phase recorded the caller's
agreement with a slot tool and moved to a non-terminal phase whose `announce`
was a fixed line and whose instruction said that the line had been said and
that any reply, agreement included, means `hangup`. The line's turn called
`hangup`; `onLineCall` (W-Q8) kept the call back and nothing asked for the line
again, so the caller heard about 9 s of silence, spoke, and the model called
`hangup` then. The terminal goodbye that followed rode in the tool result and
was verbatim, as on six follow-up calls that day.

**Measured.** `TestLiveQwenLineAfterMove`
(`internal/provider/live_line_after_move_test.go`), through the product client,
driven as the orchestrator drives the call: the caller agrees
(`q_agree_zh.pcm` "好的。", `q_agree_en.pcm` "Okay, sure.", macOS `say`); on
the slot tool's call the instructions are re-pinned and the result (its hint
the new phase's instruction) is sent; when that response ends the line is asked
for; 4 s after the last line turn ends the caller agrees again, which the
phase's instruction says is a `hangup`. Counts are runs in which the line was
said, out of the runs measured: a run in which the turn the slot tool's result
asked for called `hangup` itself before the line pre-empted it is not counted
(see below).

The call's own flow (rendered by `flow.TestRenderLineAfterMoveSession` from the
revision on the dev database and kept off the repository), zh:

| Client | First ask said | Second ask said | The caller's answer drew `hangup` |
|---|---|---|---|
| main | 0 / 10, then 0 / 9 | never asked | 19 / 19, after the silence |
| main, without the flow's `global.temperature` (0.5) | 0 / 5 | never asked | 5 / 5 |
| main, persona and rules replaced by short plain ones | 0 / 5 | never asked | 5 / 5 |
| main, the phase's instruction replaced by a one-branch one | 2 / 5 | never asked | 5 / 5 |
| second ask, override alone | 0 / 9 | 0 / 9 (`hangup` again, given up) | 9 / 9, after the silence |
| second ask, direction also in the conversation, left there | 0 / 9 | 9 / 9 | 0 / 9: the entry phase's slot tool instead, 9 / 9 |
| second ask, direction also in the conversation, taken out after (shipped) | 0 / 10 | 10 / 10 | 10 / 10 |

`testdata/line_after_move_flow.json` is a flow of the same shape written for
the repository: a NovaNet Broadband speed-upgrade call, an entry phase with
four slot tools, and a booking phase whose line confirms the upgrade time and
whose instruction says what to call on the caller's next words. Its first
wording, in which the instruction said the system had played the confirmation,
drew no failure on main (zh 4 / 4, en 5 / 5 said). Saying instead that the
model had told the caller itself in its last sentence, and must neither speak
nor call a tool until the caller replies, drew it (zh 1 / 4, en 4 / 5 said);
that wording ships. Main and the shipped client, run side by side:

| Client | zh said | en said | zh second ask | en second ask | Answer drew `hangup` |
|---|---|---|---|---|---|
| main | 1 / 4, 1 / 10 | 4 / 5, 9 / 9, 7 / 10 | never asked | never asked | every run |
| shipped | 9 / 9 | 9 / 9, 8 / 8 | 7 of 9 first asks failed, all 7 said | 1 of 17 first asks failed, said | every run |

One more branch run (en) was cut short by the harness, not the client: an
opening the provider was late to start ended the run before the move (fixed in
the harness since). An earlier test flow with one slot tool and a one-branch
phase instruction drew the defect once in 64 runs on main. On the shipped
client the line asked for a second time began to play 1.5 to 2.4 s after it was
first asked for, against 0.9 to 2.2 s for a line said at the first ask.

**Why.** The standing instruction and the tool result's hint both tell the
model that the line is already said and that the caller's next words call for
a tool; the caller's last item is an agreement; a per-response override that
says "say this line" loses to both. Naming the tool as not to be called now,
and saying the caller has not heard the line yet, still loses as an override
alone. As a caller message it wins, and then keeps steering: left in the
history, it made the caller's next agreement read as an answer to the entry
phase's question again.

**The fix (`provider.Realtime.askForLineAgain`).** A line's turn that calls a
tool, runs to its end and hands the consumer no audio is asked for once more,
on every Realtime profile: the direction (`sayExactlyAfterCall`) names the
tools the turn called as not to be called now and says the caller has not
heard the line and that nothing they said before answers it (plus
`sayExactlyWithoutTools`' running-tool sentence on qwen). It goes in the
conversation as a caller message ahead of the request carrying it as the
override, and the message is deleted when that turn ends
(`removeAgainDirection`, its id from `conversation.item.created`). Not asked
again: a turn the caller heard (W-Q8's "said the line, then called the running
tool"), a turn cut short (the caller, a keypress, a newer line), a caller who
has started speaking. Once only: a second turn that calls a tool is given up
with a WARN, and the caller hears silence until they speak or the dead-air
watch prompts them. A turn owed to a result or a cue stays owed past the second
ask. The wait line asked again stays an aside (`Event.IsLineAskedAgain`).
Covered by `internal/provider/line_asked_again_test.go` and
`TestAnAsideAskedAgainMarksTheTurnThatSaysIt`.

**Seen on the way, not fixed here.** In 18 of 300 sessions the turn the slot
tool's result asks for (requested when the tool's response ends, before the
batch closes and the line is asked for) called `hangup` before the line's
cancel reached it. The orchestrator would run that call: the flow would move to
its goodbye with the line still waiting for the floor.

**NOT MEASURED**: a live call; the second ask on openai, grok and the gateway;
the call's own flow in English; a conversation that goes on for more than one
reply after the line.

## W-Q11: the opening line's direction stays the caller's first message and a later turn says the opener again (2026-10-29)

**W-Q11 — on qwen the opening line's say-exactly direction is put in the
conversation as a caller message (`NeedsCueForFirstTurn`) and used to stay
there for the whole call; the turn that answers tool results with nothing of
its own to say sometimes fulfils it a second time and speaks the opener word
for word.** Found on live call 28100b13-e746-444a-b8f1-e213e09591f6 (#360,
provider session `sess_O9ucxDrBNjCVSvVfX4x2E`, 2026-10-28, build 4e71a20, the
#262 live round, `AI_OUTBOUND`, `qwen-audio-3.1-realtime-plus`,
`campaign_lead_followup` en). The caller said "Stop calling." 2.7 s into the
opener; the model called `record_lead` and `hangup` in one response; the turn
that answered both results said the whole opener again (about 10 s) before the
closing line. It is not tied to the cut: the control below, where the caller
waits for the opener to be heard whole and then declines, repeated it too.

**Measured.** `TestLiveQwenCutOpener` (`internal/provider/live_cut_opener_test.go`),
through the product client, driven as the orchestrator and `aicall.Session`
drive a call: the opening turn is consumed, at the cell's offset the caller
speaks (macOS `say` clips in `testdata/q_*.pcm`), the first
`input_audio_buffer.speech_started` gets `Interrupt(SPEECH, playedMs)`, and
`record_lead` and `hangup` are answered as the orchestrator answers them (the
phase moves to the closing phase, the closing line rides in `hangup`'s answer).
The session's setup is `campaign_lead_followup`'s, rendered by
`flow.TestRenderCutOpenerSession` into `testdata/cut_opener.json`. The client's
frames pass through a local proxy that logs the provider's events and carries
the treatments. A run is a repeat when a turn after the cut carries at least
half of the opener's word trigrams (character 4-grams in zh).

First round, the client as it was (T0), with treatments applied by the proxy.
Counts are repeats out of runs:

| Cell | T0 (main) | T1: the direction deleted when the opening response ends | T2: the cut opener's item rewritten to what was heard, direction kept |
|---|---|---|---|
| en stop@2700 | 2 / 45 | 0 | 4 / 25 |
| en stop@1000, @5000, @11500 | 1 / 15 each | 0 | |
| en control (no interruption) | 1 / 10 | 0 | |
| en bye, drive, stop2 | 0 | 0 | |
| zh stop@2700 | 3 / 40 | 0 | 1 / 10 |
| zh control | 1 / 10 | 0 | |
| zh other cells | 0 | 0 | |
| all cells pooled | 10 / 245 | 0 / ~210 (0 / ~235 with the 25 runs that also had T2) | no benefit |

Pooled, T0 against the direction deleted: 10 / 245 against 0 / 235 (T1 runs
and the T1-with-T2 runs), Fisher exact, one-sided p = 0.001. The history edit that matters is the direction,
not the cut opener's item: T2 alone rewrote the cut opener to the heard prefix
and kept the direction, and repeated as often as main.

Wire facts, all from the proxy's log: qwen sends no acknowledgement of
`conversation.item.truncate` and the cut opener's item keeps its full text
(W-Q9); `conversation.item.delete` of the direction is accepted and
`conversation.item.deleted` is echoed; the opening response completes about
2.2 s in, long before the caller speaks, so the delete is sent while the
opener is still being played out; in the T1 runs the opener was still said in
full in turn 1.

**The fix (`provider.Realtime.removeOpeningDirection`).** Where the opening
request put its direction in the conversation (qwen, with `OpeningText`), the
item is deleted when the response that answers that request ends, whatever its
status, from `handleResponseDone` before anything it releases is asked for,
like W-Q10's `removeAgainDirection`; its id comes from
`conversation.item.created` (`noteCreatedItem`) and an item the provider never
named stays. An opening request that is retried (`onRequestExpired`), or
replaced by a line waiting for the floor, keeps the direction until the
response that finally answers it has ended; the caller taking the floor over
the opener cancels the response and the delete still follows. Nothing is sent
on openai, grok or the gateway (no cue item), and the bare greeting cue of a
flow with no opening line stays (not measured). Covered by
`internal/provider/opening_direction_test.go`.

**Measured again on the shipped client.** The same probe, with the proxy
swallowing the client's delete of the opening direction for "main" (the
client's behaviour before the fix) and leaving the client untouched for
"shipped". Every run also checks that the opener was said in turn 1 (its
transcript covers at least half of it) and that the provider echoed
`conversation.item.deleted` for the direction. Repeats / runs:

| Cell | en main | en shipped | zh main | zh shipped |
|---|---|---|---|---|
| stop@2700 | 3 / 40 | 0 / 30 | 0 / 40 | 0 / 30 |
| stop@1000 | | 0 / 10 | | 0 / 10 |
| stop@11500 (en) or @9800 (zh), the last second | | 0 / 10 | | 0 / 10 |
| control | | 0 / 10 | | 0 / 10 |
| bye | | 0 / 10 | | 0 / 10 |
| drive | | 0 / 10 | | 0 / 10 |
| stop2 | | 0 / 10 | | 0 / 10 |
| all cells | 3 / 40 | 0 / 90 | 0 / 40 | 0 / 90 |

The opener was said in turn 1 in every run (all 300 runs of the cells above and
the "who" cells below), the delete was echoed in every shipped run and in none of the main
runs. The proxy therefore reproduces the old wire; its repeat rate on this
setup is lower than the first round's (main 3 / 80 here, 10 / 245 there), so
the first round's count is the one the p-value rests on. Pooling every main
run of both rounds against every shipped run: 13 / 325 against 0 / 180, one-sided
p = 0.003. The closing line was said (coverage of at least 0.6) in 19 / 30 (en)
and 28 / 30 (zh) shipped stop@2700 runs against 30 / 40 and 40 / 40 on main; the
shipped runs that did not say it ended with an improvised "Goodbye." or "I'll make sure you're not
contacted again." in the turn that answered the results.

**"Who is this?"** The caller asks "Who is this?" / "你是谁？" over the opener at
2.7 s. Repeating the opener there is acceptable; losing who is calling is not.
A reply identifies when it names the company or the assistant. Of 10 runs in
each cell, every reply identified, on main and shipped, en and zh (40 / 40);
none repeated the opener whole. What changed is the wording: on main the
reply was often the opener's own first sentence again (zh main 10 / 10 are
"我是星讯手机的 AI 语音助手。"), and now it is a fresh, shorter answer. The word
"AI" is in 7 / 10 en and 10 / 10 zh replies on main and in 1 / 10 en and 3 / 10
zh replies on shipped. Every reply of the shipped client:

- en: "This is the StarCom Mobile assistant calling to follow up on your interest
  in our 5G Plan." / "This is the outbound voice assistant from StarCom Mobile,
  calling to follow up on your interest in our 5G Plan." / "This is the StarCom
  Mobile AI assistant calling to follow up on your recent inquiry about our 5G
  Plan." / "This is the StarCom Mobile assistant calling about the 5G Plan you
  asked about." / "This is the StarCom Mobile assistant calling about your
  interest in our 5G Plan." / "Hi Chen, this is the StarCom Mobile assistant
  following up on your interest in our 5G Plan. We have a special offer of 20%
  off the first six months, would you like to hear more …" / "This is StarCom
  Mobile calling about the 5G Plan you asked about." / "Hi Chen, this is the
  StarCom Mobile assistant following up on your interest in our 5G Plan. We
  currently have an offer of 20% off the first six months, are you still
  interested …" / "This is the StarCom Mobile assistant calling about the 5G
  Plan you asked about. We have an offer of 20% off the first six months, and I
  wanted to see if you're interested." / "This is the StarCom Mobile assistant
  calling about your recent inquiry into our 5G Plan."
- zh: "我是星讯手机的语音助手，想跟您聊聊之前咨询过的 5G Plan 优惠活动。" /
  "我是星讯手机的语音助手，代表星讯手机对近期咨询过产品的客户做一次简短的电话回访。请问您对我们的 5G Plan 优惠活动有兴趣吗？" /
  "我是星讯手机的语音助手，代表公司对您近期咨询的产品做一次简短回访。" /
  "我是星讯手机的语音助手，代表星讯手机给您做个简短的回访。请问您最近有关注我们的5G Plan吗？" /
  "我是星讯手机的语音助手，代表公司对您进行电话回访。" /
  "我是星讯手机的语音助手，代表公司对您近期咨询的5G Plan做个简短回访。请问您现在方便聊几句吗？" /
  "我是星讯手机的 AI 助手，这次来电是想了解一下您对 5G Plan 优惠活动的意向。" /
  "我是星讯手机的语音助手，代表星讯手机对您做一次简短的电话回访。请问您现在方便吗？" /
  "我是星讯手机的 AI 助手，代表星讯手机对您近期咨询的 5G Plan 做一次简短回访。目前有个优惠活动，首六个月可享 20% 折扣，请问您有兴趣了解一下吗？" /
  "我是星讯手机的 AI 语音助手，今天来电是想了解一下您对 5G Plan 优惠活动的意向。"

The company is always named, but the AI disclosure is not. It had been
carried by the leftover direction, which made the model re-say the opener's
first sentence; without it the seed flow's rule "You are an AI assistant; say so
honestly if asked." was left to decide, and it did not always.

**The rule change.** `internal/seed/flows/campaign_lead_followup.json` now says,
in both languages, that when the caller asks who you are, who is calling, or
whether you are a person, you say you are StarCom Mobile's AI voice assistant
(zh: 星讯手机的 AI 语音助手). The fixture was re-rendered from the changed flow
and the "who" probe now counts a reply as disclosing when it says it is an AI.
On the shipped client with the new rule, over the opener at 2.7 s, n = 20 per
language: identified 20 / 20 and disclosed the AI 20 / 20 in en, identified
20 / 20 and disclosed 20 / 20 in zh, and no reply repeated the opener whole
(the first sentence-length answers are "This is StarCom Mobile's AI voice
assistant calling to follow up on your interest in our 5G Plan." in en and,
in 18 of 20 zh runs, exactly "我是星讯手机的 AI 语音助手。", in the other two
followed by "请问有什么可以帮您？"). The deployment owns the wording: a flow that
must disclose the AI on other turns says so in its instructions.

**Stop at 2.7 s on the new fixture**, repeats / closing line said / other, n = 30
per cell (opener said in turn 1 and, for shipped, the delete echoed, in every
run):

| Client | en | zh |
|---|---|---|
| main (delete swallowed) | 1 / 23 / 6 | 1 / 29 / 0 |
| shipped | 0 / 21 / 9 | 0 / 30 / 0 |

**Headline.** None of the 180 non-"who" shipped runs of the first fixture, nor
of the 60 on the new one, repeated the opener; main (the first round's T0, the
second round's swallowed delete and the new fixture's) repeated 15 / 385
across all of them, one-sided Fisher p = 0.0006 against 0 / 240 shipped. The closing line: the
earlier suspicion that the shipped client says it less often in zh does not
hold. On the new fixture the closing line was missed in 9 / 60 shipped and
8 / 60 main stop@2700 runs (p = 0.5), zh 0 / 30 and 1 / 30 (p = 1.0). On the
first fixture it was missed in 13 / 60 shipped against 10 / 80 main (p = 0.11),
and pooled over both fixtures 22 / 120 against 18 / 140 (p = 0.15): a
difference of this size is not established either way. The runs that missed it
ended on an improvised "Goodbye." or "I've noted your request and will stop
calling." in the turn that answered the results; the rule change did not move
the stance or the closing behaviour.

**Live calls (2026-10-29).** 17 calls on the dev stack, a build of this branch
(5ca168f plus the fix), `qwen-audio-3.1-realtime-plus`, `campaign_lead_followup`
with the new AI rule on DID 95061 (en) and 95062 (zh), one single-contact
campaign per call to the owner's softphone; the owner answered and spoke the
shape asked for. Each call was checked from the debug log (barge-in, the
direction's removal and the provider's confirmation, tools, #262's second ask,
the hang-up), the stored transcript and the CDR. Totals: the opener repeated
0 / 17; the direction deleted and the deletion confirmed by qwen 17 / 17
(including two openings qwen cancelled itself, calls 8 and 13); the closing
line said verbatim before the hang-up 17 / 17 (3 of them on #262's second ask);
every CDR NORMAL_CLEARING; "Who is this?" answered with the AI identity 3 / 3
(calls 4, 11, 15). "Cut" is the caller's playedMs at the interruption.

| # | Shape | Call id | Cut | Outcome |
|---|---|---|---|---|
| 1 | en stop, under 1 s | 86fa1ea1-f9c3-4454-90ed-84ef3b423491 | 5820 | stop, DNC, hang-up |
| 2 | en stop, ~3 s | 5dae0d99-3313-4e8b-913b-c3fc596616a8 | 3240 | stop, DNC |
| 3 | en stop, last second | 636ee3a9-11e3-48f5-a140-38b07f2fe348 | 11080 | stop, DNC |
| 4 | en who | 96a6e839-82ab-4edf-b74d-576bad43a81a | 9700 | AI identity, then stop, DNC |
| 5 | en stance + bye | 9bf25ee0-a99a-4631-8131-b62dcd5af45d | 4040 | fragment reply deleted (W-Q9), DNC, closing line on the second ask |
| 6 | en half word, mixed | c41b1562-42ed-4c99-b085-61e28b155013 | 3400 | fragment reply deleted, one fresh question, stop, DNC |
| 7 | en control, no cut | 522ff001-bfa0-41ba-a9be-d70bdedadacb | none | opener heard whole, stop, DNC |
| 8 | zh stop, under 1 s | da8631b5-8faa-4099-8e1b-818f60b12e24 | 1340 (qwen cancelled the opener) | re-introduced in its own words, AI disclosed, stop, DNC |
| 9 | zh stop, ~3 s | 262d2ccd-0158-4e01-abaa-9b520c74e01d | 2260 | stop, DNC |
| 10 | zh stop, last second | 38af2716-7894-4197-9dd6-af7393e836b5 | none (spoke after the opener) | one question, stop, DNC, closing line on the second ask |
| 11 | zh who | 5e0628dd-fb34-44ca-b622-8ee54de23fa5 | none (asked after the opener) | AI identity, stop, DNC |
| 12 | zh stance + bye | bfe6749a-0b33-40be-baf8-d5866a397d7f | 2800 | stop, DNC |
| 13 | zh half word, mixed | 20ad5c8c-dd3b-4768-b14b-9587aab2917c | 2340 (provider) | fragment reply deleted, stop, DNC |
| 14 | zh control, no cut | d29e267b-13fc-4798-83e4-5c30b065252c | none | stop, DNC |
| 15 | zh who over the opener | b49e6d3c-9d87-4e21-9741-5103a9ce4364 | 2980 | AI identity, stop, DNC |
| 16 | zh stop, last second | 5c5c3d6a-fd5b-49a6-b7c6-d05ddf5bcdc0 | none (spoke after the opener) | stop, DNC |
| 17 | en stop, immediately | 12dec4f7-1bad-405b-a28b-e98a8e2591cd | 1560 | fragment reply deleted, stop, DNC, closing line on the second ask |

Not reached live: a cut inside the first second (the earliest was 1340 ms: the
800 ms barge guard plus reaction time), and a zh cut in the opener's last second
(twice the owner spoke just after it ended; the probe covers it, zh shipped
stop@9800 0 / 10).

**NOT MEASURED**: real caller recordings (the clips are synthesised by `say`; the live
calls were the owner's voice, 17 of them);
the bare
greeting cue of a flow with no opening line, which stays in the conversation;
openai, grok and the gateway, which are unchanged by construction (no cue item,
no new frame; `wire_golden_test.go` and `opening_direction_test.go`); the
retry (`onRequestExpired`) and replaced-by-a-line paths of the opening request,
which `opening_direction_test.go` covers and no call reached; the no-input path of the cue experiment earlier in this document, where removing
the cues left the opening direction as the last caller item and the model
answered it; other flows than
`campaign_lead_followup`; other qwen models.

## Model: qwen-audio-3.1-realtime-flash (2026-11-03)

Issue #463. Everything above was measured on `qwen-audio-3.1-realtime-plus`.
The 2026-09-24 section recorded `close 1007 "Model not found
(qwen-audio-3.1-realtime-flash)!"`. This section re-measures on flash. It
was written before the profile changed. The profile's model became flash the
same day (owner decision, #463), together with the `required` fix below; the
traits themselves are unchanged.

**Method.** `AICC_PROVIDER_MODEL` now reaches the live tests (`liveProfile` in
`internal/provider/live_test.go`): it replaces the model of the profile whose
name equals `AICC_PROVIDER`, and nothing else. Command:

```sh
AICC_PROVIDER=qwen AICC_PROVIDER_MODEL=qwen-audio-3.1-realtime-flash \
  AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run '<TestLiveQwen...>' -v ./internal/provider/
```

Each test ran 3 times (6 for `TestLiveQwenMidCallInstructions` and
`TestLiveQwenWaitLine`, traits recorded 6 of 6 on plus). The "plus, today"
column is the same tests run the same day on plus, with the same number of
runs. The test log prints `qwen (<model>)`, which shows the model that ran.

### Handshake

The endpoint accepts `model=qwen-audio-3.1-realtime-flash`. It answers
`session.created` and then `session.updated` for the profile's `session.update`
(`longanqian_v3.1`, `server_vad` at 500 ms, `temperature` 0.3, tools):

| Echo | `session.created` | `session.updated` |
|---|---|---|
| `model` | `qwen-audio-3.1-realtime-flash` | `qwen-audio-3.1-realtime-flash` |
| `voice` | `Chelsie` (the connection's default) | `longanqian_v3.1` |
| `turn_detection` | `server_vad`, threshold 0.5, prefix 300 ms, silence 800 ms | `server_vad`, threshold 0.5, prefix 300 ms, silence 500 ms |
| `temperature` | not present | 0.3 |
| `input_audio_format` / `output_audio_format` | `pcm16` / `pcm24` | `pcm16` / `pcm` (the formats are not echoed as sent, as on plus) |
| `tools` | not present | echoed, `type` `function` |

`response.create` on an empty conversation is refused on flash too
(`Cannot create response: conversation has no messages or no user message.`),
with or without tools. `response.done` carries a `usage` object on flash.

### A tool whose `parameters` has no `required` fails on flash

With tools declared and a first response requested, flash answers
`response.done status=failed` and `error InternalError: Internal service error:
An internal error occurred while generating the response`, then closes the
socket (`1011`, once `1007`). `session.updated` had been acknowledged; the
response is what fails. Plus accepts the same tools. Whether `parameters`
carries a `required` key decides it (probe on flash: the product's
`session.update`, a cue item and a bare `response.create`; each shape twice,
the same result both times):

| `parameters` | Result |
|---|---|
| `{"type":"object","properties":{"orderId":{...}}}` (no `required`) | InternalError |
| `{"type":"object","properties":{}}` | InternalError |
| `{"type":"object"}` | InternalError |
| the same with `additionalProperties: false`, no `required` | InternalError |
| `required: ["orderId"]`, with or without a property description | completed |
| `required: []`, with or without properties | completed |

`provider.Realtime.buildTools` sends `parameters` as the flow supplied it, and
substitutes `{"type":"object","properties":{}}` when there is none, so a tool
without `required` fails the first response of any session that declares it.
Run as the repository stands, 30 of 30 runs of the tool-declaring tests failed
this way (`MidCallInstructions` tools arms, `LineAfterMove`, `LineCallsNoTool`,
`WaitLine`, `ClosingLine`, `SpeechOverTheAnswer`, `SpeechOverTheLineTail`,
`UnheardResultNote`); `Linear`, `AcceptsATemperature`, `Truncate` and the no-tool
arms of `MidCallInstructions` passed.

The rows below were measured with a working-tree-only patch to `buildTools`
that adds `"required": []` to a `parameters` object that lacks one. That patch
is now `withRequired` in `buildTools`, applied to every Realtime profile. Plus
was measured without it; a later run of ClosingLine and WaitLine on plus with
`withRequired` is recorded at the end of this section.

### Results

| Test | Trait it measures | Flash | Plus, today | Plus, recorded |
|---|---|---|---|---|
| `Linear` | 16 kHz in / 24 kHz out is what the model speaks | 3 of 3 runs | 3 of 3 | 2026-08 spike |
| `AcceptsATemperature` | `AcceptsTemperature`: `session.temperature` taken on the opening `session.update`, no reduced retry | 3 of 3 runs | 3 of 3 | accepted |
| `MidCallInstructions` idle, speaking | instructions-only update obeyed from the next reply (W-Q7) | 6 of 6 and 6 of 6 in each of 6 runs | same | 6 of 6, 6 of 6 |
| `MidCallInstructions` control | no marker without the update | no marker in any of 6 runs | same | 0 of 9 |
| `MidCallInstructions` tools | tools survive the update; the tool is called; the reply after the result carries the marker | tool called in 17 of 18 sessions (5 runs 3 of 3, 1 run 2 of 3: the model asked for the order number instead); marker 18 of 18 | tool called 15 of 15, marker 15 of 15 (5 runs; the 6th run was a network failure, below) | tool called 3 of 3 |
| `LineCallsNoTool` | `AcceptsResponseToolChoice` false / `NeedsToolFreeLineDirection` (W-Q8): a line over an unanswered call | 3 of 3 runs: line said, answer given, no call in the line's turn | 3 of 3 | the response-level field is ignored; the call is repeated unless the direction is used |
| `WaitLine` plain | the line asked for with SayExactly | line said 18 of 18; a call made in the line's turn in 17 of 18 sessions | line said 18 of 18; call made 18 of 18 | line said 0 of 3; call made 3 of 3 |
| `WaitLine` shipped | `NeedsToolFreeLineDirection`: the tool-free direction | line said 18 of 18; a call made in the line's turn in 2 of 18 (1 of 3 in two runs; the client kept it back); exactly one answer 18 of 18 | line said 18 of 18; call made 0 of 18; one answer 18 of 18 | line said 6 of 6; call made 0 of 6 |
| `ClosingLine` | a closing line after the answer is said as written, with no call | 9 of 9 | 9 of 9 | 3 of 3 |
| `LineAfterMove` | `NeedsDirectedLineInConversation` (W-Q1): a phase line after a move is said | zh said 9 of 9, en 9 of 9; the result's turn acted before the line in 0 of 18 | zh said 8 of 8, en 7 of 7; the result's turn acted before the line in 3 of 18 | shipped: zh 9 of 9, en 9 of 9 and 8 of 8 |
| `UnheardResultNote` kept, no note | the cut answer's words stay in the history: the reply gives the result | 7 of 9 | 2 of 9 | 0 of 4 (real), 1 of 4 (tts) |
| `UnheardResultNote` kept, note | the note makes the reply give the result | 9 of 9 | 9 of 9 | 5 of 5, 4 of 4 |
| `UnheardResultNote` clean, with and without note | clean history | 9 of 9 and 9 of 9 | 9 of 9 and 9 of 9 | 6 of 6, 4 of 4 |
| `SpeechOverTheAnswer` | `removeUnheardTurn`: the answer is cut before its audio; the reply gives the result | cut 9 of 9; given 8 of 9 | cut 7 of 9; given 7 of 7 | cut 3 of 3; given 3 of 3 |
| `SpeechOverTheLineTail` | the same on the line-tail shape | cut 9 of 9; given 9 of 9 | cut 8 of 9; given 8 of 8 | cut 3 of 4; given 3 of 3 |
| `Truncate` | `conversation.item.truncate` at 0 and 300 ms is accepted with no error; what the model then recites | 3 of 3 runs, no error | 3 of 3 runs, no error | no acknowledgement, no error |
| `CutOpener` shipped (en and zh: `stop@2700`, `stop@5000`, `drive@2700`, `who@2700`; 3 sessions per cell) | W-Q11: the opening direction is deleted; the opener is not said again after a cut | opener repeated 0 of 24; deletion echoed 24 of 24; the opener said in turn 1 24 of 24; "who" replies that identified the bot 6 of 6 | not re-run | repeat 0 of 30 / 0 of 10 per cell; deletion echoed in every shipped run |

Notes on rows:

- `MidCallInstructions` on plus, run 1 of 6: `read tcp ...: connection reset
  by peer` at about 10:36, when the host's LAN address changed (DHCP). That is
  a network failure, not a model result; the run is left out of the plus counts.
- `Truncate`: after a truncate at 0 ms the model recited a different code from
  the one it had said in 3 of 3 runs on both models today (another code, or a
  greeting); after 300 ms and with no truncate it recited the code it had said,
  3 of 3 each. The 2026-10-01 record ("the model recites it in full every
  time") does not hold for the 0 ms variant on either model today.
- `WaitLine` shipped on flash: in the two sessions where the model made a call
  in the line's turn, the client kept the call back (`onLineCall`); the line was
  said and exactly one answer followed.
- `UnheardResultNote` kept, no note: the count is of sessions whose reply gave
  the result.

### Not measured

No live test covers these, on either model; none was written here.

- `AcceptsResponseToolChoice` as a field (accepted and ignored): only its
  consequence is exercised (`LineCallsNoTool`).
- `NeedsCueForFirstTurn` and the removal of the opening direction after the
  opening turn (W-Q11) are exercised through `CutOpener` only. The
  empty-conversation refusal was seen in the probe above.
- Turn detection fixed once audio flows, out-of-band responses ignored, and the
  `_v3.1` voice honoured only in the first `session.update`: the voice and the
  `server_vad` value were echoed in the probe above; no test sends a second
  voice or a late turn-detection change.
- `CutOpener`: the other 60 cells of the 68-cell matrix (main, T2 and T3
  treatments, `stop2`, `bye`, the other offsets, the controls) were not
  re-measured on flash. A first attempt at the full matrix was stopped at the
  15-minute test timeout.
- The open qwen defects (#197, #310, #387, #403, #448, #453) were not retested.

### After the fix (2026-11-03)

`withRequired` in `buildTools` now gives every tool of every Realtime profile a
top-level `required`. One run each, `-count=1`, the model taken from the
profile (flash) or from `AICC_PROVIDER_MODEL=qwen-audio-3.1-realtime-plus`:

| Test | Flash (default) | Plus |
|---|---|---|
| `ClosingLine` | 3 of 3, passed | 3 of 3, passed |
| `WaitLine` | plain: call made 3 of 3; shipped: call made 0 of 3; line said 3 of 3 each; one answer 3 of 3 | the same counts |
| `MidCallInstructions` | failed: tools arm called the tool in 2 of 3 sessions (the third reply was "好的，王先生。" with no call), marker 3 of 3; control, idle, speaking and tools-control as before | not run |
| `LineAfterMove` | zh and en: line said 3 of 3 each; the result's turn acted before the line 0 of 6 | not run |

With the earlier run's 17 of 18, the tools arm of `MidCallInstructions` called
the tool in 19 of 21 flash sessions, in 2 of 7 runs short by one session (plus:
15 of 15 in 5 runs).

### Owner-placed A/B calls (2026-11-03)

There were 9 calls per model on the same build (`591b18a`), with the same script on both.

- **Calls 1–6**: an outbound slot-tool flow whose terminal phases carry fixed lines. The scenarios were agree; ask then agree; decline twice; do-not-call over the opening; silence; and backchannels.
- **Calls 7–9**: `mobile_support` on 95012 with the HTTP tool `repair_status`. Call 9 makes three lookups: not found, then two valid numbers.

| # | Flash | Plus |
|---|---|---|
| 1, 2, 4, 5, 7, 8 | pass | pass |
| 3 decline twice | closing line said again in full (#465) | the same |
| 6 backchannels | "嗯" taken as agreement; closing line said 5 times (#403, #465) | the same, 4 times |
| 9 three lookups | the 2nd and 3rd result turns repeated "好的，我查一下。" and did not read the result (#466) | all three results read, no filler |
| Answer to first bot audio | 0.73–1.01 s | 0.95–2.16 s |

The failures shared by both models are platform defects (#465, #403). The flash-only one is #466.

Owner decision, 2026-11-03: flash stays the default, for cost. #466 tracks the lookup weakness, and a lookup-heavy deployment can stay on plus with `AICC_PROVIDER_MODEL`.

## W-Q12: a later lookup's result turn repeats the tool-call turn's filler (2026-11-04)

**W-Q12 — on `qwen-audio-3.1-realtime-flash`, the filler said in the turn that calls an HTTP tool ("好的，我查一下。") was said again as the result turn of the call's 2nd and later lookups, and the result was not read.** Found on the owner-placed A/B call 9 of 2026-11-03 (`mobile_support` on 95012, three lookups: not found, then two valid numbers; plus read all three results). Issue #466.

Two facts the issue did not state: the seed flow `mobile_support` itself asked for the filler in its repair-status instruction ("最多说一句'好的,我查一下'"), and the platform already has a wait line (`global.toolWaitLine`, said by `SpeakText` when an HTTP tool runs past `ToolWaitLineAfter`, 1 s), which also stays in the conversation as an assistant turn. The result turn itself is a bare `response.create` (`Realtime.SendToolResult`).

**Owner's call (2026-11-04):** probe first, then pick by the numbers. Decision rule: B1 (a built-in rule: say nothing in a turn that calls an HTTP tool) if it reaches 0 of 10 repeats; otherwise A (a per-response direction on the result turn); C (detect a repeat and ask once more) only if neither does. Spent: 68 qwen sessions.

**Method.** `TestLiveQwenRepeatLookup` (`internal/provider/live_repeat_lookup_test.go`): one session on `mobile_support` (zh) as the orchestrator drives a call, rendered by `flow.Runtime` against the mock backend (`testdata/repeat_lookup.json`). Three lookups: RMA9999 (found=0), RMA1002 (moves to `repair_report`), RMA1001 (moves to `repair_report_multi`). A result turn passes when it carries the result's key field and does not repeat the previous bot text. `-count=1`, one session after another.

### Reproducing the defect took three ingredients

| Harness | Flash sessions | Sessions with a repeat |
|---|---|---|
| TTS caller clips, result sent 300 ms after the call, caller waits for playback | 10 | 0 (29/29 result turns read) |
| Same, but the caller speaks over the previous answer's tail (client sends `conversation.item.truncate`) | 10 | 0 (30/30) |
| The owner's own utterances cut from the flash call's recording (kept locally, not committed), barge-in, 300 ms | 5 | 0 by text; 1 suspect (a 1.2 s result turn with no transcript, then the found=0 answer one turn late) |
| Owner clips, barge-in, **result sent 3 ms after the call** (what the dev mock backend took on the calls; the filler was still being made) | 5 | **1** (lookup 2's result turn was `好的，我查一下。`) |

The arms below all use that last harness; 3 ms is the test's default.

### Arms (owner clips, barge-in, 3 ms; 10 sessions each)

| Arm | Sessions with a repeat | Result turns read | found=0 asks to check | Filler said, no tool call | Tool-call turns that said the filler |
|---|---|---|---|---|---|
| Baseline, flash | **1 / 10** | 27 / 28 | 10 / 10 | 2 | 25 / 28 |
| Baseline, plus | **0 / 10** | 30 / 30 | 10 / 10 | 0 | 20 / 30 |
| B1, flash (built-in "say nothing in a repair_status turn" rule, seed filler invitation removed; lookup 2 always and lookup 1 in even sessions take 2 s) | **0 / 10** | 30 / 30 | 10 / 10 | 0 | **12 / 30** |
| A, flash (the result turn's `response.create` carries the standing instructions plus "answer from the result now, do not repeat what you said") | **0 / 10** | 29 / 29 | 10 / 10 | 1 | 27 / 29 |

Notes:

- Two result turns in baseline flash and one in B1 were first judged failures. They said "没找到" and "查询不到", which the not-found pattern lacked. The pattern was widened, and the counts above include those turns as passes.
- **B1:** the platform wait line was said 13 times (slow lookups whose tool-call turn was silent) and was never copied into the result turn. But the rule did not stop the filler: flash still said "好的，我查一下。" in 12 of 30 tool-call turns, against 25 of 28 in baseline.
- **A:** the override was accepted, both directly and as the owed turn released after the tool-call response. The phase's rules held: on found=0 the bot asked the caller to check the number in 10 of 10.
- "Filler said, no tool call" is the #463 shape (the turn after the caller's number says `好的，我查一下。` and calls nothing). It is counted apart from repeats and is not part of this issue.
- **Caveat:** the baseline rate in this harness is 1 in 10 sessions, far below the owner's call (2 of 2 later lookups). A 0/10 arm is weak evidence: with no effect at all, 0 of 10 would still come up about a third of the time. Neither B1 nor A is separated from the other by these counts.

### Decision

B1 shipped, by the decision rule and because it is the smallest change and helps every flow. `flow.Runtime` adds a platform rule (`lookupSilenceRule`) naming the HTTP tools the phase allows: the reply that calls one only calls it and says nothing, and the result is told directly. The action-claim rule's example no longer endorses the filler with the call ("if you are not calling the lookup tool, do not say ..."). The rule wins over a flow's own instruction that asks for a filler (owner, 2026-11-04); the `mobile_support` seed flow no longer asks. The shipped text is byte for byte what the B1 arm sent. B1 does not remove the filler (still said in 12 of 30 tool-call turns). No result turn under it repeated anything, 0 of 10 sessions against 1 of 10 for baseline, which is weak evidence at that rate.

The A arm (a per-response direction on the result turn) also measured 0 of 10 and was not shipped: it needs a profile trait and a per-response frame, and it did not stop the filler either (27 of 29 tool-call turns said it).

**Fix verified on plus** (shipped rendering; owner clips, barge-in, 3 ms; 10 sessions): no repeat in 10 of 10 sessions, 30 of 30 result turns read, found=0 asks to check 10 of 10, no filler in any of the 30 tool-call turns (20 of 30 before the fix), and the wait line said on all 15 slow lookups and never repeated as the result turn. Flash on the shipped rendering is the B1 arm above: the zh text is byte for byte what that arm sent.

## W-Q13: a caller turn committed while a manual response runs is refused, not answered (2026-11-08)

**W-Q13 — qwen commits the caller's speech with `server_vad` and creates no response for it when a response this client asked for ("manual", typically the tool-result turn) is still in progress:** `invalid_value: Server VAD turn committed but no response was created because a manual response is already in progress.` Seen on 2026-10-13 (plus, call `01a13363-73fd-7af9-8223-8fe721f6027d`, lead_qualification on 95099): the caller's "Next week." waited 10 s for the dead-air cue. Issue #197.

**What the client does now.** `handleError` recognises the words (`isVADTurnRefused`, any code) and owes the caller's turn through W-Q4's flag (`onVADTurnRefused`): with a response open it is asked for when that response ends; with a request in flight and nothing open it is owed past that request's creation, because the provider made that response before it committed the speech; with the floor free it is asked for at once, unless the caller is speaking again. One flag, so a turn already owed to a tool result or a cue is still one `response.create`. The error is no longer reported as an error: one INFO line (`the provider committed the caller's turn without answering it; the turn is owed`).

**The window is narrow.** On the 2026-11-08 round, five natural attempts on flash (95088 x3, 95099 and 95555 outbound, the caller answering with two phrases without waiting for the bot) produced no refusal. The refusal needs the caller's next phrase to start before the tool-result response is created and to be committed while it is still generating, which takes about 1-2 s on flash. A phrase that starts once that response plays is a barge-in: qwen cancels the response itself (`01a1b8ec-a0d8-7b2b-a76f-5e8dc85ddf82`, early_collections: the bill_lookup answer cut at `playedMs=200`) and answers the new speech. On #448's live round (2026-11-06) one call in eleven produced it.

**Verified by injection.** The fault-injecting relay (`aicc-mockprovider -upstream … -fault vad-refused`, docs/load-tests.md) hides qwen's reply to the first caller turn and sends this error in its place. On flash (`01a1b8e3-91df-7c10-9195-87f1c1c3e07e`, 95012) the turn was asked for at once and the bot spoke 868 ms after the caller stopped; no WARN. Behind the relay on the gateway profile (mlx-realtime, `01a1b8ce-a831-7e9f-9f61-c61165804298`): 724 ms, no WARN. The injected case is the free-floor branch; the open-response and in-flight branches are covered by `owed_vad_turn_test.go` only.

**Which reply follows.** Not observed on a natural refusal (none occurred). On D1 above, flash's reply was the generic "您好，这里是星讯手机售后服务热线。请问有什么可以帮您？", its usual first reply to that question (W-Q14), and the next reply answered it.

**Other profiles.** No findings doc records this error for openai, grok or gateway; the match is on qwen's words and changes nothing for a dialect that never sends them, so no profile trait. Doubao has no `server_vad` refusal of this shape (its own protocol, `docs/design/doubao-findings.md`).

## W-Q14: an abandoned response stays open on qwen and blocks the next turn (2026-11-08)

**W-Q14 — when the watchdog abandons a response (no audio for 3 s), qwen can keep that response open: it refuses every `response.create` meanwhile ("Cannot create response while another response is in progress") and ends it only when the caller speaks again.** Seen on 2026-11-07 (flash, call `01a1b3fd-f508-7da6-86e2-73be1f3a1f55`, mobile_support on 95012): the response server VAD made for "我想查一下我的套餐。" was abandoned at 11:12:25, the dead-air cue, its retry and the drop all failed, and the response ended 19.5 s later (`status=cancelled`) when the caller said "还在吗？". On 2026-10-22 (plus) the provider produced transcript text with no audio for two responses, and the caller's five "Hello." were never answered: speech_started cancels the dead-air watch and nothing else asked for a turn. Issue #310.

**What the client does now** (`onResponseStalled`, `armTurnWatchLocked`):

- An abandoned response is cancelled (`response.cancel`, reason SYSTEM), and the floor stays held for it (`isResponseOpen` stays true) until its `response.done`, a refused cancel ("no active response"), a new `response.created`, or a 3 s cap. The consumer is told the turn is over at once (`ResponseDone` with `IsAbandoned`); the response's late audio is dropped and its done only frees the floor.
- A response nothing of which was heard owes the caller's turn, asked for once the hold ends: not for a line's or the opening's turn, and once per caller turn (reset by the caller's next speech), so a model that abandons every turn is not asked in a loop.
- After the cap, a request refused because the abandoned response still runs is settled and stays owed, instead of being retried into the same refusal and dropped.
- A caller turn with words that no `response.created` follows within 3 s of `speech_stopped` is asked for once (`the provider never answered the caller's turn; asking for it`). New speech ends the watch; a blank transcript is a silence.
- In the session, a turn abandoned before any of it was queued does not count as answering the caller, so a dead air after it takes #448's path (the model asked to answer the caller) instead of a plain NO_INPUT reprompt.

**Method.** A real stall cannot be provoked, so the round of 2026-11-08 used the fault-injecting relay (docs/load-tests.md) in front of qwen flash (owner-placed calls) and, first, in front of mlx-realtime (calls placed from fs41 with recorded caller audio). The fault hit the first VAD-answered caller turn of every call: `stall` drops the reply's audio and holds its done until the client cancels (or the caller speaks); `stall-hold` ignores the cancel, refuses `response.create` itself and releases the done after 15 s or on the caller's speech; `unanswered` hides the reply entirely. Built with the whole change through its aicall commit; the relay's barge-in fix ("mockprovider: a stalled response ends when the caller speaks over it") applied from the second stall call on.

| Call | Fault | Caller | What happened |
|---|---|---|---|
| `01a1b8d4-e28f-7190-979e-b8156459ae7c` (95012) | stall | silent | cancel, the unheard item deleted, one re-ask: first audio 4.1 s after the caller stopped, "……套餐查询不属于我们的业务范围，建议您拨打运营商客服或登录官方APP查询……", played once |
| `01a1b8d9-c31c-7201-8709-5b766ec155e7` (95001) | stall | silent | re-ask 3.9 s, answered the question, played once |
| `01a1b8df-0802-77a4-be11-7abc158e43c2` (95012) | stall-hold | silent 25 s | the 11-07 shape: cap at 3 s, the re-ask refused once and kept owed, the dead air took #448's path and its cue waited in the owed turn (not sent); on the release exactly one `response.create`: "抱歉，这里是手机售后服务热线，套餐查询需要联系运营商。……" |
| `01a1b8dd-8fc5-7e10-b2ac-f45f82ee46f9` (95012) | stall-hold | "还在吗？" after ~6 s | no request during the hold; the caller's speech released it and the owed turn was left to qwen's own reply (one reply); the replies that followed differed from each other |
| `01a1b8e1-35c5-7061-b6f2-ef915cf8ba76` (95012) | unanswered | silent | caller-turn watch fired at 3001 ms, one request, first audio 3.7 s, answered the question |

On mlx-realtime (gateway profile) the same five shapes and a control passed the same checks (`01a1b8c7-…` to `01a1b8d0-…`, 95001). Over the qwen round (18 calls, faults and controls) the new paths fired only on the faulted calls, and the old "never created the response this client asked for" WARN did not appear once (twice on 11-07).

**Answers to the issue's questions.**

- **Does the model repeat one line after a stall?** Not reproduced. Flash's *first* reply to "我想查一下我的套餐。" on mobile_support was the generic "您好，这里是星讯手机售后服务热线。请问有什么可以帮您？" in 3 of 5 first replies, before any recovery touched the conversation (the replies the relay dropped on two stall calls, and the injected #197 call). Every reply made after a stall's item was deleted and the turn asked for again answered the question, and no call repeated a line the way 11-07 did. The 11-07 repetition is more likely flash than the state the stall left, but one call is not enough to say.
- **What does the model say to #448's cue after a held stall?** qwen answered the question; mlx-realtime's model took the cue's "if you did not catch it, ask them to repeat" branch (`01a1b8cb-95b8-7f6c-8994-aa0374c98e01`).
- **Does qwen honour `response.cancel` on a stalled response?** Not observed: every stall this round was the relay's. If it does not, the hold's 3 s cap and the settled refusal bound the cost to one refused request, and the caller hears the bot when the provider lets go (on 11-07, when the caller spoke).
- **Other profiles.** The client paths are provider-neutral and passed behind the relay on gateway. Whether openai and grok keep an abandoned response open is not known; a cancel that finds nothing is already absorbed (`isAnsweredCancel`). Doubao is a separate client with no server VAD replies to suppress; it was not changed.

**Not covered.** A natural stall on qwen (none occurred); a stall with audio partway (unit tests only: cancelled, not re-asked); three abandoned caller turns in a row (unit test only).

## W-Q15: a slot tool's result sent with no turn asked for: nothing follows it, and the caller's next turn is answered (2026-11-09)

The measurement in #387 (`qwen-audio-3.1-realtime-plus`, zh) stopped once the tool result was answered. This one is on flash, the current default, in both languages, and goes on to the caller's next turn. It is the premise of `isCallerNext` (`docs/flow-authoring.md` 4.5).

**Method.** `TestLiveQuietToolResult`, one run: the real `provider.Realtime` client, `qwen-audio-3.1-realtime-flash`, the instructions and tools `flow.Runtime` renders for the `renewal_offer` fixture, 12 sequential sessions (6 en, 6 zh) with one synthetic voice (qwen3-tts-flash, Cherry). At `response.done` the instructions are re-pinned to the new phase and the result is sent with `SendToolResultQuietly` when the response spoke, and with `SendToolResult` when it did not, as `closeToolBatch` decides. Nothing is sent for 4 s, then the caller's next turn rotates through a second question, an agreement, a refusal and silence. It runs on the client, not the orchestrator: no dead-air cue, no phase engine, no line. **The sample is small: read these as whether a thing happens, not as rates.**

| Measured | Result |
|---|---|
| Sessions where the slot tool was called | 10 of 12 (the other 2 not measured, see below) |
| The response spoke, with its first audio before the tool call | 9 of 10; no call overtook the first audio |
| The response only called the tool | 1 of 10: ordinary result, and the follow-up turn gave the right answer |
| A response starting within 4 s of a quiet result | **0 of 9** |
| Caller's next turn after a quiet result: a second question | 2 answered from the fact list, each with `record_second_question` in the same response; 1 not measured |
| ... an agreement | 2 of 2 `classify`, no audio |
| ... a refusal | 2 of 2 `hangup`, no audio (not `classify` with `decline`; both end in the same closing phase) |
| ... silence (8 s) | 0 of 2 responded, so a silent caller is the dead-air path's to handle |
| Provider errors | none |

First audio from `speech_stopped`, over the 9 first answers: 460 to 704 ms.

**Three sessions were not measured, and the likely cause is the probe.** Sessions 3 and 9 (the first question, "OK, so how much data do I get?" and "Right, can I keep my phone number?") and session 10 (the second question, "哦，那每个月有多少流量？") ended with a truncated transcript and no audio ("So, would you", "Hi,", "您还有其他问题吗？"). All three are an acknowledgement followed by a question. `say` streams a clip in the background, and the probe's settle rule did not look at the caller's speech. So a reply to "OK," that the caller's next words cut could end the watch 1.5 s later, in the middle of the sentence, with the real answer still to come. The log has no event timeline, so this is the likely reason and not a proven one. The rule now waits for the caller to stop. The fixed test has **not** been run again, to save spend.

**What this says about acknowledgement forms.** Two of them were answered normally on the first question (sessions 4 and 10, zh). Whether a reply cut in the pause is followed by a proper answer, in a phase where the tool result went quietly, is left to owner-placed calls.

**Not covered.** More than one voice; a second round after the second question; a barge-in during the answer (unit tests only); openai and grok, which share this client and protocol and were not run.

### Owner-placed calls (2026-11-10)

The dev stack ran main `0135862` plus this change, on qwen flash. The fixture `renewal_offer` was bound to 95012 (zh) and 95001 (en), and a baseline (the same flow, tool first and answer in the next turn, no `isCallerNext`) to 95002 (zh) and 95011 (en). There were ten calls.

| # | Call | Script | What happened |
|---|---|---|---|
| 1 | `01a1c2d9-33f8-781a-a74a-2b5807a97a29` (95012) | question, then silence | Answered and recorded in one response, quiet. Nothing was said until the dead-air prompt ("请问您还在吗？"), 8 s after the answer played. "不用了谢谢" → `classify` decline |
| 2 | `01a1c2da-0376-7b02-be64-b9049bedbd12` (95012) | two questions, then agree | Both answered and recorded in one response each, both quiet; agree |
| 3 | `01a1c2da-ba0a-7b02-88a2-e013e8a8ef27` (95012) | "哦，" + question, then refuse | "哦。" got a reply of its own and a fragment of the question got a cut answer ("月费保持 50"). The whole question was then answered and recorded, quiet; decline |
| 4 | `01a1c2db-4423-71fb-a695-3804b4a2eebb` (95012) | question, barge-in with a second question, then agree | The answer was quiet, and the caller cut its tail (1720 ms played, generation finished). The provider's own reply answered the new question with `record_second_question`, quiet; agree |
| 5 | `01a1c2db-ee16-740f-9c80-9ce037ea0ea2` (95001) | question, then silence | Quiet; "Are you still there?" after 8 s. "No, thanks." got "I said the monthly price stays at 50 yuan, the same as now.", with no tool. A second dead-air prompt followed, then a later "No thanks." got "The price stays at 50 yuan a month." The caller's "bye" → `hangup` |
| 6 | `01a1c2dd-2131-73fa-9361-ebfeebe338de` (95001) | two questions, then agree | Both quiet; agree |
| 7 | `01a1c2e2-2087-7fea-be36-39d53e41dbb2` (95001) | "OK," + question, then refuse | "OK" got its own reply, cut at 120 ms. The response to the whole question only called `record_question`, with no audio, so the result went the ordinary way and the follow-up turn answered. The refusal, transcribed as "Lalu? 会打。", was classified as agree |
| 8 | `01a1c2e2-bf4f-7656-b99f-50e528a476b6` (95001) | question, then barge-in | Server VAD split "Can I cancel if I change my mind?". The first half got a cut reply, and the second half was classified as agree in `offer`, before any quiet result. The caller's next question was answered over the closing line, and then the line was said again |
| 9 | `01a1c2e3-6eac-79fd-b22e-e16c858eeca7` (95002, baseline) | two questions, then refuse | Tool first, the follow-up turn answered, twice; decline |
| 10 | `01a1c2e4-0e15-7fd2-b3b3-52006011f0b9` (95011, baseline) | two questions, then refuse | The same; decline |

**`turn latency` from the log** (`speech_stopped` to first audio) for the answer to a question:

- the 9 answers on the quiet path: 385 to 553 ms;
- the 4 baseline answers, each in a follow-up turn: 1356, 1499, 1635 and 1588 ms;
- call 7's fallback: 1858 ms.

These are single calls on the development box, not a benchmark.

**What the calls show:**

- **The rule held.** All 9 quiet results were followed by nothing until the caller spoke or the dead-air prompt came. The dead-air prompt was sent, not refused (calls 1 and 5). A barge-in over the answer's tail left one reply, the provider's own (call 4). A response that only called the tool kept today's path (call 7).
- **Two wrong classifications came from turns the change does not touch.** In call 7, a refusal that was transcribed as noise was taken as agreement, after an ordinary follow-up turn. In call 8, a question split by server VAD was taken as agreement in the opening phase, before any quiet result. The issue's tool-first probe arm showed the same failure.
- **Still open: call 5.** After the dead-air prompt, "No, thanks" was twice read as not having heard the answer, and the price was repeated. The dead-air cue ends "briefly repeat the current question". In a caller-next phase the bot's last turn answered a question rather than asked one, and that answer may be what it repeated. The same cue in zh (call 1) worked. This is one call, so it is not resolved here.

## W-Q16: flash answered the caller's first question and hung up in the same response (#484, 2026-11-04)

**The incident.** One owner-placed call on the dev stack, `qwen-audio-3.1-realtime-flash`, DID 95001, `novanet_support` (en), call `01a1a445-2cf5-7696-974f-8879e6d74fc8`. To "What service do you offer?" the model started the answer ("NovaNet is a telecommunications company. We provide internet, mobile, and home phone services. How can I help you today?") and called `hangup` with `{}` in the same response. The hangup moved the call into `farewell`, the answer was muted under #441 (the log has `playedMs=200`, `flushedMs=600`), and the caller heard the farewell line and the call ended. The flow then told the model twice to "call hangup immediately" when the caller wanted to finish, with `hangup` always offered. No DEBUG trace of the provider events exists for the call, so whether the words or the call came first on the wire is not known. The final transcript of the turn was logged after the call had run.

**The change** (`docs/design/02-ai-voice.md` §6, "Why the model hangs up"):

- the built-in's description, the action-claim rule and the shipped flows say *when* to hang up (the caller said goodbye, asked to end the call, or declined more help after being asked), never in the same reply as an answer and never in reply to a question;
- `hangup` takes a required `reason` (`CALLER_GOODBYE`, `CALLER_DECLINED_MORE_HELP`, `CALLER_ASKED_TO_END`, `FLOW_INSTRUCTED`); a hangup without one is refused and runs nothing;
- the answer guard refuses, once per caller turn, a `hangup` in a response whose transcript deltas before the call already hold an answer (four words or more, a Chinese character counting as half, and no goodbye). The answer keeps playing.

The guard can only judge words that arrived before the call. A `hangup` that passes with audio of its own queued and no words yet is logged (`a hangup arrived before any words of its response`), which counts how often flash puts the call first.

**Live set, 2026-11-12** (owner-placed, flash only, `novanet_support`, provider events at DEBUG). The same 20 calls before and after the change: 10 on 95001 (en) and 10 on 95002 (zh); question openers ("What service do you offer?", "你们提供什么服务？", "你们深圳能装吗？", a question after a pause, one ending in "thanks"), then a goodbye, a goodbye phrased as a question, a hang-up mid-answer, a mixed turn ("好的谢谢，那宽带多少钱？") and a wrong number at the greeting. Plus was not measured.

| | before (main) | after |
|---|---|---|
| calls ended on a caller's question | 0 of 20 | 0 of 20 |
| `hangup` calls, all after a goodbye, closing line played | 17 | 16 |
| `hangup` in a response with words of its own | 0 of 17 | 3 of 16 |
| answer-guard refusals | n/a | 0 |
| `hangup` without a valid `reason` | n/a | 0 of 16 |

- **The incident did not recur**, before or after: no response answered a question and called `hangup`. The guard therefore never fired on a live call; its refusal is covered by the deterministic tests only.
- **Item order.** In the three mixed responses (all zh, all a goodbye: "好的，再见。" twice, "好的，那我先挂了。") the message item and its whole transcript arrived before `function_call_arguments.done`. No `hangup` arrived before the words of its own response, so the guard's blind spot was not observed. Three responses are not a rate.
- **Reasons.** 15 `CALLER_GOODBYE`, 1 `CALLER_ASKED_TO_END` ("好的，就这些，快挂了吧"), each matching what the caller said.
- **A goodbye in the hangup's own response** appeared only after the change (0 of 17 before). The turn was muted and the closing line played, as for any hangup into a phase with a line. "好的，那我先挂了。" has no goodbye the claim patterns know and is seven characters, one under the guard's floor.
- **A wrong number at the greeting** ("打错了，不好意思") ended the zh call by `hangup` before the change and did not after it: the model asked again how it could help. The en call asked again both times.

  Since then the built-in's description and `novanet_support` name a caller who says they have the wrong number or called by mistake as a reason to end the call (`CALLER_ASKED_TO_END`). Four more calls on that text: "Sorry, I called by mistake." and "打错了不好意思。" ended at once; "Wrong number, sorry." ended on the second saying; "不好意思，拨错电话了。" said twice did not end, and the caller hung up. Two of four at the first saying: better, not reliable.
- The caller's transcript arrives after the `hangup` it led to (by 0.2 to 0.5 s here), so a stored transcript can show the tool call before the goodbye that caused it.

**The guard, provoked** (same day, 8 owner-placed calls). The incident's shape was forced with a probe copy of `novanet_support` whose rules order the answer and `hangup` (`FLOW_INSTRUCTED`) in one reply, bound to one en and one zh number and removed afterwards.

- **Four responses answered and called `hangup`** (3 en, 1 zh). In each the words arrived before the call (31 to 166 characters of transcript at the call), the guard refused, the answer played on and the call went on. None arrived call-first.
- **How the refusal was answered.** Two answers ended in a question and the refusal went quietly: the caller spoke next, or stayed silent and got the dead-air prompts and the closing line. Two did not and the refusal asked for a turn: one model turn asked "请问您还有其他需要吗？", the other said its whole answer again in spite of the refusal's "do not repeat your answer". In both the caller's goodbye then ended the call by `hangup` with `CALLER_GOODBYE`.
- **Three zh responses were a bare `hangup` in reply to the question**, no words and no audio, as the probe ordered. The guard judges the bot's words and had none: those calls ended on the question, with the closing line. Nothing in the engine stops a wordless `hangup` after a question; the description and the flows' rules are what stand against it, and the 40 calls on the shipped flow had none.
- In one en call the model made no tool call and repeated its answer to each goodbye; in another it repeated a refused answer to the caller's goodbyes. The probe's rules say nothing about a goodbye, so this is put down to the probe, not measured further.

## W-Q17: after a refused hangup, flash can say its whole answer again (#555, 2026-11-13)

**The problem.** In W-Q16's provoked round, two of the four refused responses ended in a statement, and the refusal went the ordinary way: it asked for a turn, in which the model was to ask whether anything else was needed. One turn did ("请问您还有其他需要吗？"). The other said its whole answer again, although the refusal said "do not repeat your answer". The turn the refusal asks for comes after the caller has been answered and has nothing new to say, the shape `isCallerNext` was made for (#387, `docs/flow-authoring.md` 4.5, W-Q15). One repeat in two is not a rate; the repeat is the defect.

**The decision** (owner, 2026-11-13; option 1 of the issue, strengthened):

- **A held refusal is always answered quietly.** On a client with `QuietToolAnswerer`, `closeToolBatch` sends the refusal without asking for a turn whatever the answer ended in. The other conditions stay: the response spoke, no answer of the batch asked for a turn, no line owed, nothing armed, and a batch that also holds slot answers only in a caller-next phase. When they fail, the refusal still asks for a turn. The `the response whose hangup was refused has ended` line still logs `endsInQuestion`.
- **Two refusal texts** (`flow.Runtime.RefuseWhileAnswering`, `ok:"0"`, no hint). A quiet refusal is read with the caller's next words, so "ask whether they need anything else" would be an instruction for a moment that has passed. It says the call was not ended because the model was answering in the same reply, that the caller has heard the answer and it is not to be said again, to answer what the caller says next, and to call `hangup` only after a goodbye or "nothing more". The refusal that asks for a turn keeps the old meaning: ask whether anything else is needed. The text is chosen, and written to the transcript, when the refusal is sent.
- **A dead-air cue of its own.** After an answer that ended in a question, the caller has been asked something, and the generic silent-caller cue ("briefly repeat the current question") is right. After one that ended in a statement there is no question to repeat, and on W-Q15's call 5 that cue had the bot repeat its answer instead (the price, twice). So the first `NO_INPUT` after a quietly refused statement gets the anything-else cue instead: "(The caller has heard your answer and has been silent since. Do not repeat any of it. Ask only, in one short sentence, whether they need anything else.)", and in zh "（来电者已经听到了你的回答，之后一直没有说话。不要重复回答里的任何内容。只用一句简短的话问对方还有没有其他需要。）". Only the cue's words change: the silence is counted, the flow's `NO_INPUT` rules and the closing-target default run, and #448's words path, the unhung-farewell backstop and a standing claim's cue come first. It is owed to one dead air and dropped by caller words, a keypress, a barge-in, any other bot turn finishing, a move, or an ending armed or decided. No provider mechanism: it is sent through `SendCue` like the generic cue.

**Deterministic checks** (fake provider, `internal/aicall/anything_else_test.go` and `answer_guard_test.go`; each asserts the exact sequence of what the model was sent):

| Scenario | Sent to the model |
|---|---|
| statement, the caller speaks next | the quiet refusal only (`SendToolResultQuietly`) |
| question, then silence | the quiet refusal, then the generic silent-caller cue |
| statement, then silence | the quiet refusal, then the anything-else cue; the next dead air gets the generic cue, and the third in a row closes the call through `closingTarget` with its line (`noInput.count` 1, 2, 3) |
| statement, caller words, later silence | the quiet refusal, then the generic cue |
| statement, then a silence a `NO_INPUT` rule moves to the goodbye | the quiet refusal, then the closing line; no cue |
| no audio in the refusing response | the refusal asks for a turn, with the "anything else" text |
| a model without quiet answers | the refusal at once, asking for a turn, with the "anything else" text |

The reply to the anything-else cue is judged for unbacked claims and is not counted towards `maxTurnsWithoutTool` (`TestTheReplyToTheAnythingElseCueIsJudgedAndNotCountedTowardsTheWall`). On the Realtime client a quiet result followed by a cue is exactly one `response.create` (`TestAQuietResultThenACueAsksForOneTurn`, openai and qwen).

### Live round: zh (2026-11-13, 10 owner-placed calls); en not run

Owner-placed calls on `qwen-audio-3.1-realtime-flash` to a zh number, with a probe copy of `novanet_support` whose last rule orders the answer (statements only) and `hangup` (`FLOW_INSTRUCTED`) in one reply. After each answer the caller waited without speaking until the bot spoke again. The en half of the set was not placed (owner's decision on the day), so the en repeat of W-Q16 has not been re-tested on a live call.

The model followed the probe in 7 of 12 answers. Those 7 hangups were all refused, all 7 refusals went quietly (`isQuiet=true`, none the ordinary way), and all 7 silences after them got the anything-else cue. No sentence of a refused answer was said twice; every reply to the cue was the question alone.

| # | Caller asked | Answer | Hangup refused | Said twice | After the caller's silence | How the call ended |
|---|---|---|---|---|---|---|
| 1 | 你们有手机套餐吗？ | two sentences, ended in a question | no hangup made | no | generic cue: "您好，请问您还在吗？您想了解哪类手机套餐呢？" | caller's goodbye, `hangup`, closing line |
| 2 | 你们客服几点上班？ | one sentence | no hangup made | no | generic cue: "请问您还在线吗？" | caller's goodbye, `hangup`, closing line |
| 3 | 你们有光纤宽带吗？ | one sentence | yes, quiet | no | anything-else cue: "请问还有其他需要帮助的吗？" | caller's goodbye, `hangup`, closing line |
| 4a | 可以在网上交费吗？ | one sentence | yes, quiet | no | anything-else cue: "请问还有其他需要帮忙的吗？" | caller asked 4b |
| 4b | 你们作息几点上班？ | one sentence | yes, quiet | no | anything-else cue: "请问还有其他需要帮忙的吗？" | caller's goodbye; the bot said "好的，再见。" with no tool call and the call closed on its own farewell (#453) |
| 5 | 你们有手机app吗。 | one sentence | yes, quiet | no | anything-else cue: "请问还有其他需要帮助的吗？" | caller silent throughout: generic cue ("请问您还在线吗？"), then the closing line on the third silence |
| 6 | 你们提供什么服务？怎么办理？ | three sentences, ended in a question | no hangup made | no | generic cue: "您好，请问您还在线吗？" | caller's goodbye, `hangup`, closing line |
| 7 | 怎么查我的套餐？ | one sentence | yes, quiet | no | anything-else cue: "请问还有其他需要帮助的吗？" | caller's goodbye, `hangup`, closing line |
| 8 | 光纤和移动上网有什么区别？ | one sentence | no hangup made | no | generic cue: "您好，请问您还在线吗？" | caller's goodbye, `hangup`, closing line |
| 9a | 怎么报故障？一般多久能修好？ | two sentences | yes, quiet | no | anything-else cue: "请问还有其他需要吗？" | caller asked 9b |
| 9b | 光纤宽带有哪几种？ | one sentence | no hangup made | no | generic cue: "您好，请问您还在吗？" | caller's goodbye, `hangup`, closing line |
| 10 | 支付有哪些付款方式？账单一般什么时候到期？ | two sentences | yes, quiet | no | anything-else cue: "请问还有其他需要帮忙的吗？" | caller silent throughout: generic cue ("请问您还在吗？"), then the closing line on the third silence |

- **The caller was never left without a prompt.** Each silence was answered at the dead-air timer: by the anything-else cue after a refused answer, by the generic cue otherwise. In the two calls where the caller never spoke again, the NO_INPUTs counted as any others and the third closed the call with the closing line.
- **The generic cue did not repeat an answer here** (calls 2, 8, 9b: a statement with no hangup made, so no refusal and no anything-else cue). W-Q15's call 5 remains the only recorded case of it; these three do not close it.
- **Not covered:** en; a caller who speaks at once after a refused answer (deterministic tests only); a refusal sent the ordinary way (none occurred).
