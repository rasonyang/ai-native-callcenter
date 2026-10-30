# Grok voice realtime — live findings (2026-10-15)

Measurement, and the record behind a profile that now exists. When this file
was written no Grok `Profile` had been added; one was, the same day
(`AICC_PROVIDER=grok`, `GrokProfile()` in `internal/provider/profile.go`, commit
`44cce39`), and the trait values in it are what the arms below measured. The
test is `internal/provider/live_grok_test.go` (gated by
`AICC_LIVE_PROVIDER_TEST` and `XAI_API_KEY`):

    AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveGrok -v ./internal/provider/

It mirrors the doubao W-D9 measurement (`doubao-findings.md`): same persona, same
appended marker paragraph, same qwen-TTS clips, here converted to G.711 mu-law
8 kHz (`testdata/q_*.ulaw`) and sent as `audio/pcmu`, which is how production
would feed Grok. Model `grok-voice-3.0`, endpoint
`wss://api.x.ai/v1/realtime?model=grok-voice-3.0`, `Authorization: Bearer`.
Labels: DOC = vendor documentation, unverified; MEASURED = observed on the wire
on 2026-10-15. Nothing here is a performance claim.

## 1. Connection and session

| What was tried | Result |
|---|---|
| Handshake | MEASURED: accepted with the bearer key. The upgrade response carries no request id (only `Cf-Ray`). The `session.id` in `session.created` is the id to quote (below). |
| First events | MEASURED: `session.created` (`voice: xai_ara`, `modalities: ["audio"]`, `tools: []`, `turn_detection: {type: null}`, `instructions: ""`), `conversation.created`, then a `ping` event with a `timestamp` every 10 s (the repo client would log it as an unmapped event). |
| Session setup | MEASURED: `session.update` with `instructions`, `voice: "eve"`, `turn_detection: {type: "server_vad"}`, nested `audio.input.format` / `audio.output.format` = `{type: "audio/pcmu", rate: 8000}` is acknowledged by `session.updated`. The echo drops `rate` for pcmu (`{"type":"audio/pcmu"}`), and still shows flat `input_audio_format: "not specified"` / `output_audio_format: "not specified"`; the nested fields are what matter. |
| Event vocabulary of a reply | MEASURED: `response.created` (`status_details: "unimplemented"`, `usage: {}`), `response.output_item.added`, `conversation.item.added`, `response.content_part.added`, `response.output_audio.delta` / `response.output_audio_transcript.delta` (many), `response.output_audio_transcript.done` (whole transcript), `response.content_part.done`, `response.output_audio.done`, `response.output_item.done`, `response.done` (`usage` with `billable_audio_seconds`, `output_audio_seconds`, token details). Caller side: `input_audio_buffer.speech_started` / `speech_stopped` (with `audio_start_ms` / `audio_end_ms`), `input_audio_buffer.committed`, `conversation.item.added`, `conversation.item.input_audio_transcription.completed`. Ids are UUIDs; `previous_item_id` is on every event, mostly `null`. |

## 2. Why caller audio first appeared to be ignored (root cause)

The first pass of the test saw no `speech_started`, no commit and no error in
40 s of correctly formatted pcmu, and even a manual `input_audio_buffer.commit`
plus `response.create` produced nothing. Debug sessions (five):

| Variant | Result |
|---|---|
| pcmu, 20 ms pump started at dial time (before `session.created`/`session.update`), mu-law silence `0xFF` between clips | MEASURED: nothing, 0 events; commit and `response.create` also silent |
| same, `rate: 8000` in the format | nothing (the echo drops `rate`) |
| same, plus flat `input_audio_format: "g711_ulaw"` | nothing (echoed back, but did not help) |
| `audio/pcm` rate 24000 (clip upsampled), no pump, sent after the ack | MEASURED: works. `speech_started` at 1394 ms, `speech_stopped` at 3163 ms, committed, transcript `你们的客服几点下班？`, reply |
| pcmu, **no audio before the `session.updated` ack**, clip + silence sent after it | MEASURED: works. `speech_started` 1614 ms, `speech_stopped` 3142 ms, same transcript, reply |
| pcmu, 20 ms pump (silence + clip) **started after the ack** | MEASURED: works, every time (all later sessions) |

Root cause: audio streamed **before the session was configured** (before the
`session.update` carrying `audio.input.format` was acknowledged) leaves the
session deaf; the same bytes sent after the ack are recognised at once.
**G.711 mu-law 8 kHz passthrough works** as input (`audio/pcmu`), so Grok does
not need resampling to PCM for the caller leg. The mechanism of the deafness
(a default format guessed for the early frames, or the VAD pinned to it) was
not isolated; the rule is only "send no audio until `session.updated`". It also
means a production client must not forward RTP into the session from the moment
the socket opens. Unlike the repo's OpenAI path, where `input_audio_buffer.append`
before the update is harmless as far as is known, this is a new ordering rule.

## 3. Mid-call instruction change (the question)

A `session.update` carrying only `instructions` (and `type: "realtime"`, as the
repo's Realtime client sends it): standing persona
`你是 NovaNet 的电话客服。请用中文，简短自然地回答，每次不超过两句话。`; update =
persona plus `从现在开始，你的每一句回答都必须以“好的，王先生”开头，然后再回答问题。`
Caller audio: pcmu 8 kHz clips at real-time pace. Three sessions per arm, 15 in
all. Replies are read from `response.output_audio_transcript.done`.

| What was tried | Obeyed |
|---|---|
| **Control**: no update, three questions | marker in **0 of 9** replies |
| Update while idle (after the first answer), then two more questions | **6 of 6**: the first reply after the update and the next one |
| Update sent while the first answer was still streaming, then two more questions | **6 of 6**; the answer in progress was not interrupted or cut |
| Tool `lookup_order` declared at start; update (instructions only) adds the marker rule and "call `lookup_order` on an order question"; order question after it | tool called **3 of 3**; reply after the tool result carries the marker **3 of 3** (the line before the call carried it too) |
| **Tool control**: tool declared, no update, same order question | tool called **3 of 3**, args `{"order_id":"12345"}`, no marker |

Quotes. Before the update, plain: `我们客服一般是晚上九点下班。 还有其他需要帮忙的吗？`
After it: `好的，王先生，宽带办理可以在官网或拨打本机热线提交资料，工作人员会帮您核对地址和套餐。`,
`好的，王先生，不同套餐资费不同，大概从每月五十八元起。` In the tools arm the line
before the call was `好的，王先生，我马上帮您查。` and after the tool result
`好的，王先生，订单12345目前已发货，预计明天送达。` (tool control: `好的，我马上帮您查。` /
`您的订单12345目前已发货， 预计明天送达。`).

- **An instructions-only update keeps the declared tools.** The tool was declared
  once at session start and never re-sent, yet it was called after the update in
  3 of 3 sessions. (The tool control shows the order question calls it with no
  instruction at all, so this proves survival, not that the phase paragraph
  caused the call.) This differs from doubao, where tools are a full overwrite.
- The update takes effect on the very next reply and persists over the following
  one, including when sent mid-reply. Compared with doubao W-D9 the result is the
  same, 6/6 and 6/6, tools 3/3.
- The `session.updated` echo of an instructions-only update contains only the
  changed instructions (plus the always-present fields); `keep_context` flips
  from `false` to `true` after the first update. `turn_detection`, `audio` and
  `tools` keys are absent from that echo but, by the behaviour above, were kept
  (VAD kept firing, audio stayed pcmu, tools survived).

Session ids (`session.created`): control `95e28937f74c4eb3ad815355a6b73fab`
`9b63548d942542b9a0f09fe65a34d5db` `f4c539f3979d4980a1604a2f1f620fe2`; idle
`1d1465f4a1984214b3e81f852cc144fb` `154bda1e93aa4c62b60a143efaa17f0c`
`59caffbe221f4337b52ba67c59905144`; speaking `0416af1748e94da3b25a9b31e39b3c17`
`fbdd6e94907f4b63a599858bcfe95811` `8f2a71c6c484479cb0330a8666f30058`;
tools-control `36cee499de704b8ca7632fc2230a75cb` `6941ce18fb2c4edbaa7a821f573f7c2f`
`c1e1b67536d14a5a974553e52092d4b2`; tools `672e63e6161f4f30b71fc72697317ed4`
`ae5f3c193e434536a056bda6576776cf` `2456b68dd59944e39177c7f246a35b8a`.

## 4. Protocol probes

One session each (ids: p1 `4ca4ab2a5dc74d5db36157f8c1529f0c`, p2
`fec6c52a5a9f465aa0961e7d412e08c8`, p3 `3233f3f12969434b99aa7b6c797cca36`, p4
`b2b534dc75fe4cec913eb8ad434c3f72`, p5 `531902e3cff24904941e6a9cbbbf00d1`, p6
`9ad6ede7000c4ffea07c245dbc481f84`, p7 `93d2fd0774924c8d9a3a15a28245b1ce`, p8
`2469b7f6354144ab91090230c6e763a4`).

| # | What was tried | Result |
|---|---|---|
| 1 | `session.update` with `session.type:"realtime"` | MEASURED: accepted, `session.updated` echoes the instructions. The same update without `type` is also accepted. The repo's update shape works unchanged. |
| 2 | `turn_detection {type: server_vad, threshold: 0.6, silence_duration_ms: 500, prefix_padding_ms: 200}` | MEASURED: no error. The echo contains `silence_duration_ms: 500` and `prefix_padding_ms: 200` but **not** `threshold`. Whether 500 ms is honoured (turn timing) was not measured. |
| 3 | `response.create` on an empty conversation, instructions asking for a greeting | MEASURED: it greets from the instructions (`您好，这里是 NovaNet 电话客服，很高兴为您服务。`), `response.done` completed. No error, no synthetic cue needed (unlike qwen). |
| 4 | Barge-in: long answer streaming, caller clip streamed 1.5 s after the first audio delta | MEASURED: the server did **not** emit `response.cancel`-style status; the first response ended with `response.done` status `completed`, `output: []`, no `output_audio_transcript.done` for it (`usage.output_audio_seconds` 26.64: it had generated 26 s of audio faster than real time), then `speech_started` / `speech_stopped` arrived for the barge-in clip and a new reply was generated. No `cancelled` status appeared and no truncation event was sent. Ordering between the server ending the response and the barge-in speech detection is as logged: `response.done` came before `speech_started`, so it cannot be told from this run whether the server ended it because of the barge-in or because generation was simply finished. Output audio is generated much faster than real time, so a client must flush its own queue and truncate, as with OpenAI. |
| 4b | `response.cancel` when nothing is active | MEASURED: in a session that had never produced a response, `error` with `code: invalid_request_error`, message `Cancellation failed: no active response found` (non-fatal). After a completed response (p4 and p5, later runs): no error and no event at all. |
| 5 | `conversation.item.truncate` on the completed assistant item (`content_index: 0`, `audio_end_ms: 500`) | MEASURED: acknowledged by `conversation.item.truncated` (`item_id`, `content_index`, `audio_end_ms: 500`, `transcript: " "`). With an unknown item id: no error and no event. |
| 6 | `temperature: 0.6` in `session.update` | MEASURED: accepted, echoed back as `temperature: 0.6` (default shown as `-1`). Whether it changes output was not measured. |
| 7 | `audio.input.transcription.model = "grok-transcribe"` | MEASURED: echoed as `input_audio_transcription: {model: "grok-transcribe"}`. Events: `conversation.item.input_audio_transcription.updated` (cumulative, `你们的` then `你们的客服几点下班？`) and `conversation.item.input_audio_transcription.completed` repeated with `status: "in_progress"` before a final one with `status: "completed"` (the in-progress ones carry text too). Note: **without** configuring a transcription model (`input_audio_transcription: null`) a single `conversation.item.input_audio_transcription.completed` (`status: "completed"`, full text) still arrived in every session. |
| 8 | G.711 pcmu 8 kHz in and out | MEASURED: works both ways (section 2). Reply to the hours question: 38243 bytes = 4780 ms at 8 bytes/ms for a 27-character answer, consistent with the reported `output_audio_seconds`. |

## 5. Trait measurements (issue #226, 2026-10-15)

Four arms in `live_grok_test.go` (`TestLiveGrokBargeIn`, `TestLiveGrokResponseOverride`,
`TestLiveGrokTerminalLine`, `TestLiveGrokTemperature`, `TestLiveGrokTranscription`),
3 sessions per arm unless stated, 44 sessions in all. Same wire as sections 1 to 4
(pcmu in and out, `server_vad`, voice `eve`). The probe frames are the repo client's
own: `SayExactly(line, "zh")` from `realtime.go`, the `speakRequest` frame, a
`function_call_output` built with `MergeHint`, `userTextItem`, and
`response.cancel`. Labels as above: MEASURED on 2026-10-15.

### 5.1 CancelsResponseItself

Setup: instructions ask for a very long answer (at least twenty sentences); the
first caller clip starts the answer; as soon as the first audio delta arrives
the second clip (`q_hours`) is queued, so speech reaches the server while the
response is still generating. Event order and offsets are from dial time.

| Arm | Result |
|---|---|
| a. no `response.cancel` sent (3 sessions) | MEASURED 3/3: the server ends the response itself, at the same millisecond and immediately before `input_audio_buffer.speech_started`. Audio deltas stop (0 after `speech_started`), `response.done` arrives with `status: "completed"` (not `cancelled`), `output: []`, and there is **no** `response.output_audio_transcript.done`, `response.output_audio.done` or `response.output_item.done` for it. `usage.output_audio_seconds` was 5.84, 6.48 and 11.92 while about 1.2 s of audio had been streamed to the client, so generation runs at several times real time and the figure is not what the caller heard. Example (a#1): first audio 3258 ms, last audio 4436 ms, `response.done` and `speech_started` at 4549 ms, `speech_stopped` 5574 ms, new `response.created` 5699 ms. |
| b. `response.cancel` sent on `speech_started` (3 sessions) | MEASURED: in 2 of 3 the response had been cut by the server in the same way (b#1, b#3); the cancel, sent 11 to 20 ms later, was answered 229 and 280 ms after `speech_started` by `error` `code: invalid_request_error`, `Cancellation failed: no active response found` (non-fatal, session continues and answers the barge-in). In b#2 the answer simply finished 570 ms before the speech (`transcript.done` and `response.done` completed), the same error followed. So a late cancel is always refused with that message and never harmful. |
| c. `response.cancel` mid-response, no caller speech (3 sessions) | MEASURED 3/3: accepted, no error. `response.done` with `status: "cancelled"` 40 to 90 ms after the cancel (4231, 4394, 4475 ms), audio deltas stop, no item completed. |

The earlier probe (4) was ambiguous because `speech_started` and the end of the
response look simultaneous; here the response is cut with audio still being
generated, and the cut is attributable to the speech because (c) shows the same
generation is still running at that moment. The cut response carries status
`completed`, not `cancelled`, so a client must not infer a cancel from the status.

Recommendation: **`CancelsResponseItself = true`.** The client then sends no cancel on speech (it
would draw the `no active response found` error every time) and still flushes its own queue and
sends `conversation.item.truncate`, which section 4 row 5 showed is acknowledged. If the error
code `invalid_request_error` is not in the client's expected-refusal list
(`isAnsweredCancel` looks for `invalid_value` and `response_cancel_not_active`), a profile-level
cancel would log a WARN, which is a second reason not to send it. A cancel for non-speech reasons
(a deployment's own interrupt) is accepted and ends in `cancelled`.

### 5.2 RequiresTerminalAnnounce

The line was `感谢您致电 NovaNet，祝您生活愉快，再见。`. Scoring from the output transcript:
VERBATIM (equal after stripping punctuation), CONTAINS, NEAR (Levenshtein ratio at least 0.9), MISS.

| Variant | Result |
|---|---|
| (a) after two caller turns, `response.create` with `response.instructions = SayExactly(line)` and nothing else (the client's `speakRequest`) | MEASURED 0/3, and not a wrong sentence: **no response at all**. No `response.created`, no error, only the 10 s `ping`s, for the whole 30 s wait. Reproduced in 2 more sessions with one caller turn (also with `modalities: ["audio"]` added): 0/2. The same frame on an **empty** conversation works: 1/1 VERBATIM (this is the opening line path). So a per-response override alone is silently dropped once the conversation has a turn in it. |
| (a2) as (a), but the same direction first put in the conversation as a caller message (`userTextItem`), then the override (the `NeedsDirectedLineInConversation` closing path) | MEASURED 3/3 VERBATIM. Also 1/1 VERBATIM with the caller message and a bare `response.create`. |
| (b) tool result carrying the line as its hint (`MergeHint(output, SayExactly(line))`, then a bare `response.create`), after `lookup_order` was called (3/3 sessions called it, `{"order_id":"12345"}`) | MEASURED 3/3 VERBATIM, sim 1.00. |

Recommendation: **`RequiresTerminalAnnounce = false`.** The engine does take text cues, and the
tool-result hint (b) is exact, 3/3. For a line asked for outside a tool result (dead air, session
limit, a phase announce after the first turn) the per-response override alone must not be used: it
produced nothing, 0/5. Set **`NeedsDirectedLineInConversation = true`** (or put the direction in
the conversation for every mid-call line, not only closing ones, since a silent drop is worse than
the history cost): the user-item-plus-override form is 3/3. The opening line is the one place the
override alone works (empty conversation). Caveat: this is a strong dependency on the exact
frame; re-run `TestLiveGrokResponseOverride` when the model version changes. The silent drop
also means the client's turn bookkeeping (`isResponseRequested`) would wait forever on a
`response.create` that never produces `response.created`. Since #231 that wait is bounded
(`responseCreatedTimeout` in `internal/provider/realtime.go`): the floor is given back and
the request retried once or superseded (docs/provider-extension.md, Grok).
`TestLiveGrokRecoversADroppedLine` (2026-10-19, trait turned off on a copy of the profile, one
caller audio turn, then a closing line override-only): grok dropped the request 5/5; the
retry with the direction put in the conversation started 220–355 ms after the bound and was
VERBATIM 5/5, one turn each.

A second drop shape, MEASURED 2026-10-19 (`TestLiveResponseCreatedLatency`, grok-voice-3.0, one
session): after the greeting, eight **bare** `response.create`s, each sent 500 ms after the
previous response ended, with room tone and nothing new in the conversation between them. 4/8 got
no `response.created` within 30 s, and they alternated: requests 1, 3, 5 and 7 were dropped, each
followed by one that was answered. Not yet known whether grok drops a bare request that has a new
item ahead of it (a tool result, a cue), which is the shape a real call sends; variant (b) above
(tool result plus bare `response.create`) was 3/3. Watch `aicc_provider_response_unanswered_total`
with `provider=grok` on live calls before relying on the bare request.

### 5.3 AcceptsTemperature

| What | Result |
|---|---|
| Range accepted in `session.update` (one session, values in turn) | MEASURED: 0, 0.2, 1.5, 2, 2.5, 5, -0.5 and 100 are all acknowledged by `session.updated` and echoed unchanged; nothing is clamped or rejected. A string (`"hot"`) draws a non-fatal `error` `Invalid event received`, and the session keeps working afterwards (a following valid update is acknowledged). No out-of-range value killed the session. |
| Effect on output, prompt "random fruit and a number 1 to 100", 3 fresh sessions per value | MEASURED: temperature 0: `苹果 37`, `苹果 42`, `苹果 42` (2 distinct of 3); temperature 2: `苹果 42` x3 (1 distinct). Mode-collapsed prompt, no usable signal. |
| Effect on output, prompt "one-sentence cat story opening", 3 sessions per value | MEASURED: 0: 3 distinct of 3; 2: 3 distinct of 3; 100: 3 distinct of 3. All different sentences at every value, so no visible difference in variety between 0 and 100 at N=3. |

So the field is accepted everywhere but no effect was observed: neither determinism at 0 nor more
variety at 2 or 100. Absence of an effect at N=3 is not proof that it is ignored; the echo proves
only that it is stored.

Recommendation: **`AcceptsTemperature = false`** (leave it off). The value is accepted but nothing
observable follows from it, there is no documented range to validate against, and a value a deployment
sets would be one nothing demonstrably reads. Turn it on only after a measurement that shows an effect
(many samples of one prompt at 0 and at the high end, compared by edit distance).

### 5.4 Transcription: default versus `grok-transcribe`

Three sessions per mode, each with three clips in turn: `q_hours` (zh), `q_order` (zh, with digits)
and `q_en_hours` (en, `say -v Samantha` converted to mu-law 8 kHz). Final transcript compared with the
known text (Chinese numerals normalised to digits).

| | default (nothing configured, echo `input_audio_transcription: null`) | `audio.input.transcription.model = "grok-transcribe"` |
|---|---|---|
| Caller words delivered | MEASURED 9/9 clips exact (`你们的客服几点下班？`, `我想查一下我的订单状态，订单号是12345。`, `What are your opening hours?`) | MEASURED 9/9 clips, identical final text |
| Events per utterance | exactly one `conversation.item.input_audio_transcription.completed` (`status: "completed"`, `transcript`, `item_id`); `conversation.item.added` of the user item also carries the transcript in its `content[].transcript` | `...transcription.updated` (cumulative `transcript`, no status; long ones end in `...`), `...completed` with `status: "in_progress"` (partial text, repeated), then one `...completed` with `status: "completed"` |
| Timing | arrives 175 to 290 ms after `speech_stopped`, 50 to 180 ms after `response.created` (9/9) | partials start while the caller is still speaking (for the 10 s clip, first one 3.7 s before `speech_stopped`); final 160 to 270 ms after `speech_stopped` |
| Sample (trimmed) | `{"item_id":"8d55…","status":"completed","transcript":"你们的客服几点下班？","type":"conversation.item.input_audio_transcription.completed"}` | `{"item_id":"514c…","transcript":"你们的客服几点","type":"…transcription.updated"}`, `{"status":"in_progress","transcript":"你们的客服几点下班？","type":"…completed"}`, `{"status":"completed","transcript":"你们的客服几点下班？","type":"…completed"}` |

Chinese was exercised with the existing qwen-TTS clips (two utterances) and English with one
synthesized clip; both modes were exact on all of them. Accuracy on real telephone speech, accents
and noisy lines is not measured.

Recommendation: **leave `TranscribeModel` empty** for the transcript and CDR. The default delivers
the caller's words with one event per utterance and the same final text, with no configuration to
carry. If a client takes `TranscribeModel`, it must key on `status == "completed"` for
`...transcription.completed` and ignore `in_progress` (the same event type repeats), and drop
`.updated`. `grok-transcribe` is worth setting only for a live caption or for early intent
detection, since its partials arrive during speech; that is not a call-center-recorder need. Note this
differs from OpenAI's profile, where transcription must be requested.

### 5.5 Surprises

- A per-response `instructions` override after the first turn is silently ignored (5.2). The opening
  line works only because the conversation is empty.
- The server ends its own response on speech, but marks it `completed` with an empty `output`,
  not `cancelled` (5.1); the audio generated (up to 11.9 s) is far more than was streamed.
- A `response.cancel` that loses the race is refused with `invalid_request_error` rather than the
  codes the client already recognises as an answered cancel.
- `temperature` accepts anything numeric and visibly does nothing.

## 6. NOT MEASURED

- Whether `silence_duration_ms: 500` changes when a turn ends (only that it is
  accepted and echoed); `threshold` is not echoed.
- A second update that contradicts the first, many updates in one call, and a
  slot value the model must recall verbatim; obedience of a style directive is
  the easiest kind to check.
- Whether audio sent before the ack is the whole story (versus a particular
  moment); the deafness mechanism itself.
- Whether `temperature` has any effect at all (5.3 saw none at N=3), and whether
  a longer wait before a mid-call override would change the silent drop (5.2 waited 1 s and 0.3 s).
- Transcription accuracy on real telephone speech; `grok-transcribe` language options.
- Behaviour on a long call (connection cap, idle `ping` handling, reconnect).
- Voices other than `eve`; the REST `session` shape beyond what is above.
- Latency and cost: not measured and not claimed.
