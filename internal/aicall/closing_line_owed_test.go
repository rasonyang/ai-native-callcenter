// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Issue #207: the tool result that carries the closing line can be answered
// with a second bare hangup and no audio. That turn is not the line.

// owedLineFlow ends in a terminal phase whose line renders a slot and holds an
// emoji, so the tests run the real rendering of the line.
const owedLineFlow = `{
	"id": "owed-line-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["hangup", "take_message"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "hangup", "target": "finish"},
			{"on": "TOOL_RESULT", "tool": "take_message", "target": "noted"}
		]
	},
	"tools": {
		"note_name": {"kind": "slot", "slot": "name", "description": "record the caller's name"}
	},
	"nodes": {
		"welcome": {"instruction": "Greet the caller.", "tools": ["note_name"]},
		"noted": {
			"instruction": "Ask whether there is anything else.",
			"announce": {"en": "I have taken your message.", "zh": "您的留言已记录。"},
			"tools": []
		},
		"finish": {
			"instruction": "Say goodbye.",
			"announce": {
				"en": "Thank you {slots.name}, goodbye 👋",
				"zh": "谢谢您{slots.name}，再见 👋"
			},
			"tools": [], "isTerminal": true
		}
	}
}`

var owedLines = map[string]string{
	"en": "Thank you Ann, goodbye 👋",
	"zh": "谢谢您Ann，再见 👋",
}

func startOwedLinePath(t *testing.T, profile provider.Profile, lang string,
	graceCap time.Duration) (*toolPath, *syncBuffer) {
	t.Helper()
	sink := &syncBuffer{}
	h := startToolPathFull(t, profile, lang, owedLineFlow, "",
		map[string]any{"name": "Ann"}, sink)
	if graceCap > 0 {
		h.actions.mu.Lock()
		h.actions.graceCap = graceCap
		h.actions.mu.Unlock()
	}
	return h, sink
}

// bareHangup has the model answer with a turn that only calls hangup, as the
// n-th tool call, and waits for the answer. It returns that answer.
func (h *toolPath) bareHangup(t *testing.T, n int) toolResult {
	t.Helper()
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_" + string(rune('0'+n)),
		ToolName: flow.ToolHangup, ToolArgs: `{}`,
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if results := h.model.recordedToolResults(); len(results) >= n {
			time.Sleep(30 * time.Millisecond)
			return results[n-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("hangup number %d was never answered", n)
	return toolResult{}
}

func (h *toolPath) stayUp(t *testing.T, why string) {
	t.Helper()
	select {
	case <-h.session.done:
		t.Fatal(why)
	case <-time.After(150 * time.Millisecond):
	}
}

// The first turn after the tool result has no audio and calls hangup again:
// the call stays up, the model is asked once for the line, and the call ends
// on the playback of the next turn that has audio.
func TestAnAudioLessTurnAfterTheLineIsAskedForIsNotTheLine(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h, _ := startOwedLinePath(t, provider.QwenProfile(), lang, 0)
			line := owedLines[lang]

			first := h.callTool(t, flow.ToolHangup, `{}`)
			if got, want := hintOf(t, first.output), provider.SayExactly(line, lang); got != want {
				t.Fatalf("hint = %q, want the rendered line %q", got, want)
			}
			h.endResponse() // the turn that carried the tool call; no audio

			second := h.bareHangup(t, 2)
			if !strings.Contains(second.output, "CALL_ENDING") {
				t.Fatalf("second hangup answered %s, want a CALL_ENDING refusal", second.output)
			}
			if hint := hintOf(t, second.output); !strings.Contains(hint, "closing") &&
				!strings.Contains(hint, "结束语") {
				t.Errorf("refusal hint = %q, want it to ask for the closing line", hint)
			}
			h.endResponse() // the audio-less turn that carried the second hangup
			h.stayUp(t, "the call ended on a turn with no audio, the line unsaid")
			if !h.actions.isArmed() {
				t.Fatal("the ending was disarmed")
			}

			// The model speaks.
			h.playTurn()
			h.awaitCallEnded(t, 2*time.Second)

			if got := h.model.spokenLines(); len(got) != 0 {
				t.Errorf("spoken lines = %v, want none: the line is asked for in the result", got)
			}
			if n := len(h.model.recordedToolResults()); n != 2 {
				t.Errorf("tool results = %d, want 2", n)
			}
		})
	}
}

// The model never speaks again: the cap ends the call once, and says why.
func TestAModelThatNeverSpeaksTheOwedLineEndsAtTheCapWithAWarning(t *testing.T) {
	h, sink := startOwedLinePath(t, provider.QwenProfile(), "en", 500*time.Millisecond)

	h.callTool(t, flow.ToolHangup, `{}`)
	h.endResponse()
	h.bareHangup(t, 2)
	h.endResponse()
	h.stayUp(t, "the call ended before the cap on a turn with no audio")

	h.awaitCallEnded(t, 3*time.Second)
	if n := strings.Count(sink.String(), "hanging up after the farewell"); n != 1 {
		t.Errorf("the hangup ran %d times, want once", n)
	}
	if !strings.Contains(sink.String(), "no closing line") ||
		!strings.Contains(sink.String(), "level=WARN") {
		t.Errorf("log lacks the no-closing-line warning:\n%s", sink.String())
	}
}

// The paraphrase case is unchanged: the next turn has audio, with other words,
// and ends the call.
func TestAParaphrasedLineStillEndsTheCall(t *testing.T) {
	h, _ := startOwedLinePath(t, provider.QwenProfile(), "en", 0)
	h.callTool(t, flow.ToolHangup, `{}`)
	h.endResponse()
	h.playTurn()
	h.awaitCallEnded(t, 2*time.Second)
}

// A model that insists on hangup after the one refusal gets its hangup: it
// ends on the playback of that turn, not on the cap.
func TestAModelThatInsistsOnHangupAfterTheRefusalGetsIt(t *testing.T) {
	h, sink := startOwedLinePath(t, provider.QwenProfile(), "en", 0)
	h.callTool(t, flow.ToolHangup, `{}`)
	h.endResponse()
	if r := h.bareHangup(t, 2); !strings.Contains(r.output, "CALL_ENDING") {
		t.Fatalf("second hangup answered %s, want a refusal", r.output)
	}
	h.endResponse()
	h.stayUp(t, "the refused hangup's turn ended the call")

	third := h.bareHangup(t, 3)
	if strings.Contains(third.output, "CALL_ENDING") {
		t.Fatalf("third hangup answered %s, want it accepted", third.output)
	}
	h.endResponse()
	h.awaitCallEnded(t, 2*time.Second)
	if !strings.Contains(sink.String(), "insisted on hangup") {
		t.Errorf("log lacks the insisted-on-hangup warning:\n%s", sink.String())
	}
}

// Several tool results of one response still ask for one response (W-Q6), and
// the line is owed after them.
func TestSeveralToolsThenHangupAskForOneResponseAndOweTheLine(t *testing.T) {
	h, _ := startOwedLinePath(t, provider.QwenProfile(), "en", 0)
	answers := h.callTools(t,
		[2]string{flow.ToolTakeMessage, `{"message":"a"}`},
		[2]string{flow.ToolTakeMessage, `{"message":"b"}`},
		[2]string{flow.ToolHangup, `{}`})
	h.endResponse()
	if len(answers) != 3 {
		t.Fatalf("answers = %d, want 3", len(answers))
	}
	if got := h.model.spokenLines(); len(got) != 0 {
		t.Errorf("spoken lines = %v, want none: the line rides in the last result", got)
	}
	if got := hintOf(t, answers[2].output); got != provider.SayExactly(owedLines["en"], "en") {
		t.Errorf("last hint = %q, want the line", got)
	}
	h.actions.mu.Lock()
	isOwed := h.actions.isLineOwed
	h.actions.mu.Unlock()
	if !isOwed {
		t.Error("the closing line is not owed")
	}
}

// Doubao says its line itself, and a flow that ends a call without a line owes
// nothing: neither is held back.
func TestNoLineOwedOnDoubaoOrWithoutALine(t *testing.T) {
	h, _ := startOwedLinePath(t, provider.DoubaoProfile(), "en", 0)
	h.callTool(t, flow.ToolHangup, `{}`)
	h.actions.mu.Lock()
	isOwed := h.actions.isLineOwed
	h.actions.mu.Unlock()
	if isOwed {
		t.Error("a line is owed on doubao")
	}

	h2 := startToolPathWith(t, provider.QwenProfile(), "en", strings.Replace(owedLineFlow,
		`"announce": {
				"en": "Thank you {slots.name}, goodbye 👋",
				"zh": "谢谢您{slots.name}，再见 👋"
			},`, "", 1))
	h2.callTool(t, flow.ToolHangup, `{}`)
	h2.actions.mu.Lock()
	isOwed = h2.actions.isLineOwed
	h2.actions.mu.Unlock()
	if isOwed {
		t.Error("a line is owed in a phase without one")
	}
	if !h2.actions.isArmed() {
		t.Fatal("the ending was not armed")
	}
	// An audio-less turn ends it, as before.
	h2.endResponse()
	h2.bareHangup(t, 2)
	h2.endResponse()
	h2.awaitCallEnded(t, 2*time.Second)
}

// Unit rules: a transfer and a limit-locked ending owe no line, and a spoken
// turn clears the debt.
func TestOnlyAHangupIntoALineOwesIt(t *testing.T) {
	t.Run("transfer", func(t *testing.T) {
		actions, session, _ := testActions(t, &fakeSwitch{})
		actions.armAs(t.Context(), true, func() {})
		actions.expectClosingLine("")
		if actions.isLineOwed {
			t.Fatal("a transfer owes a closing line")
		}
		armedTurn := session.currentTurn()
		actions.onPlaybackDone(armedTurn+1, false)
		if actions.isArmed() {
			t.Fatal("the transfer did not run on an audio-less playback, as before")
		}
	})
	t.Run("limit-locked", func(t *testing.T) {
		actions, _, _ := testActions(t, &fakeSwitch{})
		actions.arm(t.Context(), func() {})
		actions.expectClosingLine("")
		actions.lockEnding()
		if actions.isLineOwed {
			t.Fatal("a limit-decided ending owes a closing line")
		}
	})
	t.Run("spoken", func(t *testing.T) {
		actions, session, _ := testActions(t, &fakeSwitch{})
		fired := make(chan struct{})
		actions.arm(t.Context(), func() { close(fired) })
		actions.expectClosingLine("")
		armedTurn := session.currentTurn()
		actions.onTurnDone(armedTurn+1, false, false)
		actions.onBargeIn(0)
		neverFired(t, fired, "an audio-less turn counted as the line")
		actions.onTurnDone(armedTurn+2, false, true)
		actions.onPlaybackDone(armedTurn+2, true)
		select {
		case <-fired:
		case <-time.After(time.Second):
			t.Fatal("the spoken line did not run the ending")
		}
	})
}

// The turn that arms the ending may speak first ("Great, noted.") and then call
// the tools. Its audio is not the line: the next turn, a bare hangup, is still
// refused once, and the call ends on the next turn that has audio.
func TestATurnThatSpokeBeforeItsToolsDoesNotCountAsTheLine(t *testing.T) {
	h, _ := startOwedLinePath(t, provider.QwenProfile(), "en", 0)

	// Turn N: audio, then the hangup, then the end of the turn.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2),
	}
	h.model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_1", ToolName: flow.ToolHangup, ToolArgs: `{}`,
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(h.model.recordedToolResults()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	h.endResponse()
	time.Sleep(100 * time.Millisecond)
	h.stayUp(t, "the call ended before the line")

	second := h.bareHangup(t, 2)
	if !strings.Contains(second.output, "CALL_ENDING") {
		t.Fatalf("second hangup answered %s, want a refusal", second.output)
	}
	h.endResponse()
	h.stayUp(t, "the audio-less turn ended the call")

	h.playTurn()
	h.awaitCallEnded(t, 2*time.Second)
}

// The same, and the model insists: the third hangup is accepted and the call
// ends on that turn's playback, well before the cap.
func TestAModelThatInsistsAfterASpokenArmingTurnEndsWithoutWaitingForTheCap(t *testing.T) {
	h, _ := startOwedLinePath(t, provider.QwenProfile(), "en", 8*time.Second)

	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2),
	}
	h.model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_1", ToolName: flow.ToolHangup, ToolArgs: `{}`,
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(h.model.recordedToolResults()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	h.endResponse()
	time.Sleep(100 * time.Millisecond)

	if r := h.bareHangup(t, 2); !strings.Contains(r.output, "CALL_ENDING") {
		t.Fatalf("second hangup answered %s, want a refusal", r.output)
	}
	h.endResponse()
	h.stayUp(t, "the refused hangup's turn ended the call")

	if r := h.bareHangup(t, 3); strings.Contains(r.output, "CALL_ENDING") {
		t.Fatalf("third hangup answered %s, want it accepted", r.output)
	}
	h.endResponse()
	h.awaitCallEnded(t, 3*time.Second)
}
