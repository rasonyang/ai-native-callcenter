// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"
	"time"
)

// The wire half of docs/design/11-backchannels.md: on an engine whose client
// controls interruption (openai) nothing answers the caller until this client
// asks, and an acknowledgement is taken out of the conversation.

func startControlled(t *testing.T, profile Profile) (*fakeProvider, *Realtime) {
	t.Helper()
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, profile)
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	// The opening turn: asked for, created and finished, so the floor is free
	// and the counts below are the ones the caller's turns make.
	f.awaitMessage("response.create")
	f.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-open"}})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"id": "resp-open", "status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	return f, session
}

// opening is the response.create of the opening turn, already in the record.
const opening = 1

func committed(f *fakeProvider, item string) {
	f.send(map[string]any{"type": "input_audio_buffer.committed", "item_id": item})
}

// Only openai's session payload carries the two switches; every other engine's
// is byte for byte what it was.
func TestOnlyAControlledEngineSwitchesOffTheServersReactions(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, _ := startControlled(t, profile)
			update := f.awaitMessage("session.update")
			detection := turnDetectionOf(t, update)
			_, hasCreate := detection["create_response"]
			_, hasInterrupt := detection["interrupt_response"]
			if want := profile.ControlsInterruption; hasCreate != want || hasInterrupt != want {
				t.Errorf("turn_detection = %v, want the switches present = %v", detection, want)
			}
			if profile.ControlsInterruption {
				if detection["create_response"] != false || detection["interrupt_response"] != false {
					t.Errorf("turn_detection = %v, want both switches off", detection)
				}
			}
		})
	}
}

func turnDetectionOf(t *testing.T, update map[string]any) map[string]any {
	t.Helper()
	session, _ := update["session"].(map[string]any)
	if audio, ok := session["audio"].(map[string]any); ok { // GA
		input, _ := audio["input"].(map[string]any)
		detection, _ := input["turn_detection"].(map[string]any)
		return detection
	}
	detection, _ := session["turn_detection"].(map[string]any)
	return detection
}

func TestOnlyOpenAIControlsInterruption(t *testing.T) {
	for _, profile := range []Profile{QwenProfile(), GrokProfile(), GatewayProfile(), DoubaoProfile()} {
		if profile.ControlsInterruption {
			t.Errorf("%s controls interruption; only openai was measured to honour the switches", profile.Name)
		}
	}
	if !OpenAIProfile().ControlsInterruption {
		t.Error("openai does not control interruption")
	}
}

// The turn is asked for once the buffer is committed, not before.
func TestTheCallersTurnIsAskedForAfterTheCommit(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())

	if err := session.AnswerCallerTurn("item-1"); err != nil {
		t.Fatal(err)
	}
	f.awaitMessages("response.create", opening+0)

	committed(f, "item-1")
	f.awaitMessages("response.create", opening+1)
}

func TestACommitSeenBeforeTheAskIsAnsweredAtOnce(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())

	committed(f, "item-1")
	time.Sleep(50 * time.Millisecond) // the read loop has seen it
	if err := session.AnswerCallerTurn("item-1"); err != nil {
		t.Fatal(err)
	}
	f.awaitMessages("response.create", opening+1)
}

// Another utterance's commit does not release this one's turn, and a turn is
// asked for once however many things owe it.
func TestOnlyTheAskedItemsCommitReleasesItsTurn(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())

	if err := session.AnswerCallerTurn("item-2"); err != nil {
		t.Fatal(err)
	}
	committed(f, "item-1")
	f.awaitMessages("response.create", opening+0)
	committed(f, "item-2")
	f.awaitMessages("response.create", opening+1)
	committed(f, "item-2") // a repeated commit is not a second turn
	f.awaitMessages("response.create", opening+1)
}

// The response that still holds the floor (the one the caller cut) is waited
// for: the turn rides the owed flag and goes out at its response.done.
func TestTheTurnWaitsForTheResponseThatHoldsTheFloor(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())
	f.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-1"}})
	awaitEvent(t, session, EventTypeResponseStarted)

	if err := session.AnswerCallerTurn("item-1"); err != nil {
		t.Fatal(err)
	}
	committed(f, "item-1")
	f.awaitMessages("response.create", opening+0)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"id": "resp-1", "status": "cancelled"}})
	f.awaitMessages("response.create", opening+1)
}

// The transcriber is told the call's language: left to guess, it heard a Chinese
// "嗯" as Japanese or Spanish, which no acknowledgement list can match.
func TestTheTranscriberIsToldTheCallsLanguage(t *testing.T) {
	for language, want := range map[string]any{"zh-CN": "zh", "zh": "zh", "en": "en", "": nil} {
		t.Run("language="+language, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, OpenAIProfile())
			cfg := basicConfig()
			cfg.Language = language
			if err := session.Start(t.Context(), cfg); err != nil {
				t.Fatalf("start: %v", err)
			}
			update := f.awaitMessage("session.update")
			body, _ := update["session"].(map[string]any)
			audio, _ := body["audio"].(map[string]any)
			input, _ := audio["input"].(map[string]any)
			transcription, _ := input["transcription"].(map[string]any)
			if got := transcription["language"]; got != want {
				t.Errorf("transcription.language = %v, want %v (%v)", got, want, transcription)
			}
		})
	}
}

// With the switches off the caller can stop speaking in the middle of a
// response. A turn already owed then stays owed until that response ends: asked
// for at the end of the speech, the provider refuses it (live, 2026-11-11).
func TestTheEndOfSpeechDoesNotAskForATurnOverAnOpenResponse(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())
	f.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-1"}})
	awaitEvent(t, session, EventTypeResponseStarted)
	if err := session.SendUserText("a cue that has to wait"); err != nil {
		t.Fatal(err)
	}

	f.send(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": "item-1"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped", "item_id": "item-1"})
	awaitEvent(t, session, EventTypeSpeechStopped)

	time.Sleep(100 * time.Millisecond)
	if got := len(f.messagesOfType("response.create")); got != opening {
		t.Fatalf("response.create sent %d times over an open response, want %d", got, opening)
	}

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"id": "resp-1", "status": "completed"}})
	f.awaitMessages("response.create", opening+1)
}

// Declining takes the item out of the conversation and withdraws a turn that
// was waiting for its commit.
func TestADeclinedUtteranceIsDeletedAndNeverAnswered(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())

	if err := session.AnswerCallerTurn("item-1"); err != nil {
		t.Fatal(err)
	}
	if err := session.DeclineCallerTurn("item-1"); err != nil {
		t.Fatal(err)
	}
	deleted := f.awaitMessages("conversation.item.delete", 1)
	if deleted[0]["item_id"] != "item-1" {
		t.Errorf("deleted %v, want item-1", deleted[0]["item_id"])
	}
	committed(f, "item-1")
	f.awaitMessages("response.create", opening+0)
}

// Nothing re-asks a declined turn: with the provider no longer answering the
// caller, the 3 s watch over its answer has nothing to watch.
func TestNoWatchReAsksADeclinedTurn(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())
	session.responseCreatedDeadline = 100 * time.Millisecond

	f.send(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": "item-1"})
	ev := awaitEvent(t, session, EventTypeSpeechStarted)
	if ev.ItemID != "item-1" {
		t.Errorf("speech_started item = %q, want item-1", ev.ItemID)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped", "item_id": "item-1"})
	ev = awaitEvent(t, session, EventTypeSpeechStopped)
	if ev.ItemID != "item-1" {
		t.Errorf("speech_stopped item = %q, want item-1", ev.ItemID)
	}
	committed(f, "item-1")
	f.send(map[string]any{"type": "conversation.item.input_audio_transcription.delta",
		"item_id": "item-1", "delta": "嗯"})
	if ev = awaitEvent(t, session, EventTypeInputTranscript); ev.ItemID != "item-1" || ev.IsFinal {
		t.Errorf("partial = %+v, want item-1 and not final", ev)
	}
	f.send(map[string]any{"type": "conversation.item.input_audio_transcription.completed",
		"item_id": "item-1", "transcript": "嗯"})
	if ev = awaitEvent(t, session, EventTypeInputTranscript); ev.ItemID != "item-1" || !ev.IsFinal {
		t.Errorf("final = %+v, want item-1 and final", ev)
	}
	if err := session.DeclineCallerTurn("item-1"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(4 * session.responseCreatedDeadline)
	f.awaitMessages("response.create", opening+0)
}

// The control is the same for a keypress and a spoken line: the cancel of an
// interruption by speech is now ours to send.
func TestSpeechCancelsAnOpenResponseOnAControlledEngine(t *testing.T) {
	f, session := startControlled(t, OpenAIProfile())
	f.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-1"}})
	awaitEvent(t, session, EventTypeResponseStarted)

	if err := session.Interrupt(InterruptReasonSpeech, -1); err != nil {
		t.Fatal(err)
	}
	f.awaitMessages("response.cancel", 1)
}

// An engine that answers the caller itself is not asked to, and a decline or an
// ask changes nothing on it.
func TestAnEngineThatAnswersItselfIsNeverAskedToAnswer(t *testing.T) {
	f, session := startControlled(t, QwenProfile())

	if err := session.AnswerCallerTurn("item-1"); err != nil {
		t.Fatal(err)
	}
	committed(f, "item-1")
	f.awaitMessages("response.create", opening+0)
}
