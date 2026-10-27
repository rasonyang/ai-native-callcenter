// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// #286 section 4: an HTTP tool that has not returned a second after it started
// is covered by one wait line, said only if nothing else is playing, and its
// result is never lost. Time is in the test's hands twice over: the backend
// answers when the test releases it, and the wait line's clock fires when the
// test fires it, so "300 ms" is a backend released before the clock and
// "1500 ms" one released after it.

// waitLineFlow has two slow lookups, a phase the second one leads to that ends
// the call with a line of its own, and an optional global.toolWaitLine.
func waitLineFlow(toolWaitLine string) string {
	line := ""
	if toolWaitLine != "" {
		line = `"toolWaitLine": ` + toolWaitLine + `,`
	}
	return `{
	"id": "wait-line-test",
	"specVersion": "v2",
	"initialNode": "help",
	"global": {` + line + `"persona": "You answer the phone."},
	"tools": {
		"search_kb": {
			"description": "Search the knowledge base.",
			"parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]},
			"http": {"path": "/kb", "body": {"q": "{args.q}"},
				"successWhen": {"path": "retCode", "equals": "000000"},
				"result": {"answer": "data.answer"}}
		},
		"close_case": {
			"description": "Close the caller's case.",
			"parameters": {"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"]},
			"http": {"path": "/case", "body": {"id": "{args.id}"},
				"successWhen": {"path": "retCode", "equals": "000000"},
				"result": {"answer": "data.answer"}}
		}
	},
	"nodes": {
		"help": {
			"instruction": "Answer the caller's questions.",
			"tools": ["search_kb", "close_case"],
			"transitions": [{"on": "TOOL_RESULT", "tool": "close_case", "target": "closed"}]
		},
		"closed": {
			"instruction": "Say goodbye.",
			"announce": {"en": "Your case is closed, goodbye.", "zh": "您的工单已关闭，再见。"},
			"tools": [], "isTerminal": true
		}
	}
}`
}

var defaultWaitLines = map[string]string{
	"en": "One moment, let me check that.",
	"zh": "请稍等，我查一下。",
}

// slowBackend answers each request when the test releases it.
type slowBackend struct {
	server  *httptest.Server
	arrived chan string
	release chan struct{}
	// done lets every request still waiting go when the test ends.
	done chan struct{}
}

func newSlowBackend(t *testing.T) *slowBackend {
	t.Helper()
	b := &slowBackend{arrived: make(chan string, 8), release: make(chan struct{}, 8),
		done: make(chan struct{})}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b.arrived <- req.URL.Path
		select {
		case <-b.release:
		case <-b.done:
			return
		case <-req.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"retCode": "000000", "data": map[string]any{"answer": "Refunds take seven days."}})
	}))
	t.Cleanup(func() {
		close(b.done)
		b.server.Close()
	})
	return b
}

func (b *slowBackend) awaitRequest(t *testing.T, path string) {
	t.Helper()
	select {
	case got := <-b.arrived:
		if got != path {
			t.Fatalf("the backend was called on %s, want %s", got, path)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the backend was never called on %s", path)
	}
}

func (b *slowBackend) answer() { b.release <- struct{}{} }

// waitClock is the wait line's clock: each wait started gets a channel the
// test fires.
type waitClock struct {
	mu    sync.Mutex
	waits []chan time.Time
}

func (c *waitClock) start() (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	due := make(chan time.Time, 1)
	c.waits = append(c.waits, due)
	return due, func() {}
}

func (c *waitClock) started() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waits)
}

// fire makes the n-th wait (from 1) come due, once it has started.
func (c *waitClock) fire(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.started() < n {
		if time.Now().After(deadline) {
			t.Fatalf("wait %d never started", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	c.mu.Lock()
	due := c.waits[n-1]
	c.mu.Unlock()
	due <- time.Now()
}

type waitLinePath struct {
	*toolPath
	backend *slowBackend
	clock   *waitClock
	logs    *syncBuffer
}

func startWaitLinePath(t *testing.T, profile provider.Profile, lang, flowJSON string) *waitLinePath {
	t.Helper()
	backend := newSlowBackend(t)
	clock := &waitClock{}
	logs := &syncBuffer{}
	h := startToolPathFull(t, profile, lang, flowJSON, backend.server.URL, nil, logs,
		func(o *Orchestrator) { o.toolWaitDue = clock.start })
	return &waitLinePath{toolPath: h, backend: backend, clock: clock, logs: logs}
}

// callInATurn has the model call tools in one turn of its own and end it
// without a word, as a model that goes quiet to look something up does.
func (h *waitLinePath) callInATurn(calls ...[3]string) {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	for _, call := range calls {
		h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
			ToolCallID: call[0], ToolName: call[1], ToolArgs: call[2]}
	}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// awaitQuiet waits for the session to have taken in the model's turn.
func (h *waitLinePath) awaitQuiet(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !h.session.isQuiet() {
		if time.Now().After(deadline) {
			t.Fatal("the session never fell quiet")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitAnswers waits for n tool results.
func (h *waitLinePath) awaitAnswers(t *testing.T, n int) []toolResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(h.model.recordedToolResults()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d tools answered, want %d", len(h.model.recordedToolResults()), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return h.model.recordedToolResults()
}

// awaitSpoken waits for n lines asked for as written.
func (h *waitLinePath) awaitSpoken(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(h.model.spokenLines()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d lines said, want %d", len(h.model.spokenLines()), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return h.model.spokenLines()
}

// sayTheLine is the provider speaking the line it was asked for: a turn of its
// own, reported as a spoken line, with audio and its transcript.
func (h *waitLinePath) sayTheLine(text string) {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*2)}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
		Text: text, IsFinal: true}
}

func (h *waitLinePath) endTheLine() {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

func forEachLanguage(t *testing.T, run func(t *testing.T, lang string)) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) { run(t, lang) })
	}
}

// A backend that answers inside the second gets no line.
func TestAToolThatAnswersInsideASecondHasNoWaitLine(t *testing.T) {
	forEachLanguage(t, func(t *testing.T, lang string) {
		h := startWaitLinePath(t, provider.QwenProfile(), lang, waitLineFlow(""))
		h.callInATurn([3]string{"fc_1", "search_kb", `{"q":"refund"}`})
		h.backend.awaitRequest(t, "/kb")
		h.backend.answer()
		h.awaitAnswers(t, 1)
		// The clock coming due after the answer finds nobody waiting.
		h.clock.fire(t, 1)
		time.Sleep(50 * time.Millisecond)

		if lines := h.model.spokenLines(); len(lines) != 0 {
			t.Errorf("a fast tool got lines %q", lines)
		}
		if strings.Contains(h.logs.String(), "said the wait line") {
			t.Error("a fast tool logged a wait line")
		}
	})
}

// A backend that is still working at the second gets exactly one line, in the
// call's language, said as an ordinary (not closing) line before the answer;
// and the log names the tool, not what it was asked.
func TestAToolStillRunningAfterASecondGetsOneWaitLine(t *testing.T) {
	forEachLanguage(t, func(t *testing.T, lang string) {
		h := startWaitLinePath(t, provider.QwenProfile(), lang, waitLineFlow(""))
		h.callInATurn([3]string{"fc_1", "search_kb", `{"q":"refund-4471"}`})
		h.backend.awaitRequest(t, "/kb")
		h.awaitQuiet(t)
		h.clock.fire(t, 1)

		lines := h.awaitSpoken(t, 1)
		if lines[0] != defaultWaitLines[lang] {
			t.Errorf("the wait line is %q, want %q", lines[0], defaultWaitLines[lang])
		}
		if closing := h.model.spokenLinesClosing(); closing[0] {
			t.Error("the wait line was asked for as a closing line")
		}

		h.backend.answer()
		results := h.awaitAnswers(t, 1)
		if !strings.Contains(results[0].output, "seven days") {
			t.Errorf("the answer lost the result: %s", results[0].output)
		}
		time.Sleep(50 * time.Millisecond)
		calls := h.model.recordedCalls()
		if len(calls) < 2 || calls[0] != "SpeakText" || calls[len(calls)-1] != "SendToolResult" {
			t.Errorf("calls = %v, want the line first and the answer last", calls)
		}
		if n := len(h.model.spokenLines()); n != 1 {
			t.Errorf("%d lines said, want exactly one", n)
		}
		logs := h.logs.String()
		if strings.Count(logs, "said the wait line") != 1 || !strings.Contains(logs, "tool=search_kb") {
			t.Errorf("want one log line naming the tool:\n%s", logs)
		}
		if strings.Contains(logs, "refund-4471") {
			t.Error("the log carries what the tool was asked")
		}
	})
}

// The model is still being heard at the second — its own filler, still
// playing after its turn ended, or a turn still being made — so nothing is
// added.
func TestNoWaitLineIsSaidOverTheModel(t *testing.T) {
	forEachLanguage(t, func(t *testing.T, lang string) {
		t.Run("filler still playing", func(t *testing.T) {
			h := startWaitLinePath(t, provider.QwenProfile(), lang, waitLineFlow(""))
			h.leg.holdFrames(40)
			h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
			h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
				Audio: make([]byte, media.FrameSamples*2)}
			h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
				ToolCallID: "fc_1", ToolName: "search_kb", ToolArgs: `{"q":"refund"}`}
			h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
			h.backend.awaitRequest(t, "/kb")
			// The turn is over; its audio is not.
			deadline := time.Now().Add(2 * time.Second)
			for !h.isTurnOver(1) {
				if time.Now().After(deadline) {
					t.Fatal("the turn never ended")
				}
				time.Sleep(2 * time.Millisecond)
			}
			h.clock.fire(t, 1)
			h.awaitLog(t, "no wait line, the caller is hearing the bot")
			h.backend.answer()
			h.awaitAnswers(t, 1)

			if lines := h.model.spokenLines(); len(lines) != 0 {
				t.Errorf("a line was said over the model's own audio: %q", lines)
			}
		})
		t.Run("turn still being made", func(t *testing.T) {
			h := startWaitLinePath(t, provider.QwenProfile(), lang, waitLineFlow(""))
			h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
			h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
				ToolCallID: "fc_1", ToolName: "search_kb", ToolArgs: `{"q":"refund"}`}
			h.backend.awaitRequest(t, "/kb")
			h.clock.fire(t, 1)
			h.awaitLog(t, "no wait line, the caller is hearing the bot")
			h.backend.answer()
			h.awaitAnswers(t, 1)

			if lines := h.model.spokenLines(); len(lines) != 0 {
				t.Errorf("a line was said over a turn still being made: %q", lines)
			}
		})
	})
}

// Two slow tools in one response get one line: the second does not even start
// a clock, even though the line has played out and the floor is free again.
func TestTwoSlowToolsInOneBatchGetOneWaitLine(t *testing.T) {
	forEachLanguage(t, func(t *testing.T, lang string) {
		h := startWaitLinePath(t, provider.QwenProfile(), lang, waitLineFlow(""))
		h.callInATurn(
			[3]string{"fc_1", "search_kb", `{"q":"refund"}`},
			[3]string{"fc_2", "search_kb", `{"q":"warranty"}`})
		h.backend.awaitRequest(t, "/kb")
		h.awaitQuiet(t)
		h.clock.fire(t, 1)
		h.awaitSpoken(t, 1)
		h.sayTheLine(defaultWaitLines[lang])
		h.endTheLine()

		h.backend.answer()
		h.backend.awaitRequest(t, "/kb")
		h.backend.answer()
		h.awaitAnswers(t, 2)
		time.Sleep(50 * time.Millisecond)

		if n := len(h.model.spokenLines()); n != 1 {
			t.Errorf("%d lines said for one batch, want one", n)
		}
		if n := h.clock.started(); n != 1 {
			t.Errorf("%d wait clocks started for one batch, want one", n)
		}
	})
}

// On doubao a tool result sent after a line is dropped and never answered
// (doubao-findings W-D10), so a slow tool there gets no wait line: no clock is
// started, nothing is said, and the result is answered as it always was.
func TestNoWaitLineOnAProviderThatDropsTheResultAfterALine(t *testing.T) {
	forEachLanguage(t, func(t *testing.T, lang string) {
		h := startWaitLinePath(t, provider.DoubaoProfile(), lang, waitLineFlow(""))
		h.callInATurn([3]string{"fc_1", "search_kb", `{"q":"refund"}`})
		h.backend.awaitRequest(t, "/kb")
		h.awaitQuiet(t)
		// A clock started now would be the wait line's; give it the moment a
		// wrongly started one needs to show.
		time.Sleep(100 * time.Millisecond)
		if n := h.clock.started(); n != 0 {
			t.Fatalf("%d wait clocks started on a provider that drops the result after a line", n)
		}
		h.backend.answer()
		results := h.awaitAnswers(t, 1)
		if !strings.Contains(results[0].output, "seven days") {
			t.Errorf("the answer lost the result: %s", results[0].output)
		}
		if lines := h.model.spokenLines(); len(lines) != 0 {
			t.Errorf("lines said: %q", lines)
		}
	})
}

// A flow's own wording replaces the default.
func TestTheFlowsWaitLineReplacesTheDefault(t *testing.T) {
	wording := map[string]string{"en": "Bear with me a second.", "zh": "稍等片刻，马上就好。"}
	forEachLanguage(t, func(t *testing.T, lang string) {
		h := startWaitLinePath(t, provider.QwenProfile(), lang,
			waitLineFlow(`{"en": "Bear with me a second.", "zh": "稍等片刻，马上就好。"}`))
		h.callInATurn([3]string{"fc_1", "search_kb", `{"q":"refund"}`})
		h.backend.awaitRequest(t, "/kb")
		h.awaitQuiet(t)
		h.clock.fire(t, 1)
		if lines := h.awaitSpoken(t, 1); lines[0] != wording[lang] {
			t.Errorf("the wait line is %q, want the flow's %q", lines[0], wording[lang])
		}
		h.backend.answer()
		h.awaitAnswers(t, 1)
	})
}

// The result arrives while the line is playing and moves the call to a phase
// that ends it. The line is not the closing line: the ending waits through the
// line's turn and its playback and runs on the playback of the answer's turn.
func TestTheWaitLineIsNotTheClosingLine(t *testing.T) {
	forEachLanguage(t, func(t *testing.T, lang string) {
		h := startWaitLinePath(t, provider.OpenAIProfile(), lang, waitLineFlow(""))
		h.callInATurn([3]string{"fc_1", "close_case", `{"id":"C-88"}`})
		h.backend.awaitRequest(t, "/case")
		h.awaitQuiet(t)
		h.clock.fire(t, 1)
		h.awaitSpoken(t, 1)

		// The result lands before the line's turn has begun: the ending is
		// armed in the tool's turn, and the line's turn is the first after it.
		h.backend.answer()
		h.awaitAnswers(t, 1)
		deadline := time.Now().Add(2 * time.Second)
		for !h.actions.isArmed() {
			if time.Now().After(deadline) {
				t.Fatal("the move into the closing phase armed nothing")
			}
			time.Sleep(2 * time.Millisecond)
		}

		h.sayTheLine(defaultWaitLines[lang])
		h.endTheLine()
		// The line's turn has ended and, with nothing held on the leg, its
		// playback is over a couple of frame ticks later.
		deadline = time.Now().Add(2 * time.Second)
		for !h.isTurnOver(2) {
			if time.Now().After(deadline) {
				t.Fatal("the line's turn never ended")
			}
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		if h.isCallEnded() {
			t.Fatal("the call ended on the wait line, before the closing line")
		}

		h.playTurn()
		h.awaitCallEnded(t, 2*time.Second)
	})
}

// A caller who talks over the line flushes it, and still gets the answer.
func TestACallerBargingOverTheWaitLineStillGetsTheResult(t *testing.T) {
	forEachLanguage(t, func(t *testing.T, lang string) {
		// G.711 passthrough: the audio below is whole frames on the leg.
		h := startWaitLinePath(t, provider.OpenAIProfile(), lang, waitLineFlow(""))
		h.callInATurn([3]string{"fc_1", "search_kb", `{"q":"refund"}`})
		h.backend.awaitRequest(t, "/kb")
		h.awaitQuiet(t)
		h.clock.fire(t, 1)
		h.awaitSpoken(t, 1)

		h.leg.holdFrames(40)
		h.sayTheLine(defaultWaitLines[lang])
		deadline := time.Now().Add(2 * time.Second)
		for !h.session.isHoldingTheFloor() || len(h.leg.sentFrames()) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the line never became audible")
			}
			time.Sleep(2 * time.Millisecond)
		}
		h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
		h.model.events <- provider.Event{Type: provider.EventTypeInterrupted,
			InterruptedBy: provider.InterruptReasonSpeech}
		deadline = time.Now().Add(2 * time.Second)
		for len(h.model.recordedInterrupts()) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the caller's speech did not take the floor from the line")
			}
			time.Sleep(2 * time.Millisecond)
		}

		h.backend.answer()
		results := h.awaitAnswers(t, 1)
		if !strings.Contains(results[0].output, "seven days") {
			t.Errorf("the answer lost the result: %s", results[0].output)
		}
	})
}

// The turn that said the line is marked as an aside on every event of it, and
// the turn after it is not. A turn of another kind starting first means the
// line did not come first, and nothing is marked.
func TestAnAsideMarksOnlyTheLinesOwnTurn(t *testing.T) {
	t.Run("the line's turn", func(t *testing.T) {
		session, _, model := startBridge(t, provider.QwenProfile())
		awaitBridgeEvent(t, session, EventTypeReady)
		if err := session.SpeakAside("One moment."); err != nil {
			t.Fatalf("speak aside: %v", err)
		}
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples*2)}
		model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
			Text: "One moment.", IsFinal: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if said := awaitBridgeEvent(t, session, EventTypeBotSaid); !said.IsAside {
			t.Error("the line's transcript is not marked")
		}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); !done.IsAside {
			t.Error("the line's TURN_DONE is not marked")
		}
		if played := awaitBridgeEvent(t, session, EventTypePlaybackDone); !played.IsAside {
			t.Error("the line's PLAYBACK_DONE is not marked")
		}

		model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); done.IsAside {
			t.Error("the turn after the line is marked")
		}
	})
	t.Run("another turn first", func(t *testing.T) {
		session, _, model := startBridge(t, provider.QwenProfile())
		awaitBridgeEvent(t, session, EventTypeReady)
		if err := session.SpeakAside("One moment."); err != nil {
			t.Fatalf("speak aside: %v", err)
		}
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); done.IsAside {
			t.Error("a reply that came before the line is marked")
		}
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); done.IsAside {
			t.Error("a later line is marked as the aside that never came first")
		}
	})
}

// #351: the wait line's turn called the running tool and said nothing, and the
// provider asked for the line once more (provider.Event.IsLineAskedAgain). The
// turn that says it is the aside too, so its "let me check" is judged by
// nothing that judges the model's replies. A line asked again after a turn
// that was not the aside (a phase's line) is not marked.
func TestAnAsideAskedAgainMarksTheTurnThatSaysIt(t *testing.T) {
	t.Run("the aside asked again", func(t *testing.T) {
		session, _, model := startBridge(t, provider.QwenProfile())
		awaitBridgeEvent(t, session, EventTypeReady)
		if err := session.SpeakAside("One moment."); err != nil {
			t.Fatalf("speak aside: %v", err)
		}
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); !done.IsAside || done.HasAudio {
			t.Errorf("the silent first turn: aside=%v audio=%v, want an aside with no audio",
				done.IsAside, done.HasAudio)
		}

		model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true,
			IsLineAskedAgain: true}
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples*2)}
		model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
			Text: "One moment.", IsFinal: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if said := awaitBridgeEvent(t, session, EventTypeBotSaid); !said.IsAside {
			t.Error("the line asked again: its transcript is not marked")
		}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); !done.IsAside {
			t.Error("the line asked again: its TURN_DONE is not marked")
		}

		model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); done.IsAside {
			t.Error("the turn after the line asked again is marked")
		}
	})
	t.Run("a phase's line asked again", func(t *testing.T) {
		session, _, model := startBridge(t, provider.QwenProfile())
		awaitBridgeEvent(t, session, EventTypeReady)
		if err := session.Speak("Your upgrade starts at midnight.", false); err != nil {
			t.Fatalf("speak: %v", err)
		}
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		awaitBridgeEvent(t, session, EventTypeTurnDone)
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true,
			IsLineAskedAgain: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		if done := awaitBridgeEvent(t, session, EventTypeTurnDone); done.IsAside {
			t.Error("a phase's line asked again is marked as an aside")
		}
	})
}

// awaitLog waits for a line in the call's log.
func (h *waitLinePath) awaitLog(t *testing.T, words string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(h.logs.String(), words) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q:\n%s", words, h.logs.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// isTurnOver reports whether the session has taken in the end of turn n.
func (h *waitLinePath) isTurnOver(n int) bool {
	h.session.mu.Lock()
	defer h.session.mu.Unlock()
	return h.session.turnSeq >= n && !h.session.isResponding
}

// The clock the orchestrator runs without a test's is the fixed second.
func TestTheWaitLineClockIsOneSecond(t *testing.T) {
	if flow.ToolWaitLineAfter != time.Second {
		t.Errorf("ToolWaitLineAfter = %s, want 1s (#286: fixed, not a setting)", flow.ToolWaitLineAfter)
	}
}
