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
	for _, name := range []string{NameOpenAI, NameQwen, NameGateway, NameDoubao, NameGemini} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not mention %q: %v", name, err)
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
		{NameGemini, "GEMINI_API_KEY",
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
// docs/provider-extension.md). Only doubao makes a flow's terminal phases carry
// their own words: nothing its client sends makes that engine speak, and
// turning the rule on elsewhere would refuse flows that run perfectly well at
// publish. Only the Realtime profiles answer a tool result that ends the call
// with the closing line itself (W-Q1, A5c): off for a Realtime profile puts the
// line back in a turn of its own, which qwen measured at 0 of 7; on for doubao
// trades an exact line for a best-effort one; on for gemini before W-G6 is fixed
// lets an unanswered tool result end the call without the line.
func TestHowEachProfileDeliversAClosingLine(t *testing.T) {
	for _, tt := range []struct {
		profile                            Profile
		requiresAnnounce, lineInToolResult bool
	}{
		{OpenAIProfile(), false, true},
		{QwenProfile(), false, true},
		{GatewayProfile(), false, true},
		{DoubaoProfile(), true, false},
		{GeminiProfile(), false, false},
	} {
		if got := tt.profile.RequiresTerminalAnnounce; got != tt.requiresAnnounce {
			t.Errorf("%s: RequiresTerminalAnnounce = %v, want %v",
				tt.profile.Name, got, tt.requiresAnnounce)
		}
		if got := tt.profile.PutsTerminalAnnounceInToolResult; got != tt.lineInToolResult {
			t.Errorf("%s: PutsTerminalAnnounceInToolResult = %v, want %v",
				tt.profile.Name, got, tt.lineInToolResult)
		}
	}
}
