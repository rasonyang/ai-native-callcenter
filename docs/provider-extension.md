# Adding a voice provider

The extension point is the wire protocol, not Go.

`internal/provider` is one client for the OpenAI Realtime protocol plus a
`Profile` describing how a particular vendor speaks it. Adding a provider means
adding a profile and a name for `AICC_PROVIDER` — never a second client, never
an interface with two implementations behind it.

This is not an accident of the current code. Two engines already ship (OpenAI
and Qwen) and they differ only in values: an endpoint, a model, a dialect of
the session payload, and a handful of behavioural traits discovered by calling
them. A third that speaks the same protocol differs in exactly the same way.

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
| `SemanticTurnType`, `SemanticTurnSilenceMs` | The vendor's name for semantic turn detection, and the hold it forces in that mode regardless of what was asked |

The last four are the interesting ones. They are not configuration in any
meaningful sense — they are findings. Each was written down after a live call
behaved differently from the documentation, and each is a bug somewhere else in
the call if it is wrong.

## The steps

1. **Verify against the real endpoint first.** Everything above is
   unknowable from a specification. `internal/provider/live_test.go` is where
   that verification lives:

   ```sh
   AICC_LIVE_PROVIDER_TEST=1 go test ./internal/provider/ -run Live -v
   ```

   It spends real API credit, which is the point: the alternative is finding
   out during a call.

2. **Write the profile.** A constructor beside `OpenAIProfile` and
   `QwenProfile`, with a comment on every trait saying what was observed. A
   trait with no observation behind it is a guess, and guesses here fail at the
   worst moment.

3. **Name it.** Add the constant and the `ProfileFor` case. An unknown
   `AICC_PROVIDER` must keep refusing to start — a deployment silently falling
   back to another vendor is worse than not starting.

4. **Register the setting.** `.env.example` is the registry; the new name goes
   in the `AICC_PROVIDER` comment with its endpoint and model defaults.

5. **Note the voices.** Voice names are provider-specific and a deployment runs
   one provider, so a flow names a voice its own deployment offers
   (`global.voice`, published with the persona). List them where the deployment
   documentation lists the others.

## What never happens

**A second client.** `VoiceSession` is the seam between the call actor and the
one Realtime client, plus its test fake. It is not a generalisation point.
Widening it to fit a protocol that is not Realtime is how a codebase acquires
an abstraction layer nobody wanted.

**A cascade.** ASR + LLM + TTS composed in-process is out of scope permanently
(phase1-decisions A6) — no types, no interfaces, no adapters, no stubs, no
TODOs. The composition is a real and useful thing to build; it is simply
another service.

**A vendor's protocol other than this one.** If an engine does not speak the
OpenAI Realtime protocol, it does not attach here.

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
at startup, because the two shipped vendors are not both reachable with
acceptable latency from the same network — `qwen` inside mainland China,
`openai` elsewhere.
