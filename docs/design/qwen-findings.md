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

*Update 2026-09-23.* The tool-result half was since fixed by carrying the line
in the result (A5c, `PutsTerminalAnnounceInToolResult`). The half with no tool
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
unchanged; openai and gateway stay on the override alone; doubao and gemini are
other clients. **Confirmed on two live calls**, 2026-09-23, both
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
gemini and doubao: each package's own `Session`, opened by `Start` with the
welcome line, fed room noise and a synthesised "拜拜。"/"Bye." as caller audio,
since doubao takes no text turn; the tool answered through `SendToolResult` and
then `UpdateInstructions`, and today's sequence adds `SpeakText(line)` right
after, as `afterMove` does. Doubao's today is `speech_text_buffer.commit` and
exact by construction, so it has no baseline row. Thirty-nine sessions: openai 13,
gemini 11, doubao 15.

| Provider | Treatment | zh | en | What was said instead |
|---|---|---|---|---|
| openai | today | 3 / 3, audio 1 / 1 | 1 / 1 | — |
| openai | A5c | 5 / 5, audio 1 / 1 | 2 / 2 | — |
| gemini | today | 3 / 3 | 1 / 1 | — |
| gemini | A5c | 5 / 5 | 2 / 2 | — |
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

**gemini.** Today's `SpeakText` reaches the server microseconds after the tool
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
and gemini, like qwen, the line is a direction to a model whichever path carries
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
`PutsTerminalAnnounceInToolResult`: true on the Realtime profiles only — openai,
qwen and gateway. Doubao and gemini keep today's path exactly (the phase's
instruction as the hint, the answer, the instructions, then `SpeakText`), for
different reasons. Doubao's `SpeakText` is exact by construction, which a
direction to a model is not. Gemini is excluded pending gemini-findings W-G6: its
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
`TestAToolThatEndsTheCallOnDoubaoOrGeminiSpeaksTheLineAsBefore`,
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
turn as owed (`isToolTurnOwed`) instead of sending `response.create`; the
decision is taken after the item is sent, so a released request can never
precede the output it answers. `handleResponseDone` (and the watchdog's
`onResponseStalled`) calls `releaseToolTurn` after `dispatchPendingSpeak`, which
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
