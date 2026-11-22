// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// A callee heard the bot say "工具调用" aloud on a qwen call whose opening was
// cut (#623): a turn whose transcript is only tool plumbing is the model
// leaking its internals and is never something to play. The delta is not
// recorded or emitted, the turn's unplayed audio is cut, and the rest of its
// audio is refused.

// speakAroundText pumps one turn: before frames of audio, then the text
// delta, then after more frames and the turn's end.
func speakAroundText(model *fakeModel, text string, before, after int) {
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	for range before {
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples)}
	}
	model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: text}
	for range after {
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples)}
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

func TestAUtteranceThatIsOnlyToolPlumbingIsNotPlayed(t *testing.T) {
	t.Parallel()
	for _, words := range []string{"lookup_slot", "工具调用"} {
		t.Run(words, func(t *testing.T) {
			session, leg, model := startBridge(t, provider.OpenAIProfile())
			session.cfg.Session.Tools = []provider.ToolSpec{{Name: "lookup_slot"}}
			awaitBridgeEvent(t, session, EventTypeReady)

			speakAroundText(model, words, 2, 3)
			awaitBridgeEvent(t, session, EventTypeTurnDone)
			awaitBridgeEvent(t, session, EventTypePlaybackDone)

			// The fake leg plays out instantly, so sent is what the caller
			// heard: the two frames ahead of the delta, and none of the
			// three after it.
			leg.mu.Lock()
			heard := len(leg.sent)
			leg.mu.Unlock()
			if heard != 2 {
				t.Fatalf("the caller heard %d frames, want the 2 ahead of the delta", heard)
			}
			if got := session.turnWords(); got != "" {
				t.Errorf("the turn kept %q, want the plumbing unrecorded", got)
			}
		})
	}
}

// An ordinary delta is not plumbing: it is kept, emitted and played.
func TestAnOrdinaryUtteranceIsStillPlayed(t *testing.T) {
	t.Parallel()
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	session.cfg.Session.Tools = []provider.ToolSpec{{Name: "lookup_slot"}}
	awaitBridgeEvent(t, session, EventTypeReady)

	speakAroundText(model, "Let me check that for you.", 2, 3)
	said := awaitBridgeEvent(t, session, EventTypeBotSaid)
	if said.Text != "Let me check that for you." {
		t.Errorf("bot said %q", said.Text)
	}
	awaitBridgeEvent(t, session, EventTypePlaybackDone)

	awaitFramesSent(t, leg, 5)
}
