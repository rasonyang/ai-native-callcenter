// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Issue #553: a hangup made in a response with no words, in reply to a
// caller's question, ended the call; the answer guard of #484 had no words to
// judge. FLOW_INSTRUCTED is now refused in a phase that does not declare
// isFlowHangupAllowed, and a wordless hangup that runs is counted by what the
// caller last said, whenever their transcript arrives.

const (
	wordlessCountedLog = `msg="wordless hangup counted: the response that made it had said nothing`
	flowHangupRefused  = "hangup refused: the phase does not let the flow order a hangup"
)

// declaredGuardFlow is guardFlow whose welcome lets the flow order a hangup.
var declaredGuardFlow = strings.Replace(guardFlow,
	`"welcome": {"instruction": "Help the caller.", "tools": ["note"]}`,
	`"welcome": {"instruction": "Help the caller.", "tools": ["note"], "isFlowHangupAllowed": true}`, 1)

// The farewell's line in guardFlow, which rides in the hangup's answer.
var guardClosingLines = map[string]string{"en": "Thank you for calling NovaNet. Goodbye.", "zh": "感谢致电，再见。"}

// When the caller's transcript arrives, against the hangup it led to.
type transcriptTiming int

const (
	wordsBefore transcriptTiming = iota // before the response that hangs up starts
	wordsAfter                          // after the hangup ran, as measured in #484
	wordsNever                          // not before the call ends
)

// callerSpeaks plays the caller's speech as the provider reports it.
func (h *quietHarness) callerSpeaks(item string) {
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item}
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: item}
}

// transcribed is the final transcript of the caller's speech.
func (h *quietHarness) transcribed(item, text string) {
	h.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, Text: text, IsFinal: true, ItemID: item}
}

// wordlessHangUp plays a response that is a hangup and nothing else: no words,
// no audio, as in the evidence of #553.
func (h *quietHarness) wordlessHangUp(args string) {
	h.started()
	h.toolCall("fc_h", flow.ToolHangup, args)
	h.ended()
}

// callerTurnThenWordlessHangup plays the caller's turn, with its transcript
// where timing says, and a wordless hangup in reply. It returns once the
// hangup has been answered, with a late transcript not yet sent.
func (h *quietHarness) callerTurnThenWordlessHangup(t *testing.T, words string, timing transcriptTiming, args string) {
	t.Helper()
	h.callerSpeaks("item_q")
	if timing == wordsBefore {
		h.transcribed("item_q", words)
	}
	h.wordlessHangUp(args)
	h.awaitResults(t, 1)
}

// isCallEnded reports whether drive has returned.
func (h *quietHarness) isCallEnded() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// readWordlessHangups reads aicc_bot_wordless_hangups_total by
// "reason/caller_turn/provider", through the reader the turn tests installed
// (the global meter provider is set once per test binary).
func readWordlessHangups(t *testing.T) map[string]int64 {
	t.Helper()
	setTurnReaders.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(turnReader)))
	})
	var rm metricdata.ResourceMetrics
	if err := turnReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aicc_bot_wordless_hangups_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				var key []string
				for _, name := range []string{"reason", "caller_turn", "provider"} {
					v, _ := dp.Attributes.Value(attribute.Key(name))
					key = append(key, v.AsString())
				}
				out[strings.Join(key, "/")] += dp.Value
			}
		}
	}
	return out
}

// The evidence's shape: the caller asks a question, the response is a bare
// hangup with reason FLOW_INSTRUCTED, in a phase whose flow orders no hangup.
// It is refused as a conversation: nothing moves or is armed, the model is
// asked for a turn (it has said nothing), and the refusal offers no
// FLOW_INSTRUCTED back. Nothing is counted: the hangup did not run. Whenever the
// caller's transcript arrives, the decision is the same: it never waits for it.
func TestAWordlessFlowHangupWhereTheFlowOrdersNoneIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		lang, words string
		timing      transcriptTiming
	}{
		"en question, words before":              {"en", "How much is the fibre plan?", wordsBefore},
		"zh question, words after":               {"zh", "宽带多少钱？", wordsAfter},
		"en goodbye as a question, words after":  {"en", "ok, that's all, bye?", wordsAfter},
		"zh goodbye as a question, words before": {"zh", "好的，就这些，可以挂了吧？", wordsBefore},
		"en mixed, words before":                 {"en", "thanks — what else do you have?", wordsBefore},
		"en question, words never":               {"en", "How much is the fibre plan?", wordsNever},
	} {
		t.Run(name, func(t *testing.T) {
			h := newGuardHarnessWith(t, provider.QwenProfile(), guardFlow, tc.lang, true, quietConfig())
			h.callerTurnThenWordlessHangup(t, tc.words, tc.timing, `{"reason":"FLOW_INSTRUCTED"}`)
			if tc.timing == wordsAfter {
				h.transcribed("item_q", tc.words)
			}
			settle()

			results := h.model.recordedToolResults()
			if len(results) != 1 {
				t.Fatalf("results asking for a turn = %v, want the one refusal", results)
			}
			out := results[0].output
			want := map[string]string{"en": "the flow does not order an ending in this phase",
				"zh": "当前环节的流程没有要求结束通话"}[tc.lang]
			if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, want) ||
				strings.Contains(out, flow.HangupFlowInstructed) {
				t.Errorf("result = %s, want the refusal that says %q and offers no %s back", out, want,
					flow.HangupFlowInstructed)
			}
			if h.node() != "welcome" || h.actions.isArmed() || h.isCallEnded() {
				t.Errorf("node %q, armed %v, ended %v: the refused hangup acted", h.node(), h.actions.isArmed(),
					h.isCallEnded())
			}
			logs := h.logs.String()
			for _, fragment := range []string{guardBlindLog, flowHangupRefused} {
				if !strings.Contains(logs, fragment) {
					t.Errorf("the log lacks %q:\n%s", fragment, logs)
				}
			}
			for _, fragment := range []string{wordlessCountedLog, `msg="tool ran"`} {
				if strings.Contains(logs, fragment) {
					t.Errorf("the log says %q for a refused hangup:\n%s", fragment, logs)
				}
			}
		})
	}
}

// A wordless hangup with one of the caller's reasons runs, whatever the caller
// said: the guard does not work out what the caller meant. The closing line
// rides in its answer and the call ends once the caller has heard it, and the
// hangup is counted by the caller's last words: at once when they were known,
// when they arrive when they came late, and as UNKNOWN when the call ended
// first. The cheap label reads only the end of the words, so a goodbye phrased
// as a question counts as QUESTION.
func TestAWordlessHangupWithACallersReasonRunsAndIsCounted(t *testing.T) {
	for name, tc := range map[string]struct {
		lang, words, reason string
		timing              transcriptTiming
		want                string
	}{
		"en question, words before": {"en", "How much is the fibre plan?", flow.HangupCallerGoodbye,
			wordsBefore, obs.CallerTurnQuestion},
		"zh question, words after": {"zh", "宽带多少钱？", flow.HangupCallerDeclinedMoreHelp,
			wordsAfter, obs.CallerTurnQuestion},
		"en goodbye as a question, words after": {"en", "ok, that's all, bye?", flow.HangupCallerGoodbye,
			wordsAfter, obs.CallerTurnQuestion},
		"zh goodbye as a question, words before": {"zh", "好的，就这些，可以挂了吧？", flow.HangupCallerAskedToEnd,
			wordsBefore, obs.CallerTurnQuestion},
		"en mixed, words after": {"en", "thanks — what else do you have?", flow.HangupCallerGoodbye,
			wordsAfter, obs.CallerTurnQuestion},
		"en question, words never": {"en", "How much is the fibre plan?", flow.HangupCallerGoodbye,
			wordsNever, obs.CallerTurnUnknown},
		"en goodbye, words before": {"en", "No, that's all, thanks. Bye.", flow.HangupCallerGoodbye,
			wordsBefore, obs.CallerTurnOther},
		"zh goodbye, words after": {"zh", "没有了，谢谢，再见。", flow.HangupCallerDeclinedMoreHelp,
			wordsAfter, obs.CallerTurnOther},
	} {
		t.Run(name, func(t *testing.T) {
			before := readWordlessHangups(t)
			h := newGuardHarnessWith(t, provider.QwenProfile(), guardFlow, tc.lang, true, quietConfig())
			h.callerTurnThenWordlessHangup(t, tc.words, tc.timing, `{"reason":"`+tc.reason+`"}`)

			// The decision: the hangup ran, into the farewell, with its line.
			out := h.model.recordedToolResults()[0].output
			if got, want := hintOf(t, out), provider.SayExactly(guardClosingLines[tc.lang], tc.lang); got != want {
				t.Errorf("hint = %q, want the closing line", got)
			}
			if h.node() != "farewell" || !h.actions.isArmed() {
				t.Fatalf("node %q, armed %v: want the hangup run", h.node(), h.actions.isArmed())
			}
			if !strings.Contains(h.logs.String(), guardBlindLog) {
				t.Errorf("the guard's blind spot was not logged:\n%s", h.logs.String())
			}
			// Nothing waited for the words, and nothing is counted without them.
			if tc.timing != wordsBefore && strings.Contains(h.logs.String(), wordlessCountedLog) {
				t.Errorf("counted before the caller's words were known:\n%s", h.logs.String())
			}
			if tc.timing == wordsAfter {
				h.transcribed("item_q", tc.words)
				h.awaitLog(t, wordlessCountedLog)
			}

			// The closing line: the call ends only once the caller has heard it.
			h.leg.holdFrames(3)
			h.started()
			h.audio()
			h.said(guardClosingLines[tc.lang])
			h.ended()
			settle()
			if h.isCallEnded() {
				t.Fatal("the call ended before the caller heard the closing line")
			}
			h.leg.holdFrames(0)
			cdr := h.awaitEnd(t)
			if got := cdr.UserData["botReason"]; got != tc.reason {
				t.Errorf("userData.botReason = %v, want %s", got, tc.reason)
			}

			logs := h.logs.String()
			line := wordlessCountedLog + `" reason=` + tc.reason + " callerTurn=" + tc.want
			if strings.Count(logs, wordlessCountedLog) != 1 || !strings.Contains(logs, line) {
				t.Errorf("want one count line with %q:\n%s", line, logs)
			}
			if isLate := tc.timing == wordsAfter; !strings.Contains(logs,
				"isCallerWordsLate="+map[bool]string{true: "true", false: "false"}[isLate]) {
				t.Errorf("isCallerWordsLate is not %v:\n%s", isLate, logs)
			}
			key := tc.reason + "/" + tc.want + "/" + provider.QwenProfile().Name
			if got := readWordlessHangups(t)[key] - before[key]; got != 1 {
				t.Errorf("%s gained %d, want 1", key, got)
			}
		})
	}
}

// The caller's words of an earlier turn do not label a hangup made after they
// spoke again: the newer turn's words are owed, and they are the ones counted.
func TestAWordlessHangupIsCountedByTheCallersNewestTurn(t *testing.T) {
	h := newGuardHarnessWith(t, provider.QwenProfile(), guardFlow, "en", true, quietConfig())
	h.callerSpeaks("item_1")
	h.transcribed("item_1", "I'd like to hear about your broadband.")
	h.answered("We have fibre and copper plans. Which one interests you?")
	h.callerSpeaks("item_2")
	h.wordlessHangUp(goodbyeHangup)
	h.awaitResults(t, 1)
	settle()
	if strings.Contains(h.logs.String(), wordlessCountedLog) {
		t.Fatalf("counted on the earlier turn's words:\n%s", h.logs.String())
	}
	h.transcribed("item_2", "How much is the fibre plan?")
	h.awaitLog(t, wordlessCountedLog+`" reason=CALLER_GOODBYE callerTurn=QUESTION`)
}

// An armed ending, a decided ending (the time limit) and a terminal phase are
// left to the closing paths, FLOW_INSTRUCTED or not: nothing about the reason
// is judged and no new ending is counted.
func TestAWordlessFlowHangupOverAnEndingUnderwayIsLeftToTheClosingPaths(t *testing.T) {
	t.Run("armed", func(t *testing.T) {
		h := newGuardHarness(t, provider.QwenProfile(), "en", true)
		h.started()
		h.toolCall("fc_1", flow.ToolTakeMessage, `{"message":"call me"}`)
		h.ended()
		h.awaitResults(t, 1)
		h.actions.arm(t.Context(), func() {})
		h.callerSpeaks("item_q")
		h.transcribed("item_q", "How much is the fibre plan?")
		h.wordlessHangUp(`{"reason":"FLOW_INSTRUCTED"}`)
		h.awaitResults(t, 2)
		settle()
		logs := h.logs.String()
		if strings.Contains(logs, flowHangupRefused) || strings.Contains(logs, wordlessCountedLog) {
			t.Errorf("a hangup over an armed ending was judged or counted:\n%s", logs)
		}
		if !strings.Contains(logs, "hangup ignored: the call's ending is already armed") {
			t.Errorf("the hangup did not reach the armed ending's own path:\n%s", logs)
		}
	})
	t.Run("decided by the time limit", func(t *testing.T) {
		h := newGuardHarness(t, provider.QwenProfile(), "en", true)
		h.actions.lockEnding()
		h.callerSpeaks("item_q")
		h.transcribed("item_q", "How much is the fibre plan?")
		h.wordlessHangUp(`{"reason":"FLOW_INSTRUCTED"}`)
		h.awaitResults(t, 1)
		settle()
		logs := h.logs.String()
		if strings.Contains(logs, flowHangupRefused) || strings.Contains(logs, wordlessCountedLog) {
			t.Errorf("a hangup over a decided ending was judged or counted:\n%s", logs)
		}
		if !strings.Contains(logs, "hangup ignored: the call's time limit has already decided how it ends") {
			t.Errorf("the hangup did not reach the decided ending's path:\n%s", logs)
		}
	})
	t.Run("terminal phase", func(t *testing.T) {
		h := newGuardHarness(t, provider.QwenProfile(), "en", true)
		h.callerSpeaks("item_1")
		h.transcribed("item_1", "No, that's all, thanks. Bye.")
		h.wordlessHangUp(goodbyeHangup)
		h.awaitResults(t, 1)
		h.awaitLog(t, wordlessCountedLog)
		if h.node() != "farewell" {
			t.Fatalf("node %q, want farewell", h.node())
		}
		h.callerSpeaks("item_q")
		h.transcribed("item_q", "Wait, how much is the fibre plan?")
		h.wordlessHangUp(`{"reason":"FLOW_INSTRUCTED"}`)
		h.awaitResults(t, 2)
		settle()
		logs := h.logs.String()
		if strings.Contains(logs, flowHangupRefused) || strings.Count(logs, wordlessCountedLog) != 1 {
			t.Errorf("a hangup in a terminal phase was judged or counted:\n%s", logs)
		}
	})
}

// In a phase that lets the flow order a hangup, a wordless FLOW_INSTRUCTED
// hangup runs as before, and is counted like any other.
func TestAWordlessFlowHangupWhereTheFlowOrdersOneRuns(t *testing.T) {
	h := newGuardHarnessWith(t, provider.QwenProfile(), declaredGuardFlow, "zh", true, quietConfig())
	h.callerTurnThenWordlessHangup(t, "我不是机主。", wordsBefore, `{"reason":"FLOW_INSTRUCTED"}`)
	if h.node() != "farewell" || !h.actions.isArmed() {
		t.Fatalf("node %q, armed %v: want the hangup run", h.node(), h.actions.isArmed())
	}
	h.awaitLog(t, wordlessCountedLog+`" reason=FLOW_INSTRUCTED callerTurn=OTHER`)
	if strings.Contains(h.logs.String(), flowHangupRefused) {
		t.Errorf("refused in a phase that declares it:\n%s", h.logs.String())
	}
}

// Doubao makes a tool call a turn of its own, so every hangup there is
// wordless: it is counted like any other, and nothing else changes.
func TestOnDoubaoEveryHangupIsWordlessAndCounted(t *testing.T) {
	h := newGuardHarnessWith(t, provider.DoubaoProfile(), guardFlow, "zh", false, quietConfig())
	h.callerSpeaks("item_1")
	h.transcribed("item_1", "宽带多少钱？")
	h.started()
	h.audio()
	h.said("光纤宽带每月五十元。")
	h.ended()
	h.wordlessHangUp(goodbyeHangup)
	h.awaitResults(t, 1)
	if h.node() != "farewell" {
		t.Fatalf("node %q, want farewell", h.node())
	}
	h.awaitLog(t, wordlessCountedLog+`" reason=CALLER_GOODBYE callerTurn=QUESTION`)
}

// The count's bookkeeping, alone: words known count at once; words owed or
// never given wait; an empty transcript is not words; each hangup counts once,
// and what is still waiting at the end is UNKNOWN.
func TestTheWordlessCountWaitsForTheCallersWordsAndCountsEachHangupOnce(t *testing.T) {
	var logs bytes.Buffer
	w := wordlessHangups{providerName: "test", log: slog.New(slog.NewTextHandler(&logs, nil))}
	count := func(fragment string) int { return strings.Count(logs.String(), fragment) }

	w.onHangupPassed(flow.HangupCallerGoodbye, 1, "welcome")
	if count(wordlessCountedLog) != 0 {
		t.Fatal("counted with no caller words at all")
	}
	w.onCallerWords("  ")
	if count(wordlessCountedLog) != 0 {
		t.Fatal("an empty transcript counted it")
	}
	w.onCallerWords("Is that all?")
	if count(`callerTurn=QUESTION turn=1 node=welcome isCallerWordsLate=true`) != 1 {
		t.Fatalf("want it counted on the words that came:\n%s", logs.String())
	}

	w.onHangupPassed(flow.HangupCallerDeclinedMoreHelp, 2, "welcome")
	if count(`reason=CALLER_DECLINED_MORE_HELP callerTurn=QUESTION turn=2 node=welcome isCallerWordsLate=false`) != 1 {
		t.Fatalf("want it counted at once on the known words:\n%s", logs.String())
	}

	w.onCallerSpeech()
	w.onHangupPassed(flow.HangupCallerGoodbye, 3, "welcome")
	w.onHangupPassed(flow.HangupCallerGoodbye, 4, "welcome")
	if count(wordlessCountedLog) != 2 {
		t.Fatalf("counted on words the caller has spoken over:\n%s", logs.String())
	}
	w.onCallerWords("No, thanks. Bye.")
	for _, turn := range []string{"turn=3", "turn=4"} {
		if count(`callerTurn=OTHER `+turn) != 1 {
			t.Errorf("%s not counted once as OTHER:\n%s", turn, logs.String())
		}
	}

	w.onCallerSpeech()
	w.onHangupPassed(flow.HangupCallerGoodbye, 5, "welcome")
	w.onCallEnded()
	w.onCallEnded()
	if count(`callerTurn=UNKNOWN turn=5`) != 1 || count(wordlessCountedLog) != 5 {
		t.Errorf("want the last one counted once as UNKNOWN, five in all:\n%s", logs.String())
	}
}

// The label reads only whether the words end in a question.
func TestTheCallersTurnIsLabelledByItsLastSentence(t *testing.T) {
	for text, want := range map[string]string{
		"How much is the fibre plan?":     obs.CallerTurnQuestion,
		"宽带多少钱？":                          obs.CallerTurnQuestion,
		"ok, that's all, bye?":            obs.CallerTurnQuestion,
		"好的，就这些，可以挂了吧？":                   obs.CallerTurnQuestion,
		"thanks — what else do you have?": obs.CallerTurnQuestion,
		"No, that's all, thanks. Bye.":    obs.CallerTurnOther,
		"没有了，谢谢，再见。":                      obs.CallerTurnOther,
		"Is it fast? I'll take it.":       obs.CallerTurnOther,
	} {
		if got := callerTurnOf(text); got != want {
			t.Errorf("callerTurnOf(%q) = %s, want %s", text, got, want)
		}
	}
}
