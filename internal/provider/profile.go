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
	// Headers are sent with the upgrade request, beyond authorization.
	Headers map[string]string

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

	// RequiresTerminalAnnounce means no text this client sends will make the
	// engine take a turn, so the words of a phase the call does not leave have
	// to come from the flow itself.
	//
	// It is stronger than NeedsCueForFirstTurn and not the same thing: that one
	// says a cue is needed to START a conversation, and a cue is something this
	// client can invent. This one says there is no cue at all — the engine
	// speaks when it is given words, and a terminal phase with none is a caller
	// listening to silence.
	//
	// True on one profile here, doubao: every other engine this build speaks to
	// takes a text cue. A flow is refused at publish rather than at load
	// because of exactly that, the rule belongs to the deployment and not to
	// the dialect (flow.RequireTerminalAnnounce).
	RequiresTerminalAnnounce bool

	// PutsTerminalAnnounceInToolResult means a tool result that moves the call
	// into a terminal phase with a line of its own carries that line as its
	// hint (SayExactly), and the turn the result produces is the line: no
	// SpeakText follows. Where it is false the result carries the new phase's
	// instruction and SpeakText says the line in a turn of its own, as every
	// other move does.
	//
	// True on the Realtime profiles (openai, qwen, gateway). There asking for
	// the line in a turn of its own on top of the result measured 0 of 7 on
	// qwen, because the model follows the tool result it has just read over a
	// per-response override (docs/design/qwen-findings.md, W-Q1).
	//
	// False on doubao, whose SpeakText commits text the engine synthesises and
	// is exact by construction; a direction to a model is not. False on gemini
	// for now, for a different reason: that model sometimes answers a tool
	// result with nothing (gemini-findings W-G6), and the SpeakText that
	// follows the result is what rescues the line. With the line in the result
	// instead, such a turn would complete without audio and the armed ending
	// would release the caller without it. Revisit when W-G6 is fixed.
	//
	// Not RequiresTerminalAnnounce: that one says whether a flow may leave a
	// terminal phase wordless, this one says how the words it did write reach
	// the caller after a tool call.
	PutsTerminalAnnounceInToolResult bool

	// SemanticTurnType is this vendor's name for semantic turn detection.
	SemanticTurnType string
	// SemanticTurnSilenceMs is the hold the vendor forces in that mode,
	// overriding any configured value. Zero means it honours the request.
	SemanticTurnSilenceMs int
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
		// A line is a direction to the model, never audio handed over, and
		// best said in the turn the tool result produces (W-Q1).
		PutsTerminalAnnounceInToolResult: true,
		SemanticTurnType:                 "semantic_vad",
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
		// Probed against this profile's session in qwen-findings §2026-09-24.
		// There is no flash variant.
		Model:     "qwen-audio-3.1-realtime-plus",
		APIKeyEnv: "ALIYUN_API_KEY",
		Style:     styleBeta,
		// The vendor's default voice for this model, by owner decision
		// (2026-09-24).
		Voice: "longanqian_v3.1",
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
		// A line asked for in a turn of its own after a tool result lost to
		// the result 0 of 7 times; carried in the result, 7 of 7 (W-Q1).
		PutsTerminalAnnounceInToolResult: true,
		SemanticTurnType:                 "smart_turn",
		// Selecting semantic turns here rewrites the silence hold to two
		// seconds and ignores any attempt to lower it, which is why that mode
		// is opt-in rather than the default.
		SemanticTurnSilenceMs: 2000,
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
		// A line is a direction to whatever the gateway composes, never text
		// this client can make it synthesise — the same wire as openai.
		PutsTerminalAnnounceInToolResult: true,
		SemanticTurnType:                 "semantic_vad",
	}
}

// DoubaoProfile is the one provider here that is not reached over the Realtime
// protocol at all.
//
// It is therefore the one profile whose values do not all mean what they mean
// above: everything the Realtime client alone reads — Style,
// TranscribeModel, CancelsResponseItself, NeedsCueForFirstTurn,
// NeedsDirectedLineInConversation and the two semantic-turn fields — is left
// at zero, because the client that answers for this name
// (internal/provider/doubao) reads none of them. What it does read is the name,
// the endpoint, the credential, any extra Headers, the voice and the two audio
// formats.
// Model is informational for the same reason: that protocol's version is a
// constant inside its client and AICC_PROVIDER_MODEL cannot move it.
//
// RequiresTerminalAnnounce is the one capability that had to be said out loud.
// This engine answers audio and nothing else — no text this client sends makes
// it take a turn — so a phase the call never leaves has to carry its own words
// or the caller hears silence, and a flow without them is refused at publish.
// PutsTerminalAnnounceInToolResult is false for the related reason: the words a
// phase does carry are given to the engine as text to synthesise, not as a
// direction to a model, so they keep their own SpeakText.
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
		AcceptsG711:              false,
		LinearInput:              media.PCM16Format(media.RateProviderIn),
		LinearOutput:             media.PCM16Format(media.RateProviderOut),
		RequiresTerminalAnnounce: true,
		// SpeakText commits the line as text the engine synthesises
		// (speech_text_buffer.commit), so it is said as written; a tool result
		// keeps the phase's instruction and the line keeps SpeakText.
		PutsTerminalAnnounceInToolResult: false,
		// No TranscribeModel, and as on qwen that is the finding rather than an
		// omission: this engine transcribes the caller unprompted, and its
		// session payload has no field to ask for it in.
		TranscribeModel: "",
	}
}

// GeminiProfile is the second provider here that is not the Realtime protocol,
// and the third client this build can put on a call.
//
// It leaves the same fields at zero as the doubao profile does, for the same
// reason: Style, TranscribeModel, CancelsResponseItself,
// NeedsCueForFirstTurn, NeedsDirectedLineInConversation and the two
// semantic-turn fields are read by the Realtime client alone, and the client
// that answers for this name (internal/provider/gemini) reads none of them.
// What it reads is the name, the endpoint, the credential, any extra Headers,
// the voice and the two audio formats. Model is
// informational: the model name is a constant inside that client — its
// lifecycle is what the client knows how to hold a conversation with — so
// AICC_PROVIDER_MODEL is ignored here and the value is kept only so a
// deployment reading this profile can see which model it is talking to.
//
// RequiresTerminalAnnounce is false, which is the difference from doubao and
// not an oversight: this engine does take words from the client, so a closing
// line reaches the caller the way it does on openai and qwen — by instruction,
// best effort, rather than as audio we have synthesised.
// PutsTerminalAnnounceInToolResult is nevertheless false: this model sometimes
// answers a tool result with nothing (gemini-findings W-G6), and until that is
// fixed the SpeakText that follows the result is what makes the line happen.
func GeminiProfile() Profile {
	return Profile{
		Name: NameGemini,
		// The version is part of the path. A deployment behind a proxy or on a
		// regional host says so with AICC_PROVIDER_ENDPOINT, as everywhere else.
		Endpoint: "wss://generativelanguage.googleapis.com/ws/" +
			"google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
		// Pinned by the client, kept here so that a deployment reading this
		// profile can see which model it is talking to.
		Model:     "gemini-3.8-live",
		APIKeyEnv: "GEMINI_API_KEY",
		// One of the vendor's own prebuilt voice names. A flow that wants
		// another says so in global.voice, and the names are this vendor's
		// alone.
		Voice: "Kore",
		// Telephone audio is refused: this endpoint takes linear 16-bit PCM at
		// 16 kHz up and returns it at 24 kHz, both fixed rather than
		// negotiated, so both directions convert.
		AcceptsG711:              false,
		LinearInput:              media.PCM16Format(media.RateProviderIn),
		LinearOutput:             media.PCM16Format(media.RateProviderOut),
		RequiresTerminalAnnounce: false,
		// Held back until W-G6 is fixed: a tool result the model leaves
		// unanswered would end the call without the line, where SpeakText
		// after the result still says it.
		PutsTerminalAnnounceInToolResult: false,
		// No TranscribeModel, and as on qwen that is the finding rather than an
		// omission — with one honest limit on it. What was measured is that the
		// transcript of the BOT's own audio arrives whether or not the setup asks
		// for it. The caller's transcript has been asked for in every session
		// this client has opened, so whether it too would arrive unrequested is
		// not known. Either way there is nothing for a deployment to set: the
		// client sends both transcription configs itself, and this field is read
		// by nothing.
		TranscribeModel: "",
	}
}

// Provider names this build can run. A deployment runs exactly one of them,
// chosen at startup: qwen or doubao inside mainland China, openai or gemini
// elsewhere, gateway for a self-hosted service behind the Realtime protocol.
const (
	NameOpenAI = "openai"
	NameQwen   = "qwen"
	// NameGateway is not a vendor but a service of our own composing one
	// behind this protocol; it is chosen the same way for the same reason.
	NameGateway = "gateway"
	// NameDoubao is the one name here that selects a different client as well
	// as a different profile, because it is a different wire protocol. The
	// choice is made in the composition root, not here: a client in a
	// sub-package of this one cannot be built from inside it.
	NameDoubao = "doubao"
	// NameGemini is the second such name, selecting the third client for the
	// third wire protocol, and chosen in the same place for the same reason.
	NameGemini = "gemini"
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
	case NameGateway:
		profile = GatewayProfile()
	case NameDoubao:
		profile = DoubaoProfile()
	case NameGemini:
		profile = GeminiProfile()
	default:
		return Profile{}, fmt.Errorf("provider: unknown provider %q (%s, %s, %s, %s, %s)",
			name, NameOpenAI, NameQwen, NameGateway, NameDoubao, NameGemini)
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
