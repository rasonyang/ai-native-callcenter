// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
	"time"
)

// A line is words the flow chose, and the turn that says them must not call a
// tool. The live round of #286 found qwen answering the wait line's request
// with a second, identical call to the tool still running (qwen-findings
// W-Q8): the backend was hit twice and the answer said twice.

// lineToolChoice is the tool_choice a request carries in its response object,
// or "" when it carries none.
func lineToolChoice(t *testing.T, request map[string]any) string {
	t.Helper()
	response, _ := request["response"].(map[string]any)
	if _, ok := response["tools"]; ok {
		t.Errorf("a request carries a response-level tools list: %v", request)
	}
	choice, _ := response["tool_choice"].(string)
	return choice
}

// A line's request bars tools where the profile obeys the bar, and only there:
// an engine that refuses the field would lose the line with it (the gateway),
// and one that ignores it gains nothing (qwen). The retry of a dropped line is
// the same request.
func TestALineRequestBarsToolsWhereTheProfileObeysIt(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			want := ""
			if profile.AcceptsResponseToolChoice {
				want = "none"
			}
			for _, isClosing := range []bool{false, true} {
				f, session, _ := startUnansweredCall(t, profile, basicConfig(), createOn())
				if err := session.SpeakText(waitLine, isClosing); err != nil {
					t.Fatalf("speak: %v", err)
				}
				requests := f.awaitMessages("response.create", 3)
				for i, request := range requests[1:] {
					if got := lineToolChoice(t, request); got != want {
						t.Errorf("closing=%v request %d: tool_choice %q, want %q: %v",
							isClosing, i+1, got, want, request)
					}
				}
			}
		})
	}
}

func TestOnlyOpenAIObeysAResponseToolChoice(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile(), DoubaoProfile()} {
		if want := profile.Name == NameOpenAI; profile.AcceptsResponseToolChoice != want {
			t.Errorf("%s: AcceptsResponseToolChoice %v, want %v", profile.Name, profile.AcceptsResponseToolChoice, want)
		}
	}
}

// Every other request leaves the model free to call a tool: the opening, even
// when it carries a line of its own, a cue's reply and a tool result's turn.
func TestOnlyALineRequestBarsTools(t *testing.T) {
	cfg := basicConfig()
	cfg.OpeningText = "Thanks for calling NovaNet."
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)

	if err := session.SendUserText("The caller pressed 1."); err != nil {
		t.Fatalf("cue: %v", err)
	}
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"call_id": "fc_1", "name": "lookup_balance", "arguments": `{}`})
	awaitEvent(t, session, EventTypeToolCall)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)

	if err := session.SendToolResult("fc_1", `{"balance":"12.50"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	requests := f.awaitMessages("response.create", 3)
	for i, request := range requests {
		if got := lineToolChoice(t, request); got != "" {
			t.Errorf("request %d carries tool_choice %q: %v", i, got, request)
		}
	}
}

// lineTurnEvents ends the line's turn the fake created, with a function call
// in it, and returns what the client reported until the turn ended. isHeard
// gives the turn audio before the call, as qwen saying the line and then
// calling the running tool again does (W-Q8).
func lineTurnEvents(t *testing.T, f *fakeProvider, session *Realtime, status string, isHeard bool) []Event {
	t.Helper()
	f.send(map[string]any{"type": "response.created"})
	if started := awaitEvent(t, session, EventTypeResponseStarted); !started.IsSpokenLine {
		t.Fatal("the line's response is not reported as a spoken line")
	}
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_line", "type": "message"}})
	if isHeard {
		f.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
	}
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_dup", "type": "function_call", "call_id": "fc_dup"}})
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"item_id": "item_dup", "call_id": "fc_dup", "name": "transfer_to_agent", "arguments": `{}`})
	f.send(map[string]any{"type": "response.done", "response": map[string]any{
		"status": status,
		"output": []any{
			map[string]any{"id": "item_line", "type": "message"},
			map[string]any{"id": "item_dup", "type": "function_call", "call_id": "fc_dup"},
		},
	}})
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

// A function call made in a line's turn is never handed out, whatever the
// profile and however the turn ends, and it is taken out of the conversation
// so no later turn reads it as a call still waiting for its output. The call
// the line was said over is answered as usual, and a call in the next turn is
// handed out as usual. The turn that runs to its end said the line first: one
// that said nothing is asked for again (line_asked_again_test.go).
func TestACallMadeInALinesTurnIsNeverHandedOut(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile()} {
		for _, status := range []string{statusCompleted, statusCancelled} {
			t.Run(profile.Name+"/"+status, func(t *testing.T) {
				f, session := startWaitLineCall(t, profile)
				sayWaitLine(t, f, session)

				for _, event := range lineTurnEvents(t, f, session, status, status == statusCompleted) {
					if event.Type == EventTypeToolCall {
						t.Errorf("the line's turn handed out a call: %+v", event)
					}
				}
				// A line's turn cut short before any of its audio arrived is
				// unheard, so its words come out too (removeUnheardTurn).
				want := map[string]bool{"item_dup": true}
				if status == statusCancelled {
					want["item_line"] = true
				}
				deletes := f.awaitMessages("conversation.item.delete", len(want))
				for _, d := range deletes {
					if !want[d["item_id"].(string)] {
						t.Errorf("deleted %v, want %v", d["item_id"], want)
					}
					delete(want, d["item_id"].(string))
				}
				if len(want) != 0 {
					t.Errorf("not deleted: %v", want)
				}

				if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
					t.Fatalf("send tool result: %v", err)
				}
				f.awaitMessages("response.create", 3)
				f.send(map[string]any{"type": "response.created"})
				if started := awaitEvent(t, session, EventTypeResponseStarted); started.IsSpokenLine {
					t.Error("the answer's response is reported as a spoken line")
				}
				f.send(map[string]any{"type": "response.function_call_arguments.done",
					"call_id": "fc_next", "name": "transfer_to_agent", "arguments": `{}`})
				if call := awaitEvent(t, session, EventTypeToolCall); call.ToolCallID != "fc_next" {
					t.Errorf("handed out %q, want fc_next", call.ToolCallID)
				}
				f.send(map[string]any{"type": "response.done",
					"response": map[string]any{"status": "completed"}})
				awaitEvent(t, session, EventTypeResponseDone)
				f.awaitMessages("conversation.item.delete", len(deletes))
			})
		}
	}
}

// nextEvent waits for the next event of any type.
func nextEvent(t *testing.T, session *Realtime) Event {
	t.Helper()
	select {
	case event, ok := <-session.Events():
		if !ok {
			t.Fatal("the event stream closed")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
	}
	return Event{}
}

func TestOnlyQwenNeedsAToolFreeLineDirection(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile(), DoubaoProfile()} {
		if want := profile.Name == NameQwen; profile.NeedsToolFreeLineDirection != want {
			t.Errorf("%s: NeedsToolFreeLineDirection %v, want %v", profile.Name, profile.NeedsToolFreeLineDirection, want)
		}
	}
}

// A line on a profile that needs it carries the direction that bans tools,
// in the override and in the conversation item where the line goes there
// too; every other profile keeps SayExactly. The opening keeps SayExactly
// everywhere.
func TestALineCarriesTheToolFreeDirectionWhereTheProfileNeedsIt(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			want := SayExactly(waitLine, "en")
			if profile.NeedsToolFreeLineDirection {
				want = sayExactlyWithoutTools(waitLine, "en")
			}
			for _, isClosing := range []bool{false, true} {
				cfg := basicConfig()
				cfg.OpeningText = "Thanks for calling NovaNet."
				f, session, _ := startUnansweredCall(t, profile, cfg, createOn())
				if err := session.SpeakText(waitLine, isClosing); err != nil {
					t.Fatalf("speak: %v", err)
				}
				requests := f.awaitMessages("response.create", 3)
				opening, _ := nested(t, requests[0], "response", "instructions").(string)
				if opening != SayExactly(cfg.OpeningText, "en") {
					t.Errorf("the opening's direction changed: %q", opening)
				}
				for i, request := range requests[1:] {
					if got, _ := nested(t, request, "response", "instructions").(string); got != want {
						t.Errorf("closing=%v request %d: direction %q, want %q", isClosing, i+1, got, want)
					}
				}
				for _, item := range f.messagesOfType("conversation.item.create") {
					text, _ := nested(t, item, "item", "content").([]any)[0].(map[string]any)["text"].(string)
					if strings.Contains(text, waitLine) && text != want {
						t.Errorf("closing=%v: the line's item carries %q, want %q", isClosing, text, want)
					}
				}
			}
		})
	}
}

func TestTheToolFreeDirectionIsInTheSessionsLanguage(t *testing.T) {
	zh := sayExactlyWithoutTools("请稍等，我查一下。", "zh")
	if !strings.Contains(zh, "不要调用任何工具") || !strings.HasSuffix(zh, "\n请稍等，我查一下。") {
		t.Errorf("zh direction: %q", zh)
	}
	en := sayExactlyWithoutTools(waitLine, "en")
	if !strings.Contains(en, "call no tool") || !strings.HasSuffix(en, "\n"+waitLine) {
		t.Errorf("en direction: %q", en)
	}
}
