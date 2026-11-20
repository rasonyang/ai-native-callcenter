// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// "Off" has to be sayable. An empty environment value means unset everywhere
// in this configuration, so it can never blank a default that is not empty.
func TestCallerTranscriptionCanBeOverriddenAndTurnedOff(t *testing.T) {
	stock := OpenAIProfile().TranscribeModel

	kept, err := ProfileFor(NameOpenAI, Override{})
	if err != nil || kept.TranscribeModel != stock {
		t.Errorf("TranscribeModel = %q with no override, want the profile's %q",
			kept.TranscribeModel, stock)
	}

	replaced, err := ProfileFor(NameOpenAI, Override{TranscribeModel: "another-model"})
	if err != nil || replaced.TranscribeModel != "another-model" {
		t.Errorf("TranscribeModel = %q, want the override", replaced.TranscribeModel)
	}

	off, err := ProfileFor(NameOpenAI, Override{TranscribeOff: true})
	if err != nil || off.TranscribeModel != "" {
		t.Errorf("TranscribeModel = %q with transcription off, want empty — a "+
			"deployment that says off and gets transcription anyway is worse "+
			"than one that cannot say it", off.TranscribeModel)
	}

	// Off outranks a model named alongside it: saying both is a contradiction,
	// and the safe reading of a contradiction is the one that sends less.
	both, err := ProfileFor(NameOpenAI,
		Override{TranscribeModel: "another-model", TranscribeOff: true})
	if err != nil || both.TranscribeModel != "" {
		t.Errorf("TranscribeModel = %q, want off to win", both.TranscribeModel)
	}
}

// What actually goes on the wire, for both dialects, with the stock profiles.
// The profile field is only half the answer: the Beta branch of
// buildSessionUpdate never reads it, which is why setting one there would be
// dead configuration rather than a fix.
func TestTheSessionUpdateAsksForCallerTranscriptionOnlyWhereItIsRead(t *testing.T) {
	openai := &Realtime{profile: OpenAIProfile()}
	update := openai.buildSessionUpdate(SessionConfig{}, false)
	audio, _ := update["session"].(map[string]any)["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	transcription, ok := input["transcription"].(map[string]any)
	if !ok {
		t.Fatalf("the openai session.update carries no transcription: %v", input)
	}
	if transcription["model"] != OpenAIProfile().TranscribeModel {
		t.Errorf("transcription model = %v, want %q",
			transcription["model"], OpenAIProfile().TranscribeModel)
	}

	// The one retry after a rejection drops the optional fields, transcription
	// among them. Left as it is deliberately — a session that will not start
	// is worse than one that starts without the caller's words — but it means
	// a rejected session.update silently costs the caller's transcript, and
	// that is worth having written down somewhere other than a comment.
	reduced := openai.buildSessionUpdate(SessionConfig{}, true)
	audio, _ = reduced["session"].(map[string]any)["audio"].(map[string]any)
	input, _ = audio["input"].(map[string]any)
	if _, present := input["transcription"]; present {
		t.Error("the reduced retry still asks for transcription; it exists to drop " +
			"exactly the fields a vendor might have rejected")
	}

	qwen := &Realtime{profile: QwenProfile()}
	beta := qwen.buildSessionUpdate(SessionConfig{}, false)["session"].(map[string]any)
	if _, present := beta["transcription"]; present {
		t.Error("the beta dialect sent a transcription field it does not define")
	}
	if _, present := beta["audio"]; present {
		t.Error("the beta dialect sent the GA audio shape")
	}
}

// The gateway serves the GA shape only, and names its rate on the wire because
// it accepts exactly one. Getting this wrong is a rejected session.update, not
// a degraded call.
func TestTheGatewaySessionUpdateNamesLinearAudioAndItsRate(t *testing.T) {
	profile := GatewayProfile()
	gateway := &Realtime{profile: profile}

	cfg := SessionConfig{}
	cfg.InputFormat, cfg.OutputFormat = profile.FormatsFor(media.LawMu)
	session := gateway.buildSessionUpdate(cfg, false)["session"].(map[string]any)

	if session["type"] != "realtime" {
		t.Errorf("session.type = %v, want realtime — the beta shape is rejected there",
			session["type"])
	}
	audio, _ := session["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	format, _ := input["format"].(map[string]any)
	if format["type"] != "audio/pcm" || format["rate"] != 24000 {
		t.Errorf("input format = %v, want audio/pcm at 24000", format)
	}

	// Asking for transcription it only echoes would be configuration nothing
	// reads — the same dead setting the qwen profile refuses to carry.
	if _, present := input["transcription"]; present {
		t.Error("the gateway session.update asks for a transcription model whose " +
			"value the gateway only echoes; the recogniser is configured there")
	}
}

// An unknown name refuses to start, and the refusal says what the build does
// run. A deployment that silently falls back to another provider is worse than
// one that does not come up.
func TestAnUnknownProviderNamesTheOnesThereAre(t *testing.T) {
	_, err := ProfileFor("cascade", Override{})
	if err == nil {
		t.Fatal("an unknown provider started anyway")
	}
	for _, name := range []string{NameOpenAI, NameGrok, NameQwen, NameGateway, NameDoubao} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not mention %q: %v", name, err)
		}
	}
}

// The removed provider is an unknown name like any other, and the refusal names
// every value that does run. An .env left over from that deployment must stop
// startup with this message.
func TestTheRemovedGeminiProviderIsRefusedWithTheAcceptedValues(t *testing.T) {
	for _, name := range []string{"gemini", " Gemini "} {
		_, err := ProfileFor(name, Override{})
		if err == nil {
			t.Fatalf("%q started anyway", name)
		}
		want := "(openai, grok, gateway, qwen, doubao)"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to list %s", err, want)
		}
	}
}

// The linear-audio profiles are chosen by name, and their rates are what the
// vendor's socket accepts: a profile that claimed G.711 here would put PCMU on
// a socket that takes none, and the call would fail on the first frame rather
// than at startup. Every rate is an integer factor of the telephone rate, which
// is why no arbitrary-ratio resampler exists in this repository.
func TestTheLinearProfilesReachTheCallerBothWays(t *testing.T) {
	for _, tt := range []struct {
		name, apiKeyEnv string
		in, out         media.AudioFormat
	}{
		{NameGateway, "REALTIME_API_KEY", media.PCM16Format(24000), media.PCM16Format(24000)},
		{NameDoubao, "DOUBAO_API_KEY",
			media.PCM16Format(media.RateProviderIn), media.PCM16Format(media.RateProviderOut)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := ProfileFor(tt.name, Override{})
			if err != nil {
				t.Fatalf("%s is not a provider this build can run: %v", tt.name, err)
			}
			if profile.APIKeyEnv != tt.apiKeyEnv {
				t.Errorf("credential = %q, want %s", profile.APIKeyEnv, tt.apiKeyEnv)
			}
			for _, law := range []media.Law{media.LawMu, media.LawAlaw} {
				input, output := profile.FormatsFor(law)
				if input != tt.in || output != tt.out {
					t.Errorf("%s formats = %s / %s, want %s / %s", law, input, output, tt.in, tt.out)
				}
				if _, err := media.NewConverter(media.G711Format(law), input); err != nil {
					t.Errorf("caller audio cannot reach %s on %s: %v", tt.name, law, err)
				}
				if _, err := media.NewConverter(output, media.G711Format(law)); err != nil {
					t.Errorf("%s audio cannot reach the caller on %s: %v", tt.name, law, err)
				}
			}
		})
	}
}

// How each engine delivers a closing line is a profile trait (table in
// docs/provider-extension.md), and it is one capability. Only doubao makes a
// flow's terminal phases carry their own words: nothing its client sends makes
// that engine speak, and turning the rule on elsewhere would refuse flows that
// run perfectly well at publish. The same capability decides how the line
// travels: where it is off (the Realtime profiles) a tool result that ends the
// call carries the closing line itself (W-Q1, A5c) and a turn of its own
// measured 0 of 7 on qwen; on for doubao, the line keeps its own SpeakText.
func TestHowEachProfileDeliversAClosingLine(t *testing.T) {
	for _, tt := range []struct {
		profile          Profile
		requiresAnnounce bool
	}{
		{OpenAIProfile(), false},
		{QwenProfile(), false},
		{GrokProfile(), false},
		{GatewayProfile(), false},
		{DoubaoProfile(), true},
	} {
		if got := tt.profile.RequiresTerminalAnnounce; got != tt.requiresAnnounce {
			t.Errorf("%s: RequiresTerminalAnnounce = %v, want %v",
				tt.profile.Name, got, tt.requiresAnnounce)
		}
	}
}

// Only doubao drops a tool result sent after a line (doubao-findings W-D10), so
// only doubao goes without the wait line. The Realtime profiles answer a result
// after a line (the owed turn, #286) and keep it.
func TestOnlyDoubaoDropsAToolResultSentAfterALine(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile(), DoubaoProfile()} {
		want := profile.Name == NameDoubao
		if got := profile.DropsToolResultAfterLine; got != want {
			t.Errorf("%s: DropsToolResultAfterLine = %v, want %v", profile.Name, got, want)
		}
	}
}

// The flow's temperature reaches only the profiles whose session object has a
// field for it. The GA dialect has none, so sending it there would risk a
// rejected session.update; the retry after a rejection drops it with the other
// optional fields; an unset temperature changes nothing anywhere.
func TestTheSessionUpdateCarriesTheFlowsTemperatureOnlyWhereItIsAccepted(t *testing.T) {
	temperature := 0.3
	cases := []struct {
		name    string
		profile Profile
		isSent  bool
	}{
		{"qwen", QwenProfile(), true},
		{"openai", OpenAIProfile(), false},
		{"grok", GrokProfile(), false},
		{"gateway", GatewayProfile(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &Realtime{profile: tc.profile}
			for _, isReduced := range []bool{false, true} {
				set := client.buildSessionUpdate(SessionConfig{Temperature: &temperature}, isReduced)["session"].(map[string]any)
				got, isPresent := set["temperature"]
				if isWanted := tc.isSent && !isReduced; isPresent != isWanted {
					t.Fatalf("reduced=%v: temperature present = %v, want %v", isReduced, isPresent, isWanted)
				}
				if isPresent && got != temperature {
					t.Errorf("session.temperature = %v, want %v", got, temperature)
				}
				unset := client.buildSessionUpdate(SessionConfig{}, isReduced)["session"].(map[string]any)
				if _, isPresent := unset["temperature"]; isPresent {
					t.Errorf("reduced=%v: an unset temperature was sent", isReduced)
				}
			}
		})
	}
	if tc := QwenProfile(); !tc.AcceptsTemperature || OpenAIProfile().AcceptsTemperature ||
		GatewayProfile().AcceptsTemperature || GrokProfile().AcceptsTemperature || DoubaoProfile().AcceptsTemperature {
		t.Error("AcceptsTemperature is not qwen only")
	}
}

// transcriptionOf returns what a session.update carries for the caller's
// transcription in this profile's dialect, and whether it carries any.
func transcriptionOf(update map[string]any) (map[string]any, bool) {
	session, _ := update["session"].(map[string]any)
	if audio, ok := session["audio"].(map[string]any); ok {
		input, _ := audio["input"].(map[string]any)
		got, isPresent := input["transcription"].(map[string]any)
		return got, isPresent
	}
	got, isPresent := session["input_audio_transcription"].(map[string]any)
	return got, isPresent
}

// #602: qwen names no transcription model (the default varies per session and
// session.created arrives after the opening update), but the call's language
// alone is sent, so a one-syllable reply is not written in another language.
func TestQwenSessionUpdateNamesTheCallsLanguageAndNoModel(t *testing.T) {
	qwen := &Realtime{profile: QwenProfile()}
	for _, tc := range []struct{ language, want string }{
		{"zh-CN", "zh"},
		{"zh", "zh"},
		{"en", "en"},
		{"en_US", "en"},
	} {
		got, isPresent := transcriptionOf(qwen.buildSessionUpdate(SessionConfig{Language: tc.language}, false))
		if !isPresent || len(got) != 1 || got["language"] != tc.want {
			t.Errorf("language %q: input_audio_transcription = %v, want exactly {language: %q}", tc.language, got, tc.want)
		}
	}
	if _, isPresent := transcriptionOf(qwen.buildSessionUpdate(SessionConfig{}, false)); isPresent {
		t.Error("an empty call language still sent a transcription field")
	}
	if _, isPresent := transcriptionOf(qwen.buildSessionUpdate(SessionConfig{Language: "zh-CN"}, true)); isPresent {
		t.Error("the reduced retry still sent the transcription language")
	}

	off := QwenProfile()
	off.AcceptsTranscriptionLanguage = false
	if _, isPresent := transcriptionOf((&Realtime{profile: off}).buildSessionUpdate(SessionConfig{Language: "zh-CN"}, false)); isPresent {
		t.Error("a Beta profile without the trait sent a transcription field")
	}
}

// The trait means the same in the GA dialect: no model named, the language
// alone. With a model named, the existing path is unchanged.
func TestGASessionUpdateTranscriptionLanguageTrait(t *testing.T) {
	profile := GrokProfile()
	if profile.TranscribeModel != "" {
		t.Fatalf("grok names a transcription model: %q", profile.TranscribeModel)
	}
	if _, isPresent := transcriptionOf((&Realtime{profile: profile}).buildSessionUpdate(SessionConfig{Language: "zh-CN"}, false)); isPresent {
		t.Error("a GA profile without the trait and without a model sent a transcription field")
	}
	profile.AcceptsTranscriptionLanguage = true
	client := &Realtime{profile: profile}
	got, isPresent := transcriptionOf(client.buildSessionUpdate(SessionConfig{Language: "zh-CN"}, false))
	if !isPresent || len(got) != 1 || got["language"] != "zh" {
		t.Errorf("audio.input.transcription = %v, want exactly {language: zh}", got)
	}
	if _, isPresent := transcriptionOf(client.buildSessionUpdate(SessionConfig{Language: "zh-CN"}, true)); isPresent {
		t.Error("the reduced GA retry still sent the transcription language")
	}

	named := &Realtime{profile: OpenAIProfile()}
	got, _ = transcriptionOf(named.buildSessionUpdate(SessionConfig{Language: "zh-CN"}, false))
	if got["model"] != OpenAIProfile().TranscribeModel || got["language"] != "zh" {
		t.Errorf("a named model changed: %v", got)
	}

	if !QwenProfile().AcceptsTranscriptionLanguage || OpenAIProfile().AcceptsTranscriptionLanguage ||
		GatewayProfile().AcceptsTranscriptionLanguage || GrokProfile().AcceptsTranscriptionLanguage ||
		DoubaoProfile().AcceptsTranscriptionLanguage {
		t.Error("AcceptsTranscriptionLanguage is not qwen only")
	}
}

// A profile carries a per-minute quota only where a vendor document gives one
// for its default model; another model clears it, and the deployment's own
// setting wins either way, 0 turning a limit off (#604).
func TestTheQuotaLimitsAreTheDefaultModelsAndOverridable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		provider             string
		override             Override
		wantSession, wantTok int
	}{
		{"qwen flash, Beijing, as documented", NameQwen, Override{}, 60, 100000},
		{"qwen named by its own default model", NameQwen, Override{Model: "qwen-audio-3.1-realtime-flash"}, 60, 100000},
		{"qwen on another model knows no limit", NameQwen, Override{Model: "qwen-audio-3.1-realtime-plus"}, 0, 0},
		{"another model with the deployment's own", NameQwen,
			Override{Model: "qwen-audio-3.1-realtime-plus", SessionRPM: 120, IsSessionRPMSet: true}, 120, 0},
		{"a limit turned off", NameQwen, Override{TokenRPM: 0, IsTokenRPMSet: true}, 60, 0},
		{"doubao, per application id", NameDoubao, Override{}, 60, 100000},
		{"doubao's model setting is ignored by its client", NameDoubao, Override{Model: "anything"}, 60, 100000},
		{"openai depends on the account's tier", NameOpenAI, Override{}, 0, 0},
		{"openai with the account's limits", NameOpenAI,
			Override{SessionRPM: 200, IsSessionRPMSet: true, TokenRPM: 40000, IsTokenRPMSet: true}, 200, 40000},
		{"grok", NameGrok, Override{}, 0, 0},
		{"gateway", NameGateway, Override{}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ProfileFor(tt.provider, tt.override)
			if err != nil {
				t.Fatal(err)
			}
			if p.SessionRPM != tt.wantSession || p.TokenRPM != tt.wantTok {
				t.Errorf("limits = %d sessions, %d tokens per minute, want %d and %d",
					p.SessionRPM, p.TokenRPM, tt.wantSession, tt.wantTok)
			}
		})
	}
}
