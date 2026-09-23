// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// What the provider says, and what the call hears.
//
// Every downstream frame in the mapping table has a test here, and every
// assertion about which events arrived is paired with one about how many: on
// this protocol a turn that ends twice and a turn that never ends are both
// live bugs, and a test that waits for the event it likes cannot tell them
// apart.
//

// The ordinary turn, frame for frame as the probe recorded it (e1c).
func TestANormalTurnIsTranslatedInOrder(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(transcriptionStarted("item-1"))
	f.send(transcriptionDelta("item-1", "你是谁"))
	f.send(transcriptionCompleted("item-1", "你是谁呀？"))
	f.send(outputTextDelta("你好，"))
	f.send(audioStarted("default"))
	f.send(audioDelta(base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03, 0x04})))
	f.send(outputTextDone("你好，我是语音测试机器人。"))
	f.send(audioDone())

	events := expectEvents(t, session,
		provider.EventTypeSpeechStarted,
		provider.EventTypeInputTranscript,
		provider.EventTypeInputTranscript,
		provider.EventTypeSpeechStopped,
		provider.EventTypeOutputTranscript,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeResponseDone,
	)

	// The transcript is cumulative on this protocol: the delta is the whole
	// utterance so far, and only the completed frame is final.
	if got := events[1]; got.Text != "你是谁" || got.IsFinal {
		t.Errorf("the partial transcript is %q (final=%v), want the cumulative text, not final",
			got.Text, got.IsFinal)
	}
	if got := events[2]; got.Text != "你是谁呀？" || !got.IsFinal {
		t.Errorf("the completed transcript is %q (final=%v), want the final text",
			got.Text, got.IsFinal)
	}
	if got := events[6]; string(got.Audio) != string([]byte{0x01, 0x02, 0x03, 0x04}) {
		t.Errorf("the audio delta carried %v, want the decoded bytes", got.Audio)
	}
	if got := events[7]; got.Text != "你好，我是语音测试机器人。" || !got.IsFinal {
		t.Errorf("the model's transcript is %q (final=%v), want the final text",
			got.Text, got.IsFinal)
	}

	// The wire's own response.done trails the turn by seconds and carries only
	// what it cost. It is not the turn ending, and it is not an event.
	f.send(wireResponseDone())
	refuteMoreEvents(t, session)
}

// The caller talking over a reply: the turn gets no output_audio.done at all,
// only a bare response.done in the same millisecond as the next transcription
// starting (e3a, e12r1).
func TestCallerBargeInEndsTheTurnAsSpeech(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(audioStarted("default"))
	f.send(audioDelta(base64.StdEncoding.EncodeToString([]byte{0x10, 0x11})))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

	f.send(wireResponseDone())
	f.send(transcriptionStarted("item-2"))

	events := expectEvents(t, session,
		provider.EventTypeInterrupted, provider.EventTypeSpeechStarted)
	if got := events[0].InterruptedBy; got != provider.InterruptReasonSpeech {
		t.Errorf("the turn was interrupted by %q, want the caller", got)
	}
	// No RESPONSE_DONE for that turn: it never finished.
	refuteMoreEvents(t, session)
}

// A turn this client stopped is not a barge-in. Reporting it as one would put
// an interruption the caller never made into the record of the call.
func TestATurnWeCancelledIsNotBlamedOnTheCaller(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(audioStarted("default"))
	expectEvents(t, session, provider.EventTypeResponseStarted)

	if err := session.Interrupt(provider.InterruptReasonDTMF, 320); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessages("response.cancel", 1)

	f.send(map[string]any{"type": "response.canceled"})
	f.send(wireResponseDone())

	events := expectEvents(t, session, provider.EventTypeInterrupted)
	if got := events[0].InterruptedBy; got != provider.InterruptReasonSystem {
		t.Errorf("the turn was interrupted by %q, want this client", got)
	}
	refuteMoreEvents(t, session)
}

// Audio keeps arriving for up to half a second after the cancel was
// acknowledged (e3b: three deltas, 525 ms). The caller has already stopped
// hearing this turn, so none of it may be played.
func TestStaleAudioAfterACancelIsDropped(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(audioStarted("default"))
	f.send(audioDelta(base64.StdEncoding.EncodeToString([]byte{0x01})))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

	if err := session.Interrupt(provider.InterruptReasonSystem, 100); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessages("response.cancel", 1)

	f.send(map[string]any{"type": "response.canceled"})
	for range 3 {
		f.send(audioDelta(base64.StdEncoding.EncodeToString([]byte{0x02, 0x03})))
	}
	refuteMoreEvents(t, session)

	// Then the turn ends the way a cancelled one does, with nothing but a bare
	// response.done.
	f.send(wireResponseDone())
	expectEvents(t, session, provider.EventTypeInterrupted)

	// The next turn lifts the fence.
	f.send(audioStarted("default"))
	f.send(audioDelta(base64.StdEncoding.EncodeToString([]byte{0x04})))
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)
	if string(events[1].Audio) != string([]byte{0x04}) {
		t.Errorf("the first audio of the new turn is %v, want the new bytes", events[1].Audio)
	}
	refuteMoreEvents(t, session)
}

// A pre-empted response gets no terminal event of any kind (e10b, e11c): the
// next turn simply starts. Something has to close the old one out, or the call
// would hold a turn open for the rest of the conversation.
func TestAPreemptedTurnIsClosedWhenTheNextOneStarts(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(audioStarted("default"))
	f.send(audioDelta(base64.StdEncoding.EncodeToString([]byte{0x01})))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

	f.send(audioStarted("chat_tts_text"))
	events := expectEvents(t, session,
		provider.EventTypeInterrupted, provider.EventTypeResponseStarted)
	if got := events[0].InterruptedBy; got != provider.InterruptReasonSystem {
		t.Errorf("the pre-empted turn was interrupted by %q, want this client", got)
	}
	refuteMoreEvents(t, session)
}

// A voice the vendor does not have is never reported as an error: the turn
// runs to completion and carries no audio at all (e8a2). Since the format this
// client asks for is undocumented, that guard is also the detector for a
// provider that quietly stopped honouring it.
func TestATurnThatCarriedNoAudioIsFatal(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(audioStarted("chat_tts_text"))
	expectEvents(t, session, provider.EventTypeResponseStarted)

	f.send(audioDone())
	events := expectEvents(t, session, provider.EventTypeError)
	if !events[0].IsFatal {
		t.Error("a turn with no audio was reported as survivable")
	}
	if events[0].Err == nil {
		t.Error("the fatal error carried no reason")
	}
	refuteMoreEvents(t, session)
}

// No response event brackets a tool call on this protocol, so the client makes
// the turn itself. Without it the call's dead-air timer — armed only after a
// response ends — would never be armed again.
func TestAToolCallIsAWholeTurn(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(map[string]any{
		"type": "response.function_call_arguments.done",
		"items": []map[string]any{
			{"call_id": "call-a", "name": "lookup_balance", "arguments": `{"accountId":"42"}`},
			{"call_id": "call-b", "name": "lookup_balance", "arguments": ""},
		}})

	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone,
	)
	if got := events[1]; got.ToolCallID != "call-a" || got.ToolName != "lookup_balance" ||
		got.ToolArgs != `{"accountId":"42"}` {
		t.Errorf("the first tool call is %+v, want the item as it arrived", got)
	}
	// Arguments the provider left empty are still a JSON object to the engine.
	if got := events[2]; got.ToolCallID != "call-b" || got.ToolArgs != "{}" {
		t.Errorf("the second tool call is %+v, want empty arguments as an empty object", got)
	}
	refuteMoreEvents(t, session)
}

func TestUndecodableAudioIsReportedRatherThanPlayed(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(audioStarted("default"))
	expectEvents(t, session, provider.EventTypeResponseStarted)

	f.send(map[string]any{"type": "response.output_audio.delta", "delta": "not base64!!"})
	events := expectEvents(t, session, provider.EventTypeError)
	if events[0].IsFatal {
		t.Error("one undecodable frame ended the session; the rest of the turn is still usable")
	}
	refuteMoreEvents(t, session)
}

// The words of a line the flow chose are this client's to report: the provider
// sends no text events for one (e5a, e10a).
func TestASpokenLineIsAnnouncedWhenItsTurnStarts(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	if err := session.SpeakText("请稍等，我为您转接人工客服。", false); err != nil {
		t.Fatalf("speak text: %v", err)
	}
	committed := f.awaitMessages("speech_text_buffer.commit", 1)
	if got := committed[0]["text"]; got != "请稍等，我为您转接人工客服。" {
		t.Errorf("the committed text is %v, want the line the flow chose", got)
	}
	refuteMoreEvents(t, session)

	f.send(audioStarted("chat_tts_text"))
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeOutputTranscript)
	if got := events[1]; got.Text != "请稍等，我为您转接人工客服。" || !got.IsFinal {
		t.Errorf("the spoken line was reported as %q (final=%v), want the line as written",
			got.Text, got.IsFinal)
	}
	refuteMoreEvents(t, session)
}

// A lone user item is silently dropped by this engine, with or without a
// buffer commit (e4, e4b). Pretending otherwise would leave a keypress
// unanswered and the caller waiting.
func TestATextCueIsRefusedOutrightAndWritesNothing(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	err := session.SendUserText("2")
	if !errors.Is(err, ErrTextCueUnsupported) {
		t.Errorf("SendUserText returned %v, want the unsupported sentinel", err)
	}
	f.awaitMessages("conversation.item.create", 0)
	f.awaitMessages("input_audio_buffer.commit", 0)
	refuteMoreEvents(t, session)
}

// Who cancels what, and when. The engine stops on its own when it hears the
// caller, so telling it again is noise; a keypress and an application decision
// have to be said out loud, and only while there is something to stop.
func TestTheInterruptMatrix(t *testing.T) {
	t.Run("speech says nothing", func(t *testing.T) {
		f := newFakeDoubao(t, acceptSession)
		session := startedSession(t, f)

		f.send(audioStarted("default"))
		expectEvents(t, session, provider.EventTypeResponseStarted)

		if err := session.Interrupt(provider.InterruptReasonSpeech, 200); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.awaitMessages("response.cancel", 0)
		refuteMoreEvents(t, session)
	})

	t.Run("a keypress over an open turn cancels once", func(t *testing.T) {
		f := newFakeDoubao(t, acceptSession)
		session := startedSession(t, f)

		f.send(audioStarted("default"))
		expectEvents(t, session, provider.EventTypeResponseStarted)

		if err := session.Interrupt(provider.InterruptReasonDTMF, 200); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		if err := session.Interrupt(provider.InterruptReasonDTMF, 240); err != nil {
			t.Fatalf("second interrupt: %v", err)
		}
		f.awaitMessages("response.cancel", 1)
		refuteMoreEvents(t, session)
	})

	t.Run("with no turn open there is nothing to stop", func(t *testing.T) {
		f := newFakeDoubao(t, acceptSession)
		session := startedSession(t, f)

		if err := session.Interrupt(provider.InterruptReasonSystem, 0); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.awaitMessages("response.cancel", 0)
		refuteMoreEvents(t, session)
	})
}

// The methods the call actor calls are called from the goroutine draining
// Events. Emitting from any of them would deadlock the call.
func TestTheConsumerGoroutineCanDriveTheSessionWithoutDeadlocking(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range session.Events() {
			if event.Type != provider.EventTypeResponseStarted {
				continue
			}
			if err := session.Interrupt(provider.InterruptReasonDTMF, 100); err != nil {
				t.Errorf("interrupt from the consumer: %v", err)
			}
			if err := session.SpeakText("One moment.", false); err != nil {
				t.Errorf("speak from the consumer: %v", err)
			}
			if err := session.UpdateInstructions("Now close the call."); err != nil {
				t.Errorf("update from the consumer: %v", err)
			}
			return
		}
	}()

	// Enough audio to fill the channel several times over if anything emitted
	// from a caller-side method.
	f.send(audioStarted("default"))
	for range 300 {
		f.send(audioDelta(base64.StdEncoding.EncodeToString([]byte{0x01, 0x02})))
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the consumer goroutine never got through its work")
	}
	f.awaitMessages("response.cancel", 1)
	f.awaitMessages("speech_text_buffer.commit", 1)
	f.awaitMessages("session.update", 1)
}

// A turn the provider accepted and then walked away from is closed out with
// whatever arrived. Without it the flow engine waits for a completion that is
// never coming, and the caller hears silence.
func TestAnAbandonedTurnIsClosedOut(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := testSession(t, f)
	session.firstAudioDeadline = 40 * time.Millisecond
	session.deltaStallDeadline = 40 * time.Millisecond
	start(t, session, testConfig())

	f.send(audioStarted("default"))
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeError,
		provider.EventTypeResponseDone,
	)
	if events[1].IsFatal {
		t.Error("an abandoned turn ended the session; the next turn is still possible")
	}
	if got := events[2].Status; got != provider.StatusStalled {
		t.Errorf("the abandoned turn ended with status %q, want %q", got, provider.StatusStalled)
	}
	refuteMoreEvents(t, session)
}

// An event this client does not know is logged, not guessed at.
func TestAnUnmappedEventIsIgnored(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(map[string]any{"type": "conversation.item.added", "item_id": "item-9"})
	f.send(map[string]any{"type": "input_audio_buffer.committed"})
	f.send(map[string]any{"type": "some.event.from.a.later.version"})
	refuteMoreEvents(t, session)
}
