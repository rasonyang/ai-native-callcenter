// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// #351: a line's turn that calls a tool instead of saying the line, and says
// nothing, is asked for once more with the tool named in its direction. The
// call is never handed out, as before (onLineCall). The live call that found
// it: a non-terminal phase entered through a slot tool, whose instruction said
// "after the line, on any reply, agreement included, call hangup", asked for
// its line, and the line's turn called hangup on the caller's agreement; the
// caller heard nothing until they spoke.

const phaseLine = "From midnight tonight your speed is 500 megabits."

// announceItems is acceptSession plus what qwen does with a caller message the
// client puts in the conversation: it announces the item with an id of its
// own, user_<n> for the n-th item the client created.
func announceItems(f *fakeProvider, message map[string]any) {
	acceptSession(f, message)
	if message["type"] != "conversation.item.create" {
		return
	}
	item, _ := message["item"].(map[string]any)
	if item["type"] != "message" {
		return
	}
	n := len(f.messagesOfType("conversation.item.create"))
	f.send(map[string]any{"type": "conversation.item.created", "item": map[string]any{
		"id": "user_" + strconv.Itoa(n), "type": "message", "role": "user", "content": item["content"],
	}})
}

// startLineCall starts a session on a profile, in a language, and plays its
// greeting out, so the floor is free for a line. The fake names every caller
// message the client creates (announceItems).
func startLineCall(t *testing.T, profile Profile, language string) (*fakeProvider, *Realtime, *syncBuffer) {
	t.Helper()
	f := newFakeProvider(t, announceItems)
	session, logs := loggedSession(t, f, profile)
	cfg := basicConfig()
	cfg.Language = language
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)
	return f, session, logs
}

// callInsteadOfTheLine has the fake create the line's turn and end it having
// called tool and said nothing, and checks the call was not handed out.
func callInsteadOfTheLine(t *testing.T, f *fakeProvider, session *Realtime, tool, callID string) Event {
	t.Helper()
	f.send(map[string]any{"type": "response.created"})
	started := awaitEvent(t, session, EventTypeResponseStarted)
	if !started.IsSpokenLine {
		t.Fatal("the line's turn is not reported as a spoken line")
	}
	itemID := "item_" + callID
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": itemID, "type": "function_call", "call_id": callID}})
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"item_id": itemID, "call_id": callID, "name": tool, "arguments": `{}`})
	f.send(map[string]any{"type": "response.done", "response": map[string]any{
		"status": statusCompleted,
		"output": []any{map[string]any{"id": itemID, "type": "function_call", "call_id": callID}},
	}})
	for {
		event := nextEvent(t, session)
		if event.Type == EventTypeToolCall {
			t.Errorf("the line's turn handed out a call: %+v", event)
		}
		if event.Type == EventTypeResponseDone {
			return started
		}
	}
}

// sayTheLineAskedAgain has the fake create the line asked for again and say it.
func sayTheLineAskedAgain(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	f.send(map[string]any{"type": "response.created"})
	started := awaitEvent(t, session, EventTypeResponseStarted)
	if !started.IsSpokenLine || !started.IsLineAskedAgain {
		t.Errorf("the line asked again is reported as spokenLine=%v askedAgain=%v",
			started.IsSpokenLine, started.IsLineAskedAgain)
	}
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_said", "type": "message"}})
	f.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
	awaitEvent(t, session, EventTypeAudioDelta)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": statusCompleted}})
	awaitEvent(t, session, EventTypeResponseDone)
}

// awaitAgainDirectionNamed waits until the client has read the provider's
// conversation.item.created for the direction of the line asked again and
// learned its id. The fake announces the item from its read loop
// (announceItems), and the test ends the line's turn from its own goroutine:
// without this wait the two frames can reach the client in either order, and
// a direction not yet named cannot be taken out. Only a fake that names caller
// messages ever names it, so a test waits for it only on one.
func awaitAgainDirectionNamed(t *testing.T, session *Realtime) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		isNamed := session.againItemID != ""
		session.mu.Unlock()
		if isNamed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the client never learned the provider's name for the direction of the line asked again")
}

// directionOf is the per-response override a request carries.
func directionOf(t *testing.T, request map[string]any) string {
	t.Helper()
	direction, _ := nested(t, request, "response", "instructions").(string)
	return direction
}

// lineItems are the caller messages put in the conversation that carry line,
// with the id announceItems gave each.
func lineItems(t *testing.T, f *fakeProvider, line string) (texts, ids []string) {
	t.Helper()
	for i, item := range f.messagesOfType("conversation.item.create") {
		if nested(t, item, "item", "type") == "message" && strings.Contains(textOf(t, item), line) {
			texts = append(texts, textOf(t, item))
			ids = append(ids, "user_"+strconv.Itoa(i+1))
		}
	}
	return texts, ids
}

// deletedIDs are the item ids the client has asked to delete, in order.
func deletedIDs(f *fakeProvider) []string {
	var out []string
	for _, d := range f.messagesOfType("conversation.item.delete") {
		id, _ := d["item_id"].(string)
		out = append(out, id)
	}
	return out
}

// assertAnsweredAsRequest checks that request n, the result's turn, is the
// last, carries no line of its own and went out after the result.
func assertAnsweredAsRequest(t *testing.T, f *fakeProvider, n int) {
	t.Helper()
	requests := f.awaitMessages("response.create", n)
	if _, ok := requests[n-1]["response"]; ok {
		t.Errorf("the result's turn carries an override: %v", requests[n-1])
	}
	types := typesOf(f.messages())
	lastItem := len(types) - 1 - indexOf(reversed(types), "conversation.item.create")
	if nthIndex(types, "response.create", n) < lastItem {
		t.Errorf("the result's turn was asked for ahead of the result: %v", types)
	}
}

// The defect, on every Realtime profile: the line's turn calls a tool and says
// nothing. The call is not handed out and is taken out of the conversation
// before the line is asked for again. The second ask names the tool, carries
// the line, and puts its direction in the conversation ahead of its request;
// that item is taken back out once the line's turn ends, and nothing else is
// asked for.
func TestALineWhoseTurnCalledAToolIsAskedForAgain(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile(), GatewayProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, session, logs := startLineCall(t, profile, "en")
			if err := session.SpeakText(phaseLine, false); err != nil {
				t.Fatalf("speak: %v", err)
			}
			first := f.awaitMessages("response.create", 2)[1]
			if started := callInsteadOfTheLine(t, f, session, "hangup", "fc_line"); started.IsLineAskedAgain {
				t.Error("the line's first turn is reported as asked again")
			}

			requests := f.awaitMessages("response.create", 3)
			want := sayExactlyAfterCall(phaseLine, []string{"hangup"}, "en", profile.NeedsToolFreeLineDirection)
			if got := directionOf(t, requests[2]); got != want {
				t.Errorf("asked again with %q, want %q", got, want)
			}
			if got := lineToolChoice(t, requests[2]); got != lineToolChoice(t, first) {
				t.Errorf("asked again with tool_choice %q, the first request had %q", got, lineToolChoice(t, first))
			}
			if deleted := f.awaitMessages("conversation.item.delete", 1); deleted[0]["item_id"] != "item_fc_line" {
				t.Errorf("deleted %v, want the line's call", deleted[0]["item_id"])
			}
			types := typesOf(f.messages())
			if indexOf(types, "conversation.item.delete") > nthIndex(types, "response.create", 3) {
				t.Errorf("the line was asked for again before its turn's call was taken out: %v", types)
			}
			lastItem := len(types) - 1 - indexOf(reversed(types), "conversation.item.create")
			if lastItem > nthIndex(types, "response.create", 3) {
				t.Errorf("the direction went in after the request: %v", types)
			}
			// The first ask's direction goes in the conversation where the
			// profile says so (grok); the second ask's always does.
			texts, ids := lineItems(t, f, phaseLine)
			wantTexts := []string{want}
			if profile.IgnoresOverrideOnceTalking {
				wantTexts = []string{lineRequestDirection(profile, phaseLine), want}
			}
			if strings.Join(texts, "|") != strings.Join(wantTexts, "|") {
				t.Errorf("caller items %q, want %q", texts, wantTexts)
			}

			awaitAgainDirectionNamed(t, session)
			sayTheLineAskedAgain(t, f, session)
			f.awaitMessages("response.create", 3)
			f.awaitMessages("conversation.item.delete", 2)
			if got := deletedIDs(f); got[1] != ids[len(ids)-1] {
				t.Errorf("deleted %v, want the call and then the direction asked again (%s)", got, ids[len(ids)-1])
			}
			if n := strings.Count(logs.String(), "asking for the line once more"); n != 1 {
				t.Errorf("%d log lines about asking again, want 1:\n%s", n, logs.String())
			}
		})
	}
}

// lineRequestDirection is the direction of a line's first ask on a profile.
func lineRequestDirection(profile Profile, line string) string {
	if profile.NeedsToolFreeLineDirection {
		return sayExactlyWithoutTools(line, "en")
	}
	return SayExactly(line, "en")
}

// Asked again, the line's turn calls a tool once more: that call is not handed
// out either, the direction is taken out all the same, nothing is asked for a
// third time, the floor is free, and the next line is asked for at once.
func TestALineAskedAgainThatCallsAToolIsGivenUp(t *testing.T) {
	f, session, logs := startLineCall(t, QwenProfile(), "zh")
	if err := session.SpeakText("今晚零点起网速就是 500 兆。", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 2)
	callInsteadOfTheLine(t, f, session, "hangup", "fc_first")
	f.awaitMessages("response.create", 3)
	awaitAgainDirectionNamed(t, session)
	if started := callInsteadOfTheLine(t, f, session, "hangup", "fc_second"); !started.IsLineAskedAgain {
		t.Error("the line's second turn is not reported as asked again")
	}

	f.awaitMessages("response.create", 3)
	f.awaitMessages("conversation.item.delete", 3)
	if got, want := deletedIDs(f), []string{"item_fc_first", "item_fc_second", "user_2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("deleted %v, want %v", got, want)
	}
	if n := countWarnings(logs, "giving it up"); n != 1 {
		t.Errorf("%d WARN lines about giving the line up, want 1:\n%s", n, logs.String())
	}
	if isRequested, pending, isOwed := floorState(session); isRequested || pending != "" || isOwed {
		t.Errorf("the floor is held after giving up: requested=%v pending=%q owed=%v", isRequested, pending, isOwed)
	}
	if err := session.SpeakText("感谢您的时间，再见。", true); err != nil {
		t.Fatalf("speak: %v", err)
	}
	requests := f.awaitMessages("response.create", 4)
	if got := directionOf(t, requests[3]); !strings.HasSuffix(got, "感谢您的时间，再见。") || strings.Contains(got, "hangup") {
		t.Errorf("the next line is asked for with %q", got)
	}
}

// Only the second ask's item is taken out: a closing line's own direction,
// which qwen puts in the conversation on the first ask, stays where it is, as
// it always has. An item the provider never names cannot be taken out and is
// left.
func TestOnlyTheDirectionAskedAgainIsTakenOut(t *testing.T) {
	t.Run("a closing line's own item stays", func(t *testing.T) {
		f, session, _ := startLineCall(t, QwenProfile(), "en")
		if err := session.SpeakText(phaseLine, true); err != nil {
			t.Fatalf("speak: %v", err)
		}
		f.awaitMessages("response.create", 2)
		callInsteadOfTheLine(t, f, session, "transfer_to_agent", "fc_line")
		f.awaitMessages("response.create", 3)
		texts, ids := lineItems(t, f, phaseLine)
		want := sayExactlyAfterCall(phaseLine, []string{"transfer_to_agent"}, "en", true)
		if len(texts) != 2 || texts[0] != sayExactlyWithoutTools(phaseLine, "en") || texts[1] != want {
			t.Fatalf("caller items %q, want the closing line's own and %q", texts, want)
		}
		awaitAgainDirectionNamed(t, session)
		sayTheLineAskedAgain(t, f, session)
		f.awaitMessages("conversation.item.delete", 2)
		if got := deletedIDs(f); got[0] != "item_fc_line" || got[1] != ids[1] {
			t.Errorf("deleted %v, want the call and %s, not the closing line's own %s", got, ids[1], ids[0])
		}
	})
	t.Run("an item never named stays", func(t *testing.T) {
		f, session, logs := startUnansweredCall(t, QwenProfile(), basicConfig(), createOn(2, 3))
		if err := session.SpeakText(phaseLine, false); err != nil {
			t.Fatalf("speak: %v", err)
		}
		awaitEvent(t, session, EventTypeResponseStarted)
		f.send(map[string]any{"type": "response.function_call_arguments.done",
			"call_id": "fc_line", "name": "hangup", "arguments": `{}`})
		f.send(map[string]any{"type": "response.done",
			"response": map[string]any{"status": statusCompleted}})
		awaitEvent(t, session, EventTypeResponseDone)
		f.awaitMessages("response.create", 3)
		endTurn(t, f, session)
		f.awaitMessages("conversation.item.delete", 0)
		if !strings.Contains(logs.String(), "never named") {
			t.Errorf("no log line about the item never named:\n%s", logs.String())
		}
	})
}

// A terminal phase's line entered through a tool rides in the tool's result
// (the orchestrator's answerToolCall), so the turn that says it is the
// result's, not a line's: a tool it calls is handed out, and nothing is asked
// again.
func TestTheTurnOfAResultCarryingTheLineIsNotAskedAgain(t *testing.T) {
	f, session, _ := startLineCall(t, QwenProfile(), "en")
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"call_id": "fc_1", "name": "transfer_to_agent", "arguments": `{}`})
	awaitEvent(t, session, EventTypeToolCall)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": statusCompleted}})
	awaitEvent(t, session, EventTypeResponseDone)

	output := MergeHint(`{"ok":"1"}`, SayExactly("Putting you through now, goodbye.", "en"))
	if err := session.SendToolResult("fc_1", output); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	if started := awaitEvent(t, session, EventTypeResponseStarted); started.IsSpokenLine || started.IsLineAskedAgain {
		t.Error("the result's turn is reported as a line")
	}
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"call_id": "fc_2", "name": "hangup", "arguments": `{}`})
	if call := awaitEvent(t, session, EventTypeToolCall); call.ToolCallID != "fc_2" {
		t.Errorf("handed out %q, want fc_2", call.ToolCallID)
	}
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": statusCompleted}})
	awaitEvent(t, session, EventTypeResponseDone)
	f.awaitMessages("response.create", 2)
}

// The wait line (#286): asked for over a tool's unanswered call, its turn calls
// that tool again and says nothing. It is asked again with the tool named and,
// on qwen, the note that the tool is already running. The tool's result, which
// arrived while the first turn was being made, is owed past the line asked
// again and answered after it, in a turn of its own.
func TestAWaitLineWhoseTurnCalledTheToolIsAskedForAgain(t *testing.T) {
	for _, profile := range []Profile{QwenProfile(), OpenAIProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, session := startWaitLineCall(t, profile)
			sayWaitLine(t, f, session)

			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			if err := session.SendToolResult("fc_1", `{"ok":"1","answer":"Your order shipped."}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
			awaitOutputs(t, f, 1)
			f.send(map[string]any{"type": "response.function_call_arguments.done",
				"call_id": "fc_dup", "name": "transfer_to_agent", "arguments": `{}`})
			f.send(map[string]any{"type": "response.done",
				"response": map[string]any{"status": statusCompleted}})
			awaitEvent(t, session, EventTypeResponseDone)

			requests := f.awaitMessages("response.create", 3)
			direction := directionOf(t, requests[2])
			want := sayExactlyAfterCall(waitLine, []string{"transfer_to_agent"}, "en",
				profile.NeedsToolFreeLineDirection)
			if direction != want {
				t.Errorf("asked again with %q, want %q", direction, want)
			}
			if isRunningNoted := strings.Contains(direction, "already running"); isRunningNoted != profile.NeedsToolFreeLineDirection {
				t.Errorf("the note that the tool runs is there=%v on %s", isRunningNoted, profile.Name)
			}
			if _, _, isOwed := floorState(session); !isOwed {
				t.Fatal("the line asked again discharged the result's turn")
			}

			sayTheLineAskedAgain(t, f, session)
			assertAnsweredAsRequest(t, f, 4)
		})
	}
}

// A result that arrives once the line has been asked again rides past it in
// the same way as one that arrived before.
func TestAResultArrivingWhileTheLineIsAskedAgainIsAnsweredAfterIt(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	sayWaitLine(t, f, session)
	callInsteadOfTheLine(t, f, session, "transfer_to_agent", "fc_dup")
	f.awaitMessages("response.create", 3)

	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	awaitOutputs(t, f, 1)
	f.awaitMessages("response.create", 3)
	sayTheLineAskedAgain(t, f, session)
	assertAnsweredAsRequest(t, f, 4)
}

// A caller who talks over the line asked again cuts it short like any line:
// the provider is told to stop, the interruption is the caller's, the
// direction is taken out, and nothing is asked for a third time; the
// provider's own reply answers the caller.
func TestACallerWhoTalksOverTheLineAskedAgainTakesTheFloor(t *testing.T) {
	f, session, _ := startLineCall(t, QwenProfile(), "zh")
	if err := session.SpeakText("今晚零点起网速就是 500 兆。", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 2)
	callInsteadOfTheLine(t, f, session, "hangup", "fc_line")
	f.awaitMessages("response.create", 3)
	awaitAgainDirectionNamed(t, session)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_said", "type": "message"}})
	f.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
	awaitEvent(t, session, EventTypeAudioDelta)
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	if err := session.Interrupt(InterruptReasonSpeech, 200); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": statusCancelled}})
	if event := awaitEvent(t, session, EventTypeInterrupted); event.InterruptedBy != InterruptReasonSpeech {
		t.Errorf("interruptedBy = %q, want the caller", event.InterruptedBy)
	}
	f.awaitMessages("response.create", 3)
	_, ids := lineItems(t, f, "今晚零点起网速就是 500 兆。")
	if got := deletedIDs(f); len(ids) != 1 || !strings.Contains(strings.Join(got, ","), ids[0]) {
		t.Errorf("deleted %v, want the direction asked again among them (%v)", got, ids)
	}
}

// A line's turn that called a tool is asked for again only when the caller
// heard nothing of it and it ran to its end with the floor still free.
func TestALineIsNotAskedAgainWhenTheFloorHasMovedOn(t *testing.T) {
	speak := func(t *testing.T, f *fakeProvider, session *Realtime) {
		t.Helper()
		if err := session.SpeakText(phaseLine, false); err != nil {
			t.Fatalf("speak: %v", err)
		}
		f.awaitMessages("response.create", 2)
		f.send(map[string]any{"type": "response.created"})
		awaitEvent(t, session, EventTypeResponseStarted)
	}
	call := func(f *fakeProvider) {
		f.send(map[string]any{"type": "response.function_call_arguments.done",
			"call_id": "fc_line", "name": "hangup", "arguments": `{}`})
	}

	t.Run("the line was heard", func(t *testing.T) {
		f, session, _ := startLineCall(t, QwenProfile(), "en")
		speak(t, f, session)
		f.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
		call(f)
		f.send(map[string]any{"type": "response.done",
			"response": map[string]any{"status": statusCompleted}})
		awaitEvent(t, session, EventTypeResponseDone)
		f.awaitMessages("response.create", 2)
	})
	t.Run("the caller cut it short", func(t *testing.T) {
		f, session, _ := startLineCall(t, QwenProfile(), "en")
		speak(t, f, session)
		call(f)
		f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
		awaitEvent(t, session, EventTypeSpeechStarted)
		if err := session.Interrupt(InterruptReasonSpeech, -1); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.send(map[string]any{"type": "response.done",
			"response": map[string]any{"status": statusCancelled}})
		awaitEvent(t, session, EventTypeInterrupted)
		f.awaitMessages("response.create", 2)
	})
	t.Run("the caller started speaking", func(t *testing.T) {
		f, session, _ := startLineCall(t, QwenProfile(), "en")
		speak(t, f, session)
		call(f)
		f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
		awaitEvent(t, session, EventTypeSpeechStarted)
		f.send(map[string]any{"type": "response.done",
			"response": map[string]any{"status": statusCompleted}})
		awaitEvent(t, session, EventTypeResponseDone)
		f.awaitMessages("response.create", 2)
	})
	t.Run("a newer line", func(t *testing.T) {
		f, session, _ := startLineCall(t, QwenProfile(), "en")
		speak(t, f, session)
		call(f)
		if err := session.SpeakText("Goodbye.", true); err != nil {
			t.Fatalf("speak: %v", err)
		}
		f.awaitMessages("response.cancel", 1)
		// The turn ended on its own before the cancel reached it.
		f.send(map[string]any{"type": "response.done",
			"response": map[string]any{"status": statusCompleted}})
		awaitEvent(t, session, EventTypeResponseDone)
		requests := f.awaitMessages("response.create", 3)
		if got := directionOf(t, requests[2]); got != sayExactlyWithoutTools("Goodbye.", "en") {
			t.Errorf("the request after the turn is %q, want the newer line", got)
		}
	})
}

// The direction asked again names every tool the turn called, once each, in
// the session's language, and ends with the line.
func TestTheDirectionAskedAgainNamesTheTools(t *testing.T) {
	zh := sayExactlyAfterCall("今晚零点起网速就是 500 兆。", []string{"hangup", "record_lead"}, "zh", false)
	for _, words := range []string{"调用了 hangup、record_lead", "还没有听到", "不要调用 hangup、record_lead"} {
		if !strings.Contains(zh, words) {
			t.Errorf("zh direction lacks %q: %q", words, zh)
		}
	}
	if !strings.HasSuffix(zh, "：\n今晚零点起网速就是 500 兆。") || strings.Contains(zh, "已经在运行") {
		t.Errorf("zh direction: %q", zh)
	}
	if noted := sayExactlyAfterCall("请稍等，我查一下。", []string{"kb_search"}, "zh", true); !strings.Contains(noted, "已经在运行") {
		t.Errorf("zh direction with the running note: %q", noted)
	}
	en := sayExactlyAfterCall(phaseLine, []string{"hangup"}, "en", true)
	for _, words := range []string{"you called hangup", "has not heard", "do not call hangup now", "already running"} {
		if !strings.Contains(en, words) {
			t.Errorf("en direction lacks %q: %q", words, en)
		}
	}
	if !strings.HasSuffix(en, ":\n"+phaseLine) {
		t.Errorf("en direction: %q", en)
	}

	f, session, _ := startLineCall(t, QwenProfile(), "en")
	if err := session.SpeakText(phaseLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	for _, call := range [][2]string{{"fc_1", "record_lead"}, {"fc_2", "hangup"}, {"fc_3", "hangup"}} {
		f.send(map[string]any{"type": "response.function_call_arguments.done",
			"call_id": call[0], "name": call[1], "arguments": `{}`})
	}
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": statusCompleted}})
	awaitEvent(t, session, EventTypeResponseDone)
	requests := f.awaitMessages("response.create", 3)
	want := sayExactlyAfterCall(phaseLine, []string{"record_lead", "hangup"}, "en", true)
	if got := directionOf(t, requests[2]); got != want {
		t.Errorf("asked again with %q, want %q", got, want)
	}
}
