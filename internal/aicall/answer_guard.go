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
// the model is told not to repeat its answer and to end the call only once the
// caller says goodbye or declines (flow.Runtime.RefuseWhileAnswering).
//
// How the refusal reaches the model decides what else it says (#555). On a
// model that can answer a tool call without a turn (provider.
// QuietToolAnswerer) the refusal is held to the end of the response and then
// sent quietly, whatever the answer ended in: a refusal that asked for a turn
// after an answer ending in a statement had qwen flash say the whole answer
// again (qwen-findings W-Q16, W-Q17). The quiet refusal tells the model to
// answer what the caller says next. When it cannot go quietly (a model without
// quiet answers, a response that queued no audio, or a later call of the same
// response that asks for a turn, owes a line or arms an ending) it asks for a
// turn, and tells the model to ask whether anything else is needed.
//
// An answer that ended in a question has asked the caller something, and a
// silence after it is any other silence. One that ended in a statement has
// not, and the silent-caller cue's "repeat the current question" made flash
// repeat the answer (W-Q15, call 5). So the first dead air after such a quiet
// refusal, if nothing happened in between, gets the anything-else cue
// (anythingElseCue) in its place: the caller has heard the answer, it is not
// to be repeated, and the model asks only whether they need anything else.
// What happens in between that drops it is in answerGuard.
//
// The guard judges the bot's own words and never the caller's: what the
// caller meant is not something to work out from a transcript. Its evidence is
// the response's transcript as it stood when the call arrived on the wire
// (Event.soFar, read at emit like the cut of #403). Its blind spot is a
// response whose call arrives before any of its words: there is nothing to
// judge and the hangup passes, logged whether or not the response had queued
// audio. Two things stand there since #553. The runtime refuses the reason
// FLOW_INSTRUCTED in a phase that does not declare isFlowHangupAllowed, so the
// one reason that needs no act of the caller cannot end a call the flow did not
// order ended (flow.Runtime.refuseFlowInstructed). A wordless hangup that runs
// anyway, with a caller's reason the caller may never have given, is counted
// by what the caller last said (wordless_hangup.go); the count changes nothing
// about the call.
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

// answerGuard is the guard's state across one call, owned by drive: whether it
// has refused since the caller last spoke with words, the dead air a quiet
// refusal after a statement left owed the anything-else cue, and the count of
// the hangups it had no words to judge.
type answerGuard struct {
	isSpent bool
	// wordless counts the hangups whose response had said nothing (#553,
	// wordless_hangup.go). drive gives it the provider's name and the call's
	// log.
	wordless wordlessHangups
	// anythingElse is the turn whose answer the owed cue follows, and the
	// phase it was given in; turn zero is none. It is for the next dead air
	// only, and is dropped before it by anything that makes the answer not the
	// last thing the caller heard, or the caller no longer silent since:
	//
	//   - caller words (onCallerWords): the caller is not silent, and the reply
	//     to them is what a later dead air follows;
	//   - a keypress (onCallerActed): it ends a run of silence as words do
	//     (flow.Engine.OnCallerKeyed);
	//   - a barge-in (onCallerActed): the caller did not hear the answer to its
	//     end, so the cue's "they have heard your answer" would be false,
	//     whether the cut was speech or echo;
	//   - any other turn finishing (onTurnDone): a claim cue's reply, or the
	//     provider's own reply to speech without words, is then the last thing
	//     said.
	//
	// A move or an ending is checked where the cue would be sent
	// (takeAnythingElse): the phase must be the one the answer was given in, and
	// nothing armed or decided. Bare speech without a transcript does not drop
	// it: it cannot be told from echo or line noise, and the dead air after it
	// is a silence (handleDeadAir).
	anythingElse struct {
		turn int
		node string
	}
}

// onCallerWords is a final caller transcript with words: the guard may refuse
// again, the caller is not silent, and a wordless hangup waiting for the
// caller's words is counted.
func (g *answerGuard) onCallerWords(text string) {
	if strings.TrimSpace(text) != "" {
		g.isSpent = false
		g.forgetAnythingElse()
		g.wordless.onCallerWords(text)
	}
}

// onCallerSpeech is the caller beginning to speak (CALLER_SPEECH): the words of
// their newest turn are owed until its transcript arrives.
func (g *answerGuard) onCallerSpeech() { g.wordless.onCallerSpeech() }

// onCallerActed is a keypress, or a barge-in over the bot.
func (g *answerGuard) onCallerActed() { g.forgetAnythingElse() }

// onTurnDone is a turn the model finished. Any turn but the answer the cue is
// owed after makes that answer no longer the last thing said.
func (g *answerGuard) onTurnDone(turn int) {
	if g.anythingElse.turn != turn {
		g.forgetAnythingElse()
	}
}

// onRefusalAnsweredQuietly records that closeToolBatch sent the guard's
// refusal quietly for turn, in phase node, with words the response's own. An
// answer that did not end in a question, and that the caller was not cut out
// of (isCut), leaves the next dead air owed the anything-else cue.
func (g *answerGuard) onRefusalAnsweredQuietly(turn int, node, words string, isCut bool,
	log *slog.Logger) {

	g.forgetAnythingElse()
	if isCut || endsInQuestion(words) {
		return
	}
	g.anythingElse.turn, g.anythingElse.node = turn, node
	log.Info("the refused hangup's answer ended in a statement; a dead air before the caller "+
		"speaks gets the anything-else cue", "turn", turn, "node", node)
}

// takeAnythingElse reports whether this dead air, in phase node, gets the
// anything-else cue, and forgets it either way: it is owed to one dead air.
// Not over a move since the answer, nor over an ending armed or decided. A nil
// guard owes nothing.
func (g *answerGuard) takeAnythingElse(node string, actions *callActions) bool {
	if g == nil {
		return false
	}
	isOwed := g.anythingElse.turn != 0 && g.anythingElse.node == node &&
		!actions.isArmed() && !actions.isEndingDecided() && !actions.isSettlingHandover()
	g.forgetAnythingElse()
	return isOwed
}

func (g *answerGuard) forgetAnythingElse() { g.anythingElse.turn, g.anythingElse.node = 0, "" }

// anythingElseCue is what a caller who went silent after the answer of a
// refused hangup is asked, in place of the silent-caller cue: the answer was
// not a question, so there is none to repeat, and repeating the answer is the
// defect (#555).
func anythingElseCue(lang string) string {
	if lang == flow.LangZH {
		return "（来电者已经听到了你的回答，之后一直没有说话。不要重复回答里的任何内容。" +
			"只用一句简短的话问对方还有没有其他需要。）"
	}
	return "(The caller has heard your answer and has been silent since. Do not repeat any of it. " +
		"Ask only, in one short sentence, whether they need anything else.)"
}

// refusesHangup decides whether the hangup this TOOL_CALL asks for is refused
// as made while answering the caller, and logs the decisions that matter: a
// refusal, a pass because the guard was spent, and a pass on an empty snapshot,
// with or without audio the response had already queued (the guard's blind
// spot; the runtime still judges the reason, FLOW_INSTRUCTED included).
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
		log.Info("a hangup arrived before any words of its response; "+
			"the answer guard has nothing to judge and lets it through", fields...)
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
