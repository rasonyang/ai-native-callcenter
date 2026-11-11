# 11 - Backchannels: a one-word acknowledgement must not take the floor

Issue #403. Status: implemented for openai; the other engines keep their own
interruption and get damage limits.

## The problem

A caller who says "嗯" or "mm-hmm" while the bot talks is listening, not
answering. The engines' own turn detection treats it as speech: it cuts the
response and answers it. Seen on a live outbound call (2026-11-03): the first
"嗯" took the floor 2.86 s into the opening offer, the model read it as
agreement and called `customer_agrees`, which moved the call to the terminal
phase and recorded a lead; each later "嗯" cut the closing line and the model
said it all again.

Two harms, so two kinds of fix:

1. The bot is cut. Fixable only where the client can stop the engine cutting.
2. The cut turn is answered as if it were an answer: the flow moves, the
   closing line is said again. Fixable on every engine, after the fact.

## What each engine does (measured)

Probes with synthetic clips (about 250 ms of "嗯", about 640 ms of "等一下"),
spoken over a long story, one or two samples per cell. The numbers are design
inputs, not results; they do not leave this document.

| Engine | `create_response:false, interrupt_response:false` | Without them | `ControlsInterruption` |
|---|---|---|---|
| openai (gpt-realtime-2.1) | honoured, also settable mid-call. Response keeps playing; `speech_started`, `speech_stopped`, `committed`, a user item and a transcript still arrive; no automatic response, also in the idle tail. `conversation.item.delete` is acked, `response.cancel` works (`client_cancelled`) and `response.create` then answers. Speech start to first transcript partial 898-1135 ms, to final 1245-1782 ms | cuts about 45 ms after `speech_started` | true |
| qwen (flash) | accepted and ignored; cut at `speech_started`, then answered. `semantic_vad` rejected (supported: `server_vad`, `smart_turn`, `smart_turn_v2`). `turn_detection:null` stops the cut but leaves no VAD at all | cuts | false |
| grok | accepted and ignored; `turn_detection:null` still runs VAD, cuts and answers | cuts | false |
| doubao | no control; the server cuts on recognised speech | cuts | false |
| gateway (mlx-realtime) | honours the switches, but its own `barge_in` hold hid short speech during playback, so no speech event reached the client while the bot talked | cuts or hides | false |

The trait is on for openai only (owner decision). The session payload of every
other engine is byte for byte what it was; the wire goldens of the other
engines did not change.

## Owner decisions

- openai gets the complete hold-then-decide. qwen, grok, doubao and gateway
  keep the engine's own interruption and get the damage limits below.
- Hold cap: 2000 ms.
- The acknowledgement list is built into the product, fixed per language (zh,
  en) and shared by all flows. It is not authored in a flow.

## The list and the classifier (`internal/aicall/backchannel.go`)

Only the call language's list counts (`flow.Lang` of the call language, so
`zh-CN` is zh); another language's word is words.

- zh: 嗯, 嗯嗯, 嗯哼, 对, 好, 好的, 是, 是的, 哦, 噢, 行, 可以.
- en: mm-hmm, uh-huh, yeah, yep, okay, ok, right, sure, got it, I see (and the
  spellings mhm, mm hmm, uh huh).

Case and punctuation fold. One phrase repeated up to three times is still one
(对对对, 嗯嗯嗯, 好的好的, "yeah, yeah"). Three verdicts:

- `content`: not an acknowledgement and cannot become one. A question mark
  anywhere (嗯？), any clause that is not the same phrase (好的好的，那多少钱;
  对，但是…; okay but; yeah, no), a fourth repeat, another language's word.
- `ackSoFar`: a partial that is a proper prefix of an acknowledgement (uhh, mmh,
  got, 好的好), or nothing yet.
- `ack`: a whole acknowledgement. On a partial the caller may still add words,
  so the session keeps holding; on a final it decides.

An empty final is `content` (a cough takes the floor, as today). An empty
partial is no information. The function is pure and off the frame path.

## The trait

`Profile.ControlsInterruption`: the provider lets the client switch off both
server reactions to speech, so the client decides after the transcript. With it:

- `buildTurnDetection` adds `create_response:false, interrupt_response:false`.
- `CancelsResponseItself` is false for openai: nothing cancels on speech but
  this client, so `Interrupt(SPEECH)` sends `response.cancel` for an open
  response. (Readers of the trait: `Realtime.Interrupt` only; tests that used
  openai as the self-cancelling engine now use grok.)
- The transcriber is told the call's language
  (`audio.input.transcription.language`). On a live round without it, a Chinese
  "嗯" came back as "うん" or "Mmm.", "对" as "Right." or "Sí.", so the list could
  not match the most common acknowledgements. A caller speaking another language
  than the call's is transcribed less well as a result.
- The client implements `provider.CallerTurnController`, an optional interface
  in the pattern of `QuietToolAnswerer` (`VoiceSession` is not widened):
  `AnswerCallerTurn(itemID)` asks for the caller's turn and
  `DeclineCallerTurn(itemID)` deletes the item. `Event.ItemID` carries the
  utterance's id on `speech_started`, `speech_stopped` and every transcript
  event, where the wire has one.
- The answer is one `response.create`, released only once
  `input_audio_buffer.committed` has been seen for that item, riding the
  owed-turn flag that a tool result or a cue already uses, so it is one request
  however much else is owed and it waits for a response that still holds the
  floor.
- The caller-turn watch (the 3 s re-ask after speech with no `response.created`,
  #310) is not armed: the provider never answers by itself now, and the watch
  would ask for a turn the call declined. Nothing else re-asks a caller turn
  (`onVADTurnRefused` is qwen's message). A turn owed while the caller was still
  speaking is released at `speech_stopped`, because the provider's reply that
  the old rule waited for never comes.

## Hold semantics (`internal/aicall/backchannel_hold.go`)

Applies when the session's model implements the controller and the profile has
the trait. When speech starts and the bot is audibly playing (a turn being
generated, or its tail still queued, also inside the 800 ms guard: with the
switches off nothing else would answer such a turn), the utterance is held:

- No flush, no `Interrupt`, no `BARGE_IN`, no `CALLER_SPEECH`, no bump of the
  dead-air generation, no `timer.onSpeechStarted`.
- Utterances overlap (a backchannel every second; a final lands about 1.3 s
  after its start), so the session keeps a list of utterances, each with its own
  cap, matched to transcripts by item id.
- Partials accumulate per item. `content` takes the floor at once; `ackSoFar`
  and `ack` keep holding.
- Final: `ack` is a backchannel: nothing is flushed, the item is declined, and
  the transcript is published as `CUSTOMER_SAID` with `IsBackchannel`. Anything
  else takes the floor.
- Cap: 2000 ms from that utterance's start takes the floor, through an injected
  timer so tests need no real sleep.
- A keypress takes the floor at once (the ordinary DTMF path).
- Taking the floor is today's path at that moment: flush, played time = frames
  queued minus frames flushed (the last frame heard), `Interrupt`, `BARGE_IN`,
  `CALLER_SPEECH`, the deferred turn-timer calls. The guard is not applied again:
  the words are known. The caller's turn is answered at speech stop, or at once
  if speech already stopped.
- Speech when nothing is audible is not held: it is a normal turn, answered at
  speech stop without waiting for the transcript, so ordinary latency is
  unchanged. A "嗯" answering a question that finished playing is a real answer.
  A response that is being made and has not been heard is cancelled at the
  speech start (what the engine's own detection did), with no flush or
  `BARGE_IN`.
- If playback ends during a hold the verdict still applies: `ack` is declined
  and unanswered, `content` is answered.
- A flagged transcript is recorded as the caller's words and does nothing else:
  not `runtime.OnCallerSpoke`, not the farewell watch, not `timer.onCallerWords`
  (#448's cue), not the dead-air accounting. With no `BARGE_IN`, the armed
  ending, the claim watch and the farewell watch see nothing.

## Damage limits where the engine cuts by itself (trait off)

After a cut over audible playback, whether the session's barge-in or the
provider's own `Interrupted`, the final transcript of that utterance is still
classified (matched by item id when the wire has one, else the next final):

a. Flow. If the verdict is `ack`, a tool call in the response that answers the
   cut and that could move the phase (`Engine.CanMoveOnTool`: a transition on
   that tool's result leads elsewhere, judged on the rules alone, so it is an
   upper bound) is refused as a conversation: `ok:0`, a short reason that tells
   the model the caller only acknowledged, and the phase's instruction; nothing
   runs and no state changes. Read-only tools and tools with no move are
   untouched. A call that arrives before the verdict waits for it, at most
   1000 ms, and with no verdict runs as it always did. The window is the first
   response after the cut. A "嗯" said when nothing was playing is never
   affected, because no cut is recorded for it.
b. Closing line. A cut that landed on an armed closing line that had finished
   generating was counted as the first interruption before the verdict existed.
   When the verdict arrives as `ack` the cut becomes the second-interruption
   case of #465 and the line counts as said by rule. Once the line counts as
   said, by that or by an earlier interruption, an `ack` runs the armed action
   at once: nothing is left to answer, and the model's reply to the
   acknowledgement must not reach the caller (on a live qwen call it read out a
   made-up "[系统提示] 本轮通话已结束"). A first cut of a line still being
   generated leaves the line owed (#493): it is asked for again, once.

## Known limits

- An ASR swap such as 喂 heard as 嗯 is swallowed on openai: the item is
  declined and the caller is not answered. The dead-air watch is untouched, so
  a caller who really said it and waits is prompted after the usual silence.
- On the trait-off engines the bot is still cut and still answers. The limits
  keep the flow and the closing line from being moved by a backchannel; they do
  not restore the bot's sentence.
- On those engines the model's own reply to an acknowledgement still plays,
  except once a closing line counts as said. A first cut of a line still being
  generated can therefore still be followed by one model reply and one more
  telling of the line.
- The hold does not apply the 800 ms guard, so an echo of the bot's own words
  inside it takes the floor once transcribed as words. That matches what the
  engine's own detection did before the switches were turned off.
- qwen could only be fully fixed with `turn_detection:null` and a client-side
  VAD. That is a different design and is not built here.
- The check for a moving tool is on rules, not conditions: a tool whose rule
  only fires on a failure result is refused after an acknowledgement as well.
- A declined item stays in the conversation between its commit and the final
  transcript (about a second); a response created in that window can read it.
- The measurements have 1-2 samples per cell and the clips are synthetic.
