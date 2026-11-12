// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// Acting on an unbacked claim (#438): a lookup, transfer or message the bot
// promised and never made is asked for once when the caller has heard the
// promise, and a caller who goes quiet instead is told what to do rather than
// asked to hear the promise again.
//

// lookupCueFlow offers a backend lookup in its one phase, plus the built-ins.
const lookupCueFlow = `{
	"id": "claim-cue-test",
	"specVersion": "v2",
	"initialNode": "talk",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["transfer_to_agent", "hangup", "take_message"]
	},
	"tools": {
		"repair_status": {
			"description": "Look a repair order up.",
			"parameters": {"type": "object", "properties": {"rma": {"type": "string"}}, "required": ["rma"]},
			"http": {
				"path": "/repair",
				"body": {"rma": "{args.rma}"},
				"successWhen": {"path": "retCode", "equals": "000000"},
				"result": {"status": "data.status"}
			}
		}
	},
	"nodes": {
		"talk": {"instruction": "Help the caller.", "tools": ["repair_status"]}
	}
}`

// bareCueFlow offers the model nothing to act with.
const bareCueFlow = `{
	"id": "claim-cue-bare",
	"specVersion": "v2",
	"initialNode": "talk",
	"global": {"persona": "You answer the phone."},
	"nodes": {
		"talk": {"instruction": "Help the caller.", "tools": []}
	}
}`

// terminalCueFlow is a call already in its closing phase.
const terminalCueFlow = `{
	"id": "claim-cue-terminal",
	"specVersion": "v2",
	"initialNode": "bye",
	"global": {"persona": "You answer the phone.", "alwaysAllowedTools": ["transfer_to_agent"]},
	"nodes": {
		"bye": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true}
	}
}`

// wallCueFlow closes after one tool-less reply too many.
const wallCueFlow = `{
	"id": "claim-cue-wall",
	"specVersion": "v2",
	"initialNode": "talk",
	"global": {"persona": "You answer the phone.", "closingTarget": "bye", "maxTurnsWithoutTool": 1},
	"nodes": {
		"talk": {"instruction": "Help the caller.", "tools": []},
		"bye": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true}
	}
}`

const (
	zhLookupClaim = "好的，我帮您查询一下 RMA1003 的维修进度。"
	zhRepairCall  = `{"rma":"RMA1003"}`
)

func repairBackend(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"retCode": "000000", "data": map[string]any{"status": "repairing"}})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func newCueHarness(t *testing.T, flowJSON, lang string) *claimsHarness {
	t.Helper()
	return newClaimsHarnessWith(t, flowJSON, lang, repairBackend(t),
		Config{BargeGuard: -1, NoInput: -1})
}

// claimCueTexts is what the model was sent that is a claim cue.
func (h *claimsHarness) claimCueTexts() []string {
	var out []string
	for _, text := range h.model.recordedUserText() {
		if strings.Contains(text, "Your last reply told the caller") ||
			strings.Contains(text, "你上一轮对来电者说了") {
			out = append(out, text)
		}
	}
	return out
}

func (h *claimsHarness) awaitUserTexts(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.model.recordedUserText(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%d texts sent to the model, want %d: %q", len(h.model.recordedUserText()), n,
		h.model.recordedUserText())
	return nil
}

func (h *claimsHarness) awaitLog(t *testing.T, needle string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.logs.count(needle) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log never said %q:\n%s", needle, h.logs.String())
}

// settle gives playback the time it needs to finish and drive the time to act
// on it, for a test that asserts nothing happened.
func settle() { time.Sleep(150 * time.Millisecond) }

// heldTurn plays a turn whose audio the leg keeps "playing" until release, so
// the claim is judged while the caller has not yet heard it.
func (h *claimsHarness) heldTurn(text, tool, args string) (release func()) {
	h.leg.holdFrames(3)
	h.turn(text, tool, args)
	return func() { h.leg.holdFrames(0) }
}

// The caller has heard a lookup promise and nothing ran: one cue after the
// playback, not before, naming the tool.
func TestAnUnbackedLookupIsAskedForOnceTheCallerHeardIt(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	release := h.heldTurn(zhLookupClaim, "", "")
	h.awaitLog(t, unbackedClaimLog)
	settle()
	if got := h.model.recordedUserText(); len(got) != 0 {
		t.Fatalf("a cue went out before the caller heard the promise: %q", got)
	}

	release()
	texts := h.awaitUserTexts(t, 1)
	want := "（你上一轮对来电者说了“我帮您查询”，但没有调用任何查询工具，查询并没有发生，来电者在等。" +
		"现在就调用 repair_status，调用之前不要说话。如果不知道要查什么，就直接问来电者。不要再说你在查。）"
	if texts[0] != want {
		t.Errorf("cue = %q\nwant  %q", texts[0], want)
	}
	settle()
	if got := h.claimCueTexts(); len(got) != 1 {
		t.Errorf("%d claim cues, want exactly 1: %q", len(got), got)
	}
	if h.logs.count("asked the bot to act on a claim no tool call backs") != 1 {
		t.Errorf("the cue was not logged once:\n%s", h.logs.String())
	}
}

// The model answers the cue with the same promise: it is not re-asked at that
// turn's playback, and the next silence is met with a cue that names the tool
// and does not ask for the question again.
func TestACueIsNotAnsweredWithAnotherCueAndSilenceGetsTheClaimAwareOne(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	h.turn(zhLookupClaim, "", "")
	h.awaitUserTexts(t, 1)

	h.turn(zhLookupClaim, "", "") // the reply to the cue promises again
	h.awaitLog(t, "outcome=NOT_BACKED")
	settle()
	if got := h.model.recordedUserText(); len(got) != 1 {
		t.Fatalf("the answer to a cue was re-asked at its playback: %q", got)
	}

	h.session.emit(Event{Type: EventTypeNoInput})
	texts := h.awaitUserTexts(t, 2)
	silent := texts[1]
	if !strings.Contains(silent, "repair_status") || !strings.Contains(silent, "来电者一直没有说话，在等你") {
		t.Errorf("dead-air cue = %q, want the claim-aware one naming repair_status", silent)
	}
	if strings.Contains(silent, "简短重复当前的问题") {
		t.Errorf("dead-air cue still asks to repeat the question: %q", silent)
	}

	// Taken once: the next silence is the ordinary prompt.
	h.session.emit(Event{Type: EventTypeNoInput})
	texts = h.awaitUserTexts(t, 3)
	if !strings.Contains(texts[2], "简短重复当前的问题") {
		t.Errorf("third text = %q, want the ordinary dead-air cue once the claim cue was spent", texts[2])
	}
	if !strings.Contains(h.logs.String(), "trigger=PLAYBACK_DONE") ||
		!strings.Contains(h.logs.String(), "trigger=NO_INPUT") {
		t.Errorf("both triggers should be logged:\n%s", h.logs.String())
	}
}

// The model acts on the cue: the outcome is BACKED and nothing more is asked.
func TestACueTheModelActsOnIsBacked(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	h.turn(zhLookupClaim, "", "")
	h.awaitUserTexts(t, 1)

	h.turn("", "repair_status", zhRepairCall)
	h.awaitToolResults(t, 1)
	h.awaitLog(t, "outcome=BACKED")
	h.turn("您的维修单正在维修中。", "", "")
	settle()

	if got := h.claimCueTexts(); len(got) != 1 {
		t.Errorf("%d claim cues, want 1: %q", len(got), got)
	}
	if h.logs.count("outcome=NOT_BACKED") != 0 {
		t.Errorf("a cue the model acted on was counted as not backed:\n%s", h.logs.String())
	}
	// A claim cue is spent: the silence after the answer is an ordinary one.
	h.session.emit(Event{Type: EventTypeNoInput})
	texts := h.awaitUserTexts(t, 2)
	if len(h.claimCueTexts()) != 1 || !strings.Contains(texts[1], "简短重复当前的问题") {
		t.Errorf("texts = %q, want the ordinary dead-air cue", texts)
	}
}

// Claims a tool backs are never asked for again.
func TestABackedClaimIsNotAskedFor(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	// The words and the call in one response.
	h.turn(zhLookupClaim, "repair_status", zhRepairCall)
	h.awaitToolResults(t, 1)
	// The reply to the result.
	h.turn("好的，我帮您查询一下，维修中。", "", "")
	settle()
	// An armed ending.
	h.turn("", "hangup", goodbyeHangup)
	h.awaitToolResults(t, 2)
	h.turn("好的，我马上为您转接人工客服。", "", "")
	settle()

	if got := h.model.recordedUserText(); len(got) != 0 {
		t.Errorf("a backed claim was asked for: %q", got)
	}
}

// An offer is not a claim.
func TestAQuestionIsNotAskedFor(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	h.turn("要不要我帮您查询一下？", "", "")
	settle()
	if got := h.model.recordedUserText(); len(got) != 0 {
		t.Errorf("an offer was asked for: %q", got)
	}
}

// The caller talked over the promise: they have the floor now and nothing was
// heard to the end.
func TestAClaimTheCallerTalkedOverIsNotAskedFor(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	h.leg.holdFrames(3)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
		Text: zhLookupClaim, IsFinal: true}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320)}
	h.model.events <- provider.Event{Type: provider.EventTypeInterrupted}
	h.awaitLog(t, unbackedClaimLog)
	h.leg.holdFrames(0)
	settle()

	if got := h.model.recordedUserText(); len(got) != 0 {
		t.Errorf("an interrupted promise was asked for: %q", got)
	}
	// A dead-air watch after it is not claim-aware either: the caller spoke.
	h.session.emit(Event{Type: EventTypeNoInput})
	texts := h.awaitUserTexts(t, 1)
	if len(h.claimCueTexts()) != 0 {
		t.Errorf("the interrupted promise stood for the dead-air cue: %q", texts)
	}
}

// A wall crossed in the same turn closes the call; the re-ask would contradict it.
func TestTheTurnsWithoutToolWallWinsOverTheReAsk(t *testing.T) {
	h := newCueHarness(t, wallCueFlow, "zh")

	h.callerSays("你好")
	h.turn("您好。", "", "")
	h.callerSays("查一下")
	h.turn(zhLookupClaim, "", "")
	texts := h.awaitUserTexts(t, 1)
	settle()

	if len(h.claimCueTexts()) != 0 {
		t.Errorf("a claim cue went out as the wall closed the call: %q", texts)
	}
	if texts[0] != goodbyeCue("zh", false) {
		t.Errorf("first text = %q, want the wall's goodbye", texts[0])
	}
}

// A handover being settled, and a closing phase, are not asked anything.
func TestNoReAskWhileAHandoverSettlesOrInATerminalPhase(t *testing.T) {
	t.Run("handover settling", func(t *testing.T) {
		h := newCueHarness(t, lookupCueFlow, "zh")
		h.actions.mu.Lock()
		h.actions.isHandingOver = true
		h.actions.mu.Unlock()

		h.turn(zhLookupClaim, "", "")
		h.awaitLog(t, unbackedClaimLog)
		settle()
		if got := h.model.recordedUserText(); len(got) != 0 {
			t.Errorf("a cue went out while the handover settled: %q", got)
		}
	})
	t.Run("terminal phase", func(t *testing.T) {
		h := newCueHarness(t, terminalCueFlow, "zh")

		h.turn("好的，我马上为您转接人工客服。", "", "")
		h.awaitLog(t, unbackedClaimLog)
		settle()
		if got := h.model.recordedUserText(); len(got) != 0 {
			t.Errorf("a cue went out in a terminal phase: %q", got)
		}
	})
}

// Each kind names its own tool, and a phase that offers none says so.
func TestTheCueNamesTheToolThatMakesTheClaimTrue(t *testing.T) {
	cases := []struct {
		name, flow, text string
		want             []string
	}{
		{"transfer", lookupCueFlow, "好的，我马上为您转接人工客服。",
			[]string{"调用 transfer_to_agent", "转接并没有发生"}},
		{"message", lookupCueFlow, "已为您记录留言。",
			[]string{"调用 take_message", "留言并没有被记录"}},
		{"no tool for a transfer", bareCueFlow, "好的，我马上为您转接人工客服。",
			[]string{"你在这里没有任何工具能做这件事", "没有办成"}},
		{"no tool for a lookup", bareCueFlow, zhLookupClaim,
			[]string{"你在这里没有任何工具能做这件事"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCueHarness(t, tc.flow, "zh")
			h.turn(tc.text, "", "")
			got := h.awaitUserTexts(t, 1)[0]
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("cue = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// The English wording, every variant.
func TestTheCueInEnglish(t *testing.T) {
	cases := []struct {
		name, flow, text, want string
	}{
		{"lookup", lookupCueFlow, "Let me check that order for you.",
			"(Your last reply told the caller \"let me check\", but no lookup ran, so nothing is " +
				"being looked up and the caller is waiting. Call repair_status now, without saying " +
				"anything first. If you do not have what it needs (for example which order), ask " +
				"the caller for it instead. Do not say again that you are checking.)"},
		{"transfer", lookupCueFlow, "I'm transferring you to an agent now.",
			"(Your last reply told the caller \"i'm transferring you\", but no transfer was made, " +
				"so the caller is not being put through and the caller is waiting. Call " +
				"transfer_to_agent now, without saying anything first. If you do not have what it " +
				"needs (for example which queue or why), ask the caller for it instead. Do not say " +
				"again that you are transferring them.)"},
		{"message", lookupCueFlow, "I've passed on your message.",
			"(Your last reply told the caller \"i've passed on your message\", but no message was " +
				"saved, so nothing has been recorded and the caller is waiting. Call take_message " +
				"now, without saying anything first. If you do not have what it needs (for example " +
				"the message or a callback number), ask the caller for it instead. Do not say again " +
				"that it is saved.)"},
		{"no tool", bareCueFlow, "Let me check that order for you.",
			"(Your last reply told the caller \"let me check\", but nothing happened: you have no " +
				"tool here that can do that. Tell the caller plainly that it was not done, and say " +
				"what you can do instead. Do not claim it again.)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCueHarness(t, tc.flow, "en")
			h.turn(tc.text, "", "")
			if got := h.awaitUserTexts(t, 1)[0]; got != tc.want {
				t.Errorf("cue = %q\nwant  %q", got, tc.want)
			}
		})
	}

	t.Run("dead air", func(t *testing.T) {
		h := newCueHarness(t, lookupCueFlow, "en")
		h.turn("Let me check that order for you.", "", "")
		h.awaitUserTexts(t, 1)
		h.turn("Let me check that order for you.", "", "")
		h.awaitLog(t, "outcome=NOT_BACKED")
		h.session.emit(Event{Type: EventTypeNoInput})
		got := h.awaitUserTexts(t, 2)[1]
		if !strings.HasPrefix(got, "(The caller has been silent, waiting for you. Your last reply told") ||
			strings.Contains(got, "repeat the current question") || !strings.Contains(got, "repair_status") {
			t.Errorf("dead-air cue = %q", got)
		}
	})
}

// A transcript that lands after its turn finished, before the caller heard it,
// is still asked for once.
func TestALateTranscriptIsStillAskedFor(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	h.leg.holdFrames(3)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320)}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
		Text: zhLookupClaim, IsFinal: true}
	h.awaitLog(t, unbackedClaimLog)
	if got := h.model.recordedUserText(); len(got) != 0 {
		t.Fatalf("asked before the caller heard it: %q", got)
	}
	h.leg.holdFrames(0)
	h.awaitUserTexts(t, 1)
	settle()
	if got := h.claimCueTexts(); len(got) != 1 {
		t.Errorf("%d claim cues, want 1: %q", len(got), got)
	}
}

// A transcript that lands after the caller heard the turn is not re-asked at a
// playback that already happened; the dead-air cue covers it.
func TestALateTranscriptAfterPlaybackIsLeftToTheDeadAirCue(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")

	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320)}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	settle() // the caller hears the turn
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
		Text: zhLookupClaim, IsFinal: true}
	h.awaitLog(t, unbackedClaimLog)
	settle()
	if got := h.model.recordedUserText(); len(got) != 0 {
		t.Fatalf("re-asked after the playback it should have followed: %q", got)
	}

	h.session.emit(Event{Type: EventTypeNoInput})
	if got := h.awaitUserTexts(t, 1)[0]; !strings.Contains(got, "repair_status") {
		t.Errorf("dead-air cue = %q, want the claim-aware one", got)
	}
}

// The cue is sent at the moment a dead-air watch is running. The turn it asks
// for starts a new generation, so the watch armed at the claim turn's playback
// does not report NO_INPUT over the model's reply (Session.beginTurn).
func TestTheDeadAirWatchStaysQuietOverTheReplyToACue(t *testing.T) {
	const noInput = 120 * time.Millisecond
	h := newClaimsHarnessWith(t, lookupCueFlow, "zh", repairBackend(t),
		Config{BargeGuard: -1, NoInput: noInput})

	h.turn(zhLookupClaim, "", "")
	h.awaitUserTexts(t, 1)

	// The reply is under way and takes longer than the watch would.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(3 * noInput)
	if got := h.model.recordedUserText(); len(got) != 1 {
		t.Fatalf("dead air was reported over the reply to the cue: %q", got)
	}
	h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
		ToolCallID: "fc_1", ToolName: "repair_status", ToolArgs: zhRepairCall}
	h.awaitToolResults(t, 1)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.awaitLog(t, "outcome=BACKED")
}

// A cue the engine cannot take leaves the silence watched.
func TestACueThatCannotBeSentLeavesTheSilenceWatched(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "zh")
	h.model.textErr = errNoTextCue

	h.turn(zhLookupClaim, "", "")
	h.awaitLog(t, "could not re-ask an unbacked claim")
	if got := h.claimCueTexts(); len(got) != 0 {
		t.Errorf("a cue was recorded as sent: %q", got)
	}
}
