// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"log/slog"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// Counting the hangups the answer guard cannot see (#553). The guard of #484
// judges the words of the response that made a hangup, and a response whose
// call arrives before any words gives it nothing to judge: on qwen flash a
// probe flow had the model answer a caller's question with a bare hangup, and
// the call ended on the question. The owner chose two answers. FLOW_INSTRUCTED,
// the reason that needs no act of the caller, is accepted only in a phase that
// declares isFlowHangupAllowed (flow.Runtime.refuseFlowInstructed). What that
// leaves open is a wordless hangup with a caller's reason the caller never
// gave, and that is counted here, with what the caller last said, so that a
// deployment can see the rate: aicc_bot_wordless_hangups_total
// {reason, caller_turn, provider} and one INFO line per count.
//
// The guard still never works out what the caller meant: judging the caller's
// last turn was the option not chosen. The count reads the caller's words only
// to label the hangup (QUESTION when they end in a question, endsInQuestion,
// else OTHER), and changes nothing about the call: no refusal, no delay.
//
// The caller's transcript usually lands a few hundred milliseconds after the
// hangup it led to (measured in #484), and nothing waits for it. When the words
// of the caller's last turn are known as the hangup passes, it is counted at
// once. Otherwise it is remembered, and counted on the first final caller
// transcript with words that is not an acknowledgement; a call that ends
// first counts it as UNKNOWN. Each such hangup counts once.
//
// "The words of the caller's last turn" are the last final transcript with
// words, unless the caller has begun speaking since (CALLER_SPEECH): their
// newest turn is then still being transcribed, and the older words would
// label the hangup with an earlier question. Speech that is never transcribed
// (a cough, an echo) leaves the words owed, so the hangup is counted on the
// next words, or as UNKNOWN.
//
// Which hangups count: one whose response had no words when the call arrived
// (Event.soFar), over no ending armed, decided or being handed over, outside a
// terminal phase, that ran (the runtime answered ok and armed the ending). A
// hangup after another tool call of the same response counts too: the guard
// leaves that shape alone for the words narrating the other tool, and there are
// none. On doubao a tool call is a turn of its own (doubao.Session.
// onToolCalls), so every hangup there is wordless and counts like any other.

// wordlessHangups is the count's state across one call, kept on the answer
// guard and so owned by drive: the caller's last words, and the hangups still
// waiting for them.
type wordlessHangups struct {
	providerName string
	log          *slog.Logger

	// callerWords is the caller's last final transcript with words that was
	// not an acknowledgement; empty before there is one.
	callerWords string
	// isCallerWordsOwed says the caller has begun speaking since callerWords
	// arrived, so the words of their last turn are not known yet.
	isCallerWordsOwed bool
	// pending are the hangups that ran while the words were owed or unknown.
	pending []wordlessHangup
}

// wordlessHangup is a hangup waiting to be counted.
type wordlessHangup struct {
	reason string
	turn   int
	node   string
}

// onCallerSpeech is the caller beginning to speak: the words of their turn are
// owed until its transcript arrives.
func (w *wordlessHangups) onCallerSpeech() { w.isCallerWordsOwed = true }

// onCallerWords is a final caller transcript that is not an acknowledgement.
// With words in it, it is the caller's last turn, and every hangup waiting for
// it is counted.
func (w *wordlessHangups) onCallerWords(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	w.callerWords, w.isCallerWordsOwed = text, false
	pending := w.pending
	w.pending = nil
	for _, hangup := range pending {
		w.record(hangup, callerTurnOf(text), true)
	}
}

// onHangupPassed is a hangup with no words before it that ran, made in turn
// while the call was in phase node: counted now when the caller's last words
// are known, else when they arrive.
func (w *wordlessHangups) onHangupPassed(reason string, turn int, node string) {
	hangup := wordlessHangup{reason: reason, turn: turn, node: node}
	if w.callerWords == "" || w.isCallerWordsOwed {
		w.pending = append(w.pending, hangup)
		w.logger().Debug("a wordless hangup waits for the caller's words to be counted",
			"reason", reason, "turn", turn, "node", node)
		return
	}
	w.record(hangup, callerTurnOf(w.callerWords), false)
}

// onCallEnded counts what is still waiting as UNKNOWN: the caller's words
// never came.
func (w *wordlessHangups) onCallEnded() {
	pending := w.pending
	w.pending = nil
	for _, hangup := range pending {
		w.record(hangup, obs.CallerTurnUnknown, false)
	}
}

// record counts one wordless hangup and logs it. isLate says the caller's words
// arrived after the hangup had run. The words themselves are not logged: they
// are the transcript's to keep.
func (w *wordlessHangups) record(hangup wordlessHangup, callerTurn string, isLate bool) {
	obs.RecordWordlessHangup(hangup.reason, callerTurn, w.providerName)
	w.logger().Info("wordless hangup counted: the response that made it had said nothing",
		"reason", hangup.reason, "callerTurn", callerTurn, "turn", hangup.turn,
		"node", hangup.node, "isCallerWordsLate", isLate)
}

func (w *wordlessHangups) logger() *slog.Logger {
	if w.log == nil {
		return slog.Default()
	}
	return w.log
}

// callerTurnOf labels the caller's last words for the count.
func callerTurnOf(text string) string {
	if endsInQuestion(text) {
		return obs.CallerTurnQuestion
	}
	return obs.CallerTurnOther
}

// isWordlessHangup reports whether a TOOL_CALL is a hangup the answer guard has
// no words to judge and that would end the call: its response had said
// nothing when the call arrived, and no ending is armed, decided or being
// handed over, in a phase that is not terminal. Asked before the call runs.
func isWordlessHangup(event Event, runtime *flow.Runtime, actions *callActions) bool {
	return event.ToolName == flow.ToolHangup && strings.TrimSpace(event.soFar.text) == "" &&
		!actions.isArmed() && !actions.isEndingDecided() && !actions.isSettlingHandover() &&
		!runtime.Engine().IsTerminal()
}
