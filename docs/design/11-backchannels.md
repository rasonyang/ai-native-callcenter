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
  than the call's is transcribed less well as a result. Qwen names no model, so
  it sends the language alone (`Profile.AcceptsTranscriptionLanguage`, #602,
  qwen-findings W-Q19); it is a bias, so a transcript in another script can
  still reach a classifier that matches only the call's language.
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
   A greeting that cut the opening line (see "Greetings over the opening") is
   judged the same way: the tool is refused with its own reason (the caller only
   said hello as they picked up), and the opening is said again.
b. Closing line. A cut that landed on an armed closing line that had finished
   generating was counted as the first interruption before the verdict existed.
   When the verdict arrives as `ack` the cut becomes the second-interruption
   case of #465 and the line counts as said by rule. Once the line counts as
   said, by that or by an earlier interruption, an `ack` runs the armed action
   at once: nothing is left to answer, and the model's reply to the
   acknowledgement must not reach the caller (on a live qwen call it read out a
   made-up "[系统提示] 本轮通话已结束"). A first cut of a line still being
   generated leaves the line owed (#493): it is asked for again, once.

## Greetings over the opening (#594)

A callee picks up and says "喂" before anything else, and the bot's opening line
is already starting. The utterance answers nothing the bot said, so it should
neither cut the opening for good nor be answered as if it were the reply. It
applies to every AI call with an opening line (`SessionConfig.OpeningText`),
inbound and outbound, and needs no flow field.

The classifier (`classifyGreeting` in `backchannel.go`): an utterance made only
of greeting words, at most three of them in any order and with any repetition,
judged on the joined compact form the acknowledgement classifier uses. zh: 喂,
你好, 您好. en: hello, hi. "喂，您好", "喂喂喂", "你好你好" and "hi, hello" are
greetings; anything else in the utterance makes it words ("喂，请问你是哪位",
"hello, who is this?"). Unlike an acknowledgement, a trailing question mark does
not make it words: "喂？" and "hello?" are greetings. Only the call language's
list counts. A prefix of a greeting (or of a run of them) is greeting-so-far
for a partial transcript.

A greeting counts only over the opening: the opening turn (turn 1 when the call
has an opening line) or its replay. Over any other turn it is words, as before.

openai (the client holds speech): an utterance that starts while the opening is
being made or heard is held, also before any of its audio has been queued. The
partials keep the hold while they are greeting-so-far or an acknowledgement; a
final that is a greeting is declined exactly as an acknowledgement is
(`DeclineCallerTurn`, flagged `IsBackchannel`, nothing else hears it), so the
opening is never cut. At the cap a held utterance that is a greeting so far
does not cut the opening: a second cap of the same length is armed and the
final decides; words arriving in the meantime take the floor as ever. The
provider does not report a failed transcription (`...input_audio_transcription.failed`
is not handled), so a final may never come: at the second cap the utterance is
declined as a greeting (the item is deleted, the opening is not cut, the hold is
released), and a final that still arrives is recorded flagged and does nothing
else. With no text at the cap it takes the floor.

The engines that cut by themselves (qwen, grok, doubao, gateway): the cut has
happened, so the damage is limited afterwards.

- The cut is recorded when it is over the opening, also when the provider
  cancelled the opening before any audio was queued (that path used to open a
  cut only when something was queued). One cut is recorded once, whichever side
  reported it.
- A moving tool made in the response that answers the cut is refused as a
  conversation (`Runtime.RefuseAsGreeting`, en and zh), as for an
  acknowledgement.
- The opening is said again once per call, from its start, with the same
  rendered text, as a line that pre-empts (`Session.SayOpeningAgain`). If the
  model's reply to the greeting has started it is muted (`MuteTurn`) first. If
  it has not started yet the line waits for it to start (backstop 1000 ms),
  because a line requested on a free floor while qwen's own reply is about to be
  created is taken for that reply. The replay's own turn is recognised as
  opening, so a greeting that cuts the replay is refused a moving tool but gets
  no third telling.
- No replay when the flow has left the entry phase, when an ending is armed or
  under way, or when the words were not a greeting. An opening cut before it
  played in full is always owed; there is no "counts as said" estimate for it.

### The opening stays owed

A cut of the opening line is not always followed by the greeting's final
transcript on the cut that caused it. Live (qwen flash), "喂你好" was split by the
engine: it cut the opening at the first word, created a reply, and cancelled that
reply at the second. The final then arrived after a second cut, of the reply,
and was judged against that one. So the opening is owed at call level:

- A cut of the opening (turn 1 or its replay) marks it owed. While it is owed any
  later cut is judged as over the opening, with or without queued audio, so a
  cancelled reply in between is a cut of the opening's.
- A final transcript that is a greeting while the opening is owed still replays
  it when no cut judges those words (the cut was superseded, or another
  utterance's final): the mark stands in for the cut. Once per call stays.
- The mark clears when the opening has been asked for again, when a final
  transcript has words (anything that is not a greeting, an acknowledgement
  included), when a later turn with audio, neither muted nor cut, has played to the
  caller in full (`PLAYBACK_DONE`), or when the flow leaves the entry phase or an
  ending is under way.

### The opening stays owed

A cut of the opening line is not always followed by the greeting's final
transcript on the cut that caused it. Live (qwen flash), "喂你好" was split by the
engine: it cut the opening at the first word, created a reply, and cancelled that
reply at the second. The final then arrived after a second cut, of the reply,
and was judged against that one. So the opening is owed at call level:

- A cut of the opening (turn 1 or its replay) marks it owed. While it is owed any
  later cut is judged as over the opening, with or without queued audio, so a
  cancelled reply in between is a cut of the opening's.
- A final transcript that is a greeting while the opening is owed still replays
  it when no cut judges those words (the cut was superseded, or another
  utterance's final): the mark stands in for the cut. Once per call stays.
- The mark clears when the opening has been asked for again, when a final
  transcript has words (anything that is not a greeting, an acknowledgement
  included), when a later turn with audio, neither muted nor cut, has played to the
  caller in full (`PLAYBACK_DONE`), or when the flow leaves the entry phase or an
  ending is under way.

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
- A greeting is whatever the transcriber writes. A "喂" heard as 嗯 or the
  reverse is judged as what was heard (an acknowledgement over the opening is
  declined on openai and limited elsewhere, a greeting is replayed).
- A greeting said over the opening is declined or replayed on a call that has
  no wish for it: a callee who really is answering with "hello?" is greeted
  again by the opening, by design.
- On the cutting engines the opening is said twice at best: the first part the
  callee heard before the cut, then the whole line. The model's own paraphrase
  of a line sent mid-call is a risk that only a live qwen call shows
  (`TestLiveQwenCutOpener/.../hello`).
- A greeting that lands before the opening's response has been created at all
  (turn 0) is an ordinary turn.
