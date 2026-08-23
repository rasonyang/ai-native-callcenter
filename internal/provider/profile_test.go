// SPDX-License-Identifier: Apache-2.0

package provider

import "testing"

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
