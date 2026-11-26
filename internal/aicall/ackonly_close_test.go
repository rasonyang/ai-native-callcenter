// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// #635, option 3. A callee whose every turn is an acknowledgement said over
// the bot once held the call open until three silences or the duration cap:
// each 「好的。」 cut the bot, was held, and counted for nothing. Now every
// held acknowledgement counts toward closingTarget, and the run of them ends
// the call at defaultAckOnlyLimit (3), as a run of silence would.
func TestARunOfHeldAcknowledgementsClosesTheCall(t *testing.T) {
	var model *holdModel
	h := newClaimsHarnessModel(t, provider.OpenAIProfile(), quietFlow, "en", "", quietConfig(),
		func(m *fakeModel) provider.VoiceSession {
			model = &holdModel{fakeModel: m}
			return model
		}, nil)

	// The bot is always mid-line: its audio stays queued so every "mm-hmm"
	// cuts it and is held out of the transcript.
	h.leg.holdFrames(1 << 20)
	for i := range 3 {
		item := fmt.Sprintf("u%d", i)
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*50)}
		model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item}
		model.events <- provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: item}
		model.events <- provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: "mm-hmm", IsFinal: true}
		waitFor(t, fmt.Sprintf("acknowledgement %d to be judged", i+1), func() bool {
			return len(model.answers())+len(model.declines()) == i+1
		})
	}

	// The engine moves to the closing target inside drive; read it once drive
	// has returned (the engine is not safe to read while it runs).
	h.session.Close(t.Context())
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("drive did not return")
	}
	if node := h.runtime.Engine().NodeID(); node != "bye" {
		t.Fatalf("three held acknowledgements left the call in %q, want the closing target", node)
	}
	count, _ := h.runtime.Engine().Slot("ackOnly.count")
	if count != 3 {
		t.Errorf("ackOnly.count = %v, want the three held acknowledgements counted", count)
	}
}

// #635, option 1's other half: 「好的。」 said as the answer to the bot's
// question — after the bot's turn has played in full — is the caller's answer,
// and the agreement tool on it runs. The refusal text the model read on the
// cut is what once discounted it (TestTheAcknowledgementRefusalNamesNoAnswer).
func TestAPlainAcknowledgementAnswerRunsTheTool(t *testing.T) {
	h := newClaimsHarnessModel(t, provider.QwenProfile(), quietFlow, "zh", "", quietConfig(), nil, nil)

	// Nothing of the bot's is queued: the final is the caller's turn, not a
	// cut held out of the transcript.
	item := "a1"
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item}
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: item}
	h.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: "好的。", IsFinal: true}

	// A probe tool call whose answer waits for every earlier event to be
	// handled: the utterance has reached the flow by the time it returns.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "fc_probe",
		ToolName: "note", ToolArgs: `{"value":"probe"}`}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	waitFor(t, "the caller's words to reach the flow", func() bool {
		return len(h.model.recordedToolResults()) == 1
	})

	// The model judges 「好的。」 the answer to its question and calls the
	// agreement tool on it: it runs.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "fc_1",
		ToolName: "classify", ToolArgs: `{"value":"agree"}`}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	waitFor(t, "the agreement tool to be answered", func() bool {
		return len(h.model.recordedToolResults()) == 2
	})
	results := h.model.recordedToolResults()
	if got := results[len(results)-1].output; !strings.Contains(got, `"ok":"1"`) {
		t.Fatalf("the agreement tool's result = %s, want it run", got)
	}

	h.session.Close(t.Context())
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("drive did not return")
	}
	if node := h.runtime.Engine().NodeID(); node != "agreed" {
		t.Errorf("the call is in %q, want the agreement tool's target", node)
	}
}
