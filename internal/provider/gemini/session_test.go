// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// What the provider says, and what the call hears.
//
// Every downstream frame in the mapping table has a test here, and every
// assertion about which events arrived is paired with one about how many. One
// invariant governs all of them: every RESPONSE_STARTED is closed by exactly one
// of RESPONSE_DONE and INTERRUPTED — never both, never neither — and a test that
// waits for the event it likes cannot tell a turn that ends twice from one that
// never ends.
//

// encoded is audio as it arrives: base64 inside the JSON.
func encoded(audio ...byte) string {
	return base64.StdEncoding.EncodeToString(audio)
}

// The ordinary turn, frame for frame as the probe recorded it (m3b, m5).
func TestANormalTurnIsTranslatedInOrder(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(modelAudioParts("Good morning, ", encoded(0x01, 0x02, 0x03, 0x04)))
	f.send(modelAudio(encoded(0x05, 0x06)))
	f.send(outputTranscript("NovaNet support."))
	f.send(generationComplete())

	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeOutputTranscript,
		provider.EventTypeResponseDone,
	)
	if got := string(events[1].Audio); got != string([]byte{0x01, 0x02, 0x03, 0x04}) {
		t.Errorf("the audio delta carried %v, want the decoded bytes", events[1].Audio)
	}
	// The transcript arrives in fragments that nothing marks the end of, so the
	// running text is what a partial carries and the finished line is announced
	// when the turn closes.
	if got := events[2]; got.Text != "Good morning, " || got.IsFinal {
		t.Errorf("the first fragment is %q (final=%v), want the running text", got.Text, got.IsFinal)
	}
	if got := events[4]; got.Text != "Good morning, NovaNet support." || got.IsFinal {
		t.Errorf("the second fragment is %q (final=%v), want the running text",
			got.Text, got.IsFinal)
	}
	if got := events[5]; got.Text != "Good morning, NovaNet support." || !got.IsFinal {
		t.Errorf("the finished line is %q (final=%v), want the whole turn's words",
			got.Text, got.IsFinal)
	}

	// turnComplete trails the real end of the turn by the playback the server
	// presumes happened. It is bookkeeping, and it is not an event.
	f.send(turnComplete())
	refuteMoreEvents(t, session)
}

// Frames arrive with the binary opcode, pretty printed, with bare objects and
// resumption handles nobody asked for mixed in. A client that read the opcode,
// the whitespace, or anything it did not recognise would pass every other test
// here and fail on the first real call.
func TestTheNoiseTheServiceSendsIsNotAnEvent(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.noise()
	f.sendPretty(modelAudioParts("Hello.", encoded(0x01)))
	f.noise()
	f.sendPretty(generationComplete())
	f.sendPretty(turnComplete())
	f.send(map[string]any{"voiceActivity": map[string]any{"from": "a later version"}})

	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeOutputTranscript,
		provider.EventTypeResponseDone,
	)
	refuteMoreEvents(t, session)
}

// One frame may carry several audio parts, and every one of them is speech the
// caller is owed.
func TestEveryAudioPartOfAFrameIsPlayed(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(modelAudioParts("", encoded(0x01), encoded(0x02), encoded(0x03)))

	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeAudioDelta,
		provider.EventTypeAudioDelta,
	)
	for i, want := range []byte{0x01, 0x02, 0x03} {
		if got := events[i+1].Audio; len(got) != 1 || got[0] != want {
			t.Errorf("part %d carried %v, want %#x", i, got, want)
		}
	}
	refuteMoreEvents(t, session)
}

// The caller's words arrive on no particular schedule and nothing marks the last
// fragment. The model answering them is what says they have finished.
func TestWhatTheCallerSaidIsFinishedWhenTheModelAnswers(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(inputTranscript("I need "))
	f.send(inputTranscript("my balance."))
	events := expectEvents(t, session,
		provider.EventTypeInputTranscript, provider.EventTypeInputTranscript)
	if got := events[1]; got.Text != "I need my balance." || got.IsFinal {
		t.Errorf("the running transcript is %q (final=%v), want the text so far",
			got.Text, got.IsFinal)
	}

	f.send(modelAudio(encoded(0x01)))
	events = expectEvents(t, session,
		provider.EventTypeInputTranscript,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
	)
	if got := events[0]; got.Text != "I need my balance." || !got.IsFinal {
		t.Errorf("the finished line is %q (final=%v), want what the caller said",
			got.Text, got.IsFinal)
	}
	refuteMoreEvents(t, session)
}

// A fragment that arrives after the model has already answered has nowhere else
// to go: this protocol guarantees no ordering at all for these.
func TestALateTranscriptOfTheCallerIsFinishedWhenTheTurnIsOver(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

	f.send(inputTranscript("I said this late."))
	f.send(generationComplete())
	f.send(turnComplete())

	events := expectEvents(t, session,
		provider.EventTypeInputTranscript,
		provider.EventTypeResponseDone,
		provider.EventTypeInputTranscript,
	)
	if got := events[2]; got.Text != "I said this late." || !got.IsFinal {
		t.Errorf("the late line is %q (final=%v), want what the caller said",
			got.Text, got.IsFinal)
	}
	refuteMoreEvents(t, session)
}

// The caller talking over a reply. The server's own detector hears them, stops
// the turn and says so — which is the only announcement of the caller speaking
// this protocol makes while the model is talking.
func TestTheCallerTalkingOverTheModelEndsTheTurnAsSpeech(t *testing.T) {
	shapes := map[string]func(f *fakeGemini){
		"interrupted and turnComplete in two frames": func(f *fakeGemini) {
			f.send(interrupted())
			f.send(turnComplete())
		},
		"interrupted and turnComplete in one frame": func(f *fakeGemini) {
			f.send(interruptedAndComplete())
		},
	}

	for name, interrupt := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newFakeGemini(t, acceptSetup)
			session := startedSession(t, f)

			f.send(modelAudioParts("A call centre queue is", encoded(0x10, 0x11)))
			expectEvents(t, session,
				provider.EventTypeResponseStarted,
				provider.EventTypeAudioDelta,
				provider.EventTypeOutputTranscript)

			interrupt(f)

			events := expectEvents(t, session,
				provider.EventTypeOutputTranscript,
				provider.EventTypeSpeechStarted,
				provider.EventTypeInterrupted,
			)
			if got := events[0]; got.Text != "A call centre queue is" || !got.IsFinal {
				t.Errorf("the words the caller heard were reported as %q (final=%v)",
					got.Text, got.IsFinal)
			}
			if got := events[2].InterruptedBy; got != provider.InterruptReasonSpeech {
				t.Errorf("the turn was interrupted by %q, want the caller", got)
			}
			// No RESPONSE_DONE for that turn: an interrupted turn gets no
			// generationComplete at all, and one turn gets one ending.
			refuteMoreEvents(t, session)
		})
	}
}

// A turn this client replaced is not a barge-in. Reporting it as one would put
// an interruption the caller never made into the record of the call.
func TestATurnWeReplacedIsNotBlamedOnTheCaller(t *testing.T) {
	preemptions := map[string]func(t *testing.T, session *Session){
		"a line the flow chose": func(t *testing.T, session *Session) {
			if err := session.SpeakText("I am transferring you now.", false); err != nil {
				t.Fatalf("speak text: %v", err)
			}
		},
		"a keypress put into the conversation": func(t *testing.T, session *Session) {
			if err := session.SendUserText("2"); err != nil {
				t.Fatalf("send user text: %v", err)
			}
		},
	}

	for name, preempt := range preemptions {
		t.Run(name, func(t *testing.T) {
			f := newFakeGemini(t, acceptSetup)
			session := startedSession(t, f)

			f.send(modelAudio(encoded(0x01)))
			expectEvents(t, session,
				provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

			preempt(t, session)
			f.awaitMessages("clientContent", 2)

			// The server confirms the pre-emption the way it confirms any
			// interruption, and then starts the turn it was asked for.
			f.send(interrupted())
			f.send(turnComplete())

			events := expectEvents(t, session, provider.EventTypeInterrupted)
			if got := events[0].InterruptedBy; got != provider.InterruptReasonSystem {
				t.Errorf("the turn was interrupted by %q, want this client", got)
			}
			refuteMoreEvents(t, session)
		})
	}
}

// The caller speaking over the tail of a turn that has already finished
// generating. There is nothing left to close — the turn ended when the model
// stopped — but the call still has to flush what it was about to play.
func TestSpeechOverAFinishedTurnIsStillTheCallerTakingTheFloor(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(modelAudio(encoded(0x01)))
	f.send(generationComplete())
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeResponseDone)

	f.send(interrupted())
	f.send(turnComplete())

	expectEvents(t, session, provider.EventTypeSpeechStarted)
	// And no second ending for a turn that already had one.
	refuteMoreEvents(t, session)
}

// The invariant, counted across a conversation that ends its turns every way
// this protocol can.
func TestEveryTurnIsClosedExactlyOnce(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	// A turn that runs to completion.
	f.send(modelAudioParts("One.", encoded(0x01)))
	f.send(generationComplete())
	f.send(turnComplete())
	// A turn the caller talks over.
	f.send(modelAudioParts("Two.", encoded(0x02)))
	f.send(interrupted())
	f.send(turnComplete())
	// A turn spent calling a function, and the speech that follows the answer.
	f.send(toolCallFrame(functionCallOf("fc_1", "lookup_balance", nil)))
	f.send(modelAudioParts("Three.", encoded(0x03)))
	f.send(generationComplete())
	f.send(turnComplete())
	// A turn the caller talks over after it finished generating: the one shape
	// that closes nothing.
	f.send(modelAudioParts("Four.", encoded(0x04)))
	f.send(generationComplete())
	f.send(interrupted())
	f.send(turnComplete())

	events := settledEvents(t, session)

	started := countOf(events, provider.EventTypeResponseStarted)
	done := countOf(events, provider.EventTypeResponseDone)
	interruptions := countOf(events, provider.EventTypeInterrupted)
	// Five turns, not four: the function call is a turn of its own and the
	// speech that follows the answer is another, which is what the server does
	// too.
	if started != 5 {
		t.Errorf("%d turns started, want 5 (%v)", started, typesOf(events))
	}
	if done+interruptions != started {
		t.Errorf("%d turns started and %d ended (%d done, %d interrupted): %v",
			started, done+interruptions, done, interruptions, typesOf(events))
	}
	if done != 4 || interruptions != 1 {
		t.Errorf("the turns ended %d done and %d interrupted, want 4 and 1 (%v)",
			done, interruptions, typesOf(events))
	}
	// The caller talking over a turn that had already ended takes the floor and
	// closes nothing.
	if got := countOf(events, provider.EventTypeSpeechStarted); got != 2 {
		t.Errorf("the caller took the floor %d times, want 2 (%v)", got, typesOf(events))
	}
}

//
// Tool calls.
//

// No frame brackets a tool call on this protocol: the calls simply arrive, and
// the model says nothing more until it is answered. The turn is synthesised
// around them because the call's dead-air timer is armed in exactly one place,
// after a turn ends, and nothing else would arm it.
func TestAToolCallIsAWholeTurnAndTheAnswerStartsAnother(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(toolCallFrame(
		functionCallOf("fc_a", "lookup_order", map[string]any{"order_id": "A1234"}),
		functionCallOf("fc_b", "lookup_customer", nil),
	))

	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone,
	)
	if got := events[1]; got.ToolCallID != "fc_a" || got.ToolName != "lookup_order" ||
		got.ToolArgs != `{"order_id":"A1234"}` {
		t.Errorf("the first tool call is %+v, want the call as it arrived", got)
	}
	// Arguments the model left out are still a JSON object to the engine.
	if got := events[2]; got.ToolCallID != "fc_b" || got.ToolArgs != "{}" {
		t.Errorf("the second tool call is %+v, want absent arguments as an empty object", got)
	}
	refuteMoreEvents(t, session)

	// The speech that follows the answer is a new turn, which is what the
	// server does too.
	if err := session.SendToolResult("fc_a", `{"status":"shipped"}`, ""); err != nil {
		t.Fatalf("answer the first call: %v", err)
	}
	if err := session.SendToolResult("fc_b", `{"tier":"gold"}`, ""); err != nil {
		t.Fatalf("answer the second call: %v", err)
	}
	f.send(modelAudioParts("Your order has shipped.", encoded(0x01)))
	f.send(generationComplete())
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeOutputTranscript,
		provider.EventTypeResponseDone,
	)
	refuteMoreEvents(t, session)
}

// A blocking tool call is the server being legitimately silent — measured at a
// minute and counting. Abandoning the turn would put the caller through to a
// person because a database was slow.
func TestTheWatchdogIsSilentWhileAToolCallIsPending(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)
	session.firstAudioDeadline = 30 * time.Millisecond
	session.deltaStallDeadline = 30 * time.Millisecond
	start(t, session, testConfig())

	f.send(toolCallFrame(functionCallOf("fc_1", "lookup_balance", nil)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone)

	// Several deadlines' worth of the silence the model is entitled to.
	time.Sleep(300 * time.Millisecond)
	refuteMoreEvents(t, session)
}

// answersWith replies to a tool result the moment it reaches the provider, and
// with nothing else. It is what makes the three tests below exact: the model's
// reply cannot arrive late for a reason the test invented.
func answersWith(frames ...map[string]any) func(*fakeGemini, map[string]any) {
	return func(f *fakeGemini, message map[string]any) {
		if _, isSetup := message["setup"]; isSetup {
			f.send(map[string]any{"setupComplete": map[string]any{}})
			return
		}
		if _, isAnswered := message["toolResponse"]; !isAnswered {
			return
		}
		for _, frame := range frames {
			f.send(frame)
		}
	}
}

// answeredSession is a session with a tool call outstanding and a short patience
// for the answer to it.
func answeredSession(t *testing.T, f *fakeGemini) *Session {
	t.Helper()

	session := testSession(t, f)
	session.firstAudioDeadline = 150 * time.Millisecond
	session.deltaStallDeadline = 150 * time.Millisecond
	start(t, session, testConfig())

	f.send(toolCallFrame(functionCallOf("fc_1", "lookup_balance", nil)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone)
	return session
}

// A tool result the model takes and then says nothing about.
//
// Measured on a live call: sixty-four seconds in which the model answered tool
// results with more tool calls and then with nothing at all, while the caller
// heard silence and nothing in this client was waiting on anything — the turn
// that would have been watched ended when the calls were handed over.
//
// The turn the model owed is the turn that is reported: started and stalled,
// which is one beginning and one ending, and which the flow already knows what
// to do with.
func TestAToolResultTheModelNeverAnswersIsReportedAsAStall(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := answeredSession(t, f)

	if err := session.SendToolResult("fc_1", `{"balance":"12.30"}`, ""); err != nil {
		t.Fatalf("answer the call: %v", err)
	}

	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeError,
		provider.EventTypeResponseDone,
	)
	if events[1].IsFatal {
		t.Error("an unanswered tool result ended the session; the next turn is still possible")
	}
	if got := events[2].Status; got != provider.StatusStalled {
		t.Errorf("the turn ended with status %q, want %q", got, provider.StatusStalled)
	}
	// Once, not once per deadline: the watchdog is not re-armed by a stall it
	// has already reported.
	refuteMoreEvents(t, session)
}

// The model answering a tool result with another tool call is the conversation
// working, not a stall — and it is half of what the live call actually did.
func TestAToolCallAfterAToolResultIsNotAStall(t *testing.T) {
	f := newFakeGemini(t, answersWith(
		toolCallFrame(functionCallOf("fc_2", "lookup_balance", nil))))
	session := answeredSession(t, f)

	if err := session.SendToolResult("fc_1", `{"balance":"12.30"}`, ""); err != nil {
		t.Fatalf("answer the call: %v", err)
	}

	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone)
	if got := events[1].ToolCallID; got != "fc_2" {
		t.Errorf("the model called %q, want the second call", got)
	}
	// Several deadlines' worth of the silence that follows a call nobody has
	// answered yet, which is silence the model is entitled to.
	time.Sleep(500 * time.Millisecond)
	refuteMoreEvents(t, session)
}

// The ordinary ending: the model takes the answer and speaks.
func TestSpeechAfterAToolResultIsNotAStall(t *testing.T) {
	f := newFakeGemini(t, answersWith(
		modelAudioParts("Your balance is twelve thirty.", encoded(0x01)),
		generationComplete()))
	session := answeredSession(t, f)

	if err := session.SendToolResult("fc_1", `{"balance":"12.30"}`, ""); err != nil {
		t.Fatalf("answer the call: %v", err)
	}

	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeOutputTranscript,
		provider.EventTypeResponseDone)
	time.Sleep(500 * time.Millisecond)
	refuteMoreEvents(t, session)
}

// The gap between the model stopping and the server saying the playback is over
// is seconds long, and it is not a stall either.
func TestTheWatchdogIsSilentBetweenGenerationAndTurnComplete(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)
	session.firstAudioDeadline = 30 * time.Millisecond
	session.deltaStallDeadline = 30 * time.Millisecond
	start(t, session, testConfig())

	f.send(modelAudio(encoded(0x01)))
	f.send(generationComplete())
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeResponseDone)

	time.Sleep(300 * time.Millisecond)
	refuteMoreEvents(t, session)

	f.send(turnComplete())
	refuteMoreEvents(t, session)
}

// A turn the provider accepted and then walked away from is closed out with
// whatever arrived. Without it the flow engine waits for a completion that is
// never coming, and the caller hears silence.
func TestAnAbandonedTurnIsClosedOut(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)
	session.firstAudioDeadline = 40 * time.Millisecond
	session.deltaStallDeadline = 40 * time.Millisecond
	start(t, session, testConfig())

	f.send(modelAudioParts("This is as far as", encoded(0x01)))
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeOutputTranscript,
		provider.EventTypeOutputTranscript,
		provider.EventTypeError,
		provider.EventTypeResponseDone,
	)
	if !events[3].IsFinal {
		t.Error("the words that did arrive were never finished")
	}
	if events[4].IsFatal {
		t.Error("an abandoned turn ended the session; the next turn is still possible")
	}
	if got := events[5].Status; got != provider.StatusStalled {
		t.Errorf("the abandoned turn ended with status %q, want %q", got, provider.StatusStalled)
	}
	refuteMoreEvents(t, session)
}

//
// Turns with nothing in them, and turns nobody can decode.
//

// Proactive audio is permanently enabled on this model: it is allowed to decide
// that the right answer is to say nothing. A silent turn is a conversation, not
// a fault, and failing the call over one would hang up on a caller the model
// simply had no answer for.
func TestATurnThatSaidNothingIsATurnAllTheSame(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	// A turn that began and carried nothing the caller could hear. The other
	// client here treats that as fatal, because on that engine it means a voice
	// the vendor does not have; here it means the model chose silence, and
	// hanging up on a caller for it would be a fault this client invented.
	f.send(modelAudio("not base64!!"))
	f.send(generationComplete())

	events := expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeError,
		provider.EventTypeResponseDone,
	)
	if events[1].IsFatal {
		t.Error("a turn that carried no audio ended the session")
	}
	if got := events[2].Status; got != "" {
		t.Errorf("the turn ended with status %q, want an ordinary ending", got)
	}
	refuteMoreEvents(t, session)

	// And the session is still a session: the next turn is heard as usual.
	f.send(turnComplete())
	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)
}

// A frame that is not JSON at all is not a reason to end a phone call.
func TestAGarbledFrameIsIgnored(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.sendRaw([]byte(`{"serverContent":`))
	refuteMoreEvents(t, session)

	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)
}

//
// Stopping a turn with no way to say so.
//

// There is no cancel on this protocol: the only way to stop a turn is to start
// another one, and doing that from Interrupt would put words into the
// conversation nobody asked for. So the rest of the turn is dropped instead —
// the caller has already stopped hearing it — and the turn is reported as the
// interruption it was rather than as a response that ran to completion.
func TestAKeypressStopsTheTurnWithoutSayingAnything(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

	before := len(f.settledFrames(2))
	if err := session.Interrupt(provider.InterruptReasonDTMF, 320); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.assertNothingFollowed(before)

	// The model goes on producing; none of it reaches the caller.
	f.send(modelAudio(encoded(0x02)))
	f.send(modelAudio(encoded(0x03)))
	refuteMoreEvents(t, session)

	f.send(generationComplete())
	events := expectEvents(t, session, provider.EventTypeInterrupted)
	if got := events[0].InterruptedBy; got != provider.InterruptReasonDTMF {
		t.Errorf("the turn was interrupted by %q, want the keypress", got)
	}
	refuteMoreEvents(t, session)

	// The next turn is heard normally: the fence belonged to the turn it
	// stopped.
	f.send(modelAudio(encoded(0x04)))
	events = expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)
	if got := events[1].Audio; len(got) != 1 || got[0] != 0x04 {
		t.Errorf("the first audio of the new turn is %v, want the new bytes", got)
	}
}

// Who stops what, and what is said about it. The server hears the caller and
// stops on its own, so telling it again is noise it has no frame for; a keypress
// and an application decision have nowhere to go but the fence.
func TestTheInterruptMatrix(t *testing.T) {
	t.Run("the caller's own speech says nothing and stops nothing", func(t *testing.T) {
		f := newFakeGemini(t, acceptSetup)
		session := startedSession(t, f)

		f.send(modelAudio(encoded(0x01)))
		expectEvents(t, session,
			provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

		before := len(f.settledFrames(2))
		if err := session.Interrupt(provider.InterruptReasonSpeech, 200); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.assertNothingFollowed(before)

		// Audio still reaches the caller: the server decides when that turn is
		// over, and until it says so the caller is still being spoken to.
		f.send(modelAudio(encoded(0x02)))
		expectEvents(t, session, provider.EventTypeAudioDelta)
	})

	t.Run("asking twice is asking once", func(t *testing.T) {
		f := newFakeGemini(t, acceptSetup)
		session := startedSession(t, f)

		f.send(modelAudio(encoded(0x01)))
		expectEvents(t, session,
			provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

		if err := session.Interrupt(provider.InterruptReasonSystem, 200); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		if err := session.Interrupt(provider.InterruptReasonDTMF, 240); err != nil {
			t.Fatalf("second interrupt: %v", err)
		}

		f.send(generationComplete())
		events := expectEvents(t, session, provider.EventTypeInterrupted)
		if got := events[0].InterruptedBy; got != provider.InterruptReasonSystem {
			t.Errorf("the turn was interrupted by %q, want the first decision", got)
		}
		refuteMoreEvents(t, session)
	})

	t.Run("with no turn open there is nothing to stop", func(t *testing.T) {
		f := newFakeGemini(t, acceptSetup)
		session := startedSession(t, f)

		before := len(f.settledFrames(2))
		if err := session.Interrupt(provider.InterruptReasonSystem, 0); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.assertNothingFollowed(before)
		refuteMoreEvents(t, session)
	})
}

// A keypress is stopped and then spoken: the fence keeps the caller from hearing
// the rest, and the cue that follows pre-empts the turn for real. The
// interruption the server then reports is still the keypress, not the caller.
func TestAKeypressThatIsThenSpokenIsStillTheKeypress(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

	if err := session.Interrupt(provider.InterruptReasonDTMF, 320); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := session.SendUserText("2"); err != nil {
		t.Fatalf("send user text: %v", err)
	}
	f.awaitMessages("clientContent", 2)

	f.send(interrupted())
	f.send(turnComplete())

	events := expectEvents(t, session, provider.EventTypeInterrupted)
	if got := events[0].InterruptedBy; got != provider.InterruptReasonDTMF {
		t.Errorf("the turn was interrupted by %q, want the keypress", got)
	}
	refuteMoreEvents(t, session)
}

//
// The connection ending on the provider's terms.
//

// goAway is the connection's lifetime running out with the caller on the line.
// It is fatal at once rather than when the deadline passes: every second spent
// waiting for a connection that is going to close is a second of the transfer
// that has to happen not happening.
func TestGoAwayEndsTheSessionWithACauseOfItsOwn(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)

	before := len(f.settledFrames(2))
	f.send(goAwayFrame("12.5s"))

	event := awaitEvent(t, session, provider.EventTypeError)
	if !event.IsFatal {
		t.Error("the connection was ending and the call was told it could continue")
	}
	if got := event.FailureCause; got != provider.FailureCauseSessionExpired {
		t.Errorf("the failure was reported as %q, want the session's lifetime running out", got)
	}
	if got := session.outcome(); got != outcomeSessionExpired {
		t.Errorf("the session ended as %q, want %q", got, outcomeSessionExpired)
	}
	// Nothing more is written to a connection that is closing.
	f.assertNothingFollowed(before)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := events[len(events)-1].Type; got != provider.EventTypeClosed {
		t.Errorf("the last event was %s, want CLOSED", got)
	}
}

// The methods the call actor calls are called from the goroutine draining
// Events. Emitting from any of them would deadlock the call.
func TestTheConsumerGoroutineCanDriveTheSessionWithoutDeadlocking(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
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
			if err := session.SendUserText("2"); err != nil {
				t.Errorf("cue from the consumer: %v", err)
			}
			return
		}
	}()

	// Enough audio to fill the channel several times over if anything emitted
	// from a caller-side method.
	f.send(modelAudio(encoded(0x01)))
	for range 300 {
		f.send(modelAudio(encoded(0x01, 0x02)))
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the consumer goroutine never got through its work")
	}
	// The opening turn, the line, and the cue: the instructions write nothing.
	f.awaitMessages("clientContent", 3)
}
