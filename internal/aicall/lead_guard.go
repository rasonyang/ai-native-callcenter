// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"log/slog"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// The lead guard (#612). On a campaign deployment most invalid leads were plain
// refusals recorded as HIGH, and the transcript carried the refusal: the callee
// said "不要不要。" after the opening and the agreement tool moved the call a
// second later; or words 3 s into the opening ("好吧。") moved it before the
// offer had been heard. So a tool call that would record that the customer is
// interested (flow.Runtime.ClaimsInterest: a move into a phase that declares a
// HIGH or MEDIUM lead, or record_lead with one) is judged before it runs, on
// every engine, by two rules:
//
//   - A, the timing. If the caller words it answers began before the caller
//     had heard the opening to its final sentence (the opening line, its
//     replay, or while a cut opening is owed; caller_turns.go), it is refused
//     as a conversation, whatever the words were: a callee cannot agree to an
//     offer they have not heard. That holds in whichever response the call
//     comes, the reply to the cut, the replay's own or a later one with no new
//     words: the cut refusals see only the first, and a cut judged as words
//     not even that. The opening is said again on #610's budget,
//     once per call (Session.SayOpeningAgain); once that is spent, the tool is
//     refused and nothing is said again.
//   - B, the caller's words. The final transcript of the utterance the tool
//     answered is waited for, at most leadFinalWait and not at all when it is
//     in (qwen delivers it 0.2 to 0.5 s after the call). A final made only of
//     refusal phrases (isRefusalOnly) refuses the tool, every time. No final
//     in time, a blank one, or no caller words at all refuse it once per
//     caller turn, and the model is asked to have the callee confirm; a
//     guarded call of a later response on the same caller turn runs, so a
//     transcriber that dropped a real yes costs one exchange. Within one
//     response the first verdict stands for its later guarded calls.
//
// B judges the caller's words, which the answer guard of #484 and #553 never
// does (answer_guard.go): there a hangup is judged on the bot's own words,
// because what the caller meant is not something to work out from a
// transcript. For these tools only the owner decided otherwise (2026-11-19):
// the transcript of a lead's calls carried the refusal, the cost of a wrong
// HIGH is a callback to someone who said no, and the judge is conservative (a
// whole utterance of refusal phrases, or no words at all). The hangup guard
// keeps its rule.
//
// Over an ending already under way (armed, decided by the time limit, a
// handover settling) or in a terminal phase there is no turn left to confirm
// in, so words not caught do not refuse there; a refusal and A still do.
//
// The guard runs after the cut refusals (#403, #594, #610): a moving tool they
// refuse is theirs, and logged there. Nothing runs before the verdict: the
// lead is written by the tool (record_lead) or on entering the phase it moves
// into (Runtime.recordDeclaredLead), so a refused call writes none, and an
// answer held for the end of the response (isAnswerHeld) is only ever the
// answer of a call that ran. The wait blocks this call's loop, as the cut's
// does; the session goes on playing and transcribing meanwhile, and the answer
// that follows asks for the one turn it always did.
//
// One line per judged call (the verdict, the guard, when the final came
// against the call, the wait), never the words; one count per call
// (aicc_bot_lead_guard_total{reason, provider}).

// leadFinalWait is how long a guarded tool waits for the final transcript of
// the caller utterance it answered. Owner decision, 2026-11-19.
const leadFinalWait = 1000 * time.Millisecond

// interestVerdict is what the lead guard concluded about one guarded call.
type interestVerdict int

const (
	// interestWords: the caller's words were not a refusal; the tool runs.
	interestWords interestVerdict = iota
	// interestOpeningUnheard: guard A; refused.
	interestOpeningUnheard
	// interestDeclined: the final was only refusal phrases; refused.
	interestDeclined
	// interestNoFinal, interestBlankFinal, interestNoCallerTurn: nothing the
	// caller said is known; refused once per caller turn.
	interestNoFinal
	interestBlankFinal
	interestNoCallerTurn
)

// reason is the verdict as the log and the count name it.
func (v interestVerdict) reason() string {
	switch v {
	case interestOpeningUnheard:
		return obs.LeadGuardOpeningUnheard
	case interestDeclined:
		return obs.LeadGuardDeclined
	case interestNoFinal:
		return obs.LeadGuardNoFinal
	case interestBlankFinal:
		return obs.LeadGuardBlankFinal
	case interestNoCallerTurn:
		return obs.LeadGuardNoCallerTurn
	}
	return obs.LeadGuardWords
}

// isUnconfirmed says the verdict is that nothing the caller said is known.
func (v interestVerdict) isUnconfirmed() bool {
	return v == interestNoFinal || v == interestBlankFinal || v == interestNoCallerTurn
}

// interestGuard is the lead guard's state across one call, owned by drive.
type interestGuard struct {
	// unconfirmedSeq is the caller turn (callerTurn.seq, 0 for none) whose
	// unknown words have refused a call, in response unconfirmedTurn: a call
	// of a later response on that caller turn runs.
	unconfirmedSeq, unconfirmedTurn int
	hasUnconfirmed                  bool
	// last is the latest decision and the response and caller turn it was
	// made for: a later guarded call of the same response, on the same caller
	// turn, gets the same one.
	last struct {
		turn, seq          int
		verdict            interestVerdict
		isRefused, isAgain bool
		isSet              bool
	}
}

// refusesInterest judges a guarded tool call (flow.Runtime.ClaimsInterest)
// and reports whether it is refused, with the refusal in output. replay is
// the cut to say the opening again on once the refusal is answered, nil for
// none (guard A, while the opening can still be said again). It may wait for
// the final transcript (Session.awaitCallerFinal).
func (o *Orchestrator) refusesInterest(event Event, g *interestGuard, session *Session,
	runtime *flow.Runtime, actions *callActions, log *slog.Logger) (output string, replay *openCut, isRefused bool) {

	answered := event.answered
	seq := 0
	if answered != nil {
		seq = answered.seq
	}
	var verdict interestVerdict
	isSameResponse := g.last.isSet && g.last.turn == event.Turn && g.last.seq == seq
	isSaidAgain := false
	var waited time.Duration
	switch {
	case isSameResponse:
		verdict, isRefused, isSaidAgain = g.last.verdict, g.last.isRefused, g.last.isAgain
	case answered == nil:
		verdict = interestNoCallerTurn
	default:
		if _, _, _, isUnheard := session.callerFinal(answered); isUnheard {
			verdict = interestOpeningUnheard
			break
		}
		if event.cut != nil {
			session.spendFinalWaitAfterCut(answered, event.cut)
		}
		waited = session.awaitCallerFinal(answered)
		text, isFinal, _, _ := session.callerFinal(answered)
		switch {
		case !isFinal:
			verdict = interestNoFinal
		case strings.TrimSpace(text) == "":
			verdict = interestBlankFinal
		case isRefusalOnly(session.cfg.Session.Language, text):
			verdict = interestDeclined
		}
	}

	reason := verdict.reason()
	isEndingNear := actions.IsEndingUnderway() || runtime.Engine().IsTerminal()
	if !isSameResponse {
		switch {
		case verdict == interestOpeningUnheard:
			isRefused = true
			isSaidAgain = o.canSayOpeningAgain(session, runtime, actions)
		case verdict == interestDeclined:
			isRefused = true
		case verdict.isUnconfirmed():
			isSpent := g.hasUnconfirmed && g.unconfirmedSeq == seq && g.unconfirmedTurn != event.Turn
			isRefused = !isSpent && !isEndingNear
			if isRefused {
				g.unconfirmedSeq, g.unconfirmedTurn, g.hasUnconfirmed = seq, event.Turn, true
			}
		}
		g.last.turn, g.last.seq, g.last.verdict = event.Turn, seq, verdict
		g.last.isRefused, g.last.isAgain, g.last.isSet = isRefused, isSaidAgain, true
	}
	if verdict.isUnconfirmed() && !isRefused {
		reason = obs.LeadGuardUnconfirmed
	}

	guardName, final := "B", "NONE"
	if verdict == interestOpeningUnheard {
		guardName = "A"
	}
	if answered != nil {
		if _, isFinal, finalAt, _ := session.callerFinal(answered); isFinal {
			final = "BEFORE_CALL"
			if finalAt.After(event.calledAt) {
				final = "AFTER_CALL"
			}
		}
	}
	log.Info("a tool that would record the customer's interest was judged",
		"tool", event.ToolName, "turn", event.Turn, "callerTurn", seq, "guard", guardName,
		"verdict", verdict.reason(), "isRefused", isRefused, "final", final,
		"waitedMs", waited.Milliseconds(), "isOpeningSaidAgain", isSaidAgain,
		"isSameResponse", isSameResponse, "isEndingUnderway", isEndingNear)
	obs.RecordLeadGuard(reason, o.cfg.Profile.Name)
	if !isRefused {
		return "", nil, false
	}

	switch verdict {
	case interestOpeningUnheard:
		output = runtime.RefuseAsUnheardOffer(event.ToolName, isSaidAgain)
		if isSaidAgain && !isSameResponse {
			// The reply that made the call is muted when the opening is said
			// again, as the model's reply to a greeting is: on the cut it
			// answers when there is one, else on its own turn.
			replay = event.cut
			if replay == nil {
				replay = &openCut{isOpening: true, answerTurn: event.Turn, done: make(chan struct{})}
			}
		}
	case interestDeclined:
		output = runtime.RefuseAsDeclined(event.ToolName)
	default:
		output = runtime.RefuseAsUnconfirmed(event.ToolName)
	}
	return output, replay, true
}

// refusalMaxPhrases is how many phrases an utterance may be made of and still
// be judged a refusal (isRefusalOnly): "不需要，谢谢，不需要" is three.
const refusalMaxPhrases = 8

// refusals is the built-in list of refusal phrases, fixed per language and in
// compact form (compactAck: letters and digits lower-cased, Han characters,
// nothing else), shared by every flow like the acknowledgements. Each says no
// to an offer on its own. The list is conservative: a phrase that also means
// something else in a common sentence ("不要紧", "不用谢", "no problem") must not
// be one, and the classifier needs the whole utterance to be refusals.
var refusals = map[string][]string{
	"zh": {"不要", "不要了", "不需要", "不需要了", "不用", "不用了", "不必", "不必了", "不了",
		"算了", "算了吧", "不想要", "不想", "不想了解", "不需要了解", "不想听", "不考虑", "不考虑了",
		"不感兴趣", "没兴趣", "没有兴趣", "不办", "不办了", "不买", "不行",
		"暂时不需要", "暂时不用", "暂时不要", "先不用", "先不用了", "先不要", "先不要了",
		"真不需要", "真的不需要", "真不用", "真的不用",
		"我不要", "我不需要", "我不用", "我不想要", "我不感兴趣", "我没兴趣",
		"别打了", "别再打了", "不要再打了", "不要打了", "别打电话了", "不要打电话了"},
	"en": {"no", "nope", "nah", "nothanks", "nothankyou", "notinterested", "imnotinterested",
		"iamnotinterested", "notreally", "notnow", "notrightnow", "notforme",
		"idontneedit", "idontwantit", "idontneedthat", "idontwantthat", "idontneedanything",
		"idontwantanything", "dontcallme", "dontcallmeagain", "stopcalling", "stopcallingme"},
}

// refusalCompanions are the words a refusal may come with and still be one:
// thanks, and fillers. Never alone: "谢谢" by itself is no refusal.
var refusalCompanions = map[string][]string{
	"zh": {"谢谢", "谢谢你", "谢谢您", "谢谢啊", "谢了", "多谢", "嗯", "哦", "噢", "啊", "呃", "额", "哎", "唉"},
	"en": {"thanks", "thankyou", "thanksanyway", "thankyouanyway", "sorry", "imsorry", "um", "uh", "er",
		"erm", "well"},
}

// isRefusalOnly reports whether a caller's final transcript is a refusal and
// nothing else: its compact form, punctuation and spaces dropped, is made of
// refusal phrases of the call language's list, at least one, with at most
// thanks and fillers beside them, and no more than refusalMaxPhrases in all
// ("不要不要", "不需要，谢谢", "不用不用了", "No thanks.", "I'm not interested").
// Any split counts, as for isAckRun, because the transcriber's punctuation is
// unreliable. A question mark anywhere makes it no refusal ("不要钱吗？"), and
// so does any other word ("要不要钱", "有没有合约", "没问题", "不客气", "好的").
// Only the call language's list counts. Pure; it runs off the frame path.
func isRefusalOnly(language, text string) bool {
	if strings.ContainsAny(text, "?？") {
		return false
	}
	lang := flow.Lang(language)
	phrases := refusals[lang]
	compact := compactAck(text)
	if phrases == nil || compact == "" {
		return false
	}
	return isRefusalRun(phrases, refusalCompanions[lang], compact, refusalMaxPhrases, false)
}

// isRefusalRun reports whether s is one to max phrases of refusals and
// companions, in any order, with at least one refusal among them (or already
// seen, hasRefusal). Every split is tried.
func isRefusalRun(refusals, companions []string, s string, max int, hasRefusal bool) bool {
	if s == "" {
		return hasRefusal
	}
	if max == 0 {
		return false
	}
	for _, phrase := range refusals {
		if strings.HasPrefix(s, phrase) && isRefusalRun(refusals, companions, s[len(phrase):], max-1, true) {
			return true
		}
	}
	for _, word := range companions {
		if strings.HasPrefix(s, word) && isRefusalRun(refusals, companions, s[len(word):], max-1, hasRefusal) {
			return true
		}
	}
	return false
}
