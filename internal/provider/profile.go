// SPDX-License-Identifier: Apache-2.0

package provider

import (
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
	// TranscribeModel enables transcription of caller audio. Empty leaves it
	// to the provider.
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

// OpenAIProfile is the English-language provider.
//
// Both companding laws were verified accepted and stored in each direction,
// which makes this leg a pure byte passthrough: no decode, no resample, no law
// conversion anywhere between the caller and the model.
func OpenAIProfile() Profile {
	return Profile{
		Name:                  "openai",
		Endpoint:              "wss://api.openai.com/v1/realtime",
		Model:                 "gpt-realtime-2.1",
		APIKeyEnv:             "OPENAI_API_KEY",
		Style:                 styleGA,
		Voice:                 "marin",
		AcceptsG711:           true,
		LinearInput:           media.PCM16Format(media.RateProviderOut),
		LinearOutput:          media.PCM16Format(media.RateProviderOut),
		CancelsResponseItself: true,
		SemanticTurnType:      "semantic_vad",
	}
}

// QwenProfile is the Chinese-language provider.
//
// Its audio-format fields are never echoed back, so acceptance of anything
// other than the documented rates cannot be confirmed from the handshake. The
// documented 16 kHz in / 24 kHz out is therefore what this path uses, and the
// conversion happens on our side.
func QwenProfile() Profile {
	return Profile{
		Name:         "qwen",
		Endpoint:     "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
		Model:        "qwen-audio-3.0-realtime-plus",
		APIKeyEnv:    "ALIYUN_API_KEY",
		Style:        styleBeta,
		Voice:        "longanqian",
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

// Override replaces where a provider is reached and which model answers.
//
// The vendor's own address is a default, not a fact: a deployment may sit
// behind a proxy, in a region with its own host, or in front of a Realtime
// gateway that composes its own pipeline behind the same protocol — and none
// of those can be reached without saying so. This is the whole extension
// mechanism: a new engine is a new endpoint, never a new client. An empty
// field keeps the profile's own value.
type Override struct {
	Endpoint string
	Model    string
}

// ProfileForLanguage picks the provider that speaks a language best and
// applies the deployment's override for it, keyed by provider name.
func ProfileForLanguage(language string, overrides map[string]Override) Profile {
	profile := OpenAIProfile()
	if strings.HasPrefix(strings.ToLower(language), "zh") {
		profile = QwenProfile()
	}
	if override, ok := overrides[profile.Name]; ok {
		if override.Endpoint != "" {
			profile.Endpoint = override.Endpoint
		}
		if override.Model != "" {
			profile.Model = override.Model
		}
	}
	return profile
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
