# Adding a voice provider

The extension point is the wire protocol, not Go.

`internal/provider` is a client for the OpenAI Realtime protocol plus a
`Profile` describing how a particular vendor speaks it. Adding an engine that
speaks that protocol means adding a profile and a name for `AICC_PROVIDER` —
never a second client, never an interface with two implementations behind it.

This is not an accident of the current code. Four engines already ship that way
(OpenAI, Grok, Qwen and the Realtime gateway) and they differ only in values: an
endpoint, a model, a dialect of the session payload, and a handful of
behavioural traits discovered by calling them. A fifth that speaks the same
protocol differs in exactly the same way.

Only a different **protocol** earns another client, and one has. `doubao` is
ByteDance's full-duplex dialogue API, and `internal/provider/doubao` is the
client that speaks it. That is the same rule applied rather than an exception
to it — `internal/transcribe` has held two clients under the same sentence since
it was written, for the same reason. A new *engine* on a protocol already spoken
here is a profile; a new *grammar* is a client, and the test is lifecycle
rather than field names. Doubao bootstraps its session and waits to be told it
exists, has no way to ask for a turn, announces neither the start nor the end of
the caller speaking, returns tool results as items with a role, and ends with a
handshake. None of that is expressible as values in a `Profile`.

Another client changes nothing above it. It lives in its own sub-package, it
implements the same provider-neutral `VoiceSession`, and every event it produces
is one the Realtime client already produces. It shares transport and audio
plumbing — `wsconn`, `provider.Watchdog`, `provider.MergeHint`,
`provider.SayExactly`, `internal/provider/pacer` — only where ownership,
lifetime and failure behaviour are identical, and it shares no protocol event,
ever. Decoding is where clients are supposed to differ, and factoring that
together is the abstraction layer this rule exists to prevent.

Two of those shared pieces are worth their own sentence, because they are the
shape sharing is allowed to take. `pacer` carries frames and names no protocol:
a client hands it a function that turns a frame into bytes and a function that
writes them, and keeps every decision about what the frames *say* — doubao takes
a frame per tick because its engine reads the uplink as a clock. `SayExactly` is
a demand about a conversation rather than about a wire: say these words, add
nothing, in the language this session is being held in. Both are shared because
every client that uses them means the same thing by them, which is the only
test.

## What a profile is

Every field of `provider.Profile` exists because a vendor forced it to
(`internal/provider/profile.go`):

| Field | What it settles |
|---|---|
| `Name`, `Endpoint`, `Model` | Who this is and where it answers. Both are overridable per deployment. |
| `APIKeyEnv` | The environment variable the credential comes from — the vendor's own name for it, not an `AICC_*` setting |
| `Style` | `GA` nests audio settings under `session.audio.input/output`; `BETA` is the older flat shape. Both are in the wild. |
| `Voice` | The profile's default voice, used only when the flow does not name one |
| `AcceptsTemperature` | Whether `SessionConfig.Temperature` (the flow's `global.temperature`) is sent. Nil leaves the provider's default on every profile. True on qwen (`session.temperature`, documented range [0, 2), default per model and undocumented for ours). False on openai (the GA `session` object has no such field), grok (accepted and echoed for any number, but no effect on output was seen at 3 samples per value, grok-findings 5.3), gateway (none documented) and doubao (no sampling parameter in its session body). A profile without support ignores the field and logs that once per process, not once per call. Only the opening session configuration carries it: never a mid-call instruction update, and not the Realtime client's retry after a rejected `session.update`, which drops the optional fields. |
| `AcceptsResponseToolChoice` | Whether a line's `response.create` carries `tool_choice: "none"` in its `response` object, so the turn that says a line (`SpeakText`: an announce, the wait line) does not call a tool. No other request carries it. True on openai (documented, not measured). False on qwen (accepted and ignored, as is `response.tools: []`: a line asked for over an unanswered call repeated the call; qwen-findings W-Q8), grok (undocumented, not measured) and gateway (refused as "Unsupported parameter", which would cost the line). Whatever the profile, the client never hands out a call made in a line's turn and deletes its item when the turn ends; a turn that called a tool and said nothing is asked for once more (see "A line's turn that calls a tool" below). |
| `NeedsToolFreeLineDirection` | Whether a mid-call line's direction (`SpeakText`) also bans any tool call in its turn and says the tool the line mentions is already running, in place of the plain `SayExactly`. True on qwen only: with `SayExactly` the wait line over a running tool was silent 3 of 3 (the flow's action-claim rule makes "我查一下" need a lookup call), with this direction it was said 6 of 6 with no call (qwen-findings W-Q8). The opening line and a tool result's hint keep `SayExactly`. |
| `DropsToolResultAfterLine` | Whether a line spoken while a tool call is still owed (`SpeakText` between the call and its result) ends the turn the call belongs to, so the result sent afterwards is dropped with no error and never answered. When it is true the orchestrator says no wait line over a slow HTTP tool (#286). True on doubao only: with no line before it a result was answered 1 of 1; after a line 0 of 3 (SayHello, ChatTTSText, SayHello plus a forced audio commit) and in 3 of 3 live calls (doubao-findings W-D10). The Realtime profiles owe the result's turn past the line and answer it. The orchestrator reads it, not a client. |
| `AcceptsG711` | Whether telephone audio passes through untouched. True removes conversion from the hot path entirely. |
| `LinearInput` / `LinearOutput` | The PCM formats used when it does not. Fixed by the vendor, not negotiated. |
| `CancelsResponseItself` | Whether it stops generating when it hears the caller, or has to be told |
| `NeedsCueForFirstTurn` | Whether it refuses to speak into an empty conversation. Our bot greets first, so those providers need a synthetic cue. |
| `NeedsDirectedLineInConversation` | Whether a mid-call line that *ends the call* (`SpeakText(text, isClosing=true)`) must also be put in the conversation as a caller message carrying the same `SayExactly` direction, ahead of the request that carries it as a per-response override. True on qwen, where the override alone lost to the conversation already there (qwen-findings W-Q1). A line the call goes on from keeps the override alone on every profile. |
| `IgnoresOverrideOnceTalking` | Whether a `response.create` carrying only a per-response `instructions` override is silently dropped once the conversation holds a turn: no `response.created`, no error. True on grok (0 of 5; grok-findings 5.2). Every mid-call line asked for with `SpeakText`, closing or not, then goes into the conversation as a caller message first and the override follows (3 of 3 verbatim) — the mechanism `NeedsDirectedLineInConversation` applies to closing lines only. The opening line is exempt: the override alone works on an empty conversation. |
| `RequiresTerminalAnnounce` | Whether the engine speaks only words it is handed: *no* text will make it take a turn. Stronger than the row above: a cue is something a client can invent, and this says there is no cue at all, so a phase the call stops at must carry its own words or the caller hears silence, and a publish is refused otherwise (`flow.RequireTerminalAnnounce`, `TERMINAL_ANNOUNCE_REQUIRED`). The same capability decides how the closing line travels. Where it is true (doubao) a tool result that moves the call into a terminal phase carries the phase's instruction and the line is said with `SpeakText`, which is exact by construction; the session limit also has no cue to ask for a goodbye or handover with. Where it is false (openai, grok, qwen, gateway; grok measured in grok-findings 5.2, where a tool-result hint was verbatim 3 of 3) the tool result carries the line as its hint (`SayExactly`), so the turn the result produces is the line, because a line asked for on top of the result lost to it on qwen (qwen-findings W-Q1). |

The last five are the interesting ones. They are not configuration in any
meaningful sense — they are findings. Each was written down after a live call
behaved differently from the documentation, and each is a bug somewhere else in
the call if it is wrong.

A profile answered by a client other than the Realtime one fills in only what
that client reads. `DoubaoProfile` leaves `Style`, `TranscribeModel`,
`CancelsResponseItself`, `NeedsCueForFirstTurn`,
`NeedsDirectedLineInConversation`, `AcceptsResponseToolChoice`,
`NeedsToolFreeLineDirection` at zero, and
says so in its doc comment: a trait nothing reads is worse than an absent one,
because the next person takes it for a statement about the vendor. `Model` is
informational for the same reason — the version is a constant inside the
client, and `AICC_PROVIDER_MODEL` cannot move it. `RequiresTerminalAnnounce` is
true on doubao, whose engine takes no text cue at all and whose `SpeakText`
commits text the engine synthesises. So is `DropsToolResultAfterLine`, which the
orchestrator reads: that engine drops a tool result sent after a line.

**Mid-call instructions (`UpdateInstructions`, an instructions-only
`session.update`).** Qwen: MEASURED 2026-10-15 to be obeyed from the next reply,
mid-reply included, with tools surviving (qwen-findings W-Q7; the doubao
counterpart is doubao-findings W-D9). OpenAI (`gpt-realtime-2.1`, G.711 mu-law
passthrough): MEASURED 2026-10-15 with the same test and arms,
`TestLiveOpenAIMidCallInstructions`
(`internal/provider/live_instructions_test.go`), three sessions per arm, 15 in
all, no error frame and no close. Appended paragraph making every answer begin
"好的，王先生":

| Arm | Obeyed |
|---|---|
| Control, no update | marker in 0 of 9 replies |
| Update while idle, then two questions | 6 of 6 (answer before the update plain 3 of 3) |
| Update while the first answer was speaking, then two questions | 6 of 6; the spoken answer was not marked |
| Tool control (no update) | `lookup_order` called 3 of 3 |
| Tools declared at start, update adds the marker rule | tool called 3 of 3; reply after the result carries the marker 3 of 3 |

Before the update: `我们的人工客服服务时间是工作日早上九点到晚上六点。…` After it:
`好的，王先生，您可以通过 NovaNet 官网或 App 提交办理申请，也可以拨打客服热线由专员为您开通。…`
Tools: `好的，王先生，我先去系统里查一下这个订单的状态。` then
`lookup_order {"orderId":"12345"}`, then `好的，王先生，您的订单已发货，预计明天送达。`
So an instructions-only update is a sufficient phase change on openai, mid-reply
included, and tools survive it. NOT MEASURED, as on qwen: conflicting or repeated
updates, a slot value recalled verbatim, an update late in a long call. Run
with `-count=1`. Every engine this repository ships (openai, grok, qwen, gateway, doubao) applies a
mid-call instruction change at once; the provider layer assumes it.

**A line's turn that calls a tool (#351).** A line asked for mid-call
(`SpeakText`: a phase's `announce`, the wait line) can come back as a turn that
calls a tool instead of saying it. The client never hands that call out and
deletes its item when the turn ends (W-Q8). If the turn ran to its end and
none of its audio reached the consumer, the client asks for the line once
more, on every Realtime profile, with no branch on the name. The second
direction names the tools the turn called as not to be called now and says the
caller has not heard the line yet (`sayExactlyAfterCall`; on qwen it also says
any tool the line mentions is already running, as `NeedsToolFreeLineDirection`
does). It travels as the request's override and, on every profile, as a caller
message ahead of it, and that message is deleted once the turn ends: it is
there for that turn only. The client learns the message's id from the
provider's `conversation.item.created` (`.added` in the GA dialect); a message
never named stays. On qwen the override alone lost again 9 of 9, and the
message left in made the next turn answer the caller with the wrong tool 9 of
9. Once only: if the second turn calls a tool too, the line is given up with a
WARN, and the caller hears silence until they speak or the dead-air watch
prompts them. A turn the caller heard, a turn cut short (by the caller, a
keypress or a newer line) and a caller who has started speaking are never asked
again. A turn owed to a tool result or a cue stays owed past the second ask. The
second turn's `ResponseStarted` carries `IsLineAskedAgain`, so a wait line
asked again stays an aside in `aicall`. Doubao has no such turn: its
`SpeakText` commits text the engine synthesises. Measured on qwen in
qwen-findings W-Q10.

## Grok (xAI Grok Voice)

`AICC_PROVIDER=grok`, key `XAI_API_KEY`, `wss://api.x.ai/v1/realtime`, model
`grok-voice-3.0`: a profile on the Realtime client (issue #226). Evidence:
[grok-findings](design/grok-findings.md). How it differs from the openai row:

| Trait | Value | Why |
|---|---|---|
| `Style`, `AcceptsG711` | GA, true | `audio/pcmu` / `audio/pcma` at 8 kHz pass through both ways |
| `Voice` | `eve` | one of `eve`, `ara`, `rex`, `sal`, `leo`; `global.voice` wins |
| `NeedsCueForFirstTurn` | false | `response.create` on an empty conversation greets from the instructions |
| `TranscribeModel` | empty | the engine transcribes the caller unasked, 9 of 9 clips exact; `grok-transcribe` gave the same text plus partials during speech (5.4) |
| `CancelsResponseItself` | true | the server ends its response itself just before `speech_started`, 3 of 3 (5.1); a late cancel is refused with `invalid_request_error` |
| `IgnoresOverrideOnceTalking` | true | a per-response override alone is dropped once the conversation has a turn, 0 of 5; item-then-override 3 of 3 (5.2) |
| `RequiresTerminalAnnounce` | false | text cues work; tool-result hint verbatim 3 of 3 (5.2) |
| `AcceptsTemperature` | false | accepted and echoed, no effect seen at N=3 (5.3) |

Its dialect is absorbed by the shared client, with no branch on the profile's
name: the `ping` event is ignored; `...input_audio_transcription.updated` is
cumulative and is not a transcript line; `...transcription.completed` is final
only when its `status` is absent or `completed` (a repeat with `in_progress` is
dropped), so a caller utterance is exactly one line; a refused `response.cancel`
("no active response found", code `invalid_request_error`) within the cancel
window of one this client sent is not an error; a `conversation.item.truncate` on
an unknown id gets no answer at all.

The silent drop is why the trait exists, and the client no longer waits on it
for ever: every `response.create` it sends waits at most
`responseCreatedTimeout` for `response.created` (the watchdog only watches an
open response, so nothing else would end that wait). On expiry the floor is
given back with one WARN and a count in `aicc_provider_response_unanswered_total`;
a line waiting for the floor is then asked for instead, otherwise the request is
retried once (a line with its direction put in the conversation), and a retry
that is dropped too is given up. A request that was only late makes the provider
refuse the retry, which the client recognises as its own and does not report.
The same holds on every Realtime profile, with no branch on the name.

One ordering rule is new: audio sent before the session configuration is
acknowledged leaves the session deaf, with no error. `Start` returns only after
`session.updated`, and `aicall.Session.Start` starts the caller-audio pump after
`model.Start` returns (`TestNoCallerAudioReachesTheModelBeforeItsStartReturns`).
As on gateway and doubao, set `AICC_TRANSCRIBE_PROVIDER` when live transcription
of the human phase is on: xAI has no recogniser client here.

## The steps

1. **Verify against the real endpoint first.** Everything above is
   unknowable from a specification. Every client keeps that verification beside
   itself, in a `live_test.go` gated on the same variable:

   ```sh
   AICC_LIVE_PROVIDER_TEST=1 go test ./internal/provider/... -run Live -v
   ```

   It spends real API credit, which is the point: the alternative is finding
   out during a call.

2. **Write the profile.** A constructor beside `OpenAIProfile` and
   `QwenProfile`, with a comment on every trait saying what was observed. A
   trait with no observation behind it is a guess, and guesses here fail at the
   worst moment.

3. **Name it.** Add the constant and the `ProfileFor` case. An unknown
   `AICC_PROVIDER` must keep refusing to start — a deployment silently falling
   back to another vendor is worse than not starting. A name that also selects a
   *client* gets a second registration, in `cmd/aicc/wiring.go`: a client in a
   sub-package of `internal/provider` cannot be built from inside it, so the
   composition root is the only place where both are in scope.

4. **Register the setting.** `.env.example` is the registry; the new name goes
   in the `AICC_PROVIDER` comment with its endpoint and model defaults.

5. **Note the voices.** Voice names are provider-specific and a deployment runs
   one provider, so a flow names a voice its own deployment offers
   (`global.voice`, published with the persona). List them where the deployment
   documentation lists the others.

## The opening gate

`SessionConfig.OpeningGate` (#98) holds back the request for the call's first
turn. Every client honours it the same way: dial and finish the handshake as
usual, then `provider.AwaitOpeningGate(ctx, cfg.OpeningGate, <closed>)` before
asking for the first turn (the greeting request, the cue a provider needs, the
committed opening line). Nil asks at once, which is what a call without the
setting does. Rules a new client keeps:

- `Start` blocks on the gate and returns after the first turn is requested, so
  the call's audio pumps start afterwards and no caller audio reaches the
  provider ahead of the greeting. The wait is not part of any handshake
  deadline: it runs after the handshake is confirmed.
- It ends early with an error on ctx cancellation or the session closing, and
  closes its socket; nothing may leak.
- Send nothing while waiting. The uplink pacer of a client that has one starts
  after the wait, as it does without a gate: no caller audio exists yet, and
  doubao's measured behaviour (a minute muted with nothing sent was harmless;
  the documented idle release is ten minutes, `doubao-findings.md` §6/§7) makes
  an idle session safe for the length a wait can have.
- A client that has no first turn to ask for (doubao without an opening line)
  has nothing to hold and does not wait.

The gate is built by `aicall.Session` from `AICC_BOT_GREETING_MEDIA_WAIT`; a
client never builds one.

## What never happens

**A second client for a protocol that already has one.** A vendor's dialect of
Realtime is a profile. If the difference between an engine and one that already
works here is an endpoint, a model, a field name or a behavioural trait, it is
values, and writing a client for it is writing the same client twice.

**A widened `VoiceSession`.** It is the seam between the call actor and whatever
client answers, plus its test fake, and it is stated in AICC's own vocabulary —
sessions, turns, speech, tool calls. A protocol concept has never appeared in it
and never will. A second client is added *underneath* it, saying the same
things; the day one of them needs the seam to grow a method named after
something on its wire is the day that client is doing the seam's job.

**Shared protocol code.** Two clients may share a socket, a keepalive, a
watchdog and a pure function, and only because ownership and runtime semantics
are identical in both — the same test `internal/transcribe` applies. They share
no event, no decoder and no dispatch. That is where they are supposed to differ.

**A cascade.** ASR + LLM + TTS composed in-process is out of scope permanently
(phase1-decisions A6) — no types, no interfaces, no adapters, no stubs, no
TODOs. The composition is a real and useful thing to build; it is simply
another service.

**Another protocol on a whim.** A new client is a wire protocol's worth of
lifecycle, failure modes and tests, verified against the live endpoint before a
line of it is written. That one has been admitted is not a precedent for
another: each was argued from lifecycle, against the real service, with the
findings written down first. An engine that speaks none of the protocols here
reaches a call through the Realtime gateway, which is what the gateway is for; a
client of its own has to be worth that, and has to be decided rather than
drifted into.

## Attaching another protocol

`AICC_PROVIDER=doubao` is the one name here that selects a client as well as a
profile. What it costs, and what was measured to justify it, is written down in
[doubao-findings](design/doubao-findings.md); what a deployment has to know is
short.

```sh
AICC_PROVIDER=doubao
DOUBAO_API_KEY=…                  # the vendor's own name for it
AICC_TRANSCRIBE_PROVIDER=qwen     # the human phase's recogniser is separate
# AICC_PROVIDER_MODEL is ignored — the protocol version is pinned in the client
```

Two of doubao's properties reach the flows rather than the environment. Voice
names are this vendor's own and go in `global.voice`, as on every provider. And
because nothing this client sends makes that engine take a turn, **every
terminal phase must carry an `announce`** — `RequiresTerminalAnnounce` turns
that into a publish rule, so a flow that would have left a caller in silence is
refused with a reason rather than discovered on a call. The flow's time limit
(`global.maxDurationSec`) is the one ending that rule cannot see: with no
`global.closingTarget` a doubao call is hung up at the limit with no line at
all, because there is no goodbye cue to send. Doubao flows should set
`closingTarget`, whose terminal phase already carries an `announce`. This is
documented, not enforced at publish. An entry phase with no
`announce` is legal and means the bot answers and waits for the caller to speak.

On the Realtime engines the closing line is best effort, and the platform
checks it (issue [#262](https://github.com/rasonyang/ai-native-callcenter/issues/262)).
When a hangup moves the call into a terminal phase with an `announce`, the words
of the turn that follows are held against the line. A turn that does not say it,
typically one response that ended the call with `record_lead` and `hangup` and
was answered with the model's own confirmation, is answered by asking for the
line once more through the closing `SpeakText`, after the caller has heard that
turn whole; a double goodbye is possible and accepted. A second miss is logged
at WARN and the call ends on that turn. Doubao says its line itself and is not
checked.

## Attaching something that is not a vendor

The interesting case is the one the design reserves: a **Realtime gateway** — a
separate service that composes ASR, an LLM and TTS behind the Realtime
protocol, so anything can answer a call without this repository learning how it
works.

From here it is indistinguishable from a vendor. It gets a profile whose
endpoint points at itself, its own `AICC_PROVIDER` value, and its own dialect
if it needs one. It impersonates nobody: it is its own name, speaking a
published protocol.

It now has one — `AICC_PROVIDER=gateway`, `GatewayProfile()`:

```sh
AICC_PROVIDER=gateway
AICC_PROVIDER_ENDPOINT=ws://127.0.0.1:9090/v1/realtime   # empty keeps :8080
REALTIME_API_KEY=…                # the credential the gateway accepts
AICC_TRANSCRIBE_PROVIDER=qwen     # the human phase's recogniser is separate
```

The traits that make it a profile rather than an endpoint override:

- **`AcceptsG711: false`, 24 kHz linear both ways.** It refuses telephone
  audio outright, so both directions resample. Reaching it as
  `AICC_PROVIDER=openai` with an endpoint override would put G.711 on a socket
  that rejects it, and the call would fail on its first frame rather than at
  startup — which is exactly the lie a profile exists to prevent.
- **`TranscribeModel: ""`.** It accepts `audio.input.transcription`, but the
  model and language in it only echo; what recognises the caller is configured
  on the gateway's own profile. A value here would be one nothing reads.
- **`Voice: ""`.** The voice belongs to whichever engine the gateway drives,
  and the flow names it (`global.voice`) — so a name in this profile could
  only be wrong for some deployment. Voice names are the engine's, not the
  gateway's: a flow published for one composition is not portable to another.
- **`CancelsResponseItself: true`.** Its turn detection cancels the response
  when it hears the caller.

The gateway serves plain `ws://` — the official SDKs demand `wss://`, this
client does not — and it is not a recogniser, which is why
`AICC_TRANSCRIBE_PROVIDER` has to be said rather than following
`AICC_PROVIDER`.

Reaching a gateway through an endpoint override alone is still the right way
to *try* one, because the endpoint is configuration and not a constant. It is
the wrong way to ship one, for the reason above: the profile then lies about
which traits hold.

## Where a call's language comes in

Nowhere. A DID's language sets the greeting and the prompt language. The voice comes
from the flow's `global.voice`, or the profile's default where the flow names
none (A7).
It has never selected a provider and reintroducing that mapping is a regression
(phase1-decisions A1). One provider answers every call in a deployment, chosen
at startup, because the vendors that ship here are not all reachable with
acceptable latency from the same network — `qwen` or `doubao` inside mainland
China, `openai` elsewhere.
