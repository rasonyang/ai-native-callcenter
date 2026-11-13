// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Issue #484: on qwen flash the model answered the caller's first question and
// called hangup in the same response; the answer was muted and the call ended
// on the farewell. A hangup in a response that has already answered is refused
// as a conversation (answer_guard.go), and a hangup without a valid reason is
// refused by the runtime.

// guardFlow is novanet_support's shape: hangup is offered from the greeting
// on, and any hangup result moves to a terminal goodbye with its own line.
const guardFlow = `{
	"id": "guard-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"closingTarget": "farewell",
		"alwaysAllowedTools": ["hangup", "take_message"],
		"transitions": [{"on": "TOOL_RESULT", "tool": "hangup", "target": "farewell"}]
	},
	"tools": {
		"note": {"kind": "slot", "slot": "note", "description": "Record a note."}
	},
	"nodes": {
		"welcome": {"instruction": "Help the caller.", "tools": ["note"]},
		"farewell": {
			"instruction": "Say goodbye.",
			"announce": {"en": "Thank you for calling NovaNet. Goodbye.", "zh": "感谢致电，再见。"},
			"tools": [], "isTerminal": true
		}
	}
}`

const (
	guardRefusedLog = "hangup refused: the response that made it was answering the caller"
	guardSpentLog   = "after the answer guard had already refused once"
	guardBlindLog   = "a hangup arrived before any words of its response"
	goodbyeHangup   = `{"reason":"CALLER_GOODBYE"}`
)

// The #484 answer, in the pieces a Realtime response delivers it in, before
// and after the call.
var guardAnswers = map[string]struct {
	before                     []string
	after, question, statement string
}{
	"en": {
		before:    []string{"NovaNet is a telecommunications company. ", "We provide internet, mobile, and home phone services."},
		after:     " How can I help you today?",
		question:  "NovaNet is a telecommunications company. We provide internet, mobile, and home phone services. How can I help you today?",
		statement: "NovaNet is a telecommunications company. We provide internet, mobile, and home phone services.",
	},
	"zh": {
		before:    []string{"诺瓦网络是一家电信公司，", "我们提供宽带、手机和固话服务。"},
		after:     "请问还有什么可以帮您？",
		question:  "诺瓦网络是一家电信公司，我们提供宽带、手机和固话服务。请问还有什么可以帮您？",
		statement: "诺瓦网络是一家电信公司，我们提供宽带、手机和固话服务。",
	},
}

func newGuardHarness(t *testing.T, profile provider.Profile, lang string, isQuietModel bool) *quietHarness {
	t.Helper()
	return newGuardHarnessWith(t, profile, guardFlow, lang, isQuietModel, quietConfig())
}

// newGuardHarnessWith is newGuardHarness on a flow and a session configuration
// of the test's own.
func newGuardHarnessWith(t *testing.T, profile provider.Profile, flowJSON, lang string, isQuietModel bool,
	cfg Config) *quietHarness {
	t.Helper()
	h := &quietHarness{}
	wrap := func(m *fakeModel) provider.VoiceSession {
		if !isQuietModel {
			return m
		}
		h.quiet = &quietModel{fakeModel: m}
		return h.quiet
	}
	h.claimsHarness = newClaimsHarnessModel(t, profile, flowJSON, lang, "", cfg, wrap, nil)
	return h
}

// The two texts of the guard's refusal (flow.Runtime.RefuseWhileAnswering), by
// what tells them apart: the one that asks for a turn has the model ask whether
// anything else is needed, the quiet one has it answer the caller's next words.
var (
	refusalAsksForATurn = map[string]string{"en": "Ask whether they need anything else", "zh": "问对方还有没有其他需要"}
	refusalSentQuietly  = map[string]string{"en": "Answer what they say next", "zh": "回应对方接下来说的话"}
)

// delta is a piece of the model's transcript, as the Realtime deltas carry it.
func (h *quietHarness) delta(text string) {
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: text}
}

func (h *quietHarness) node() string { return h.runtime.Engine().NodeID() }

// allResults is every tool answer the model was given, either way.
func (h *quietHarness) allResults() []toolResult {
	results := h.model.recordedToolResults()
	if h.quiet != nil {
		results = append(results, h.quiet.quietResults()...)
	}
	return results
}

func (h *quietHarness) awaitResults(t *testing.T, n int) []toolResult {
	t.Helper()
	waitFor(t, "the tool calls to be answered", func() bool { return len(h.allResults()) >= n })
	return h.allResults()
}

// answerAndHangUp plays the #484 response: answer words, audio, the hangup,
// the rest of the answer (ending in a question when isQuestion), and the end.
func (h *quietHarness) answerAndHangUp(lang string, isQuestion bool, args string) {
	answer := guardAnswers[lang]
	h.started()
	for _, piece := range answer.before {
		h.delta(piece)
	}
	h.audio()
	h.toolCall("fc_h", flow.ToolHangup, args)
	final := answer.statement
	if isQuestion {
		h.delta(answer.after)
		final = answer.question
	}
	h.said(final)
	h.audio()
	h.ended()
}

// The #484 response: answer words, then hangup, then more words ending in a
// question. The hangup is refused, nothing moves or is armed, the answer is not
// muted, and the refusal goes without asking for a turn: the caller has been
// asked and speaks next.
func TestAHangupInAResponseThatAnsweredIsRefused(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h := newGuardHarness(t, provider.OpenAIProfile(), lang, true)

			h.answerAndHangUp(lang, true, goodbyeHangup)
			h.awaitSends(t, "SendToolResultQuietly")
			settle()

			out := h.quiet.quietResults()[0].output
			want := "the call was not ended"
			if lang == "zh" {
				want = "通话没有结束"
			}
			if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, want) ||
				!strings.Contains(out, refusalSentQuietly[lang]) {
				t.Errorf("result = %s, want the quiet refusal that says %q", out, want)
			}
			if h.node() != "welcome" || h.actions.isArmed() {
				t.Errorf("node %q, armed %v: the refused hangup moved or armed something", h.node(), h.actions.isArmed())
			}
			if h.session.isMutedTurn(1) || strings.Contains(h.logs.String(), "muted the turn") {
				t.Error("the answer was muted")
			}
			if n := len(h.leg.sentFrames()); n != 2 {
				t.Errorf("%d frames played, want both of the answer's", n)
			}
			if got := h.model.recordedToolResults(); len(got) != 0 {
				t.Errorf("a turn was asked for: %v", got)
			}
			logs := h.logs.String()
			for _, fragment := range []string{guardRefusedLog, "turn=1", "textRunes=", "turnAudioMs=20", "queuedMs=0",
				"endsInQuestion=true isQuiet=true"} {
				if !strings.Contains(logs, fragment) {
					t.Errorf("the log lacks %q:\n%s", fragment, logs)
				}
			}
			if strings.Contains(logs, "tool ran") {
				t.Errorf("the refused hangup ran:\n%s", logs)
			}
		})
	}
}

// The same response ending in a statement: the refusal goes quietly too
// (#555). Asked for a turn, qwen flash said the whole answer again; now no
// turn is asked for, the bot holds no floor, and the refusal tells the model
// to answer what the caller says next.
func TestARefusalAfterAStatementGoesQuietly(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			eachProfile(t, func(t *testing.T, profile provider.Profile) {
				h := newGuardHarness(t, profile, lang, true)

				h.answerAndHangUp(lang, false, goodbyeHangup)
				h.awaitSends(t, "SendToolResultQuietly")
				settle()
				if got := h.sends(); got != "SendToolResultQuietly" {
					t.Errorf("sends = %s, want the quiet refusal and nothing more", got)
				}
				if got := h.model.recordedToolResults(); len(got) != 0 {
					t.Errorf("a turn was asked for: %v", got)
				}
				out := h.quiet.quietResults()[0].output
				if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, refusalSentQuietly[lang]) ||
					strings.Contains(out, refusalAsksForATurn[lang]) {
					t.Errorf("result = %s, want the quiet refusal", out)
				}
				if h.holdsTheFloor() || h.session.isHoldingTheFloor() {
					t.Error("the bot holds the floor for a turn nobody asked for")
				}
				if !strings.Contains(h.logs.String(), "endsInQuestion=false isQuiet=true") {
					t.Errorf("the close was not logged:\n%s", h.logs.String())
				}
				if h.node() != "welcome" || h.actions.isArmed() {
					t.Errorf("node %q, armed %v", h.node(), h.actions.isArmed())
				}
			})
		})
	}
}

// A model that cannot answer quietly gets the refusal at once, the ordinary
// way, before its response has even ended, with the text that has the model
// ask whether anything else is needed.
func TestARefusalOnAModelWithoutQuietAnswersGoesAtOnce(t *testing.T) {
	h := newGuardHarness(t, provider.OpenAIProfile(), "en", false)

	answer := guardAnswers["en"]
	h.started()
	for _, piece := range answer.before {
		h.delta(piece)
	}
	h.audio()
	h.toolCall("fc_h", flow.ToolHangup, goodbyeHangup)
	h.awaitSends(t, "SendToolResult")
	if out := h.model.recordedToolResults()[0].output; !strings.Contains(out, `"ok":"0"`) ||
		!strings.Contains(out, refusalAsksForATurn["en"]) {
		t.Errorf("result = %s, want the refusal that asks whether anything else is needed", out)
	}
	h.ended()
	settle()
	if h.node() != "welcome" || h.actions.isArmed() {
		t.Errorf("node %q, armed %v", h.node(), h.actions.isArmed())
	}
}

// On a model that can answer quietly, a held refusal whose response cannot go
// quiet (it queued no audio) is released the ordinary way at the response's
// end, with the text that asks whether anything else is needed.
func TestAHeldRefusalThatCannotGoQuietAsksForATurn(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h := newGuardHarness(t, provider.QwenProfile(), lang, true)

			answer := guardAnswers[lang]
			h.started()
			for _, piece := range answer.before {
				h.delta(piece)
			}
			h.toolCall("fc_h", flow.ToolHangup, goodbyeHangup)
			h.said(answer.statement)
			settle()
			if got := h.sends(); got != "" {
				t.Fatalf("sends = %q while the response is open, want the refusal held", got)
			}
			h.ended()
			h.awaitSends(t, "SendToolResult")
			if out := h.model.recordedToolResults()[0].output; !strings.Contains(out, refusalAsksForATurn[lang]) {
				t.Errorf("result = %s, want the refusal that asks whether anything else is needed", out)
			}
			if !strings.Contains(h.logs.String(), "endsInQuestion=false isQuiet=false") {
				t.Errorf("the close was not logged:\n%s", h.logs.String())
			}
		})
	}
}

// Qwen's other order: the call comes before any of the response's words. The
// guard has nothing to judge and lets it through: this is its blind spot, and
// the pass is logged whether or not the response had already queued audio
// (#553: before, a response with no audio left no line at all).
func TestAHangupBeforeAnyWordsPasses(t *testing.T) {
	for name, hasAudio := range map[string]bool{"with audio queued": true, "with nothing yet": false} {
		t.Run(name, func(t *testing.T) {
			h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
			h.started()
			if hasAudio {
				h.audio()
			}
			h.toolCall("fc_h", flow.ToolHangup, goodbyeHangup)
			h.delta(guardAnswers["en"].question)
			h.audio()
			h.ended()
			h.awaitResults(t, 1)
			settle()
			if h.node() != "farewell" || !h.actions.isArmed() {
				t.Errorf("node %q, armed %v: want the hangup run", h.node(), h.actions.isArmed())
			}
			if !strings.Contains(h.logs.String(), guardBlindLog) {
				t.Errorf("the blind spot's pass was not logged:\n%s", h.logs.String())
			}
			if strings.Contains(h.logs.String(), guardRefusedLog) {
				t.Error("the guard refused a hangup with nothing said before it")
			}
		})
	}
}

// A goodbye, a near-empty acknowledgement, and a hangup after another tool
// call of the same response (#262) still end the call.
func TestTheGuardLetsAGoodbyeAnAcknowledgementAndABatchThrough(t *testing.T) {
	for name, tc := range map[string]struct {
		lang  string
		words []string
		calls [][2]string
	}{
		"en goodbye":         {"en", []string{"Goodbye, take care!"}, [][2]string{{flow.ToolHangup, goodbyeHangup}}},
		"zh goodbye":         {"zh", []string{"好的，再见！"}, [][2]string{{flow.ToolHangup, goodbyeHangup}}},
		"en acknowledgement": {"en", []string{"Okay, ", "thank you."}, [][2]string{{flow.ToolHangup, goodbyeHangup}}},
		"zh acknowledgement": {"zh", []string{"好的，没问题。"}, [][2]string{{flow.ToolHangup, goodbyeHangup}}},
		"a goodbye after a long answer": {"en", []string{"NovaNet is a telecommunications company. ",
			"Thanks for calling, have a great day!"}, [][2]string{{flow.ToolHangup, goodbyeHangup}}},
		"words narrating another tool first": {"en", []string{"Great, I have noted that your router keeps rebooting for you."},
			[][2]string{{"note", `{"value":"router"}`}, {flow.ToolHangup, goodbyeHangup}}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newGuardHarness(t, provider.OpenAIProfile(), tc.lang, true)
			h.started()
			for _, piece := range tc.words {
				h.delta(piece)
			}
			h.audio()
			for i, call := range tc.calls {
				h.toolCall("fc_"+string(rune('a'+i)), call[0], call[1])
			}
			h.ended()
			h.awaitResults(t, len(tc.calls))
			settle()
			if h.node() != "farewell" || !h.actions.isArmed() {
				t.Errorf("node %q, armed %v: want the call ending", h.node(), h.actions.isArmed())
			}
			if strings.Contains(h.logs.String(), guardRefusedLog) {
				t.Errorf("the guard refused:\n%s", h.logs.String())
			}
		})
	}
}

// Over an armed ending, in a terminal phase, or once the time limit decided
// the ending, neither the guard nor the reason is judged.
func TestTheGuardAndTheReasonAreNotJudgedOverAnEndingUnderway(t *testing.T) {
	t.Run("armed", func(t *testing.T) {
		h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
		h.started()
		h.toolCall("fc_1", flow.ToolTakeMessage, `{"message":"call me"}`)
		h.ended()
		h.awaitResults(t, 1)
		h.actions.arm(t.Context(), func() {})
		h.answerAndHangUp("en", true, `{}`)
		h.awaitResults(t, 2)
		settle()
		logs := h.logs.String()
		if strings.Contains(logs, guardRefusedLog) || strings.Contains(logs, "hangup refused: no valid reason") {
			t.Errorf("a hangup over an armed ending was judged:\n%s", logs)
		}
		if !strings.Contains(logs, "hangup ignored: the call's ending is already armed") {
			t.Errorf("the hangup did not reach the armed ending's own path:\n%s", logs)
		}
	})
	t.Run("decided by the time limit", func(t *testing.T) {
		h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
		h.actions.lockEnding()
		h.answerAndHangUp("en", true, `{}`)
		h.awaitResults(t, 1)
		settle()
		logs := h.logs.String()
		if strings.Contains(logs, guardRefusedLog) || strings.Contains(logs, "hangup refused: no valid reason") {
			t.Errorf("a hangup over a decided ending was judged:\n%s", logs)
		}
		if !strings.Contains(logs, "hangup ignored: the call's time limit has already decided how it ends") {
			t.Errorf("the hangup did not reach the decided ending's path:\n%s", logs)
		}
	})
	t.Run("terminal phase", func(t *testing.T) {
		h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
		h.started()
		h.toolCall("fc_1", flow.ToolHangup, goodbyeHangup)
		h.ended()
		h.awaitResults(t, 1)
		if h.node() != "farewell" {
			t.Fatalf("node %q, want farewell", h.node())
		}
		h.answerAndHangUp("en", true, `{}`)
		h.awaitResults(t, 2)
		settle()
		logs := h.logs.String()
		if strings.Contains(logs, guardRefusedLog) || strings.Contains(logs, "hangup refused: no valid reason") {
			t.Errorf("a hangup in a terminal phase was judged:\n%s", logs)
		}
	})
}

// Once per caller turn: a second hangup before the caller has spoken words
// goes through, logged. After new caller words the guard is live again.
func TestTheGuardRefusesOncePerCallerTurn(t *testing.T) {
	t.Run("the second attempt passes", func(t *testing.T) {
		h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
		h.answerAndHangUp("en", false, goodbyeHangup)
		h.awaitResults(t, 1)
		// The model's reply to the refusal answers again and hangs up again.
		h.answerAndHangUp("en", false, goodbyeHangup)
		h.awaitResults(t, 2)
		settle()
		if h.node() != "farewell" || !h.actions.isArmed() {
			t.Errorf("node %q, armed %v: want the second hangup run", h.node(), h.actions.isArmed())
		}
		logs := h.logs.String()
		if strings.Count(logs, guardRefusedLog) != 1 || !strings.Contains(logs, guardSpentLog) {
			t.Errorf("want one refusal and one logged pass:\n%s", logs)
		}
	})
	t.Run("new caller words make it live again", func(t *testing.T) {
		h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
		h.answerAndHangUp("en", true, goodbyeHangup)
		h.awaitResults(t, 1)
		h.callerSays("And how much is the fibre plan?")
		settle()
		h.answerAndHangUp("en", true, goodbyeHangup)
		h.awaitResults(t, 2)
		settle()
		if h.node() != "welcome" || h.actions.isArmed() {
			t.Errorf("node %q, armed %v: want the second hangup refused too", h.node(), h.actions.isArmed())
		}
		if n := strings.Count(h.logs.String(), guardRefusedLog); n != 2 {
			t.Errorf("%d refusals, want 2:\n%s", n, h.logs.String())
		}
	})
	t.Run("an empty transcript is not words", func(t *testing.T) {
		h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
		h.answerAndHangUp("en", false, goodbyeHangup)
		h.awaitResults(t, 1)
		h.callerSays("  ")
		settle()
		h.answerAndHangUp("en", false, goodbyeHangup)
		h.awaitResults(t, 2)
		settle()
		if h.node() != "farewell" {
			t.Errorf("node %q: an empty transcript made the guard live again", h.node())
		}
	})
}

// Doubao synthesises a tool call as a turn of its own (doubao.Session.
// onToolCalls): the words came in the turn before, so the snapshot is empty and
// the guard never fires. Nothing here names the provider.
func TestOnDoubaoTheToolCallsOwnTurnHasNoWordsToJudge(t *testing.T) {
	h := newGuardHarness(t, provider.DoubaoProfile(), "zh", false)
	answer := guardAnswers["zh"]
	h.started()
	for _, piece := range answer.before {
		h.delta(piece)
	}
	h.audio()
	h.said(answer.statement)
	h.ended()
	h.started()
	h.toolCall("fc_h", flow.ToolHangup, goodbyeHangup)
	h.ended()
	h.awaitResults(t, 1)
	settle()
	if h.node() != "farewell" || strings.Contains(h.logs.String(), guardRefusedLog) {
		t.Errorf("node %q; the guard fired on doubao's synthesised turn:\n%s", h.node(), h.logs.String())
	}
}

// A hangup with no reason, or an unknown one, is refused by the runtime: no
// move, nothing armed, and the answer says which reasons there are. With a
// reason, every value ends the call and reaches the log and the CDR.
func TestAHangupWithoutAReasonIsRefusedThroughTheCall(t *testing.T) {
	for name, args := range map[string]string{"missing": `{}`, "unknown": `{"reason":"BYE"}`} {
		t.Run(name, func(t *testing.T) {
			h := newGuardHarness(t, provider.OpenAIProfile(), "en", true)
			h.started()
			h.toolCall("fc_h", flow.ToolHangup, args)
			h.ended()
			results := h.awaitResults(t, 1)
			settle()
			out := results[0].output
			if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, flow.HangupCallerDeclinedMoreHelp) {
				t.Errorf("result = %s, want a refusal naming the reasons", out)
			}
			if h.node() != "welcome" || h.actions.isArmed() {
				t.Errorf("node %q, armed %v", h.node(), h.actions.isArmed())
			}
			if !strings.Contains(h.logs.String(), "hangup refused: no valid reason") {
				t.Errorf("the refusal was not logged:\n%s", h.logs.String())
			}
		})
	}
}

// In a phase that lets the flow order a hangup (#553), every reason ends the
// call; the hangup said nothing, so it is counted by the caller's last words.
func TestEveryHangupReasonEndsTheCallAndReachesTheRecord(t *testing.T) {
	for _, reason := range flow.HangupReasons {
		t.Run(reason, func(t *testing.T) {
			h := newGuardHarnessWith(t, provider.OpenAIProfile(), declaredGuardFlow, "en", true, quietConfig())
			h.callerSays("No, that's all, thanks.")
			h.started()
			// A stray argument beside the reason is ignored.
			h.toolCall("fc_h", flow.ToolHangup, `{"reason":"`+reason+`","isFarewellSpoken":true}`)
			h.ended()
			h.awaitResults(t, 1)
			if h.node() != "farewell" || !h.actions.isArmed() {
				t.Fatalf("node %q, armed %v", h.node(), h.actions.isArmed())
			}
			h.started()
			h.audio()
			h.said("Thank you for calling NovaNet. Goodbye.")
			h.ended()
			cdr := h.awaitEnd(t)
			if got := cdr.UserData["botReason"]; got != reason {
				t.Errorf("userData.botReason = %v, want %s", got, reason)
			}
			if !cdr.IsContained {
				t.Error("a call the bot hung up was not contained")
			}
			logs := h.logs.String()
			if !strings.Contains(logs, `msg="tool ran"`) || !strings.Contains(logs, "reason="+reason) {
				t.Errorf("the reason is not in the log:\n%s", logs)
			}
			if !strings.Contains(logs, wordlessCountedLog+`" reason=`+reason+" callerTurn=OTHER") {
				t.Errorf("the wordless hangup was not counted as OTHER:\n%s", logs)
			}
		})
	}
}

// A hangup alone after the caller's "no, that's all" ends the call in the
// terminal phase with the closing line heard: not on the tool call's turn, not
// when the line has finished generating, only once its audio has played.
func TestAGoodbyeHangupStillEndsWithTheClosingLineHeard(t *testing.T) {
	h := startMutedPath(t, provider.OpenAIProfile(), "en", owedLineFlow)
	h.model.events <- provider.Event{Type: provider.EventTypeInputTranscript,
		Text: "No, that's all, thanks.", IsFinal: true}
	h.callAndAwait(t, flow.ToolHangup, `{"reason":"CALLER_DECLINED_MORE_HELP"}`)
	if got, want := hintOf(t, h.model.recordedToolResults()[0].output),
		provider.SayExactly(owedLines["en"], "en"); got != want {
		t.Fatalf("hint = %q, want the closing line", got)
	}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.settle()
	if h.isCallEnded() {
		t.Fatal("the call ended on the tool call's own turn")
	}

	// The line's turn finishes generating with its audio still queued.
	h.leg.holdFrames(5)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(100)}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: owedLines["en"], IsFinal: true}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.settle()
	if h.isCallEnded() {
		t.Fatal("the call ended when the line finished generating, before the caller heard it")
	}
	h.leg.holdFrames(0)
	h.awaitCallEnded(t, 2*time.Second)
	if strings.Contains(h.logs.String(), guardRefusedLog) {
		t.Error("the guard refused a bare hangup")
	}
}

// The session's own record of a turn's words: deltas as they come, a final
// only for a turn that sent none, cut at a character boundary past the bound,
// reset at the next turn, and snapshot at the tool call in wire order.
func TestTheTurnsWordsAreSnapshotAtTheToolCall(t *testing.T) {
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: "NovaNet is"}
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*3)}
	model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: " a telecom company."}
	model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "a", ToolName: "x", ToolArgs: "{}"}
	model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: " Anything else?"}
	model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
		Text: "NovaNet is a telecom company. Anything else?", IsFinal: true}
	model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "b", ToolName: "y", ToolArgs: "{}"}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}

	first := awaitBridgeEvent(t, session, EventTypeToolCall)
	if first.soFar.text != "NovaNet is a telecom company." || first.soFar.audio != 60*time.Millisecond ||
		first.soFar.priorCalls != 0 {
		t.Errorf("first call's snapshot = %+v", first.soFar)
	}
	second := awaitBridgeEvent(t, session, EventTypeToolCall)
	if second.soFar.text != "NovaNet is a telecom company. Anything else?" || second.soFar.priorCalls != 1 {
		t.Errorf("second call's snapshot = %+v: the final repeated the deltas or the count is off", second.soFar)
	}
	done := awaitBridgeEvent(t, session, EventTypeTurnDone)
	if done.soFar.text != second.soFar.text {
		t.Errorf("TURN_DONE words = %q", done.soFar.text)
	}

	// A turn with only a final, and a long one.
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: "好的，再见。", IsFinal: true}
	model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "c", ToolName: "z", ToolArgs: "{}"}
	model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: strings.Repeat("长", maxTurnTextBytes)}
	model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "d", ToolName: "z", ToolArgs: "{}"}
	third := awaitBridgeEvent(t, session, EventTypeToolCall)
	if third.soFar.text != "好的，再见。" || third.soFar.priorCalls != 0 || third.soFar.audio != 0 {
		t.Errorf("a new turn's snapshot = %+v, want only its own final", third.soFar)
	}
	fourth := awaitBridgeEvent(t, session, EventTypeToolCall)
	if n := len(fourth.soFar.text); n > maxTurnTextBytes || n < maxTurnTextBytes-3 {
		t.Errorf("the words kept are %d bytes, want at most %d", n, maxTurnTextBytes)
	}
	if !strings.HasPrefix(fourth.soFar.text, "好的，再见。长") || !utf8.ValidString(fourth.soFar.text) {
		t.Errorf("the bound cut a character in half, or the final and the delta did not join: %q",
			fourth.soFar.text[len(fourth.soFar.text)-6:])
	}
}

func TestWhatCountsAsAnAnswer(t *testing.T) {
	for text, want := range map[string]bool{
		"":                    false,
		"Okay.":               false,
		"Okay, thank you.":    false,
		"You're welcome.":     false,
		"Sure, no problem.":   false,
		"好的，没问题。":             false,
		"好的，谢谢您。":             false,
		"Goodbye, take care!": false,
		"好的，再见！":              false,
		"NovaNet is a telecommunications company. Thanks for calling, have a great day!": false,
		"NovaNet is a telecommunications company.":                                       true,
		"NovaNet is a telecommunications":                                                true,
		"How can I help you today?":                                                      true,
		"Have a great day! Is there anything else I can do?":                             true,
		"诺瓦网络是一家电信公司":                                                                    true,
		"我们提供宽带、手机和固话服务":                                                                 true,
		"请问还有什么可以帮您？":                                                                    true,
	} {
		if got := isSubstantiveAnswer(text); got != want {
			t.Errorf("isSubstantiveAnswer(%q) = %v, want %v (%d half words)", text, got, want, answerHalfWords(text))
		}
	}
}
