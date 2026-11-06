// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"regexp"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// An unbacked claim is the bot telling the caller it is doing something, or
// has done it, in a turn where nothing was done (#44). The flow cannot see
// one: its only events are tool results and silence, so "I'm transferring you"
// with no transfer_to_agent behind it moves no phase, arms no transfer and
// leaves the caller waiting for a person who is not coming. Live calls on qwen
// showed it for transfers ("I've already transferred you", ten turns running
// with the queue open), lookups ("我帮您查询一下", eight times with no lookup) and
// goodbyes with nothing ending the call.
//
// Detection reads the bot's own transcript, so it is a signal and not proof.
// The patterns are the claim's own phrasing — a first person doing the thing,
// or the thing reported done — and leave out the shapes that only offer it: a
// question, a condition, a "can" or "would you like". A claim missed here is
// one the conversation already hid; a claim invented here would make the
// count one nobody trusts, so the patterns err narrow.
//
// A claim is first measured (#44) and, for the kinds a tool can make true, then
// acted on once (#438): LOOKUP, TRANSFER and MESSAGE. The caller who has heard
// the promise is re-asked once, at the end of the turn's playback, with a cue
// that names the tool; a caller who goes quiet instead gets the same cue in
// place of the dead-air prompt, which would otherwise ask the model to repeat
// the promise as "the current question". FAREWELL has its own path (#147).

// claim is one kind of action a bot line claims, and the words that claimed it.
type claim struct {
	kind   string
	phrase string
}

// claimPattern recognises one kind of claim.
type claimPattern struct {
	kind string
	re   *regexp.Regexp
}

// claimPatterns are checked against each sentence of a bot line. The English
// ones see the sentence lowercased with typographic apostrophes straightened.
var claimPatterns = []claimPattern{
	// Transfer: "I'm transferring you", "I've already transferred you", "I've
	// initiated the transfer", "the transfer … is being processed", "let me
	// put you through", "我帮您转接人工", "正在为您转接", "已为您接通".
	{store.UnbackedClaimTransfer, regexp.MustCompile(
		`\bi(?:'m| am) (?:now |just )?(?:transferring|connecting|putting) you\b` +
			`|\bi(?:'ve| have) (?:(?:now|just|already) )*(?:transferred|connected|put) you\b` +
			`|\bi(?:'ve| have) (?:(?:now|just|already) )*(?:initiated|started|made|requested|placed) (?:the|a|your) transfer\b` +
			`|\b(?:i(?:'ll| will|'m going to| am going to)|let me)(?: now| just)? (?:transfer|connect|put) you\b` +
			`|\bthe transfer\b.{0,40}\b(?:is being processed|is in progress|is underway|has been (?:initiated|started|made|completed|processed))` +
			`|\byou(?:'re| are) (?:now )?being (?:transferred|connected|put through)\b` +
			`|\byou(?:'ll| will) (?:now )?be (?:transferred|connected|put through)\b` +
			`|(?:我|这就|马上|现在|正在|立即|立刻|已经?)(?:来|就|会)?(?:为|给|帮)您(?:转接|转到|转给|接通|转人工)` +
			`|(?:正在|马上|立即|立刻|已经?)(?:转接|接通)` +
			`|(?:为|给)您转接中`)},
	// Lookup: "let me check", "I'm looking that up", "我帮您查询一下",
	// "正在为您核实".
	{store.UnbackedClaimLookup, regexp.MustCompile(
		`\bi(?:'m| am) (?:now |just )?(?:checking|looking (?:that |this |it )?up|looking into|pulling up|searching)\b` +
			`|\b(?:let me|i(?:'ll| will))(?: now| just| quickly)? (?:check|look (?:that |this |it )?up|look into|pull up|search)\b` +
			`|(?:我|这就|马上|现在|正在|立即)(?:来|就|会)?(?:为|给|帮)您(?:查询|查一下|查查|查找|核实|核查|查看)` +
			`|(?:正在|马上)(?:为您)?(?:查询|查找|核实|核查)` +
			`|我(?:来|先)?查(?:询)?一下`)},
	// Message: only the completed form. "I'll take your message" usually comes
	// before the message has been said, and the tool call rightly after it.
	{store.UnbackedClaimMessage, regexp.MustCompile(
		`\bi(?:'ve| have) (?:(?:now|just|already) )*(?:saved|recorded|noted down|logged|taken|passed on|submitted|forwarded) (?:your|the) (?:message|callback|request|details|number)\b` +
			`|\byour (?:message|callback request|request) (?:has been|is) (?:saved|recorded|logged|passed on|submitted|forwarded)\b` +
			`|已(?:经)?(?:为|帮|给)?您(?:记录|登记|提交|转达|留言)` +
			`|(?:留言|信息|请求|回电请求)已(?:经)?(?:记录|登记|提交|转达)`)},
	// Farewell: a goodbye is a claim that the call is ending.
	// Besides the word itself, the closing phrases the seeded flows use ("Thanks
	// for your time", "祝您生活愉快"). A plain "thank you" is not one: it is said
	// mid-call all the time.
	{store.UnbackedClaimFarewell, regexp.MustCompile(
		`(?i)\b(?:good-?bye|bye-bye|bye now)\b|` +
			`\bthank(?:s| you) for your (?:time|cooperation)\b|` +
			`\bhave a (?:great|good|nice|wonderful|lovely) (?:day|evening|night)\b|` +
			`\btake care(?:[.!,]|\s*$)|` +
			`再见|拜拜|祝您[^，。！？,.!?]{0,8}愉快|感谢您的(?:配合|时间)|感谢配合`)},
}

// notAClaim marks a sentence that offers or supposes rather than claims.
var notAClaim = regexp.MustCompile(`\bif\b|\bwhether\b|如果|的话|是否|要不要|吗`)

// detectClaims returns the actions a bot line claims, each kind once.
func detectClaims(text string) []claim {
	var claims []claim
	seen := map[string]bool{}
	for _, sentence := range sentences(text) {
		if strings.HasSuffix(sentence, "?") || strings.HasSuffix(sentence, "？") {
			continue
		}
		normalized := strings.ToLower(strings.ReplaceAll(sentence, "’", "'"))
		if notAClaim.MatchString(normalized) {
			continue
		}
		for _, pattern := range claimPatterns {
			if seen[pattern.kind] {
				continue
			}
			if phrase := pattern.re.FindString(normalized); phrase != "" {
				seen[pattern.kind] = true
				claims = append(claims, claim{kind: pattern.kind, phrase: phrase})
			}
		}
	}
	return claims
}

// sentences splits a line after each terminator, keeping it, so a question
// can be told from a statement.
func sentences(text string) []string {
	var out []string
	start := 0
	for i, r := range text {
		switch r {
		case '.', '!', '?', '。', '！', '？', '\n':
			end := i + len(string(r))
			if s := strings.TrimSpace(text[start:end]); s != "" {
				out = append(out, s)
			}
			start = end
		}
	}
	if s := strings.TrimSpace(text[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// claimWatch follows the bot's lines turn by turn, on the call's own
// goroutine, and reports the claims no tool call backs.
//
// A turn is judged when it is done, not when its words arrive: a response can
// speak first and call the tool after, and the transcript of the words lands
// before the call does. A claim is backed when its turn made a tool call, when
// the turn is the reply to one (the bot saying "I'm transferring you now"
// after transfer_to_agent returned is the truth), or when an ending is already
// armed (a goodbye the platform asked for, or a line before a transfer the
// flow decided).
type claimWatch struct {
	// pending is the text of turns not yet done.
	pending map[int]string
	// lastDoneTurn and isLastDoneBacked judge text that arrives after its own
	// turn finished; 0 is none, turns are numbered from 1.
	lastDoneTurn     int
	isLastDoneBacked bool
	// isToolReplyNext says the next turn to finish answers a tool result.
	isToolReplyNext bool
	// isFarewellUnhung says the latest finished turn said goodbye in words
	// with nothing behind it: no tool call, no armed ending (#147). The next
	// turn of any kind, or a tool call, takes it back.
	isFarewellUnhung bool

	// standing is the first actionable claim (claimIsActionable) of the latest
	// finished turn that nothing backed, nil when there is none. A tool call,
	// the next turn done, an interrupted turn, or a cue sent for it takes it
	// back (#438).
	standing *claim
	// standingTurn is the turn standing was said in.
	standingTurn int
	// reAskTurn is the turn whose playback is owed a re-ask: standing's turn,
	// unless that turn answered a claim cue (a claim cue is sent once, so the
	// reply to one is never re-asked at its playback). 0 is none.
	reAskTurn int
	// playedTurn is the latest turn whose playback finished, so a claim read
	// from a transcript that arrived after the caller heard the turn is not
	// re-asked (the dead-air path still covers it).
	playedTurn int
	// cue is the claim cue sent whose answer has not finished yet; its next
	// finished turn is that answer.
	cue *claimCueSent
	// lastDoneAnswered says the latest finished turn was the answer to a claim
	// cue, for a transcript that arrives late.
	lastDoneAnswered bool
	// isLastDoneInterrupted says the caller talked over the latest finished
	// turn: nothing in it was heard to the end.
	isLastDoneInterrupted bool
	// outcome is the answer to a claim cue that has just finished, until
	// takeCueOutcome reads it.
	outcome *claimCueOutcome
}

// claimCueSent is a claim cue waiting for the turn that answers it.
type claimCueSent struct {
	kind    string
	trigger string
}

// claimCueOutcome is how a claim cue fared: whether the turn that answered it
// made a tool call.
type claimCueOutcome struct {
	kind     string
	trigger  string
	isBacked bool
}

// claimIsActionable says a tool call can make the claim true. A goodbye is
// not: it has its own path (#147).
func claimIsActionable(kind string) bool {
	switch kind {
	case store.UnbackedClaimLookup, store.UnbackedClaimTransfer, store.UnbackedClaimMessage:
		return true
	}
	return false
}

// firstActionable is the first claim of found, in detection order, a tool can
// make true.
func firstActionable(found []claim) *claim {
	for i := range found {
		if claimIsActionable(found[i].kind) {
			c := found[i]
			return &c
		}
	}
	return nil
}

// stand makes found's first actionable claim the one standing for turn.
func (w *claimWatch) stand(turn int, found []claim) {
	w.standing, w.standingTurn, w.reAskTurn = nil, 0, 0
	c := firstActionable(found)
	if c == nil {
		return
	}
	w.standing, w.standingTurn = c, turn
	if !w.lastDoneAnswered && !w.isLastDoneInterrupted && turn > w.playedTurn {
		w.reAskTurn = turn
	}
}

// standingClaim is the claim a silent caller is waiting on, nil when none.
func (w *claimWatch) standingClaim() *claim {
	if w == nil {
		return nil
	}
	return w.standing
}

// consumeStanding takes the standing claim back once a cue has been sent for it.
func (w *claimWatch) consumeStanding() { w.standing, w.standingTurn, w.reAskTurn = nil, 0, 0 }

// onPlaybackDone is the caller having heard a turn to its end. It returns the
// claim to re-ask when that turn is the one owed a re-ask. The owed re-ask is
// always taken, so a gate that skips it does not leave it for a later playback.
func (w *claimWatch) onPlaybackDone(turn int) *claim {
	w.playedTurn = max(w.playedTurn, turn)
	if w.reAskTurn == 0 || w.reAskTurn != turn || w.standing == nil {
		return nil
	}
	c := w.standing
	w.reAskTurn = 0
	return c
}

// cueSent records that a claim cue went out: the next finished turn answers it.
func (w *claimWatch) cueSent(kind, trigger string) {
	w.cue = &claimCueSent{kind: kind, trigger: trigger}
	w.consumeStanding()
}

// takeCueOutcome returns the outcome of the claim cue whose answer just
// finished, once.
func (w *claimWatch) takeCueOutcome() (claimCueOutcome, bool) {
	if w.outcome == nil {
		return claimCueOutcome{}, false
	}
	out := *w.outcome
	w.outcome = nil
	return out, true
}

// hasFarewellWithoutHangup reports whether the bot's last finished turn said
// goodbye and did nothing to end the call. A caller who then goes quiet is
// not waiting for a question; the conversation is over and nobody ended it.
func (w *claimWatch) hasFarewellWithoutHangup() bool { return w.isFarewellUnhung }

// onToolCall takes the farewell back: the model is acting after all.
func (w *claimWatch) onToolCall() {
	w.isFarewellUnhung = false
	w.consumeStanding()
}

// onTurnInterrupted takes the farewell back: a turn the caller talked over was
// not heard to the end, so nothing was said that the call could be left on.
func (w *claimWatch) onTurnInterrupted() {
	w.isFarewellUnhung = false
	w.isLastDoneInterrupted = true
	w.consumeStanding()
}

// consumeFarewell takes the farewell back once it has been acted on.
func (w *claimWatch) consumeFarewell() { w.isFarewellUnhung = false }

// hasFarewell reports whether any of the claims is a goodbye.
func hasFarewell(found []claim) bool {
	for _, c := range found {
		if c.kind == store.UnbackedClaimFarewell {
			return true
		}
	}
	return false
}

// onBotSaid takes a final line of the bot's. A line for the turn that just
// finished is judged at once; one for an older turn is let go, since what
// backed it can no longer be known.
func (w *claimWatch) onBotSaid(turn int, text string) []claim {
	if text == "" {
		return nil
	}
	switch {
	case w.lastDoneTurn != 0 && turn == w.lastDoneTurn:
		if w.isLastDoneBacked {
			return nil
		}
		found := detectClaims(text)
		w.isFarewellUnhung = w.isFarewellUnhung || hasFarewell(found)
		if w.standing == nil {
			w.stand(turn, found)
		}
		return found
	case turn < w.lastDoneTurn:
		return nil
	}
	if w.pending == nil {
		w.pending = map[int]string{}
	}
	if prior := w.pending[turn]; prior != "" {
		text = prior + " " + text
	}
	w.pending[turn] = text
	return nil
}

// onTurnDone judges a finished turn's lines and returns its unbacked claims.
func (w *claimWatch) onTurnDone(turn int, isToolCall, isArmed bool) []claim {
	isBacked := isToolCall || w.isToolReplyNext || isArmed
	w.isToolReplyNext = isToolCall
	w.lastDoneTurn = turn
	w.isLastDoneBacked = isBacked
	w.isFarewellUnhung = false
	w.standing, w.standingTurn, w.reAskTurn = nil, 0, 0
	w.lastDoneAnswered = w.cue != nil
	w.isLastDoneInterrupted = false
	if w.cue != nil {
		w.outcome = &claimCueOutcome{kind: w.cue.kind, trigger: w.cue.trigger, isBacked: isToolCall}
		w.cue = nil
	}

	text := w.pending[turn]
	for pendingTurn := range w.pending {
		if pendingTurn <= turn {
			delete(w.pending, pendingTurn)
		}
	}
	if isBacked || text == "" {
		return nil
	}
	found := detectClaims(text)
	w.isFarewellUnhung = hasFarewell(found)
	w.stand(turn, found)
	return found
}

// Trigger values of a claim cue: what made the platform ask.
const (
	claimCueOnPlayback = "PLAYBACK_DONE"
	claimCueOnNoInput  = "NO_INPUT"
)

// claimTools are the tools the current phase lets the model call to make the
// claim true: the flow's backend tools for a lookup, transfer_to_agent for a
// transfer, take_message for a message. Empty when the phase offers none.
func claimTools(runtime *flow.Runtime, kind string) []string {
	switch kind {
	case store.UnbackedClaimLookup:
		var names []string
		for _, spec := range runtime.Tools() {
			if runtime.IsHTTPTool(spec.Name) {
				names = append(names, spec.Name)
			}
		}
		return names
	case store.UnbackedClaimTransfer:
		if runtime.IsToolAllowed(flow.ToolTransferToAgent) {
			return []string{flow.ToolTransferToAgent}
		}
	case store.UnbackedClaimMessage:
		if runtime.IsToolAllowed(flow.ToolTakeMessage) {
			return []string{flow.ToolTakeMessage}
		}
	}
	return nil
}

// claimCue tells the model that its last reply promised something no tool call
// made true, and what to do now. The wording is concrete on purpose: it quotes
// the promise, says nothing is happening, names the tool and says to call it
// before speaking, because a general reminder is what the model already failed
// to follow (actionClaimRule). isCallerSilent is the dead-air trigger: the
// caller has gone quiet waiting for it.
//
// callerWords is the merged cue of #448: the dead air is a caller turn the
// engine dropped, and its words ride in the same cue. That caller was not
// silent, so the silent prefix is left out, and the cue ends by asking for the
// words to be answered too.
func claimCue(lang string, c claim, tools []string, isCallerSilent bool, callerWords string) string {
	if lang == flow.LangZH {
		return claimCueZH(c, tools, isCallerSilent, callerWords)
	}
	return claimCueEN(c, tools, isCallerSilent, callerWords)
}

func claimCueEN(c claim, tools []string, isCallerSilent bool, callerWords string) string {
	var b strings.Builder
	b.WriteString("(")
	if isCallerSilent && callerWords == "" {
		b.WriteString("The caller has been silent, waiting for you. ")
	}
	b.WriteString("Your last reply told the caller \"" + c.phrase + "\", but ")
	if len(tools) == 0 {
		b.WriteString("nothing happened: you have no tool here that can do that. " +
			"Tell the caller plainly that it was not done, and say what you can do instead. " +
			"Do not claim it again.")
	} else {
		var happening, missing, again string
		switch c.kind {
		case store.UnbackedClaimTransfer:
			happening = "no transfer was made, so the caller is not being put through"
			missing = "what it needs (for example which queue or why)"
			again = "that you are transferring them"
		case store.UnbackedClaimMessage:
			happening = "no message was saved, so nothing has been recorded"
			missing = "what it needs (for example the message or a callback number)"
			again = "that it is saved"
		default:
			happening = "no lookup ran, so nothing is being looked up"
			missing = "what it needs (for example which order)"
			again = "that you are checking"
		}
		b.WriteString(happening + " and the caller is waiting. Call " + strings.Join(tools, " or ") +
			" now, without saying anything first. If you do not have " + missing +
			", ask the caller for it instead. Do not say again " + again + ".")
	}
	if callerWords != "" {
		b.WriteString(" The caller also said \"" + callerWords + "\" and got no reply; answer that too.")
	}
	b.WriteString(")")
	return b.String()
}

func claimCueZH(c claim, tools []string, isCallerSilent bool, callerWords string) string {
	var b strings.Builder
	b.WriteString("（")
	if isCallerSilent && callerWords == "" {
		b.WriteString("来电者一直没有说话，在等你。")
	}
	b.WriteString("你上一轮对来电者说了“" + c.phrase + "”，但")
	if len(tools) == 0 {
		b.WriteString("实际什么都没有发生：你在这里没有任何工具能做这件事。" +
			"请直接告诉来电者这件事没有办成，并说明你能做什么。不要再说你在做这件事。")
	} else {
		var happening, missing, again string
		switch c.kind {
		case store.UnbackedClaimTransfer:
			happening = "没有调用任何转接工具，转接并没有发生，来电者没有被接通"
			missing = "转接需要的信息（例如转到哪个队列、为什么）"
			again = "你在转接"
		case store.UnbackedClaimMessage:
			happening = "没有调用任何留言工具，留言并没有被记录"
			missing = "留言需要的信息（例如留言内容或回电号码）"
			again = "已经记录"
		default:
			happening = "没有调用任何查询工具，查询并没有发生"
			missing = "要查什么"
			again = "你在查"
		}
		b.WriteString(happening + "，来电者在等。现在就调用 " + strings.Join(tools, " 或 ") +
			"，调用之前不要说话。如果不知道" + missing + "，就直接问来电者。不要再说" + again + "。")
	}
	if callerWords != "" {
		b.WriteString("来电者还说了“" + callerWords + "”，但没有得到回复，请一并回答。")
	}
	b.WriteString("）")
	return b.String()
}
