// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// A slot tool the model calls while answering the caller, in a phase where the
// caller speaks next (flow.Node.IsCallerNext): its result is sent without
// asking the model for another turn. Each test says whether a turn is asked for
// (SendToolResult) or not (SendToolResultQuietly) and whether the bot holds the
// floor afterwards.
//

// quietModel is a model that can answer a tool call without a turn
// (provider.QuietToolAnswerer), as the Realtime client does.
type quietModel struct {
	*fakeModel
	results []toolResult
}

func (m *quietModel) SendToolResultQuietly(id, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, toolResult{id, output})
	m.calls = append(m.calls, "SendToolResultQuietly")
	return nil
}

func (m *quietModel) quietResults() []toolResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.results)
}

// quietFlow renews a plan. offer is where the call starts; ask records a
// question and moves to listen, a phase where the caller speaks next, which
// alternates with listen2 so that any number of questions can be asked.
// classify "decline" lands in recap, a plain phase with a line of its own,
// "maybe" in a plain phase without one, and "agree" in a terminal phase.
const quietFlow = `{
	"id": "quiet-test",
	"specVersion": "v2",
	"initialNode": "offer",
	"global": {
		"persona": "You renew plans.",
		"closingTarget": "bye",
		"maxTurnsWithoutTool": 2,
		"alwaysAllowedTools": ["take_message", "hangup"]
	},
	"tools": {
		"note": {"kind": "slot", "slot": "note", "description": "Record a note."},
		"ask": {"kind": "slot", "slot": "question", "description": "Record the caller's question."},
		"classify": {"kind": "slot", "slot": "answer", "description": "Record the decision.",
			"enum": ["agree", "decline", "maybe"]}
	},
	"nodes": {
		"offer": {
			"instruction": "Offer the renewal.",
			"tools": ["note", "ask", "classify"],
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "ask", "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "listen"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "agree"}, "target": "agreed"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "decline"}, "target": "recap"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "maybe"}, "target": "plain"}
			]
		},
		"listen": {
			"instruction": "Wait for the caller.",
			"isCallerNext": true,
			"tools": ["note", "ask", "classify"],
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "ask", "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "listen2"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "agree"}, "target": "agreed"}
			]
		},
		"listen2": {
			"instruction": "Wait for the caller again.",
			"isCallerNext": true,
			"tools": ["note", "ask", "classify"],
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "ask", "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "listen"}
			]
		},
		"plain": {"instruction": "Ask what they think.", "tools": ["classify"]},
		"recap": {
			"instruction": "Recap the offer.",
			"announce": {"en": "Let me recap the offer.", "zh": "我来重复一下优惠。"},
			"tools": ["classify"]
		},
		"agreed": {
			"instruction": "Renewed.",
			"announce": {"en": "Renewed, goodbye.", "zh": "已续约，再见。"},
			"tools": [], "isTerminal": true
		},
		"bye": {
			"instruction": "Close the call.",
			"announce": {"en": "Thank you, goodbye.", "zh": "谢谢，再见。"},
			"tools": [], "isTerminal": true
		}
	}
}`

// withoutCallerNext is quietFlow as a flow written before the field existed.
var withoutCallerNext = strings.ReplaceAll(quietFlow, `"isCallerNext": true,`, "")

type quietHarness struct {
	*claimsHarness
	quiet *quietModel
}

// newQuietHarness drives a call on flowJSON over a model that answers quietly,
// or over one that cannot when isQuietModel is false.
func newQuietHarness(t *testing.T, flowJSON string, isQuietModel bool, cfg Config) *quietHarness {
	t.Helper()
	return newQuietHarnessBackend(t, flowJSON, isQuietModel, cfg, "", nil)
}

// newQuietHarnessBackend is newQuietHarness for a flow whose HTTP tools call
// the backend at backendURL, with a hook that runs on the built call before
// drive starts.
func newQuietHarnessBackend(t *testing.T, flowJSON string, isQuietModel bool, cfg Config,
	backendURL string, setup func(*claimsHarness)) *quietHarness {
	t.Helper()
	h := &quietHarness{}
	wrap := func(m *fakeModel) provider.VoiceSession {
		if !isQuietModel {
			return m
		}
		h.quiet = &quietModel{fakeModel: m}
		return h.quiet
	}
	h.claimsHarness = newClaimsHarnessModel(t, provider.QwenProfile(), flowJSON, "en", backendURL, cfg, wrap, setup)
	return h
}

func quietConfig() Config { return Config{BargeGuard: -1, NoInput: -1} }

func (h *quietHarness) started() {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
}

func (h *quietHarness) audio() {
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples)}
}

func (h *quietHarness) toolCall(id, name, args string) {
	h.model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: id, ToolName: name, ToolArgs: args}
}

func (h *quietHarness) said(text string) {
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: text, IsFinal: true}
}

func (h *quietHarness) ended() {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// answered plays a whole response that speaks and then calls tools, as a
// Realtime response delivers them.
func (h *quietHarness) answered(text string, calls ...[2]string) {
	h.started()
	h.said(text)
	h.audio()
	for i, call := range calls {
		h.toolCall("fc_"+call[0]+string(rune('a'+i)), call[0], call[1])
	}
	h.ended()
}

// sends are the ways the model was given a tool result, a line or a cue, in
// order, without the instruction updates between them.
func (h *quietHarness) sends() string {
	var kept []string
	for _, call := range h.model.recordedCalls() {
		if call != "UpdateInstructions" {
			kept = append(kept, call)
		}
	}
	return strings.Join(kept, ",")
}

func (h *quietHarness) awaitSends(t *testing.T, want string) {
	t.Helper()
	waitFor(t, "sends "+want, func() bool { return h.sends() == want })
}

// holdsTheFloor reports a claim on the floor from an answer asked for a turn.
func (h *quietHarness) holdsTheFloor() bool {
	h.session.mu.Lock()
	defer h.session.mu.Unlock()
	return !h.session.turnOwedUntil.IsZero() && time.Now().Before(h.session.turnOwedUntil)
}

// 1. The response spoke, called a slot tool that succeeded and moved into a
// caller-next phase: the result is sent without a turn, and no floor is
// claimed.
func TestASlotToolCalledWhileAnsweringIsAnsweredWithoutATurn(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.answered("It costs fifty yuan a month.", [2]string{"ask", `{"value":"how much is it"}`})
	h.awaitSends(t, "SendToolResultQuietly")

	if got := h.quiet.quietResults(); len(got) != 1 || !strings.Contains(got[0].output, `"ok":"1"`) {
		t.Errorf("quiet results = %v, want the one slot answer", got)
	}
	if got := h.model.recordedToolResults(); len(got) != 0 {
		t.Errorf("SendToolResult was used for %v, want the quiet way only", got)
	}
	settle()
	if h.holdsTheFloor() || h.session.isHoldingTheFloor() {
		t.Error("the bot holds the floor for a turn nobody asked for")
	}
	if !strings.Contains(h.logs.String(), "tool results sent without asking for a turn") {
		t.Errorf("the quiet close left no trace in the log:\n%s", h.logs.String())
	}
}

// The control for the floor: an answer that does ask for a turn claims it.
func TestAnAnswerThatAsksForATurnClaimsTheFloor(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.answered("Hello.", [2]string{"classify", `{"value":"maybe"}`})
	h.awaitSends(t, "SendToolResult")
	// The claim follows the send (Session.AnswerTool), so it is waited for.
	waitFor(t, "the floor claimed for the turn asked for", h.holdsTheFloor)
}

// 2. A response that made only the call has said nothing: the follow-up turn is
// the answer, as today. A call before the first audio of the same response is
// a response that answered around it: quiet.
func TestAResponseThatSaidNothingGetsItsTurn(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.started()
	h.toolCall("fc_1", "ask", `{"value":"how much is it"}`)
	h.ended()
	h.awaitSends(t, "SendToolResult")
	if got := h.quiet.quietResults(); len(got) != 0 {
		t.Errorf("a response with no audio answered quietly: %v", got)
	}
}

func TestAToolCallThatOvertakesTheFirstAudioIsStillQuiet(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.started()
	h.toolCall("fc_1", "ask", `{"value":"how much is it"}`)
	h.said("It costs fifty yuan a month.")
	h.audio()
	h.ended()
	h.awaitSends(t, "SendToolResultQuietly")
}

// 3. A refusal gets a turn so the model can correct itself: an empty value,
// and a tool the phase does not offer.
func TestARefusedSlotCallGetsATurn(t *testing.T) {
	for name, call := range map[string][2]string{
		"empty value":    {"ask", `{"value":""}`},
		"not in phase":   {"classify", `{"value":"unheard-of"}`},
		"unknown to all": {"nothing", `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			h := newQuietHarness(t, quietFlow, true, quietConfig())

			h.answered("Sure.", call)
			h.awaitSends(t, "SendToolResult")
			if got := h.quiet.quietResults(); len(got) != 0 {
				t.Errorf("a refusal was answered quietly: %v", got)
			}
		})
	}
}

// 4. A batch with a tool that is not a slot tool is answered the ordinary way
// whatever the order: one turn is asked for, and the results go out in the order
// the calls came.
func TestABatchWithABuiltInAsksForOneTurnInEitherOrder(t *testing.T) {
	for name, calls := range map[string][][2]string{
		"slot first":     {{"ask", `{"value":"q"}`}, {flow.ToolTakeMessage, `{"message":"call me"}`}},
		"built-in first": {{flow.ToolTakeMessage, `{"message":"call me"}`}, {"ask", `{"value":"q"}`}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newQuietHarness(t, quietFlow, true, quietConfig())

			h.answered("Noted.", calls...)
			h.awaitSends(t, "SendToolResult,SendToolResult")
			if got := h.quiet.quietResults(); len(got) != 0 {
				t.Errorf("a batch with a built-in answered quietly: %v", got)
			}
			var order []string
			for _, result := range h.model.recordedToolResults() {
				order = append(order, result.id)
			}
			want := []string{"fc_" + calls[0][0] + "a", "fc_" + calls[1][0] + "b"}
			if !slices.Equal(order, want) {
				t.Errorf("results went out as %v, want the order of the calls %v", order, want)
			}
		})
	}
}

// 5. A batch that enters a phase with a line of its own: the held results go out
// the ordinary way, before the line.
func TestABatchEnteringALineGoesTheOrdinaryWayBeforeTheLine(t *testing.T) {
	t.Run("a phase that is not the end", func(t *testing.T) {
		h := newQuietHarness(t, quietFlow, true, quietConfig())

		h.answered("Let me think.", [2]string{"note", `{"value":"n"}`}, [2]string{"classify", `{"value":"decline"}`})
		h.awaitSends(t, "SendToolResult,SendToolResult,SpeakText")
		if got := h.model.spokenLines(); len(got) != 1 || got[0] != "Let me recap the offer." {
			t.Errorf("spoken lines = %v, want the recap once", got)
		}
	})
	t.Run("the closing phase, whose line rides in the result", func(t *testing.T) {
		h := newQuietHarness(t, quietFlow, true, quietConfig())

		h.answered("Great.", [2]string{"note", `{"value":"n"}`}, [2]string{"classify", `{"value":"agree"}`})
		h.awaitSends(t, "SendToolResult,SendToolResult")
		if got := h.quiet.quietResults(); len(got) != 0 {
			t.Errorf("a batch that ends the call answered quietly: %v", got)
		}
	})
}

// 6. An ending that is already armed asks for the closing line's turn as ever.
func TestAnArmedEndingKeepsTodaysPath(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.started()
	h.toolCall("fc_h", flow.ToolHangup, `{}`)
	h.ended()
	h.awaitSends(t, "SendToolResult")
	if !h.actions.isArmed() {
		t.Fatal("hangup did not arm the ending")
	}

	h.answered("Sure.", [2]string{"ask", `{"value":"q"}`})
	h.awaitSends(t, "SendToolResult,SendToolResult")
	if got := h.quiet.quietResults(); len(got) != 0 {
		t.Errorf("a result was sent quietly over an armed ending: %v", got)
	}
}

// 7. The caller cuts the response in after the call. Interrupted or not, the
// response spoke, nothing is asked for, and the provider's own reply to the
// caller is the next turn.
func TestABargeInAfterTheCallLeavesTheNextTurnToTheProvider(t *testing.T) {
	t.Run("the provider cancels the response", func(t *testing.T) {
		h := newQuietHarness(t, quietFlow, true, quietConfig())

		h.started()
		h.said("It costs fifty")
		h.audio()
		h.toolCall("fc_1", "ask", `{"value":"how much is it"}`)
		h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
		h.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
		h.awaitSends(t, "Interrupt,SendToolResultQuietly")

		// The provider's own reply to the caller.
		h.started()
		h.audio()
		h.ended()
		settle()
		if got := h.sends(); got != "Interrupt,SendToolResultQuietly" {
			t.Errorf("sends = %s, want nothing asked for beyond the quiet result", got)
		}
	})
	t.Run("the response finishes anyway, with the audio flushed", func(t *testing.T) {
		h := newQuietHarness(t, quietFlow, true, quietConfig())

		h.started()
		h.said("It costs fifty yuan.")
		h.audio()
		h.toolCall("fc_1", "ask", `{"value":"how much is it"}`)
		h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
		// A cancel sent after generation ended is rejected: the response ends
		// normally, and the barge-in's flush left it with no audio to report.
		h.ended()
		h.awaitSends(t, "Interrupt,SendToolResultQuietly")
	})
}

// 8. A dead-air prompt right after the answer has played is not refused: the
// bot holds no floor for a turn nobody asked for.
func TestADeadAirPromptAfterAQuietAnswerIsNotHeldBack(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, Config{BargeGuard: -1, NoInput: 150 * time.Millisecond})

	h.answered("It costs fifty yuan a month.", [2]string{"ask", `{"value":"how much is it"}`})
	h.awaitSends(t, "SendToolResultQuietly,SendUserText")

	if got := h.model.recordedUserText(); len(got) != 1 || !strings.Contains(got[0], "silent") {
		t.Errorf("user text = %v, want the dead-air cue", got)
	}
}

// 9. The caller's next replies count towards maxTurnsWithoutTool from the first
// one: three tool-less replies after a quiet exchange cross a limit of two and
// close the call. Counted as the answer to a tool result, the first would be
// skipped and the third would not cross.
func TestRepliesAfterAQuietExchangeCountTowardsTheWall(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.callerSays("How much is it?")
	h.answered("Fifty yuan a month.", [2]string{"ask", `{"value":"how much is it"}`})
	h.awaitSends(t, "SendToolResultQuietly")

	for _, reply := range []string{"Yes.", "Still fifty.", "Fifty."} {
		h.callerSays("And?")
		h.started()
		h.said(reply)
		h.audio()
		h.ended()
	}
	waitFor(t, "the wall to close the call", func() bool {
		return slices.Contains(h.model.spokenLines(), "Thank you, goodbye.")
	})
}

// 10. A slot call that moves nothing and one that moves into caller-next: both
// are quiet, in the order the calls came.
func TestTwoSlotCallsEndingInCallerNextAreBothQuiet(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.answered("Fifty yuan.", [2]string{"note", `{"value":"price"}`}, [2]string{"ask", `{"value":"how much"}`})
	h.awaitSends(t, "SendToolResultQuietly,SendToolResultQuietly")

	var order []string
	for _, result := range h.quiet.quietResults() {
		order = append(order, result.id)
	}
	if want := []string{"fc_notea", "fc_askb"}; !slices.Equal(order, want) {
		t.Errorf("quiet results = %v, want %v", order, want)
	}
	if got := h.model.recordedToolResults(); len(got) != 0 {
		t.Errorf("SendToolResult was used: %v", got)
	}
}

// 11. A batch that moves into caller-next and then out to a plain phase ends
// somewhere the model has to speak: one turn is asked for, once.
func TestABatchMovingOutOfCallerNextAsksForOneTurn(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.answered("Fifty yuan.", [2]string{"ask", `{"value":"how much"}`}, [2]string{"classify", `{"value":"maybe"}`})
	h.awaitSends(t, "SendToolResult,SendToolResult")
	if got := h.quiet.quietResults(); len(got) != 0 {
		t.Errorf("a batch that ended in a plain phase answered quietly: %v", got)
	}
}

// 12. A flow that does not declare the field behaves as it always did: each
// answer goes out as the call arrives, before the response has ended.
func TestAFlowWithoutTheFieldAnswersAsTheCallsArrive(t *testing.T) {
	h := newQuietHarness(t, withoutCallerNext, true, quietConfig())

	h.started()
	h.said("Fifty yuan.")
	h.audio()
	h.toolCall("fc_1", "ask", `{"value":"how much"}`)
	// The response is still open.
	h.awaitSends(t, "SendToolResult")
	h.ended()
	settle()
	if got := h.sends(); got != "SendToolResult" {
		t.Errorf("sends = %s, want the one answer, made at the call", got)
	}
	if got := h.quiet.quietResults(); len(got) != 0 {
		t.Errorf("a flow without the field answered quietly: %v", got)
	}
}

// 13. A model that cannot answer quietly (doubao) gets today's path, as the
// calls arrive, whatever the flow declares.
func TestAModelWithoutAQuietAnswerKeepsTodaysPath(t *testing.T) {
	h := newQuietHarness(t, quietFlow, false, quietConfig())

	h.started()
	h.said("Fifty yuan.")
	h.audio()
	h.toolCall("fc_1", "ask", `{"value":"how much"}`)
	h.awaitSends(t, "SendToolResult")
	h.ended()
	settle()
	if got := h.sends(); got != "SendToolResult" {
		t.Errorf("sends = %s, want the one answer, made at the call", got)
	}
}

// 14. After a quiet close the bot's next reply is judged like any reply to the
// caller: a claim it makes is not taken as backed by a tool result that never
// asked for it.
func TestTheReplyAfterAQuietExchangeIsJudgedByTheClaimWatch(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.callerSays("How much is it?")
	h.answered("Fifty yuan a month.", [2]string{"ask", `{"value":"how much"}`})
	h.awaitSends(t, "SendToolResultQuietly")

	h.callerSays("Please pass a message on.")
	h.started()
	h.said("I've passed on your message.")
	h.audio()
	h.ended()
	h.awaitLog(t, unbackedClaimLog)
}

// The control for 14: a tool-first response answered the ordinary way has its
// next reply taken as the tool's, whose words the call backs.
func TestTheReplyToAnOrdinaryToolResultIsNotJudgedAsAClaim(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.started()
	h.toolCall("fc_1", "ask", `{"value":"how much"}`)
	h.ended()
	h.awaitSends(t, "SendToolResult")

	h.started()
	h.said("I've passed on your message.")
	h.audio()
	h.ended()
	settle()
	if got := h.logs.count(unbackedClaimLog); got != 0 {
		t.Errorf("logged %d unbacked claims for the reply to a tool result", got)
	}
}

// The held answers wait for the end of the response: nothing is sent while it
// is open, and an aside's turn ending does not close the batch.
func TestHeldAnswersWaitForTheEndOfTheResponse(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.started()
	h.said("Fifty yuan.")
	h.audio()
	h.toolCall("fc_1", "ask", `{"value":"how much"}`)
	settle()
	if got := h.sends(); got != "" {
		t.Fatalf("sends = %q while the response is open, want none", got)
	}
	h.ended()
	h.awaitSends(t, "SendToolResultQuietly")
}

// Each quiet answer is the same function_call_output the ordinary one is: the
// output the model reads is the tool's, with the new phase's hint.
func TestTheQuietAnswerCarriesTheToolsOutput(t *testing.T) {
	h := newQuietHarness(t, quietFlow, true, quietConfig())

	h.answered("Fifty yuan.", [2]string{"ask", `{"value":"how much"}`})
	h.awaitSends(t, "SendToolResultQuietly")
	got := h.quiet.quietResults()
	if len(got) != 1 || got[0].id != "fc_aska" {
		t.Fatalf("quiet results = %v", got)
	}
	if hint := hintOf(t, got[0].output); !strings.Contains(hint, "Wait for the caller.") {
		t.Errorf("hint = %q, want the new phase's instruction", hint)
	}
}

// lead_qualification declares no caller-next phase, and relies on the turn each
// answer asks for: its next question comes from it. A model that says "got it"
// and records the interest in one response still gets that turn.
func TestLeadQualificationStillGetsItsFollowUpTurn(t *testing.T) {
	data, err := os.ReadFile("../seed/flows/lead_qualification.json")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := flow.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if spec.HasCallerNextPhase() {
		t.Fatal("lead_qualification declares a caller-next phase: the regression guard is moot")
	}
	h := newQuietHarness(t, string(data), true, quietConfig())

	h.answered("Got it.", [2]string{"record_interest", `{"value":"high"}`})
	h.awaitSends(t, "SendToolResult")
	if got := h.quiet.quietResults(); len(got) != 0 {
		t.Errorf("lead_qualification answered quietly: %v", got)
	}
	// The claim follows the send (Session.AnswerTool), so it is waited for.
	waitFor(t, "the floor claimed for the follow-up turn", h.holdsTheFloor)
}

// The renewal-offer fixture is the flow the pattern was written for: its first
// question is answered in words, recorded in the same response, and the call
// then waits for the caller.
func TestTheRenewalOfferFixtureAnswersItsQuestionsQuietly(t *testing.T) {
	data, err := os.ReadFile("../flowsim/testdata/flows/renewal_offer.json")
	if err != nil {
		t.Fatal(err)
	}
	h := newQuietHarness(t, string(data), true, quietConfig())

	h.answered("Yes, it stays at fifty yuan.", [2]string{"record_question", `{"value":"does the price change"}`})
	h.awaitSends(t, "SendToolResultQuietly")
	h.answered("Sixty gigabytes.", [2]string{"record_second_question", `{"value":"how much data"}`})
	h.awaitSends(t, "SendToolResultQuietly,SendToolResultQuietly")
}

// quietHTTPFlow is quietFlow with an HTTP tool, search_kb, in the offer phase.
var quietHTTPFlow = strings.Replace(
	strings.Replace(quietFlow, `"tools": {
		"note"`, `"tools": {
		"search_kb": {
			"description": "Search the knowledge base.",
			"parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]},
			"http": {"path": "/kb", "body": {"q": "{args.q}"},
				"successWhen": {"path": "retCode", "equals": "000000"},
				"result": {"answer": "data.answer"}}
		},
		"note"`, 1),
	`"instruction": "Offer the renewal.",
			"tools": ["note", "ask", "classify"],`,
	`"instruction": "Offer the renewal.",
			"tools": ["note", "ask", "classify", "search_kb"],`, 1)

// A batch with a real HTTP tool is answered the ordinary way in either order:
// the slot answer held so far is released ahead of the HTTP answer (or, when
// the HTTP tool came first, goes out behind it), one turn is asked for, and
// nothing is sent quietly. The results go out in the order of the calls.
func TestABatchWithAnHTTPToolAsksForOneTurnInEitherOrder(t *testing.T) {
	for name, calls := range map[string][][2]string{
		"slot first": {{"ask", `{"value":"q"}`}, {"search_kb", `{"q":"refund"}`}},
		"HTTP first": {{"search_kb", `{"q":"refund"}`}, {"ask", `{"value":"q"}`}},
	} {
		t.Run(name, func(t *testing.T) {
			backend := newSlowBackend(t)
			h := newQuietHarnessBackend(t, quietHTTPFlow, true, quietConfig(), backend.server.URL, nil)

			h.started()
			h.said("Noted.")
			h.audio()
			for i, call := range calls {
				h.toolCall("fc_"+call[0]+string(rune('a'+i)), call[0], call[1])
			}
			h.ended()
			backend.awaitRequest(t, "/kb")
			backend.answer()

			h.awaitSends(t, "SendToolResult,SendToolResult")
			if got := h.quiet.quietResults(); len(got) != 0 {
				t.Errorf("a batch with an HTTP tool answered quietly: %v", got)
			}
			var order []string
			for _, result := range h.model.recordedToolResults() {
				order = append(order, result.id)
			}
			want := []string{"fc_" + calls[0][0] + "a", "fc_" + calls[1][0] + "b"}
			if !slices.Equal(order, want) {
				t.Errorf("results went out as %v, want the order of the calls %v", order, want)
			}
			settle()
			if got := h.sends(); got != "SendToolResult,SendToolResult" {
				t.Errorf("sends = %s, want the two answers and nothing more", got)
			}
		})
	}
}

// An HTTP tool that runs past the wait line's clock, with a slot answer held
// meanwhile: the wait line is decided as it always was (said once, the caller
// hearing nothing else), the held answer stays held while the tool runs, and
// the batch's answers then all go out the ordinary way, in the order of the
// calls, behind the line.
func TestASlowHTTPToolInABatchWithAHeldSlotAnswerKeepsTodaysWaitLine(t *testing.T) {
	backend := newSlowBackend(t)
	clock := &waitClock{}
	h := newQuietHarnessBackend(t, quietHTTPFlow, true, quietConfig(), backend.server.URL,
		func(h *claimsHarness) { h.o.toolWaitDue = clock.start })

	h.started()
	h.said("Noted.")
	h.audio()
	h.toolCall("fc_ask", "ask", `{"value":"q"}`)
	h.toolCall("fc_kb", "search_kb", `{"q":"refund"}`)
	h.ended()
	backend.awaitRequest(t, "/kb")
	waitFor(t, "the session to fall quiet", h.session.isQuiet)
	clock.fire(t, 1)

	waitFor(t, "the wait line", func() bool { return len(h.model.spokenLines()) == 1 })
	if got := h.model.spokenLines()[0]; got != defaultWaitLines["en"] {
		t.Errorf("the wait line is %q, want %q", got, defaultWaitLines["en"])
	}
	if got := h.sends(); got != "SpeakText" {
		t.Fatalf("sends = %s while the tool runs, want the line alone: the slot answer stays held", got)
	}

	backend.answer()
	h.awaitSends(t, "SpeakText,SendToolResult,SendToolResult")
	var order []string
	for _, result := range h.model.recordedToolResults() {
		order = append(order, result.id)
	}
	if want := []string{"fc_ask", "fc_kb"}; !slices.Equal(order, want) {
		t.Errorf("results went out as %v, want %v", order, want)
	}
	if got := h.quiet.quietResults(); len(got) != 0 {
		t.Errorf("a batch with an HTTP tool answered quietly: %v", got)
	}
	if n := strings.Count(h.logs.String(), "said the wait line"); n != 1 {
		t.Errorf("the wait line was logged %d times, want once", n)
	}
}
