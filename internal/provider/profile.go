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
		SemanticTurnType:      "semantic_vad",
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
		Name:      "qwen",
		Endpoint:  "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
		Model:     "qwen-audio-3.0-realtime-plus",
		APIKeyEnv: "ALIYUN_API_KEY",
		Style:     styleBeta,
		Voice:     "longanqian",
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
		SemanticTurnType:     "smart_turn",
		// Selecting semantic turns here rewrites the silence hold to two
		// seconds and ignores any attempt to lower it, which is why that mode
		// is opt-in rather than the default.
		SemanticTurnSilenceMs: 2000,
	}
}

// Provider names this build can run. A deployment runs exactly one of them,
// chosen at startup: Qwen inside mainland China, OpenAI elsewhere.
const (
	NameOpenAI = "openai"
	NameQwen   = "qwen"
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
	default:
		return Profile{}, fmt.Errorf("provider: unknown provider %q (%s, %s)",
			name, NameOpenAI, NameQwen)
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
