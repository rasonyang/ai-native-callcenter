// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// Whether the caller's own words appear in the bot phase's transcript is a
// property of the profile, and the two vendors reach the same place by
// opposite routes: one has to be asked, the other does it unprompted. Design
// 08 §4.2 left the second half open; live calls on 2026-08-23 closed it.
func TestWhetherTheCallerIsTranscribedDuringTheBotPhase(t *testing.T) {
	if got := OpenAIProfile().TranscribeModel; got == "" {
		t.Error("the openai profile asks for no input transcription; the bot phase's " +
			"transcript would be the bot's own words and its tool traces, with " +
			"nothing the caller said in it")
	}
	if got := QwenProfile().TranscribeModel; got != "" {
		t.Errorf("the qwen profile carries a transcription model (%q) that its own "+
			"dialect never sends — dead configuration, and the caller is already "+
			"transcribed there unprompted", got)
	}
}

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

// The gateway takes linear PCM at one rate in both directions and refuses
// telephone audio outright, so the conversion it forces has to be real: a
// profile that claimed G.711 here would put PCMU on a socket that rejects it,
// and the call would fail on the first frame rather than at startup.
func TestTheGatewayTakesLinearAudioAtOneRateBothWays(t *testing.T) {
	profile, err := ProfileFor(NameGateway, Override{})
	if err != nil {
		t.Fatalf("gateway is not a provider this build can run: %v", err)
	}

	input, output := profile.FormatsFor(media.LawMu)
	want := media.PCM16Format(24000)
	if input != want || output != want {
		t.Fatalf("formats = %s / %s, want %s both ways", input, output, want)
	}

	// Both directions are an integer factor from the telephone rate, which is
	// the whole reason no arbitrary-ratio resampler exists in this repository.
	if _, err := media.NewConverter(media.G711Format(media.LawMu), input); err != nil {
		t.Errorf("caller audio cannot reach the gateway: %v", err)
	}
	if _, err := media.NewConverter(output, media.G711Format(media.LawMu)); err != nil {
		t.Errorf("gateway audio cannot reach the caller: %v", err)
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
	for _, name := range []string{NameOpenAI, NameQwen, NameGateway, NameDoubao, NameGemini} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not mention %q: %v", name, err)
		}
	}
}

// Every engine reached over the Realtime protocol takes a text cue, so none of
// them makes a flow's terminal phases carry their own words. Said out loud
// because the capability is the reason a publish can be refused, and turning it
// on for a profile that does not need it would refuse flows that run perfectly
// well. Doubao is the exception and has its own test below.
func TestNoRealtimeProfileMakesAFlowWriteItsOwnClosingLines(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GatewayProfile()} {
		if profile.RequiresTerminalAnnounce {
			t.Errorf("%s requires terminal announcements; it is reached through a "+
				"client that can prompt a turn with text", profile.Name)
		}
	}
}

// The doubao profile is chosen by its own name, and what it names is a
// different protocol rather than a different vendor of this one.
//
// Its audio is the reason the values matter: 16 kHz up and 24 kHz down, linear
// both ways, with telephone audio refused outright. A profile that claimed
// G.711 here would put PCMU on a socket that takes none, and the call would
// fail on the first frame instead of at startup.
func TestDoubaoIsChosenByNameAndTakesLinearAudioBothWays(t *testing.T) {
	profile, err := ProfileFor(NameDoubao, Override{})
	if err != nil {
		t.Fatalf("doubao is not a provider this build can run: %v", err)
	}
	if profile.Name != NameDoubao {
		t.Errorf("profile name = %q, want %q", profile.Name, NameDoubao)
	}
	if profile.APIKeyEnv != "DOUBAO_API_KEY" {
		t.Errorf("credential = %q, want DOUBAO_API_KEY", profile.APIKeyEnv)
	}

	for _, law := range []media.Law{media.LawMu, media.LawAlaw} {
		input, output := profile.FormatsFor(law)
		if want := media.PCM16Format(media.RateProviderIn); input != want {
			t.Errorf("%s input format = %s, want %s", law, input, want)
		}
		if want := media.PCM16Format(media.RateProviderOut); output != want {
			t.Errorf("%s output format = %s, want %s", law, output, want)
		}
		if _, err := media.NewConverter(media.G711Format(law), input); err != nil {
			t.Errorf("caller audio cannot reach doubao on %s: %v", law, err)
		}
		if _, err := media.NewConverter(output, media.G711Format(law)); err != nil {
			t.Errorf("doubao audio cannot reach the caller on %s: %v", law, err)
		}
	}
}

// The one capability this profile turns on, and the only one that changes what
// a deployment will accept: a phase the call does not leave has to carry its
// own words, because nothing this client sends will make that engine speak.
func TestDoubaoMakesAFlowWriteItsOwnClosingLines(t *testing.T) {
	if !DoubaoProfile().RequiresTerminalAnnounce {
		t.Error("the doubao profile does not require terminal announcements; a flow " +
			"whose last phase says nothing would publish, and the caller would " +
			"reach it and hear silence")
	}
}

// The gemini profile is chosen by its own name too, and what it names is a
// third protocol rather than a third vendor of the first one.
//
// Its audio is the reason the values matter: 16 kHz up and 24 kHz down, linear
// both ways, with telephone audio refused outright. A profile that claimed
// G.711 here would put PCMU on a socket that takes none, and the call would
// fail on the first frame instead of at startup.
//
// The endpoint override is checked here rather than trusted: the version is
// part of the path on this vendor, so a deployment behind a proxy has to be
// able to replace the whole of it.
func TestGeminiIsChosenByNameAndTakesLinearAudioBothWays(t *testing.T) {
	profile, err := ProfileFor(NameGemini, Override{})
	if err != nil {
		t.Fatalf("gemini is not a provider this build can run: %v", err)
	}
	if profile.Name != NameGemini {
		t.Errorf("profile name = %q, want %q", profile.Name, NameGemini)
	}
	if profile.APIKeyEnv != "GEMINI_API_KEY" {
		t.Errorf("credential = %q, want GEMINI_API_KEY", profile.APIKeyEnv)
	}
	if profile.Voice == "" {
		t.Error("the gemini profile names no voice; a flow that leaves global.voice " +
			"empty would start a session with none")
	}

	for _, law := range []media.Law{media.LawMu, media.LawAlaw} {
		input, output := profile.FormatsFor(law)
		if want := media.PCM16Format(media.RateProviderIn); input != want {
			t.Errorf("%s input format = %s, want %s", law, input, want)
		}
		if want := media.PCM16Format(media.RateProviderOut); output != want {
			t.Errorf("%s output format = %s, want %s", law, output, want)
		}
		if _, err := media.NewConverter(media.G711Format(law), input); err != nil {
			t.Errorf("caller audio cannot reach gemini on %s: %v", law, err)
		}
		if _, err := media.NewConverter(output, media.G711Format(law)); err != nil {
			t.Errorf("gemini audio cannot reach the caller on %s: %v", law, err)
		}
	}

	moved, err := ProfileFor(NameGemini, Override{Endpoint: "wss://proxy.internal/bidi"})
	if err != nil {
		t.Fatalf("ProfileFor: %v", err)
	}
	if moved.Endpoint != "wss://proxy.internal/bidi" {
		t.Errorf("endpoint = %q, want the deployment's own", moved.Endpoint)
	}
}

// Gemini demands nothing of a flow, and that is the difference from the other
// non-Realtime client rather than a field nobody set. This engine speaks the
// words it is given, so a closing line is an instruction like any other and a
// flow whose terminal phases carry none publishes and runs.
func TestGeminiLeavesAFlowsClosingLinesToTheFlow(t *testing.T) {
	if GeminiProfile().RequiresTerminalAnnounce {
		t.Error("the gemini profile requires terminal announcements; flows that run " +
			"perfectly well on it would be refused at publish")
	}
}
