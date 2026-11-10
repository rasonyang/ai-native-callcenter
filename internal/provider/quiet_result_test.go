// SPDX-License-Identifier: Apache-2.0

package provider

import "testing"

//
// SendToolResultQuietly: the result joins the conversation and nothing is
// asked for. The caller was answered in the response that made the call, and
// speaks next.
//

// completeResponse ends the open response as the provider does.
func completeResponse(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
}

// startQuietCall opens a response that called a tool and is still generating
// audio, on a profile of the test's choice.
func startQuietCall(t *testing.T, profile Profile) (*fakeProvider, *Realtime) {
	t.Helper()
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, profile)
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)
	return f, session
}

// A call that arrives mid-response, with audio still being generated: the
// result goes into the conversation at once and no turn is asked for, then or
// when the response ends. None is left owed either: the next result asks for
// its turn at once.
func TestAQuietResultMidResponseLeavesNoTurnOwed(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, session := startQuietCall(t, profile)
			before := len(f.messagesOfType("response.create"))

			if err := session.SendToolResultQuietly("fc_1", `{"ok":"1"}`); err != nil {
				t.Fatalf("send quietly: %v", err)
			}
			awaitOutputs(t, f, 1)
			f.awaitMessages("response.create", before)

			completeResponse(t, f, session)
			f.awaitMessages("response.create", before)

			// Nothing is owed: a result that does want its turn asks for it
			// now, not on the strength of a request that was never made.
			if err := session.SendToolResult("fc_2", `{"ok":"1"}`); err != nil {
				t.Fatalf("send: %v", err)
			}
			f.awaitMessages("response.create", before+1)
		})
	}
}

// A turn already owed or requested still happens, once, and reads the quiet
// result with the rest of the conversation.
func TestATurnAlreadyOwedOrRequestedStillHappensOnce(t *testing.T) {
	t.Run("owed by an earlier result while the response is open", func(t *testing.T) {
		f, session := startQuietCall(t, QwenProfile())
		before := len(f.messagesOfType("response.create"))

		if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send: %v", err)
		}
		if err := session.SendToolResultQuietly("fc_2", `{"ok":"1"}`); err != nil {
			t.Fatalf("send quietly: %v", err)
		}
		f.awaitMessages("response.create", before)

		completeResponse(t, f, session)
		f.awaitMessages("response.create", before+1)
		if got := len(functionCallOutputs(f)); got != 2 {
			t.Errorf("function_call_output items = %d, want both results in the conversation", got)
		}
	})
	t.Run("owed by a cue while the response is open", func(t *testing.T) {
		f, session := startQuietCall(t, OpenAIProfile())
		before := len(f.messagesOfType("response.create"))

		if err := session.SendUserText("(The caller pressed 2.)"); err != nil {
			t.Fatalf("cue: %v", err)
		}
		if err := session.SendToolResultQuietly("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send quietly: %v", err)
		}
		completeResponse(t, f, session)
		f.awaitMessages("response.create", before+1)
	})
	t.Run("requested and not yet created", func(t *testing.T) {
		f, session := startQuietCall(t, QwenProfile())
		completeResponse(t, f, session)
		before := len(f.messagesOfType("response.create"))

		if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send: %v", err)
		}
		f.awaitMessages("response.create", before+1)
		if err := session.SendToolResultQuietly("fc_2", `{"ok":"1"}`); err != nil {
			t.Fatalf("send quietly: %v", err)
		}
		f.send(map[string]any{"type": "response.created"})
		awaitEvent(t, session, EventTypeResponseStarted)
		completeResponse(t, f, session)
		// The one request covered the turn; nothing asked again.
		f.awaitMessages("response.create", before+1)
	})
}

// A quiet result is not a result the caller is waiting to hear: the bot's reply
// to the caller's next words, cut before a sound of it, is not reported as the
// unheard answer to a tool.
func TestACutReplyAfterAQuietResultInjectsNoUnheardNote(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())

	if err := session.SendToolResultQuietly("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send quietly: %v", err)
	}
	f.awaitMessages("response.create", 1)

	// The caller speaks and the provider answers with a response of its own,
	// which the caller then talks over before any audio.
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	answerTurn(t, f, session, "item_reply")
	finishAnswerTurn(t, f, session, statusCancelled)

	f.awaitMessages("conversation.item.delete", 1)
	awaitNotes(t, f, 0)
}

// The caller's next speech: the provider's own response is accepted, and
// nothing is requested on top of it.
func TestTheProvidersOwnReplyAfterAQuietResultIsTheNextTurn(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	before := len(f.messagesOfType("response.create"))

	if err := session.SendToolResultQuietly("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send quietly: %v", err)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	answerTurn(t, f, session, "item_reply")
	sendAudio(t, f, session)
	finishAnswerTurn(t, f, session, statusCompleted)

	f.awaitMessages("response.create", before)
	f.refuteMessage("response.cancel")
	if got := len(functionCallOutputs(f)); got != 1 {
		t.Errorf("function_call_output items = %d, want the one quiet result", got)
	}
}

// A barge-in on the response that made the call, with the result sent quietly
// after it: the provider's reply to the caller is the next turn and nothing is
// double-requested.
func TestABargeInBeforeAQuietResultIsLeftToTheProvider(t *testing.T) {
	f, session := startQuietCall(t, QwenProfile())
	before := len(f.messagesOfType("response.create"))

	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	if err := session.Interrupt(InterruptReasonSpeech, 320); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})
	awaitEvent(t, session, EventTypeInterrupted)

	if err := session.SendToolResultQuietly("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send quietly: %v", err)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	completeResponse(t, f, session)

	f.awaitMessages("response.create", before)
}
