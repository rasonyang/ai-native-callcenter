// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// sessionStyle is which dialect of the session payload a provider speaks.
type sessionStyle string

const (
	// styleGA nests audio settings under session.audio.input/output.
	styleGA sessionStyle = "GA"
	// styleBeta is the older flat shape, still spoken by several vendors.
	styleBeta sessionStyle = "BETA"
)

// Profile is everything that differs between vendors of the same protocol.
// Behaviour lives in one client; only these values change.
type Profile struct {
	Name     string
	Endpoint string
	Model    string
	// APIKeyEnv is where the credential comes from.
	APIKeyEnv string

	Style sessionStyle
	Voice string
	// TranscribeModel asks the conversation session to transcribe what the
	// caller says, under audio.input.transcription. Empty leaves it to the
	// provider, which is not the same as off: one vendor here transcribes
	// unasked and the other does not.
	TranscribeModel string

	// AcceptsG711 means telephone audio can be passed through untouched, with
	// no decoding or resampling anywhere in the path.
	AcceptsG711 bool
	// LinearInput and LinearOutput are the formats used when G.711 is not an
	// option. They are fixed by the vendor, not negotiated.
	LinearInput  media.AudioFormat
	LinearOutput media.AudioFormat

	// CancelsResponseItself means the provider stops generating on its own
	// when it hears the caller. Where this is false the client has to say so
	// explicitly, and a missed cancel leaves the model talking over the caller.
	CancelsResponseItself bool

	// NeedsCueForFirstTurn means the provider refuses to speak into an empty
	// conversation and must be given something to answer. Our bot answers the
	// phone and greets first, so on those providers the opening turn has to be
	// prompted with a synthetic cue.
	NeedsCueForFirstTurn bool

	// NeedsDirectedLineInConversation means a line that ends the call, asked
	// for mid-call (SpeakText with isClosing), must also be put in the
	// conversation, as a caller message carrying the same SayExactly
	// direction, ahead of the response.create that carries it as a
	// per-response override. With the override alone the model answers the
	// last thing on the caller's side of the conversation instead: 4 of 7 on
	// qwen, where the opening turn, which carries the direction both ways, was
	// 8 of 8 (docs/design/qwen-findings.md, W-Q1). On live call
	// 01a0cbb9-3f93-702d-8c62-7536378c1b93 a dead-air move into the closing
	// phase asked for its goodbye with the override alone, and the model
	// repeated the two dead-air check-ins already in the conversation instead;
	// the caller was released without hearing a goodbye.
	//
	// Closing lines only. The item stays in the history, and what it does to
	// the turns after it has not been measured; a call that is ending has
	// none. A line in a phase the conversation goes on from keeps the override
	// alone.
	//
	// Not NeedsCueForFirstTurn: that one records that an empty conversation is
	// refused, a fact about starting a conversation. This one is about what the
	// model listens to once there is one. The cost is the one the opening turn
	// already pays: the direction enters the history as caller text.
	NeedsDirectedLineInConversation bool

	// IgnoresOverrideOnceTalking means that once the conversation holds a turn,
	// a response.create carrying only a per-response instructions override
	// (speakRequest, SayExactly) is silently dropped: no response.created, no
	// error, nothing for as long as anyone waits. Measured on grok 0 of 5 after
	// one or two caller turns, against 3 of 3 verbatim when the same direction
	// is first put in the conversation as a caller message and the override
	// follows (docs/design/grok-findings.md section 5.2). The opening line is
	// unaffected: on an empty conversation the override alone works.
	//
	// Where it is true, every mid-call line asked for with SpeakText, closing or
	// not, puts its direction in the conversation first, the mechanism
	// NeedsDirectedLineInConversation applies to closing lines only. The item
	// stays in the history; a silent drop is worse than that cost. It reaches
	// no transcript: the caller-side transcript is built from input audio
	// transcription events, which a text item never produces.
	//
	// Not NeedsDirectedLineInConversation: that one records that the override
	// loses to the conversation (the model answers the last caller message
	// instead) and is applied to closing lines because a call that is ending has
	// no later turns to spoil. This one records that the override is not heard
	// at all, so no line can do without the item.
	IgnoresOverrideOnceTalking bool

	// RequiresTerminalAnnounce says the engine speaks only words it is handed:
	// no text this client sends makes it take a turn, so a phase the call does
	// not leave has to carry its own words (a flow without them is refused at
	// publish, flow.RequireTerminalAnnounce), and those words reach the caller
	// as SpeakText, in a turn of their own.
	//
	// It is one capability with two faces. For a flow: a terminal phase needs an
	// announce. For the closing path: the line is committed with SpeakText, so
	// a tool result that moves the call into the terminal phase carries only the
	// phase's instruction, and there is no text cue to ask for a goodbye or a
	// handover with (session limit). Where it is false the engine takes a text
	// cue, and a tool result that moves the call into a terminal phase with a
	// line of its own carries that line as its hint (SayExactly) — the turn the
	// result produces is the line, and no SpeakText follows. Asking for the line
	// in a turn of its own on top of the result measured 0 of 7 on qwen, because
	// the model follows the tool result it has just read over a per-response
	// override (docs/design/qwen-findings.md, W-Q1). Doubao's SpeakText commits
	// text the engine synthesises and is exact by construction; a direction to a
	// model is not.
	//
	// Stronger than NeedsCueForFirstTurn and not the same thing: that one says a
	// cue is needed to START a conversation, and a cue is something this client
	// can invent. This one says there is no cue at all.
	//
	// True on one profile here, doubao. A flow is refused at publish rather than
	// at load because the rule belongs to the deployment and not to the dialect.
	RequiresTerminalAnnounce bool

	// AcceptsTemperature means the session configuration has a sampling
	// temperature field and this client sends SessionConfig.Temperature in it.
	// Where it is false the value is ignored: the OpenAI GA session object has
	// no such field, the gateway's is not documented, and Doubao's session has
	// no sampling parameter at all. Sending it anyway risks a rejected
	// session.update, which costs the whole session.
	AcceptsTemperature bool

	// AcceptsResponseToolChoice means a response.create may carry tool_choice
	// in its response object and the engine obeys it, and this client sends
	// "none" on every request for a line (SpeakText: a phase's announce, the
	// wait line), so the turn that says the line does not call a tool. Other
	// requests carry none: the turn that answers a tool result or a caller must
	// still be able to call one.
	//
	// A line asked for while a tool's call is unanswered was answered with a
	// second, identical call to the same tool on qwen: 4 calls of 4 in the
	// #286 live round, and 3 of 3 sessions of TestLiveQwenLineCallsNoTool, one
	// without the field, one with tool_choice "none", one with an empty tools
	// list (qwen-findings W-Q8). Whatever the profile, the client never hands
	// such a call out and takes it back out of the conversation (onLineCall);
	// this field is what lets an engine that obeys it say the line instead of
	// spending the turn on a call.
	//
	// Where it is false the field is not sent: an engine that ignores it gains
	// nothing, and one that refuses it costs the line (the Realtime gateway
	// rejects response.tool_choice and response.tools alike as "Unsupported
	// parameter").
	AcceptsResponseToolChoice bool

	// NeedsToolFreeLineDirection means a line asked for mid-call (SpeakText)
	// carries a direction that also bans any tool call in its turn and says
	// that the tool the line mentions is already running
	// (sayExactlyWithoutTools), in place of the plain SayExactly demand. The
	// opening line and a tool result's hint keep SayExactly.
	//
	// Measured on qwen, the wait line on the session of the #286 live round
	// (TestLiveQwenWaitLine, qwen-findings W-Q8): with SayExactly the line's
	// turn called the running tool again and said nothing, 3 of 3 (2 of 5
	// live calls silent, the rest late); with this direction the line was
	// said and no tool was called, 6 of 6. The flow's action-claim rule is
	// why: "我查一下" may only be said with a lookup tool's call.
	//
	// Not AcceptsResponseToolChoice: that bars tools on the wire, where an
	// engine obeys it, and qwen does not. This is the bar in words, and it is
	// set only where it was measured; a longer direction is a change to every
	// line's wording on the engines where SayExactly was measured verbatim.
	NeedsToolFreeLineDirection bool

	// DropsToolResultAfterLine means a line spoken while a tool call is
	// still owed (SpeakText between the call and its result) ends the turn
	// that call belongs to: the result sent afterwards is dropped without an
	// error and the model never answers it. The orchestrator then never says
	// a slow tool's wait line (#286), because the caller would hear the line
	// and then nothing.
	//
	// Measured on doubao (doubao-findings W-D10): three live calls on
	// 2026-10-22 and TestLiveDoubaoWaitLine, where a result with no line
	// before it was answered 1 of 1 and after a line 0 of 3, whether the line
	// went out as SayHello or ChatTTSText and whether a turn was then forced
	// with an audio commit. Unlike the traits above it is read by the
	// orchestrator, not by a client, so it is set on doubao's profile.
	DropsToolResultAfterLine bool
}

// OpenAIProfile is the provider used outside mainland China.
//
// Both companding laws were verified accepted and stored in each direction,
// which makes this leg a pure byte passthrough: no decode, no resample, no law
// conversion anywhere between the caller and the model.
func OpenAIProfile() Profile {
	return Profile{
		Name:      "openai",
		Endpoint:  "wss://api.openai.com/v1/realtime",
		Model:     "gpt-realtime-2.1",
		APIKeyEnv: "OPENAI_API_KEY",
		Style:     styleGA,
		Voice:     "marin",
		// Without this the caller is not transcribed during the bot phase at
		// all: this vendor does not transcribe input unless asked, so the
		// "transcript" of a bot call was the bot's own words and its tool
		// traces, with nothing the caller said in it. The model name and the
		// field it goes in are both measured (design 08 §Appendix, the
		// transcription session's audio.input.transcription.model).
		TranscribeModel:       "gpt-live-transcribe",
		AcceptsG711:           true,
		LinearInput:           media.PCM16Format(media.RateProviderOut),
		LinearOutput:          media.PCM16Format(media.RateProviderOut),
		CancelsResponseItself: true,
		// Documented: the GA response object takes tool_choice ("none",
		// "auto", "required" or a forced function), read 2026-10-22. Not
		// measured live here.
		AcceptsResponseToolChoice: true,
	}
}

// QwenProfile is the provider used inside mainland China.
//
// Its audio-format fields are never echoed back, so acceptance of anything
// other than the documented rates cannot be confirmed from the handshake. The
// documented 16 kHz in / 24 kHz out is therefore what this path uses, and the
// conversion happens on our side.
func QwenProfile() Profile {
	return Profile{
		Name:     "qwen",
		Endpoint: "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
		// Flash since 2026-11-03 (#463, qwen-findings "Model:
		// qwen-audio-3.1-realtime-flash"); it was plus before (qwen-findings
		// §2026-09-24, which found no flash then). Plus stays selectable with
		// AICC_PROVIDER_MODEL=qwen-audio-3.1-realtime-plus.
		Model:     "qwen-audio-3.1-realtime-flash",
		APIKeyEnv: "ALIYUN_API_KEY",
		Style:     styleBeta,
		// The vendor's default voice for this model, by owner decision
		// (2026-09-24).
		Voice: "longanqian_v3.1",
		// session.temperature, documented range [0, 2); the default for this
		// model is undocumented.
		AcceptsTemperature: true,
		// No TranscribeModel, and that is the finding rather than an omission.
		// Design 08 §4.2 left it open — "Qwen-Audio-Realtime's default
		// behaviour for input transcription is unverified" — and live calls on
		// 2026-08-23 settled it: this dialect sends
		// conversation.item.input_audio_transcription.* unprompted, and the
		// caller's words reach the transcript as CUSTOMER|MODEL rows, while
		// the Beta branch of buildSessionUpdate sends no transcription field
		// at all. Setting one here would be a value nothing reads — the same
		// dead configuration this campaign has been filing against.
		AcceptsG711:  false,
		LinearInput:  media.PCM16Format(media.RateProviderIn),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
		// This provider expects the client to cancel the response itself when
		// the caller starts speaking.
		CancelsResponseItself: false,
		// Verified live: asking for a turn on an empty conversation is
		// rejected with "conversation has no messages or no user message".
		NeedsCueForFirstTurn: true,
		// A closing line asked for with a per-response override alone lost to
		// the conversation already there (W-Q1, and a dead-air goodbye on live
		// call 01a0cbb9-…); the opening turn, told both ways, did not.
		NeedsDirectedLineInConversation: true,
		// Undocumented (the vendor's response.create lists no fields at all,
		// the instructions override included), and measured 2026-10-22
		// (qwen-findings W-Q8): tool_choice "none" and an empty tools list on
		// the response are both accepted without an error and both ignored;
		// the line's turn called the tool anyway.
		AcceptsResponseToolChoice: false,
		// Measured 2026-10-22 (qwen-findings W-Q8): the plain SayExactly line
		// over a running tool was silent 3 of 3, this one said 6 of 6.
		NeedsToolFreeLineDirection: true,
	}
}

// GatewayProfile is the Realtime gateway: a separate service that composes ASR,
// an LLM and TTS behind this same protocol, so an engine that never spoke
// Realtime can answer a call without this repository learning how it works
// (phase1-decisions A6, docs/provider-extension.md §"Attaching something that
// is not a vendor"). It impersonates no vendor — it answers under its own name.
//
// From here it is indistinguishable from a vendor, which is the point: it
// differs from the two above in values, not in code.
func GatewayProfile() Profile {
	return Profile{
		Name: "gateway",
		// The gateway's own default listen address, co-located with this
		// process. It serves plain ws:// only — the official SDKs demand
		// wss://, our client does not — and a deployment that moves it says so
		// with AICC_PROVIDER_ENDPOINT.
		Endpoint:  "ws://127.0.0.1:8080/v1/realtime",
		Model:     "cascade",
		APIKeyEnv: "REALTIME_API_KEY",
		Style:     styleGA,
		// The voice belongs to whatever engine the gateway drives, so the
		// deployment's own flows name it (global.voice) and there is no
		// vendor default that would be right here.
		Voice: "",
		// Telephone audio is refused outright: this endpoint takes linear PCM
		// at 24 kHz in both directions and nothing else, so both directions
		// resample (8 kHz is a factor of three away, which media.Converter
		// handles without an arbitrary-ratio resampler).
		AcceptsG711:  false,
		LinearInput:  media.PCM16Format(media.RateProviderOut),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
		// No TranscribeModel, and like Qwen's that is the finding rather than
		// an omission: the gateway accepts audio.input.transcription but its
		// model and language only echo. What actually recognises the caller is
		// configured on the gateway's own profile, so a value here would be
		// one nothing reads.
		TranscribeModel: "",
		// Its turn detection cancels the response when it hears the caller
		// (turn_detection.interrupt_response, on by default), so saying so
		// again would be noise.
		CancelsResponseItself: true,
		// No cue, and that is measured rather than assumed: asking for a turn
		// on an empty conversation was verified against a live instance on
		// 2026-09-02 and answered with a spoken greeting. The composed engine
		// is sent instructions with no messages, which the vendor behind it
		// accepts — the refusal that forces a cue on Qwen's own realtime
		// dialect does not exist here.
		NeedsCueForFirstTurn: false,
		// Its response.create rejects response.tool_choice and response.tools
		// as "Unsupported parameter: ... Cascade does not implement it."
		// (cascade-realtime-gateway, internal/protocol/decode.go,
		// decodeResponseCreate). Sending the bar would cost every line.
		AcceptsResponseToolChoice: false,
	}
}

// DoubaoProfile is the one provider here that is not reached over the Realtime
// protocol at all.
//
// It is therefore the one profile whose values do not all mean what they mean
// above: everything the Realtime client alone reads — Style,
// TranscribeModel, CancelsResponseItself, NeedsCueForFirstTurn,
// NeedsDirectedLineInConversation, AcceptsResponseToolChoice,
// NeedsToolFreeLineDirection — is left at zero, because the client that answers for this name
// (internal/provider/doubao) reads none of them. What it does read is the name,
// the endpoint, the credential, the voice and the two audio
// formats.
// Model is informational for the same reason: that protocol's version is a
// constant inside its client and AICC_PROVIDER_MODEL cannot move it.
//
// Two capabilities had to be said out loud, because the orchestrator reads
// them, not the client.
//
// RequiresTerminalAnnounce: this engine answers audio and nothing else — no
// text this client sends makes it take a turn — so a phase the call never
// leaves has to carry its own words or the caller hears silence, and a flow
// without them is refused at publish. The words a phase does carry are given
// to the engine as text to synthesise, not as a direction to a model, so they
// keep their own SpeakText.
//
// DropsToolResultAfterLine: a line committed while a tool call is owed starts
// a new round, and the call's result is dropped (doubao-findings W-D10), so a
// slow tool gets no wait line.
func DoubaoProfile() Profile {
	return Profile{
		Name:     NameDoubao,
		Endpoint: "wss://openspeech.bytedance.com/api/v3/duplex/realtime/dialogue",
		// Pinned by the client, kept here so that a deployment reading this
		// profile can see which protocol version it is talking to.
		Model:     "1.2.6.1",
		APIKeyEnv: "DOUBAO_API_KEY",
		// One of the vendor's own voice names. A flow that wants another says
		// so in global.voice, and the names are this vendor's alone.
		Voice: "zh_female_vv_jupiter_bigtts",
		// Telephone audio is refused: this endpoint takes linear 16-bit PCM at
		// 16 kHz up and returns it at 24 kHz, both fixed rather than
		// negotiated, so both directions convert.
		AcceptsG711:  false,
		LinearInput:  media.PCM16Format(media.RateProviderIn),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
		// SpeakText commits the line as text the engine synthesises
		// (speech_text_buffer.commit), so it is said as written; a tool result
		// keeps the phase's instruction and the line keeps SpeakText.
		RequiresTerminalAnnounce: true,
		DropsToolResultAfterLine: true,
		// No TranscribeModel, and as on qwen that is the finding rather than an
		// omission: this engine transcribes the caller unprompted, and its
		// session payload has no field to ask for it in.
		TranscribeModel: "",
	}
}

// GrokProfile is xAI's Grok Voice, reached over the OpenAI Realtime protocol in
// its GA shape: a profile on the one Realtime client, never a client of its own
// (phase1-decisions A6). What was measured on the wire is in
// docs/design/grok-findings.md.
//
// Telephone audio passes through untouched ("audio/pcmu" / "audio/pcma", 8 kHz,
// both ways). One ordering rule is new: audio sent before the session
// configuration is acknowledged leaves the session deaf, with no error. The
// client's Start already returns only after session.updated, and aicall starts
// pumping caller audio after Start returns.
func GrokProfile() Profile {
	return Profile{
		Name:      NameGrok,
		Endpoint:  "wss://api.x.ai/v1/realtime",
		Model:     "grok-voice-3.0",
		APIKeyEnv: "XAI_API_KEY",
		Style:     styleGA,
		// One of xAI's voices (eve, ara, rex, sal, leo); the one the live
		// measurement ran with. A flow's global.voice still wins.
		Voice:        "eve",
		AcceptsG711:  true,
		LinearInput:  media.PCM16Format(media.RateProviderOut),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
		// No TranscribeModel: this engine transcribes the caller in every
		// session even when unasked (grok-findings 4, probe 7), and 5.4 measured
		// both modes exact on 9 of 9 clips with one event per utterance by
		// default. "grok-transcribe" adds only partials during speech.
		TranscribeModel: "",
		// Measured (grok-findings 5.1, 3 of 3): the server ends its own
		// response immediately before speech_started, so no cancel is sent on
		// speech (it would draw "no active response found" every time).
		CancelsResponseItself: true,
		// Measured 0 of 5 (grok-findings 5.2): a per-response override alone is
		// silently dropped once the conversation has a turn. Every mid-call
		// line therefore travels as a caller item first, then the override.
		IgnoresOverrideOnceTalking: true,
		// Measured: response.create on an empty conversation greets from the
		// instructions (grok-findings §4, probe 3).
		NeedsCueForFirstTurn: false,
		// Measured (grok-findings 5.2): the engine takes text cues, and a tool
		// result carrying the line as its hint was verbatim 3 of 3.
		RequiresTerminalAnnounce: false,
		// Measured (grok-findings 5.3): any number is accepted and echoed, but
		// no effect on output was seen at 3 samples per value; off until one is.
		AcceptsTemperature: false,
		// Undocumented: xAI's response.create documents the instructions
		// override alone. Not measured, so not sent; a line's tool call is
		// still never handed out (onLineCall).
		AcceptsResponseToolChoice: false,
	}
}

// Provider names this build can run. A deployment runs exactly one of them,
// chosen at startup: qwen or doubao inside mainland China, openai or grok
// elsewhere, gateway for a self-hosted service behind the Realtime protocol.
const (
	NameOpenAI = "openai"
	NameQwen   = "qwen"
	// NameGrok is xAI's Grok Voice, a dialect of the Realtime protocol.
	NameGrok = "grok"
	// NameGateway is not a vendor but a service of our own composing one
	// behind this protocol; it is chosen the same way for the same reason.
	NameGateway = "gateway"
	// NameDoubao is the one name here that selects a different client as well
	// as a different profile, because it is a different wire protocol. The
	// choice is made in the composition root, not here: a client in a
	// sub-package of this one cannot be built from inside it.
	NameDoubao = "doubao"
)

// Override replaces where the deployment's provider is reached and which model
// answers there.
//
// The vendor's own address is a default, not a fact: a deployment may sit
// behind a proxy, in a region with its own host, or in front of a Realtime
// gateway that composes its own pipeline behind the same protocol. An empty
// field keeps the profile's own value.
type Override struct {
	Endpoint string
	Model    string
	// TranscribeModel replaces the profile's own, and TranscribeOff turns
	// caller transcription off outright.
	//
	// Two fields rather than one because an empty environment value means
	// unset, so a profile default that is not empty cannot be blanked by
	// leaving the variable empty. Off has to be something somebody can say.
	TranscribeModel string
	TranscribeOff   bool
}

// ProfileFor returns the profile of the provider this deployment runs, with
// its connection details applied.
//
// Which provider answers is a property of the deployment, not of the call: it
// is resolved once at startup and every conversation uses it. A call's
// language chooses what the model is told to speak, never who it speaks to
// (phase1-decisions A1).
func ProfileFor(name string, override Override) (Profile, error) {
	var profile Profile
	switch strings.ToLower(strings.TrimSpace(name)) {
	case NameOpenAI:
		profile = OpenAIProfile()
	case NameQwen:
		profile = QwenProfile()
	case NameGrok:
		profile = GrokProfile()
	case NameGateway:
		profile = GatewayProfile()
	case NameDoubao:
		profile = DoubaoProfile()
	default:
		return Profile{}, fmt.Errorf("provider: unknown provider %q (%s, %s, %s, %s, %s)",
			name, NameOpenAI, NameGrok, NameGateway, NameQwen, NameDoubao)
	}
	if override.Endpoint != "" {
		profile.Endpoint = override.Endpoint
	}
	if override.Model != "" {
		profile.Model = override.Model
	}
	switch {
	case override.TranscribeOff:
		profile.TranscribeModel = ""
	case override.TranscribeModel != "":
		profile.TranscribeModel = override.TranscribeModel
	}
	return profile, nil
}

// FormatsFor decides what audio this profile will exchange for a call whose
// telephone leg uses the given law.
//
// Passing G.711 straight through is preferred wherever the provider takes it:
// it removes conversion from the hot path entirely and, with it, every
// artefact conversion could introduce.
func (p Profile) FormatsFor(law media.Law) (input, output media.AudioFormat) {
	if p.AcceptsG711 {
		return media.G711Format(law), media.G711Format(law)
	}
	return p.LinearInput, p.LinearOutput
}

// endpointURL is the connection address with the model selected.
func (p Profile) endpointURL() string {
	separator := "?"
	if strings.Contains(p.Endpoint, "?") {
		separator = "&"
	}
	return p.Endpoint + separator + "model=" + p.Model
}

// formatName is how this dialect names an audio format on the wire.
func (p Profile) formatName(format media.AudioFormat) (name string, rateHz int) {
	if p.Style == styleGA {
		switch format.Encoding {
		case media.EncodingMuLaw:
			return "audio/pcmu", 0
		case media.EncodingALaw:
			return "audio/pcma", 0
		default:
			return "audio/pcm", format.RateHz
		}
	}
	// The older dialect names linear audio only, at the vendor's fixed rate.
	return "pcm", 0
}
