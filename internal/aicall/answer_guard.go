// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// The answer guard (#484). On qwen flash the model answered a caller's first
// question ("What service do you offer?") and called hangup in the same
// response: the answer was muted, the closing line said, and the call ended
// fifteen seconds in. A hangup made in a response that has already said
// something of substance is therefore refused as a conversation, before the
// tool runs: the answer is not muted and keeps playing, nothing is armed, and
// the model is told to ask whether anything else is needed and to end the call
// only once the caller says goodbye or declines (flow.Runtime.
// RefuseWhileAnswering).
//
// The guard judges the bot's own words and never the caller's: what the
// caller meant is not something to work out from a transcript. Its evidence is
// the response's transcript as it stood when the call arrived on the wire
// (Event.soFar, read at emit like the cut of #403). Its blind spot is a
// response whose call arrives before any of its words: there is nothing to
// judge and the hangup passes. A hangup that passes with audio of its own
// queued and no words yet is logged, because that order is what the live round
// needs to count.
//
// It does not judge:
//
//   - a call over an ending already armed, decided by the time limit, or
//     being handed over: the closing paths answer those (#465);
//   - a call in a terminal phase: the call is ending by the flow's word;
//   - a hangup after another tool call of the same response, the #262 shape
//     (record_lead and hangup in one response): its words narrate the other
//     tool, not an answer;
//   - a hangup the phase does not allow: the runtime refuses that anyway.
//
// It refuses once per caller turn: a model that calls hangup again after the
// refusal, before the caller has said anything with words, gets its hangup
// (logged), so a false refusal costs one exchange and never a loop.

// answerGuardMinHalfWords is how much a response must have said, in half
// words (answerHalfWords), for a hangup in it to be refused: four words, or
// eight characters of Chinese. Three words is the longest acknowledgement a
// model says on its way out without answering anything ("Okay, thank you.",
// "You're welcome.", "好的，没问题。"), and four is where a clause that tells the
// caller something begins ("NovaNet is a telecommunications…", "我们提供宽带和手机…").
// A goodbye of any length passes (isSubstantiveAnswer). A false refusal costs
// the caller one more exchange, once per caller turn; a false pass is #484.
const answerGuardMinHalfWords = 8

// answerGuard is the guard's state across one call: whether it has refused
// since the caller last spoke with words.
type answerGuard struct {
	isSpent bool
}

// onCallerWords is a final caller transcript with words: the guard may refuse
// again.
func (g *answerGuard) onCallerWords(text string) {
	if strings.TrimSpace(text) != "" {
		g.isSpent = false
	}
}

// refusesHangup decides whether the hangup this TOOL_CALL asks for is refused
// as made while answering the caller, and logs the decisions that matter: a
// refusal, a pass because the guard was spent, and a pass on an empty snapshot
// over audio the response had already queued.
func (o *Orchestrator) refusesHangup(event Event, guard *answerGuard, runtime *flow.Runtime,
	actions *callActions, log *slog.Logger) bool {

	if event.ToolName != flow.ToolHangup || !runtime.IsToolAllowed(flow.ToolHangup) ||
		actions.isArmed() || actions.isEndingDecided() || actions.isSettlingHandover() ||
		runtime.Engine().IsTerminal() || event.soFar.priorCalls > 0 {
		return false
	}
	soFar := event.soFar
	text := strings.TrimSpace(soFar.text)
	fields := []any{"turn", event.Turn, "textRunes", utf8.RuneCountInString(text),
		"turnAudioMs", soFar.audio.Milliseconds(), "queuedMs", soFar.queued.Milliseconds()}
	if text == "" {
		if soFar.audio > 0 {
			log.Info("a hangup arrived before any words of its response, with its audio already queued; "+
				"the answer guard has nothing to judge and lets it through", fields...)
		}
		return false
	}
	if !isSubstantiveAnswer(text) {
		return false
	}
	if guard.isSpent {
		log.Info("a hangup in a response that was answering the caller, after the answer guard "+
			"had already refused once since the caller last spoke; letting it through", fields...)
		return false
	}
	guard.isSpent = true
	log.Info("hangup refused: the response that made it was answering the caller; "+
		"the answer plays on and the call goes on", fields...)
	return true
}

// isSubstantiveAnswer reports whether a response's words so far are an answer
// a hangup must not ride on: no goodbye (a turn that ends by asking is no
// goodbye, as claims.go has it), and at least answerGuardMinHalfWords of them.
func isSubstantiveAnswer(text string) bool {
	if text == "" {
		return false
	}
	if hasFarewell(detectClaims(text)) && !endsInQuestion(text) {
		return false
	}
	return answerHalfWords(text) >= answerGuardMinHalfWords
}

// answerHalfWords is how much a text says, in half words: two for each word of
// a script written with spaces (a run of letters or digits, an apostrophe
// inside it kept: "you're" is one word), one for each character of a script
// written without them (Han, kana, Hangul), whose characters carry about half
// a word each.
func answerHalfWords(text string) int {
	n := 0
	inWord := false
	for _, r := range text {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			n++
			inWord = false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if !inWord {
				n += 2
				inWord = true
			}
		case inWord && (r == '\'' || r == '’'):
			// Inside the word.
		default:
			inWord = false
		}
	}
	return n
}
