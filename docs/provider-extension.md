# Adding a voice provider

The extension point is the wire protocol, not Go.

`internal/provider` is a client for the OpenAI Realtime protocol plus a
`Profile` describing how a particular vendor speaks it. Adding an engine that
speaks that protocol means adding a profile and a name for `AICC_PROVIDER` —
never a second client, never an interface with two implementations behind it.

This is not an accident of the current code. Three engines already ship that way
(OpenAI, Qwen and the Realtime gateway) and they differ only in values: an
endpoint, a model, a dialect of the session payload, and a handful of
behavioural traits discovered by calling them. A fourth that speaks the same
protocol differs in exactly the same way.

Only a different **protocol** earns another client, and two have. `doubao` is
ByteDance's full-duplex dialogue API and `gemini` is Google's Live bidirectional
API; `internal/provider/doubao` and `internal/provider/gemini` are the clients
that speak them. That is the same rule applied rather than an exception to it —
`internal/transcribe` has held two clients under the same sentence since it was
written, for the same reason. A new *engine* on a protocol already spoken here
is a profile; a new *grammar* is a client, and the test is lifecycle rather than
field names. Doubao bootstraps its session and waits to be told it exists, has
no way to ask for a turn, announces neither the start nor the end of the caller
speaking, returns tool results as items with a role, and ends with a handshake.
Gemini configures its session once and cannot change any of it while the
connection is open, declares a turn over twice because the model stopping and
the caller having heard it are different moments, offers no cancel at all, and
reports errors only by closing the socket with a code. None of that is
expressible as values in a `Profile`.

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
writes them, and keeps every decision about what the frames *say* — one takes a
frame per tick because its engine reads the uplink as a clock, the other keeps a
deep queue and drains it whole because writes to it were measured blocking for
seconds at a time — where that happens is open (`gemini-findings.md` §2.5: the
network path those measurements ran over was not a controlled condition) and the
policy is right either way, since that engine accepts a burst and dropping the
caller's words would buy nothing. `SayExactly` is a
demand about a conversation rather than about a wire: say these words, add
nothing, in the language this session is being held in. Both are shared because
the two clients mean the same thing by them, which is the only test.

## What a profile is

Every field of `provider.Profile` exists because a vendor forced it to
(`internal/provider/profile.go`):

| Field | What it settles |
|---|---|
| `Name`, `Endpoint`, `Model` | Who this is and where it answers. Both are overridable per deployment. |
| `APIKeyEnv` | The environment variable the credential comes from — the vendor's own name for it, not an `AICC_*` setting |
| `Headers` | Anything the upgrade request needs beyond authorization |
| `Style` | `GA` nests audio settings under `session.audio.input/output`; `BETA` is the older flat shape. Both are in the wild. |
| `Voice` | The profile's default voice, used only when the flow does not name one |
| `AcceptsG711` | Whether telephone audio passes through untouched. True removes conversion from the hot path entirely. |
| `LinearInput` / `LinearOutput` | The PCM formats used when it does not. Fixed by the vendor, not negotiated. |
| `CancelsResponseItself` | Whether it stops generating when it hears the caller, or has to be told |
| `NeedsCueForFirstTurn` | Whether it refuses to speak into an empty conversation. Our bot greets first, so those providers need a synthetic cue. |
| `NeedsDirectedLineInConversation` | Whether a mid-call line that *ends the call* (`SpeakText(text, isClosing=true)`) must also be put in the conversation as a caller message carrying the same `SayExactly` direction, ahead of the request that carries it as a per-response override. True on qwen, where the override alone lost to the conversation already there (qwen-findings W-Q1). A line the call goes on from keeps the override alone on every profile. |
| `RequiresTerminalAnnounce` | Whether *no* text will make it take a turn. Stronger than the row above: a cue is something a client can invent, and this says there is no cue at all, so a phase the call stops at must carry its own words or the caller hears silence. A publish is refused otherwise. |
| `PutsTerminalAnnounceInToolResult` | Whether a tool result that moves the call into a terminal phase with an `announce` carries the line as its hint (`SayExactly`), so the turn the result produces is the line, or carries the phase's instruction and has the line said with `SpeakText`. True on the Realtime profiles, where a line asked for on top of the result lost to it on qwen (qwen-findings W-Q1). False on doubao, whose `SpeakText` is exact by construction, and on gemini until a tool result it leaves unanswered is fixed (gemini-findings W-G6). |
| `SemanticTurnType`, `SemanticTurnSilenceMs` | The vendor's name for semantic turn detection, and the hold it forces in that mode regardless of what was asked |

The last seven are the interesting ones. They are not configuration in any
meaningful sense — they are findings. Each was written down after a live call
behaved differently from the documentation, and each is a bug somewhere else in
the call if it is wrong.

A profile answered by a client other than the Realtime one fills in only what
that client reads. `DoubaoProfile` and `GeminiProfile` both leave `Style`,
`Headers`, `TranscribeModel`, `CancelsResponseItself`, `NeedsCueForFirstTurn`,
`NeedsDirectedLineInConversation` and both semantic-turn fields at zero, and say so in their doc comments: a trait
nothing reads is worse than an absent one, because the next person takes it for
a statement about the vendor. `Model` is informational on both for the same
reason — the version, or the model name, is a constant inside the client, and
`AICC_PROVIDER_MODEL` cannot move it. Where the two differ is
`RequiresTerminalAnnounce`: true on doubao, whose engine takes no text cue at
all, and false on gemini, which speaks a line it is given the way the Realtime
client's engines do. `PutsTerminalAnnounceInToolResult` is false on both, for
different reasons: doubao's `SpeakText` commits text the engine synthesises,
and gemini sometimes answers a tool result with nothing, which the `SpeakText`
after it still covers (gemini-findings W-G6).

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
line of it is written. That two have now been admitted is not a precedent for a
third: each was argued from lifecycle, against the real service, with the
findings written down first. An engine that speaks none of the protocols here
reaches a call through the Realtime gateway, which is what the gateway is for; a
client of its own has to be worth that, and has to be decided rather than
drifted into.

## Attaching another protocol

`AICC_PROVIDER=doubao` and `AICC_PROVIDER=gemini` are the two names here that
select a client as well as a profile. What each costs, and what was measured to
justify it, is written down in
[doubao-findings](design/doubao-findings.md) and
[gemini-findings](design/gemini-findings.md); what a deployment has to know is
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
refused with a reason rather than discovered on a call. An entry phase with no
`announce` is legal and means the bot answers and waits for the caller to speak.

```sh
AICC_PROVIDER=gemini
GEMINI_API_KEY=…                  # the vendor's own name for it
AICC_TRANSCRIBE_PROVIDER=openai   # the human phase's recogniser is separate
# AICC_PROVIDER_MODEL is ignored — the model name is pinned in the client
# outbound access to generativelanguage.googleapis.com is required
```

Gemini asks nothing of a flow that the Realtime providers do not, and three
things of a deployment. Voice names are this vendor's own, in `global.voice` as
always. **The language the bot speaks is steered by the flow's instructions and
by nothing else** — this session has no language field, and the native-audio
models refuse to be told one — so a flow that wants Chinese asks for Chinese in
its own words. And **the provider ends the connection** once its own session
lifetime runs out, with the caller still on the line: there is no reconnect, the
call is released with the hangup cause `PROVIDER_SESSION_EXPIRED`, and the
caller is rescued to the DID's fallback queue. A deployment that puts long
conversations behind this provider wants that queue to exist.

One consequence of the setup-once session is worth knowing before writing a
flow for it: a phase change cannot be pushed to the model. It rides the next
thing the client says — a tool result's hint, or the text cue a keypress or a
silence produces — which is enough for the flows this repository ships and is
the reason `gemini-findings.md` §2.6 exists.

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

Nowhere. A DID's language sets the greeting, the prompt language and the voice.
It has never selected a provider and reintroducing that mapping is a regression
(phase1-decisions A1). One provider answers every call in a deployment, chosen
at startup, because the vendors that ship here are not all reachable with
acceptable latency from the same network — `qwen` or `doubao` inside mainland
China, `openai` or `gemini` elsewhere.

On `gemini` this is more than a rule about routing: that session has no language
field at all, so the DID's language reaches the model as words in its
instructions and in nothing else.
