// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
	"time"
)

// #197: qwen commits the caller's speech with server VAD and then declines to
// answer it, because a response this client asked for (typically the turn
// that follows a tool result) is still in progress:
//
//	Server VAD turn committed but no response was created because a manual
//	response is already in progress.
//
// Nothing answered the caller until the 8 s dead-air prompt. The turn is owed
// instead, through the same flag a tool result waiting for the floor uses, so
// it is asked for exactly once.

const vadTurnRefusedMessage = "Server VAD turn committed but no response was created because a manual response is already in progress."

func sendVADTurnRefused(f *fakeProvider) {
	f.send(map[string]any{"type": "error", "error": map[string]any{
		"type": "invalid_request_error", "code": "invalid_value",
		"message": vadTurnRefusedMessage,
	}})
}

func startVADCall(t *testing.T) (*fakeProvider, *Realtime, *syncBuffer) {
	t.Helper()
	f := newFakeProvider(t, acceptSession)
	session, logs := loggedSession(t, f, QwenProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	return f, session, logs
}

func sendResponseDone(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
}

// refusalIsNotReported sends a marker the read loop handles after the error
// and checks that no EventTypeError arrived before it.
func refusalIsNotReported(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-session.Events():
			if event.Type == EventTypeError {
				t.Fatalf("the refused VAD turn was reported as an error: %v", event.Err)
			}
			if event.Type == EventTypeSpeechStarted {
				return
			}
		case <-deadline:
			t.Fatal("the marker event never arrived")
		}
	}
}

func TestAVADTurnRefusedWhileAResponseIsOpenIsAskedForWhenItEnds(t *testing.T) {
	f, session, logs := startVADCall(t)
	openATurnThatCallsATool(t, f, session)

	sendVADTurnRefused(f)
	// Nothing is asked for while the response holds the floor.
	f.awaitMessages("response.create", 1)

	sendResponseDone(t, f, session)
	f.awaitMessages("response.create", 2)
	if got := strings.Count(logs.String(), "the provider committed the caller's turn without answering it"); got != 1 {
		t.Errorf("the refusal was logged %d times, want 1", got)
	}
}

func TestAVADTurnRefusedOnTopOfAToolResultIsStillOneTurn(t *testing.T) {
	f, session, _ := startVADCall(t)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	sendVADTurnRefused(f)
	f.awaitMessages("response.create", 1)

	sendResponseDone(t, f, session)
	f.awaitMessages("response.create", 2)
}

func TestAVADTurnRefusedWithNothingOpenIsAskedForAtOnce(t *testing.T) {
	f, session, _ := startVADCall(t)
	finishTheOpeningTurn(t, f, session)

	sendVADTurnRefused(f)
	f.awaitMessages("response.create", 2)
}

func TestAVADTurnRefusedWhileARequestIsInFlightIsNotAnsweredByIt(t *testing.T) {
	f, session, _ := startVADCall(t)
	finishTheOpeningTurn(t, f, session)

	// A cue is asked for and the provider has not created its response yet.
	if err := session.SendUserText("cue"); err != nil {
		t.Fatalf("send cue: %v", err)
	}
	f.awaitMessages("response.create", 2)

	sendVADTurnRefused(f)
	f.awaitMessages("response.create", 2)

	// The response the cue asked for was made before the caller's turn was
	// committed: its creation does not discharge the debt, its end pays it.
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.awaitMessages("response.create", 2)
	sendResponseDone(t, f, session)
	f.awaitMessages("response.create", 3)
}

func TestAVADTurnRefusedWhileTheCallerIsSpeakingLeavesTheTurnToTheProvider(t *testing.T) {
	f, session, _ := startVADCall(t)
	finishTheOpeningTurn(t, f, session)

	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	sendVADTurnRefused(f)
	f.awaitMessages("response.create", 1)

	// The provider's own reply to the speech discharges the debt.
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	sendResponseDone(t, f, session)
	f.awaitMessages("response.create", 1)
}

func TestAVADTurnRefusedThenSpeechHoldsTheReleaseUntilItsReply(t *testing.T) {
	f, session, _ := startVADCall(t)
	openATurnThatCallsATool(t, f, session)

	sendVADTurnRefused(f)
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	sendResponseDone(t, f, session)
	f.awaitMessages("response.create", 1)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.awaitMessages("response.create", 1)
}

func TestAVADTurnRefusedIsNotReportedAsAnError(t *testing.T) {
	f, session, _ := startVADCall(t)
	openATurnThatCallsATool(t, f, session)

	sendVADTurnRefused(f)
	refusalIsNotReported(t, f, session)
}
