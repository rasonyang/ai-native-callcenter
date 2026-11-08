// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	syncatomic "sync/atomic"
	"testing"
	"time"
)

// #310: a response the watchdog gives up is not gone. On the call of
// 2026-11-07 qwen kept the abandoned response open for another 20 seconds and
// refused every request made meanwhile, and a caller turn the provider
// committed and never answered was left to a dead-air prompt that the caller's
// own speech kept cancelling. These tests drive the fake provider through both.

const (
	// stallDeadline is the watchdog's bound in these tests: short, and long
	// enough that the fake's round trip never trips it.
	stallDeadline = 100 * time.Millisecond
	// holdDeadline caps the floor hold for an abandoned response, and
	// turnDeadline the caller-turn watchdog.
	holdDeadline = 400 * time.Millisecond
	turnDeadline = 300 * time.Millisecond
)

// strictProvider is acceptSession plus the refusal a real provider gives a
// response.create made while a response is running. A test says when the
// provider's response is running by flipping isRunning before it sends the
// event, so the refusal depends on the test's own sequence, not on timing.
//
// The fake records a frame before it calls the reply, so a test that saw the
// frame and then flipped isRunning could flip it ahead of the reply's check.
// handled counts the response.create frames whose reply has run, and
// awaitMessages waits for it, so a test never moves on while the verdict on a
// frame it just saw is still being decided.
type strictProvider struct {
	*fakeProvider
	isRunning syncatomic.Bool
	handled   syncatomic.Int32
}

func (s *strictProvider) awaitMessages(messageType string, n int) []map[string]any {
	s.t.Helper()
	got := s.fakeProvider.awaitMessages(messageType, n)
	if messageType == "response.create" {
		deadline := time.Now().Add(2 * time.Second)
		for int(s.handled.Load()) < n && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	return got
}

func newStrictProvider(t *testing.T) *strictProvider {
	t.Helper()
	s := &strictProvider{}
	s.fakeProvider = newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		acceptSession(f, message)
		if message["type"] == "response.create" {
			if s.isRunning.Load() {
				f.send(map[string]any{"type": "error", "error": map[string]any{
					"type": "invalid_request_error", "code": "invalid_value",
					"message": "Cannot create response while another response is in progress.",
				}})
			}
			s.handled.Add(1)
		}
	})
	return s
}

func (s *strictProvider) createResponse(id string) {
	s.isRunning.Store(true)
	s.send(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
}

func (s *strictProvider) finishResponse(id, status string) {
	s.isRunning.Store(false)
	s.send(map[string]any{"type": "response.done",
		"response": map[string]any{"id": id, "status": status}})
}

// startStallCall starts a qwen session whose watchdogs are short and plays the
// greeting out, so the next response is an ordinary one (the opening's is never
// re-asked).
func startStallCall(t *testing.T, f *fakeProvider, options ...callOption) (*Realtime, *syncBuffer) {
	t.Helper()
	session, logs := loggedSession(t, f, QwenProfile())
	session.firstAudioDeadline = stallDeadline
	session.deltaStallDeadline = stallDeadline
	for _, option := range options {
		option(session)
	}
	session.stallHoldDeadline = holdDeadline
	session.responseCreatedDeadline = turnDeadline
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)
	return session, logs
}

type callOption func(*Realtime)

// calmWatchdogs keeps the response watchdog out of the way of a test about the
// caller-turn watchdog, which keeps a response open for a while. It is set
// before Start, which hands the deadlines to the watchdog.
func calmWatchdogs(session *Realtime) {
	session.firstAudioDeadline = time.Minute
	session.deltaStallDeadline = time.Minute
}

// awaitAbandon waits for the consumer to be told the turn was given up and
// returns the ResponseDone it got.
func awaitAbandon(t *testing.T, session *Realtime) Event {
	t.Helper()
	awaitEvent(t, session, EventTypeError)
	return awaitEvent(t, session, EventTypeResponseDone)
}

// eventsUntil returns what the consumer was told before the marker the read
// loop emits when it handles speech_started, which is sent after whatever the
// caller wants checked. The read loop is sequential, so everything it emitted
// for the events before the marker is in the stream before the marker is.
func eventsUntil(t *testing.T, f *fakeProvider, session *Realtime) []Event {
	t.Helper()
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	var seen []Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-session.Events():
			if event.Type == EventTypeSpeechStarted {
				return seen
			}
			seen = append(seen, event)
		case <-deadline:
			t.Fatal("the marker event never arrived")
		}
	}
}

func refuteEvents(t *testing.T, seen []Event, types ...EventType) {
	t.Helper()
	for _, event := range seen {
		for _, bad := range types {
			if event.Type == bad {
				t.Errorf("the consumer was told %s (%q) when it should not have been", event.Type, event.Text)
			}
		}
	}
}

func callerSays(f *fakeProvider, words string) {
	f.send(map[string]any{"type": "conversation.item.input_audio_transcription.completed",
		"transcript": words})
}

// quiet waits out a window in which a request must not be sent. A bounded
// quiet window is how the other tests assert that nothing was sent.
func quiet(f *fakeProvider, messageType string, n int, window time.Duration) {
	f.t.Helper()
	time.Sleep(window)
	if got := len(f.messagesOfType(messageType)); got != n {
		f.t.Fatalf("the client sent %d %q messages, want %d; it sent %v",
			got, messageType, n, typesOf(f.messages()))
	}
}

func TestAStalledResponseWithNoAudioIsCancelledAndItsTurnAskedForOnce(t *testing.T) {
	s := newStrictProvider(t)
	session, logs := startStallCall(t, s.fakeProvider)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	abandoned := awaitAbandon(t, session)
	if !abandoned.IsAbandoned {
		t.Error("the ResponseDone for a given-up response does not say so")
	}
	s.awaitMessages("response.cancel", 1)
	// The floor is held for the response the provider has not let go of.
	s.awaitMessages("response.create", 1)
	if got := countWarnings(logs, "response abandoned"); got != 1 {
		t.Errorf("%d abandon WARN lines, want 1", got)
	}

	s.finishResponse("resp_1", "cancelled")
	s.awaitMessages("response.create", 2)
	seen := eventsUntil(t, s.fakeProvider, session)
	refuteEvents(t, seen, EventTypeInterrupted, EventTypeResponseDone, EventTypeError)
}

func TestAStalledResponseWithAudioIsCancelledButNotAskedForAgain(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	s.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
	awaitEvent(t, session, EventTypeAudioDelta)
	awaitAbandon(t, session)
	s.awaitMessages("response.cancel", 1)

	s.finishResponse("resp_1", "cancelled")
	s.awaitMessages("response.create", 1)

	// The caller speaks and the provider answers nothing: the watchdog asks.
	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	callerSays(s.fakeProvider, "are you there")
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	if got := len(s.messagesOfType("response.create")); got != 1 {
		t.Fatalf("the caller's turn was asked for before its deadline: %d requests", got)
	}
	s.awaitMessages("response.create", 2)
}

// The call of 2026-11-07: the provider keeps the abandoned response open, the
// dead-air cue arrives during the hold, and everything is asked for once, when
// the provider lets go.
func TestACueDuringTheHoldWaitsForTheAbandonedResponseToEnd(t *testing.T) {
	s := newStrictProvider(t)
	session, logs := startStallCall(t, s.fakeProvider)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	awaitAbandon(t, session)

	if err := session.SendUserText("are you there?"); err != nil {
		t.Fatalf("send cue: %v", err)
	}
	s.awaitMessages("conversation.item.create", 2)
	s.awaitMessages("response.create", 1)

	s.finishResponse("resp_1", "cancelled")
	s.awaitMessages("response.create", 2)
	seen := eventsUntil(t, s.fakeProvider, session)
	refuteEvents(t, seen, EventTypeError, EventTypeInterrupted, EventTypeResponseDone)
	if strings.Contains(logs.String(), "level=WARN msg=\"the provider never created") {
		t.Errorf("a request was left to expire:\n%s", logs.String())
	}
}

// The same call when the provider is slower than the hold's cap: our request is
// refused because the abandoned response still runs, and it is neither retried
// nor dropped; the response's own end asks for the turn.
func TestARequestRefusedBecauseTheAbandonedResponseStillRunsIsNotRetried(t *testing.T) {
	s := newStrictProvider(t)
	session, logs := startStallCall(t, s.fakeProvider)
	before := unansweredCounts(t, "qwen")

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	awaitAbandon(t, session)

	// The cap frees the floor and the owed turn is asked for; the provider
	// still holds the response and refuses it.
	s.awaitMessages("response.create", 2)
	// Past the request's own deadline: a request left to expire would be
	// retried by now.
	quiet(s.fakeProvider, "response.create", 2, session.responseCreatedDeadline+100*time.Millisecond)

	s.finishResponse("resp_1", "cancelled")
	s.awaitMessages("response.create", 3)
	s.createResponse("resp_2")
	awaitEvent(t, session, EventTypeResponseStarted)
	s.finishResponse("resp_2", "completed")
	awaitEvent(t, session, EventTypeResponseDone)
	s.awaitMessages("response.create", 3)
	assertUnanswered(t, "qwen", before, logs, map[string]int64{})
	if got := countWarnings(logs, "did not confirm the cancel"); got != 1 {
		t.Errorf("%d cap WARN lines, want 1", got)
	}
}

func TestTheAbandonedResponsesLateAudioIsNotEmittedAndASecondStallDoesNothing(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	awaitAbandon(t, session)

	s.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
	s.send(map[string]any{"type": "response.audio_transcript.delta", "delta": "hello"})
	session.onResponseStalled(true)
	seen := eventsUntil(t, s.fakeProvider, session)
	refuteEvents(t, seen, EventTypeAudioDelta, EventTypeError, EventTypeResponseDone)
	var transcripts int
	for _, event := range seen {
		if event.Type == EventTypeOutputTranscript {
			transcripts++
		}
	}
	if transcripts != 1 {
		t.Errorf("the abandoned response's transcript passed %d times, want 1", transcripts)
	}
	s.awaitMessages("response.cancel", 1)
}

func TestANewResponseDuringTheHoldClearsItAndIsReportedNormally(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	awaitAbandon(t, session)

	s.createResponse("resp_2")
	awaitEvent(t, session, EventTypeResponseStarted)
	s.finishResponse("resp_2", "completed")
	done := awaitEvent(t, session, EventTypeResponseDone)
	if done.IsAbandoned {
		t.Error("the new response's done is reported as abandoned")
	}
	// The provider's new response is the answer the abandoned one owed.
	s.awaitMessages("response.create", 1)
}

func TestACancelRefusedAsNoActiveResponseFreesTheFloorAndAsksOnce(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	awaitAbandon(t, session)
	s.awaitMessages("response.cancel", 1)

	s.isRunning.Store(false)
	s.send(map[string]any{"type": "error", "error": map[string]any{
		"type": "invalid_request_error", "code": "invalid_value",
		"message": "Conversation has no active response",
	}})
	s.awaitMessages("response.create", 2)
	s.createResponse("resp_2")
	awaitEvent(t, session, EventTypeResponseStarted)
	s.finishResponse("resp_2", "completed")
	awaitEvent(t, session, EventTypeResponseDone)
	// The hold's cap finds nothing left to do.
	quiet(s.fakeProvider, "response.create", 2, holdDeadline+100*time.Millisecond)
}

func TestACallerTurnTheProviderNeverAnswersIsAskedForOnce(t *testing.T) {
	s := newStrictProvider(t)
	session, logs := startStallCall(t, s.fakeProvider)

	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	callerSays(s.fakeProvider, "I want to check my plan")
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	if got := len(s.messagesOfType("response.create")); got != 1 {
		t.Fatalf("asked for before the deadline: %d requests", got)
	}
	s.awaitMessages("response.create", 2)
	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	s.finishResponse("resp_1", "completed")
	awaitEvent(t, session, EventTypeResponseDone)
	quiet(s.fakeProvider, "response.create", 2, 2*turnDeadline)
	if got := strings.Count(logs.String(), "the provider never answered the caller's turn"); got != 1 {
		t.Errorf("%d INFO lines, want 1", got)
	}
}

func TestAResponseCreatedBeforeTheDeadlineAnswersTheCallerTurn(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider, calmWatchdogs)

	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	callerSays(s.fakeProvider, "hello")
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	quiet(s.fakeProvider, "response.create", 1, 2*turnDeadline)
}

func TestNewSpeechRestartsTheCallerTurnWatch(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider)

	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	callerSays(s.fakeProvider, "hello")
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	awaitEvent(t, session, EventTypeSpeechStopped)
	awaitEvent(t, session, EventTypeSpeechStarted)
	// The caller is still talking, well past the first deadline.
	quiet(s.fakeProvider, "response.create", 1, 2*turnDeadline)

	callerSays(s.fakeProvider, "hello again")
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	s.awaitMessages("response.create", 2)
}

func TestACallerTurnWithoutWordsIsLeftAsSilence(t *testing.T) {
	for _, transcript := range []string{"", " \n"} {
		t.Run("blank "+transcript, func(t *testing.T) {
			s := newStrictProvider(t)
			startStallCall(t, s.fakeProvider)
			s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
			s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
			callerSays(s.fakeProvider, transcript)
			quiet(s.fakeProvider, "response.create", 1, 2*turnDeadline)
		})
	}
	t.Run("no transcript", func(t *testing.T) {
		s := newStrictProvider(t)
		startStallCall(t, s.fakeProvider)
		s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
		s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
		quiet(s.fakeProvider, "response.create", 1, 2*turnDeadline)
	})
}

func TestWordsThatArriveAfterTheDeadlineAskForTheTurnThen(t *testing.T) {
	s := newStrictProvider(t)
	startStallCall(t, s.fakeProvider)

	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	quiet(s.fakeProvider, "response.create", 1, 2*turnDeadline)
	callerSays(s.fakeProvider, "hello")
	s.awaitMessages("response.create", 2)
}

func TestAToolResultAndAnUnansweredCallerTurnAreOneRequest(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider, calmWatchdogs)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	s.send(map[string]any{"type": "response.function_call_arguments.done",
		"call_id": "fc_1", "name": "lookup", "arguments": `{}`})
	awaitEvent(t, session, EventTypeToolCall)
	if err := session.SendToolResult("fc_1", `{"ok":true}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	callerSays(s.fakeProvider, "hello")
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	// Past the caller-turn deadline, with the response still open.
	quiet(s.fakeProvider, "response.create", 1, 2*turnDeadline)

	s.finishResponse("resp_1", "completed")
	s.awaitMessages("response.create", 2)
}

func TestAVADRefusalAndAnUnansweredCallerTurnAreOneRequest(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider, calmWatchdogs)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	callerSays(s.fakeProvider, "hello")
	s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	sendVADTurnRefused(s.fakeProvider)
	quiet(s.fakeProvider, "response.create", 1, 2*turnDeadline)

	s.finishResponse("resp_1", "completed")
	s.awaitMessages("response.create", 2)
}

// A provider that abandons every turn is asked once per caller turn, however
// many times it does.
func TestRepeatedStallsAreReAskedOncePerCallerTurn(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider)

	for turn := 1; turn <= 3; turn++ {
		s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
		callerSays(s.fakeProvider, "hello")
		s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
		s.createResponse("vad")
		awaitAbandon(t, session)
		s.finishResponse("vad", "cancelled")
		// The re-ask is created and abandoned too.
		s.awaitMessages("response.create", 1+turn)
		s.createResponse("reask")
		awaitAbandon(t, session)
		s.finishResponse("reask", "cancelled")
		s.awaitMessages("response.cancel", 2*turn)
		s.awaitMessages("response.create", 1+turn)
	}
}

// stallDirectly abandons the response now open without waiting for the
// watchdog, so the next response is not itself abandoned while the test runs.
func stallDirectly(t *testing.T, session *Realtime) {
	t.Helper()
	session.onResponseStalled(false)
	awaitAbandon(t, session)
}

// The cancel the abandon sent explains the end of the abandoned response only.
// If the hold ends another way, the reason must not survive to label a
// caller's barge-in on the next response as this client's own cut: the session
// reports no barge-in for those (#493).
func TestTheAbandonsCancelReasonDoesNotLabelALaterBargeIn(t *testing.T) {
	t.Run("cancel refused as no active response", func(t *testing.T) {
		s := newStrictProvider(t)
		session, _ := startStallCall(t, s.fakeProvider, calmWatchdogs)

		s.createResponse("A")
		awaitEvent(t, session, EventTypeResponseStarted)
		stallDirectly(t, session)
		s.awaitMessages("response.cancel", 1)
		s.isRunning.Store(false)
		s.send(map[string]any{"type": "error", "error": map[string]any{
			"type": "invalid_request_error", "code": "invalid_value",
			"message": "Conversation has no active response",
		}})
		s.awaitMessages("response.create", 2)

		s.createResponse("B")
		awaitEvent(t, session, EventTypeResponseStarted)
		s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
		awaitEvent(t, session, EventTypeSpeechStarted)
		s.finishResponse("B", "cancelled")
		event := awaitEvent(t, session, EventTypeInterrupted)
		if event.InterruptedBy != InterruptReasonSpeech {
			t.Errorf("the barge-in is labelled %q, want %q", event.InterruptedBy, InterruptReasonSpeech)
		}
	})
	t.Run("a new response during the hold", func(t *testing.T) {
		s := newStrictProvider(t)
		session, _ := startStallCall(t, s.fakeProvider, calmWatchdogs)

		s.createResponse("A")
		awaitEvent(t, session, EventTypeResponseStarted)
		stallDirectly(t, session)
		s.createResponse("B")
		awaitEvent(t, session, EventTypeResponseStarted)
		s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
		awaitEvent(t, session, EventTypeSpeechStarted)
		s.finishResponse("B", "cancelled")
		event := awaitEvent(t, session, EventTypeInterrupted)
		if event.InterruptedBy != InterruptReasonSpeech {
			t.Errorf("the barge-in is labelled %q, want %q", event.InterruptedBy, InterruptReasonSpeech)
		}
	})
}

// The abandoned response's done, arriving once the provider has moved on, is
// not the end of the response that is open now.
func TestTheLateDoneOfAnAbandonedResponseDoesNotCloseTheOneNowOpen(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider, calmWatchdogs)

	s.createResponse("A")
	awaitEvent(t, session, EventTypeResponseStarted)
	stallDirectly(t, session)
	s.createResponse("B")
	awaitEvent(t, session, EventTypeResponseStarted)

	s.finishResponse("A", "cancelled")
	// Late audio of A is dropped too.
	s.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA", "response_id": "A"})
	seen := eventsUntil(t, s.fakeProvider, session)
	refuteEvents(t, seen, EventTypeResponseDone, EventTypeInterrupted, EventTypeAudioDelta)
	if !session.isResponseOpen.Load() {
		t.Fatal("the abandoned response's done closed the response now open")
	}

	s.finishResponse("B", "completed")
	done := awaitEvent(t, session, EventTypeResponseDone)
	if done.IsAbandoned {
		t.Error("B's own done is reported as abandoned")
	}
}
