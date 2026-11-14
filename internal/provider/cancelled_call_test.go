// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
)

// #357: a response this client has decided to cancel is an ordinary response
// until the provider answers the cancel, and the call it finishes in that
// window must not reach the flow. The shape that was measured: a slot tool's
// result moves the call into a phase with a mid-call line, the turn that
// result owed (B) calls hangup, and the line's cancel arrives too late.

const cancelledLine = "Thanks, I have noted that down."

// startTurnBForALine plays a call to the point where a slot tool's result has
// been answered, the turn it owed (B) is open, and the phase's line has been
// asked for, so B is being cancelled for it. It returns after the cancel was
// sent.
func startTurnBForALine(t *testing.T, profile Profile) (*fakeProvider, *Realtime) {
	t.Helper()
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, profile)
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)

	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	// B is requested; the line waits for it to exist.
	f.awaitMessages("response.create", 2)
	if err := session.SpeakText(cancelledLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.awaitMessages("response.cancel", 1)
	return f, session
}

// callInTurn has the open response begin and finish a function call.
func callInTurn(f *fakeProvider, itemID, callID, name string) {
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": itemID, "type": "function_call", "call_id": callID}})
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"item_id": itemID, "call_id": callID, "name": name, "arguments": `{}`})
}

// endTurnAndCollect ends the open response and returns what the client
// reported until it was over.
func endTurnAndCollect(t *testing.T, f *fakeProvider, session *Realtime, status string,
	output ...any) []Event {
	t.Helper()
	response := map[string]any{"status": status}
	if len(output) > 0 {
		response["output"] = output
	}
	f.send(map[string]any{"type": "response.done", "response": response})
	end := EventTypeResponseDone
	if status == statusCancelled {
		end = EventTypeInterrupted
	}
	var events []Event
	for {
		event := nextEvent(t, session)
		events = append(events, event)
		if event.Type == end {
			return events
		}
	}
}

func refuteToolCalls(t *testing.T, events []Event) {
	t.Helper()
	for _, event := range events {
		if event.Type == EventTypeToolCall {
			t.Errorf("a call of a cancelled response was handed out: %+v", event)
		}
	}
}

func toolCalls(events []Event) []string {
	var ids []string
	for _, event := range events {
		if event.Type == EventTypeToolCall {
			ids = append(ids, event.ToolCallID)
		}
	}
	return ids
}

// deletedItems waits for n item deletions and returns their ids.
func deletedItems(f *fakeProvider, n int) map[string]bool {
	ids := make(map[string]bool)
	for _, message := range f.awaitMessages("conversation.item.delete", n) {
		ids[message["item_id"].(string)] = true
	}
	return ids
}

func assertLineAskedFor(t *testing.T, f *fakeProvider) {
	t.Helper()
	requests := f.awaitMessages("response.create", 3)
	direction, _ := nested(t, requests[2], "response", "instructions").(string)
	if !strings.Contains(direction, cancelledLine) {
		t.Errorf("the request after the turn is not the line: %q", direction)
	}
}

// The measured shape: B calls hangup before the cancel reaches it. Nothing is
// handed out, the item is taken out of the conversation when B ends, and the
// line is asked for afterwards. The second case has the fake read the cancel
// first, as a provider that has the frame and still finishes the call does.
func TestACallFinishedInATurnTheLineCancelsIsNotHandedOut(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile()} {
		for _, isCancelRead := range []bool{false, true} {
			name := profile.Name + "/cancel-unread"
			if isCancelRead {
				name = profile.Name + "/cancel-read"
			}
			t.Run(name, func(t *testing.T) {
				f := newFakeProvider(t, acceptSession)
				session := testSession(t, f, profile)
				if err := session.Start(t.Context(), basicConfig()); err != nil {
					t.Fatalf("start: %v", err)
				}
				awaitEvent(t, session, EventTypeSessionReady)
				openATurnThatCallsATool(t, f, session)
				if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
					t.Fatalf("send tool result: %v", err)
				}
				f.send(map[string]any{"type": "response.done",
					"response": map[string]any{"status": "completed"}})
				awaitEvent(t, session, EventTypeResponseDone)
				f.awaitMessages("response.create", 2)
				if err := session.SpeakText(cancelledLine, false); err != nil {
					t.Fatalf("speak: %v", err)
				}
				f.send(map[string]any{"type": "response.created"})
				awaitEvent(t, session, EventTypeResponseStarted)
				if isCancelRead {
					f.awaitMessages("response.cancel", 1)
				}

				callInTurn(f, "item_hangup", "fc_hangup", "hangup")
				events := endTurnAndCollect(t, f, session, statusCancelled,
					map[string]any{"id": "item_hangup", "type": "function_call", "call_id": "fc_hangup"})
				refuteToolCalls(t, events)

				if got := deletedItems(f, 1); !got["item_hangup"] {
					t.Errorf("deleted %v, want item_hangup", got)
				}
				assertLineAskedFor(t, f)
			})
		}
	}
}

// The cancel lost the race: B completed and the provider refuses the cancel.
// The call it made was still held, and is still an unanswered item the next
// turn would read, so it is taken out all the same.
func TestAHeldCallIsTakenOutWhenTheCancelLostTheRace(t *testing.T) {
	f, session := startTurnBForALine(t, QwenProfile())

	callInTurn(f, "item_hangup", "fc_hangup", "hangup")
	events := endTurnAndCollect(t, f, session, statusCompleted,
		map[string]any{"id": "item_hangup", "type": "function_call", "call_id": "fc_hangup"})
	refuteToolCalls(t, events)
	f.send(map[string]any{"type": "error", "error": map[string]any{
		"type": "invalid_request_error", "code": "invalid_value",
		"message": "Conversation has no active response"}})

	if got := deletedItems(f, 1); !got["item_hangup"] {
		t.Errorf("deleted %v, want item_hangup", got)
	}
	assertLineAskedFor(t, f)
}

func TestEveryCallOfACancelledTurnIsHeldAndTakenOut(t *testing.T) {
	f, session := startTurnBForALine(t, OpenAIProfile())

	callInTurn(f, "item_a", "fc_a", "hangup")
	callInTurn(f, "item_b", "fc_b", "transfer_to_agent")
	events := endTurnAndCollect(t, f, session, statusCancelled,
		map[string]any{"id": "item_a", "type": "function_call", "call_id": "fc_a"},
		map[string]any{"id": "item_b", "type": "function_call", "call_id": "fc_b"})
	refuteToolCalls(t, events)

	got := deletedItems(f, 2)
	if !got["item_a"] || !got["item_b"] {
		t.Errorf("deleted %v, want item_a and item_b", got)
	}
	assertLineAskedFor(t, f)
}

// With no line pending and no cancel sent, a call is the flow's as ever and its
// item stays: the answer to it is on its way.
func TestACallWithNoCancelSentIsHandedOutAndKept(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	callInTurn(f, "item_hangup", "fc_hangup", "hangup")
	call := awaitEvent(t, session, EventTypeToolCall)
	if call.ToolCallID != "fc_hangup" {
		t.Errorf("handed out %q, want fc_hangup", call.ToolCallID)
	}
	events := endTurnAndCollect(t, f, session, statusCompleted,
		map[string]any{"id": "item_hangup", "type": "function_call", "call_id": "fc_hangup"})
	refuteToolCalls(t, events)
	f.refuteMessage("conversation.item.delete")
}

// A call the response finished before the cancel was decided was handed out
// and is answered as usual: it stays, and only the later call is held.
func TestACallFinishedBeforeTheCancelWasDecidedIsKept(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	openATurnThatCallsATool(t, f, session)
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	callInTurn(f, "item_early", "fc_early", "record_answer")
	if call := awaitEvent(t, session, EventTypeToolCall); call.ToolCallID != "fc_early" {
		t.Fatalf("handed out %q, want fc_early", call.ToolCallID)
	}
	// Now the phase's line is asked for over the open turn.
	if err := session.SpeakText(cancelledLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	callInTurn(f, "item_late", "fc_late", "hangup")
	events := endTurnAndCollect(t, f, session, statusCancelled,
		map[string]any{"id": "item_early", "type": "function_call", "call_id": "fc_early"},
		map[string]any{"id": "item_late", "type": "function_call", "call_id": "fc_late"})
	refuteToolCalls(t, events)

	got := deletedItems(f, 1)
	if !got["item_late"] || got["item_early"] {
		t.Errorf("deleted %v, want item_late alone", got)
	}
	assertLineAskedFor(t, f)
}

// A line deferred while the caller speaks goes out when the response the
// provider's own turn detection creates exists, and cancels it then
// (onResponseCreated): that response's calls are held too.
func TestACallInATurnCancelledForALineDeferredBehindTheCallerIsHeld(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	if err := session.SpeakText(cancelledLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 0)

	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.awaitMessages("response.cancel", 1)

	callInTurn(f, "item_hangup", "fc_hangup", "hangup")
	events := endTurnAndCollect(t, f, session, statusCancelled,
		map[string]any{"id": "item_hangup", "type": "function_call", "call_id": "fc_hangup"})
	refuteToolCalls(t, events)
	if got := deletedItems(f, 1); !got["item_hangup"] {
		t.Errorf("deleted %v, want item_hangup", got)
	}
	assertLineAskedFor(t, f)
}

// A barge-in or a keypress cancel opens the same window: the cancel needs a
// round trip, and a call finished in it is not handed out. Where the provider
// cancels on speech itself, this client sends nothing and the rule does not
// apply; a keypress is always ours.
func TestACallFinishedAfterAnInterruptWasSentIsHeld(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile Profile
		reason  InterruptReason
	}{
		{"qwen/speech", QwenProfile(), InterruptReasonSpeech},
		{"qwen/keypress", QwenProfile(), InterruptReasonDTMF},
		{"grok/keypress", GrokProfile(), InterruptReasonDTMF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, tt.profile)
			if err := session.Start(t.Context(), basicConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)

			if err := session.Interrupt(tt.reason, -1); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			f.awaitMessages("response.cancel", 1)
			callInTurn(f, "item_hangup", "fc_hangup", "hangup")
			events := endTurnAndCollect(t, f, session, statusCancelled,
				map[string]any{"id": "item_hangup", "type": "function_call", "call_id": "fc_hangup"})
			refuteToolCalls(t, events)
			if got := deletedItems(f, 1); !got["item_hangup"] {
				t.Errorf("deleted %v, want item_hangup", got)
			}
		})
	}
}

// Where the provider hears the caller itself, no cancel is sent by this
// client, so a call in that window is not held by this rule.
func TestACallAfterAnInterruptTheProviderHeardItselfIsNotHeld(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, GrokProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	if err := session.Interrupt(InterruptReasonSpeech, -1); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessages("response.cancel", 0)
	callInTurn(f, "item_x", "fc_x", "lookup")
	if call := awaitEvent(t, session, EventTypeToolCall); call.ToolCallID != "fc_x" {
		t.Errorf("handed out %q, want fc_x", call.ToolCallID)
	}
}

// The watchdog's cancel of a stalled response is not this rule's: the provider
// has not said the response is over, a late call is still handed out and its
// item is not deleted (removeUnfinishedCalls).
func TestALateCallOfAStalledResponseIsStillHandedOut(t *testing.T) {
	s := newStrictProvider(t)
	session, _ := startStallCall(t, s.fakeProvider)

	s.createResponse("resp_1")
	awaitEvent(t, session, EventTypeResponseStarted)
	awaitAbandon(t, session)
	s.awaitMessages("response.cancel", 1)

	callInTurn(s.fakeProvider, "item_late", "fc_late", "lookup")
	if call := awaitEvent(t, session, EventTypeToolCall); call.ToolCallID != "fc_late" {
		t.Errorf("handed out %q, want fc_late", call.ToolCallID)
	}
}
