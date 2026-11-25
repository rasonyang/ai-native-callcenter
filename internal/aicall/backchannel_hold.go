// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// This file is the session's half of docs/design/11-backchannels.md.
//
// On an engine whose client controls interruption (Profile.ControlsInterruption,
// today openai) speech over the bot's audible playback neither cuts it nor is
// answered until the words are known. Each such utterance is held: nothing is
// flushed, the provider is not told, no BARGE_IN or CALLER_SPEECH is published
// and the dead-air watch is left alone. The utterance's transcript decides:
// an acknowledgement ("mm-hmm") is declined and the bot goes on; anything else,
// or the cap, takes the floor through the same path a barge-in always took.
//
// On the other engines the engine cuts by itself. There the same classifier
// judges the utterance afterwards, with a run of different acknowledgements
// ("嗯，好的", isAckRun) counted as one too, and its verdict limits the damage
// (openCut): an acknowledgement does not move the flow, and does not count as
// the caller answering an armed closing line.

// holdCap is how long an utterance is held without a verdict before it takes
// the floor. Owner decision, 2026-11-11.
const holdCap = 2000 * time.Millisecond

// cutVerdictWait is how long a tool call that could move the flow waits for the
// verdict on the cut that preceded its response. Past it the call runs as it
// always did, unless the cut was of the opening (cutVerdictWordsUnknown).
const cutVerdictWait = 1000 * time.Millisecond

type utteranceState int

const (
	// utteranceHeld: speech started over audible playback; no verdict yet.
	utteranceHeld utteranceState = iota
	// utteranceFloor: the utterance has the floor (it was not held, or the
	// hold ended).
	utteranceFloor
	// utteranceDeclined: a greeting over the opening that no final transcript
	// decided in time; it was declined at the second cap (onHoldCap). A final
	// that still arrives is recorded flagged and nothing more.
	utteranceDeclined
)

// utterance is one stretch of caller speech the session is tracking.
type utterance struct {
	itemID string
	text   string
	state  utteranceState
	// isStopped is set by speech_stopped. isFloorDone says takeFloor has run to
	// the end; the stop is acted on once both hold, by whoever comes second.
	isStopped   bool
	isFloorDone bool
	isStopActed bool
	stopCap     func() bool
	startedAt   time.Time
	// isOverOpening says the utterance was held over the call's opening line
	// (or its replay): a greeting is then declined like an acknowledgement.
	// Anywhere else a greeting is words.
	isOverOpening bool
	// isCapExtended says the first cap found a greeting so far over the opening
	// and armed a second one.
	isCapExtended bool
}

// cutVerdict is the verdict on the utterance that cut the bot on an engine
// that cuts by itself.
type cutVerdict int

const (
	cutVerdictUnknown cutVerdict = iota
	cutVerdictAck
	cutVerdictContent
	// cutVerdictGreeting: a greeting ("喂") that cut the call's opening line.
	cutVerdictGreeting
	// cutVerdictWordsUnknown: words that cut the call's opening line while it
	// could still be said again (isOpeningReplayable), and that no transcript
	// made known: the final was blank, or none came within cutVerdictWait
	// (#610). The callee has not heard the opening, so the cut is judged as a
	// greeting's is.
	cutVerdictWordsUnknown
)

// String is the verdict as the log names it.
func (v cutVerdict) String() string {
	switch v {
	case cutVerdictAck:
		return "ACKNOWLEDGEMENT"
	case cutVerdictContent:
		return "WORDS"
	case cutVerdictGreeting:
		return "GREETING"
	case cutVerdictWordsUnknown:
		return "WORDS_UNKNOWN"
	}
	return "NONE"
}

// openCut is a cut the engine made over audible playback (trait off), kept
// until the utterance's final transcript says what it was.
type openCut struct {
	// event is the BARGE_IN that reported it.
	event Event
	// itemID is the utterance that cut, empty when the wire names none.
	itemID string
	// isAdopting says itemID is not known yet: the next speech start names it.
	isAdopting bool
	// answerTurn is the first turn that began after the cut (zero before one).
	answerTurn int
	// isOpening says the cut was over the call's opening line or its replay,
	// so a greeting is judged as such (classifyGreeting). isOpeningLine says
	// it was over that line's own turn, not over another turn while the
	// opening was owed: only then do played and unplayed time date the cut
	// within the opening (openingHeardToItsEnd).
	isOpening     bool
	isOpeningLine bool
	verdict       cutVerdict
	done          chan struct{}
	// isOpeningUnheard says an acknowledgement cut the opening before its
	// final clause while it could still be said again: no answer to an
	// offer the callee has not heard, so the opening is owed once more (#610).
	isOpeningUnheard bool
	// isReplayAsked says the opening was asked for again on this cut's verdict
	// (SayOpeningAgain).
	isReplayAsked bool
	// isWaitOver says a tool call waited cutVerdictWait for the verdict and no
	// final came: the lead guard (#612) does not wait for it again.
	isWaitOver bool
	// line is where the cut fell, decided when it was made (openCut):
	// obs.TurnCutLineOpening when isOpening, TurnCutLineClosing over the armed
	// ending's closing line (Config.IsClosingLine), else TurnCutLineLater.
	// isCounted says the cut is in aicc_bot_turn_cuts_total and the call's
	// tally (countCut, #626): set once, so a final that arrives after the cut
	// was counted MISSING does not count it again.
	line      string
	isCounted bool
}

// turnCutTally is a call's cuts as aicc_bot_turn_cuts_total counts them, by
// words and by line, for the call's one "ai turn cuts" line (#626). Guarded by
// backchannelState.mu.
type turnCutTally struct {
	words, blank, missing   int
	opening, later, closing int
}

// add counts one cut.
func (t *turnCutTally) add(words, line string) {
	switch words {
	case obs.TurnCutWords:
		t.words++
	case obs.TurnCutWordsBlank:
		t.blank++
	case obs.TurnCutWordsMissing:
		t.missing++
	}
	switch line {
	case obs.TurnCutLineOpening:
		t.opening++
	case obs.TurnCutLineClosing:
		t.closing++
	case obs.TurnCutLineLater:
		t.later++
	}
}

// backchannelState is everything the session keeps for backchannels. mu is
// taken after Session.mu when both are needed, and nothing is called with it
// held.
type backchannelState struct {
	mu         sync.Mutex
	utterances []*utterance
	cut        *openCut
	// lastItem is the latest utterance the provider named; isLastItemFinal says
	// its final transcript has arrived.
	lastItem        string
	isLastItemFinal bool
	// isOpeningOwed says the opening line was cut before it played in full and
	// has not been said again or answered by words since: any cut opened now
	// is judged as over the opening (a reply to the greeting that was itself
	// cancelled is a cut of the opening's, #594). owedAfterTurn is the latest
	// turn that cut it; a turn after it heard in full clears the mark
	// (noteOpeningHeard).
	isOpeningOwed bool
	owedAfterTurn int
	// cuts is the call's tally of counted cuts (countCut).
	cuts turnCutTally
}

// isHolding reports whether this session holds backchannels: the provider's
// profile gives the client the control and its client implements it.
func (s *Session) isHolding() bool { return s.turns != nil }

// holdTimer is the cap's clock: time.AfterFunc, replaced by a test.
func (s *Session) holdTimer(d time.Duration, f func()) (stop func() bool) {
	if s.holdAfter != nil {
		return s.holdAfter(d, f)
	}
	timer := time.AfterFunc(d, f)
	return timer.Stop
}

// cutWaitChan is the verdict wait's clock: time.After, replaced by a test.
func (s *Session) cutWaitChan(d time.Duration) <-chan time.Time {
	if s.cutWait != nil {
		return s.cutWait(d)
	}
	return time.After(d)
}

// isAudible reports whether the caller is hearing the bot, or about to: a turn
// is playing, or its tail is still queued after generation ended. It is the
// condition bargeIn takes the floor under.
func (s *Session) isAudible() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isBotSpeaking || (s.framesQueued > 0 && s.leg.Pending() > 0)
}

// find returns the tracked utterance with the id; an empty id (a
// dialect that names none) means the oldest one still open.
func (b *backchannelState) find(itemID string) *utterance {
	for _, u := range b.utterances {
		if itemID == "" || u.itemID == itemID {
			return u
		}
	}
	return nil
}

func (b *backchannelState) remove(u *utterance) {
	for i, other := range b.utterances {
		if other == u {
			b.utterances = append(b.utterances[:i], b.utterances[i+1:]...)
			return
		}
	}
}

// onSpeechStarted handles the provider reporting that the caller started to
// speak.
func (s *Session) onSpeechStarted(itemID string) {
	// First: a barge-in flushes the audio the utterance's start is dated by.
	s.noteCallerStarted(itemID)
	if !s.isHolding() {
		// Without the control the engine has cut the bot already, or will. An
		// open cut that does not know its utterance yet learns it here.
		s.noteSpeechItem(itemID)
		s.cancelDeadAirWatch()
		s.mu.Lock()
		s.timer.onSpeechStarted()
		s.mu.Unlock()
		s.bargeIn(provider.InterruptReasonSpeech)
		// After the barge-in, so a consumer sees the cut before the speech.
		s.emit(Event{Type: EventTypeCallerSpeech})
		return
	}

	u := &utterance{itemID: itemID, startedAt: time.Now()}
	// The opening is held over even before any of it is audible: a greeting
	// that lands while it is still being made must not cancel it.
	isHeld := s.isAudible() || s.isOpeningLive()
	if isHeld {
		u.isOverOpening = s.isOpeningLive()
	}
	s.bc.mu.Lock()
	s.bc.lastItem, s.bc.isLastItemFinal = itemID, false
	if isHeld {
		u.state = utteranceHeld
	} else {
		u.state = utteranceFloor
	}
	s.bc.utterances = append(s.bc.utterances, u)
	s.bc.mu.Unlock()

	if !isHeld {
		s.takeFloor(u, true)
		return
	}
	s.log.Info("holding speech over the bot's audio until the words are known",
		"itemId", itemID, "capMs", holdCap.Milliseconds())
	stop := s.holdTimer(holdCap, func() { s.onHoldCap(u) })
	s.bc.mu.Lock()
	u.stopCap = stop
	s.bc.mu.Unlock()
}

// onHoldCap takes the floor for an utterance still held at the cap.
func (s *Session) onHoldCap(u *utterance) {
	if s.isClosed() {
		return
	}
	s.bc.mu.Lock()
	isHeld := u.state == utteranceHeld
	isGreeting := u.isOverOpening && classifyGreeting(s.cfg.Session.Language, strings.TrimSpace(u.text)) != verdictContent &&
		strings.TrimSpace(u.text) != ""
	s.bc.mu.Unlock()
	if !isHeld {
		return
	}
	if isGreeting {
		s.onGreetingCap(u)
		return
	}
	s.log.Info("a held utterance reached the cap with no verdict; it takes the floor",
		"itemId", u.itemID, "capMs", holdCap.Milliseconds())
	s.takeFloor(u, false)
}

// takeFloor gives the utterance the floor: today's path at this moment. The
// dead-air watch is cancelled, the turn timer hears the speech start, the
// bot's audio is flushed and the provider told (the played time is the last
// frame heard), BARGE_IN and CALLER_SPEECH are published, and if the speech
// has already stopped the caller's turn is answered now; otherwise at the stop.
// isOnStart says the utterance has just started with nothing held: the 800 ms
// guard applies as it always did.
func (s *Session) takeFloor(u *utterance, isOnStart bool) {
	s.bc.mu.Lock()
	if u.state == utteranceFloor && !isOnStart {
		s.bc.mu.Unlock()
		return
	}
	u.state = utteranceFloor
	if u.stopCap != nil {
		u.stopCap()
		u.stopCap = nil
	}
	s.bc.mu.Unlock()

	s.cancelDeadAirWatch()
	s.mu.Lock()
	s.timer.onSpeechStarted()
	s.mu.Unlock()
	s.cutIn(provider.InterruptReasonSpeech, isOnStart)
	s.emit(Event{Type: EventTypeCallerSpeech})

	s.bc.mu.Lock()
	u.isFloorDone = true
	s.bc.mu.Unlock()
	s.actOnStop(u)
}

// actOnStop answers the caller's turn once the utterance both has the floor and
// has stopped, whichever came last.
func (s *Session) actOnStop(u *utterance) {
	s.bc.mu.Lock()
	if !u.isStopped || !u.isFloorDone || u.isStopActed {
		s.bc.mu.Unlock()
		return
	}
	u.isStopActed = true
	s.bc.mu.Unlock()

	s.mu.Lock()
	s.timer.onSpeechStopped()
	s.mu.Unlock()
	s.noteCallerAnswerAsked(u.itemID)
	if err := s.turns.AnswerCallerTurn(u.itemID); err != nil {
		s.log.Warn("could not ask for the caller's turn", "itemId", u.itemID, "error", err)
	}
}

// onSpeechStopped handles the provider reporting the end of an utterance.
func (s *Session) onSpeechStopped(itemID string) {
	s.noteCallerStopped(itemID)
	if !s.isHolding() {
		s.mu.Lock()
		s.timer.onSpeechStopped()
		s.mu.Unlock()
		return
	}
	s.bc.mu.Lock()
	var u *utterance
	for _, candidate := range s.bc.utterances {
		if !candidate.isStopped && (itemID == "" || candidate.itemID == itemID) {
			u = candidate
			break
		}
	}
	if u != nil {
		u.isStopped = true
	}
	s.bc.mu.Unlock()
	if u == nil {
		// Speech the session never saw start: answered as ever.
		s.mu.Lock()
		s.timer.onSpeechStopped()
		s.mu.Unlock()
		s.noteCallerAnswerAsked(itemID)
		if err := s.turns.AnswerCallerTurn(itemID); err != nil {
			s.log.Warn("could not ask for the caller's turn", "itemId", itemID, "error", err)
		}
		return
	}
	s.actOnStop(u)
}

// onInputTranscript handles a piece of the caller's transcript.
func (s *Session) onInputTranscript(event provider.Event) {
	if !event.IsFinal {
		if s.isHolding() {
			s.holdPartial(event)
		}
		s.emit(Event{Type: EventTypeCustomerSaid, Text: event.Text, IsFinal: false})
		return
	}

	isBackchannel := false
	var openingCut *openCut
	if s.isHolding() {
		isBackchannel = s.holdFinal(event)
	} else {
		openingCut = s.noteCutVerdict(event)
	}
	s.noteCallerFinal(event.ItemID, event.Text, isBackchannel)
	if isBackchannel {
		// Recorded as the caller's words, flagged; nothing else hears it: not
		// the turn timer (#448's cue), not the flow, not the dead-air watch.
		s.emit(Event{Type: EventTypeCustomerSaid, Text: event.Text, IsFinal: true, IsBackchannel: true})
		return
	}
	s.mu.Lock()
	s.timer.onCallerWords(event.Text)
	s.mu.Unlock()
	s.emit(Event{Type: EventTypeCustomerSaid, Text: event.Text, IsFinal: true, openingCut: openingCut})
}

// holdPartial accumulates a partial transcript on its held utterance; words
// that cannot become an acknowledgement take the floor at once.
func (s *Session) holdPartial(event provider.Event) {
	s.bc.mu.Lock()
	u := s.bc.find(event.ItemID)
	if u == nil || u.state != utteranceHeld {
		s.bc.mu.Unlock()
		return
	}
	u.text += event.Text
	text := strings.TrimSpace(u.text)
	s.bc.mu.Unlock()
	if text == "" {
		return
	}
	isOpening := false
	s.bc.mu.Lock()
	isOpening = u.isOverOpening
	s.bc.mu.Unlock()
	isWords := classifyBackchannel(s.cfg.Session.Language, text) == verdictContent
	if isWords && isOpening {
		isWords = classifyGreeting(s.cfg.Session.Language, text) == verdictContent
	}
	if isWords {
		s.log.Info("a held utterance is words, not an acknowledgement; it takes the floor",
			"itemId", u.itemID, "text", text)
		s.takeFloor(u, false)
	}
}

// holdFinal applies the final transcript of an utterance and reports whether it
// was a backchannel (declined, unanswered).
func (s *Session) holdFinal(event provider.Event) (isBackchannel bool) {
	text := strings.TrimSpace(event.Text)
	isAck := text != "" && classifyBackchannel(s.cfg.Session.Language, text) == verdictAck
	isGreeting := text != "" && classifyGreeting(s.cfg.Session.Language, text) == verdictGreeting

	s.bc.mu.Lock()
	u := s.bc.find(event.ItemID)
	if event.ItemID == s.bc.lastItem {
		s.bc.isLastItemFinal = true
	}
	if u == nil {
		s.bc.mu.Unlock()
		return false
	}
	if u.state == utteranceDeclined {
		// Declined at the second cap: the late final is recorded flagged.
		s.bc.remove(u)
		s.bc.mu.Unlock()
		return true
	}
	if u.state != utteranceHeld {
		s.bc.remove(u)
		s.bc.mu.Unlock()
		return false
	}
	// A greeting is declined only over the opening; said anywhere else it is
	// words, as it always was.
	isAck = isAck || (isGreeting && u.isOverOpening)
	if isAck {
		// Settled under the lock: the cap's timer may be firing right now, and
		// it must find the utterance no longer held, or the bot would be cut
		// for words that are then declined.
		u.state = utteranceFloor
		if u.stopCap != nil {
			u.stopCap()
			u.stopCap = nil
		}
		s.bc.remove(u)
		s.bc.mu.Unlock()
		s.log.Info("the caller acknowledged; the bot goes on and the words are not answered",
			"itemId", u.itemID, "text", text)
		if err := s.turns.DeclineCallerTurn(u.itemID); err != nil {
			s.log.Warn("could not decline an acknowledgement", "itemId", u.itemID, "error", err)
		}
		return true
	}
	s.bc.mu.Unlock()
	// Words, or nothing a transcript could keep (a cough): the floor is taken.
	s.log.Info("a held utterance's final is not an acknowledgement; it takes the floor",
		"itemId", u.itemID, "text", text)
	s.takeFloor(u, false)
	s.bc.mu.Lock()
	s.bc.remove(u)
	s.bc.mu.Unlock()
	return false
}

// noteSpeechItem records the utterance a speech start names, for the engines
// that cut by themselves: an open cut still looking for its utterance adopts it.
func (s *Session) noteSpeechItem(itemID string) {
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	s.bc.lastItem, s.bc.isLastItemFinal = itemID, false
	if cut := s.bc.cut; cut != nil && cut.isAdopting && cut.verdict == cutVerdictUnknown {
		cut.itemID, cut.isAdopting = itemID, false
	}
}

// openCut remembers a cut the engine made over audible playback, so the verdict
// on the words that caused it can limit the damage. It is for engines that cut
// by themselves; a held session has the words before it cuts.
func (s *Session) openCut(cut Event) {
	if s.isHolding() {
		return
	}
	isOpeningLine := s.isOpeningTurn(cut.Turn)
	isOpening := isOpeningLine
	// Where the cut fell is fixed now, not when it is counted (#626). Asked
	// with no lock held: the actions take their own (Config.IsClosingLine).
	isClosingLine := s.cfg.IsClosingLine != nil && s.cfg.IsClosingLine(cut.Turn)
	s.bc.mu.Lock()
	if s.bc.isOpeningOwed {
		isOpening = true
	}
	if isOpening {
		s.bc.isOpeningOwed = true
		s.bc.owedAfterTurn = max(s.bc.owedAfterTurn, cut.Turn)
	}
	line := obs.TurnCutLineLater
	switch {
	case isOpening:
		line = obs.TurnCutLineOpening
	case isClosingLine:
		line = obs.TurnCutLineClosing
	}
	next := &openCut{event: cut, isOpening: isOpening, isOpeningLine: isOpeningLine, line: line,
		done: make(chan struct{})}
	if isOpening {
		// The lead guard's record of who spoke over the opening (#612).
		s.noteOpeningCut(next)
	}
	// The utterance that cut is the latest one the provider named, unless its
	// transcript is already in (then it was an earlier utterance, and the next
	// speech start is this one).
	if s.bc.lastItem != "" && !s.bc.isLastItemFinal {
		next.itemID = s.bc.lastItem
	} else {
		next.isAdopting = true
	}
	prev := s.bc.cut
	s.bc.cut = next
	s.bc.mu.Unlock()
	if prev != nil {
		// No final can judge the cut this one replaces any more: unless one
		// already has, it counts as missing (#626).
		s.countCut(prev, obs.TurnCutWordsMissing)
	}
}

// noteResponseStarted tells an open cut which turn answers it.
func (s *Session) noteResponseStarted(turn int) {
	s.bc.mu.Lock()
	if cut := s.bc.cut; cut != nil && cut.answerTurn == 0 {
		cut.answerTurn = turn
	}
	s.bc.mu.Unlock()
}

// cutFor returns the open cut a tool call of this turn is answering, nil when
// none: the window is the response that answered the cut.
func (s *Session) cutFor(turn int) *openCut {
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	if cut := s.bc.cut; cut != nil && turn != 0 && cut.answerTurn == turn {
		return cut
	}
	return nil
}

// noteCutVerdict classifies the final transcript that follows a cut. An
// acknowledgement is reported to the call's OnCutAcknowledged; words close the
// window for the next utterances' sake (a later non-backchannel utterance
// replaces the cut). A greeting over the opening is not an acknowledgement and
// is not reported there, nor is a blank final over an opening that can still
// be said again (#610). Either returns the cut, and so does an acknowledgement
// that cut the opening before its final clause (#610, #598): the transcript's
// event carries it and the orchestrator, which knows the phase and the ending,
// can say the opening again (Session.SayOpeningAgain).
func (s *Session) noteCutVerdict(event provider.Event) (replayCut *openCut) {
	replayCut = s.judgeCut(event)
	text := strings.TrimSpace(event.Text)
	if text == "" {
		return replayCut
	}
	isGreeting := classifyGreeting(s.cfg.Session.Language, text) == verdictGreeting
	isStandIn := false
	s.bc.mu.Lock()
	switch {
	case !isGreeting && replayCut == nil:
		// Words answer what the opening was owed to; so does an
		// acknowledgement heard over its final clause, or over a replay. A
		// greeting, or an acknowledgement before the final clause (#610),
		// leaves it owed until it is replayed.
		s.bc.isOpeningOwed = false
	case isGreeting && replayCut == nil && s.bc.isOpeningOwed:
		// The opening is owed and no cut of its own judged these words (the
		// cut was superseded, or this is another utterance's final): they
		// still owe it. The mark stands in for the cut.
		replayCut = &openCut{isOpening: true, verdict: cutVerdictGreeting}
		isStandIn = true
	}
	s.bc.mu.Unlock()
	if isStandIn {
		// A turn that began since is the reply, to be muted if it is live.
		s.mu.Lock()
		if !s.isOpeningTurnLocked(s.turnSeq) {
			replayCut.answerTurn = s.turnSeq
		}
		s.mu.Unlock()
	}
	return replayCut
}

// countCut records the cut in aicc_bot_turn_cuts_total and the call's tally,
// once (#626): words is what its final transcript made of it, obs.TurnCutWords,
// TurnCutWordsBlank or TurnCutWordsMissing; the line it fell on was fixed when
// it was made (openCut). The first count of a cut is the one kept. It calls
// nothing with bc.mu held and takes no other lock.
func (s *Session) countCut(cut *openCut, words string) {
	s.bc.mu.Lock()
	if cut.isCounted {
		s.bc.mu.Unlock()
		return
	}
	cut.isCounted = true
	s.bc.cuts.add(words, cut.line)
	s.bc.mu.Unlock()
	obs.RecordBotTurnCut(s.providerName, words, cut.line)
}

// finishTurnCuts counts the cut the call ended on as MISSING, unless a final
// judged it, and logs the call's cuts in one line when it had any (#626). It
// runs once every pump has returned, so no final can judge a cut any more and
// no cut can open; a tool call still waiting on a cut (cutVerdictOf) can only
// count that same cut, once.
func (s *Session) finishTurnCuts() {
	s.bc.mu.Lock()
	cut := s.bc.cut
	s.bc.mu.Unlock()
	if cut != nil {
		s.countCut(cut, obs.TurnCutWordsMissing)
	}
	s.bc.mu.Lock()
	t := s.bc.cuts
	s.bc.mu.Unlock()
	if t.words+t.blank+t.missing == 0 {
		return
	}
	args := []any{
		"cutsWords", t.words, "cutsBlank", t.blank, "cutsMissing", t.missing,
		"cutsOpening", t.opening, "cutsLater", t.later, "cutsClosing", t.closing,
	}
	if s.cfg.CampaignID != "" {
		args = append(args, "campaignId", s.cfg.CampaignID)
	}
	s.log.Info("ai turn cuts", args...)
}

// judgeCut is the verdict on the open cut for a final transcript
// (noteCutVerdict). It returns the cut when the verdict says the opening is to
// be said again: a greeting over it, or, while it can still be said again,
// words left unknown by a blank final or an acknowledgement before its final
// clause (#610, #598).
func (s *Session) judgeCut(event provider.Event) (replayCut *openCut) {
	// Asked before bc.mu, and with no lock held (isOpeningReplayable).
	isReplayable := s.isOpeningReplayable()
	s.bc.mu.Lock()
	if event.ItemID == s.bc.lastItem {
		s.bc.isLastItemFinal = true
	}
	cut := s.bc.cut
	// A cut still waiting for its speech start cannot be judged by a named
	// utterance: that final belongs to something said before the cut.
	if cut == nil || cut.verdict != cutVerdictUnknown ||
		(cut.isAdopting && event.ItemID != "") ||
		(cut.itemID != "" && event.ItemID != "" && cut.itemID != event.ItemID) {
		s.bc.mu.Unlock()
		return nil
	}
	text := strings.TrimSpace(event.Text)
	// A run of different acknowledgements ("嗯，好的") is one here too (#600);
	// the hold keeps the classifier's single phrase.
	isAck := text != "" && (classifyBackchannel(s.cfg.Session.Language, text) == verdictAck ||
		isAckRun(s.cfg.Session.Language, text))
	isGreeting := !isAck && cut.isOpening && text != "" &&
		classifyGreeting(s.cfg.Session.Language, text) == verdictGreeting
	// A blank final over the opening says nothing of what the callee said, and
	// they have not heard the opening: judged as a greeting is, while the
	// opening can still be said again. Anywhere else it is content, as a cough
	// that takes the floor always was.
	isWordsUnknown := text == "" && cut.isOpening && isReplayable
	// An acknowledgement over the opening is no answer to an offer the callee
	// has not heard either (#610), unless it fell in the opening's final
	// clause (#598): then the callee heard all of it but the end, and it is the
	// acknowledgement it always was.
	var finalClause time.Duration
	isHeardToEnd := false
	if isAck && cut.isOpening && isReplayable {
		finalClause, isHeardToEnd = s.openingHeardToItsEnd(cut)
		cut.isOpeningUnheard = !isHeardToEnd
	}
	isOpeningUnheard := cut.isOpeningUnheard
	switch {
	case isAck:
		cut.verdict = cutVerdictAck
	case isGreeting:
		cut.verdict = cutVerdictGreeting
	case isWordsUnknown:
		cut.verdict = cutVerdictWordsUnknown
	default:
		cut.verdict = cutVerdictContent
	}
	verdict := cut.verdict
	close(cut.done)
	s.bc.mu.Unlock()
	words := obs.TurnCutWords
	if text == "" {
		words = obs.TurnCutWordsBlank
	}
	s.countCut(cut, words)
	if text == "" {
		// The engine heard speech and its transcriber wrote nothing: the one
		// trace of words the call's transcript leaves out.
		s.log.Info("the final transcript of the words that cut the bot is blank",
			"turn", cut.event.Turn, "isOpening", cut.isOpening, "verdict", verdict.String())
	}
	if isGreeting {
		s.log.Info("the engine cut the opening for a greeting; limiting the damage",
			"text", text, "turn", cut.event.Turn)
		return cut
	}
	if isWordsUnknown {
		return cut
	}
	if !isAck {
		return nil
	}
	s.log.Info("the engine cut the bot for an acknowledgement; limiting the damage",
		"text", text, "turn", cut.event.Turn)
	switch {
	case isOpeningUnheard:
		s.log.Info("the acknowledgement cut the opening before its final clause; the opening is owed once more",
			"turn", cut.event.Turn, "playedMs", cut.event.Played.Milliseconds(),
			"unplayedMs", cut.event.Unplayed.Milliseconds())
	case isHeardToEnd:
		s.log.Info("the acknowledgement fell in the opening's final clause; the opening counts as heard",
			"turn", cut.event.Turn, "playedMs", cut.event.Played.Milliseconds(),
			"unplayedMs", cut.event.Unplayed.Milliseconds(), "finalClauseMs", finalClause.Milliseconds())
	}
	if s.cfg.OnCutAcknowledged != nil {
		s.cfg.OnCutAcknowledged(cut.event)
	}
	if isOpeningUnheard {
		return cut
	}
	return nil
}

// openingHeardToItsEnd reports whether a cut of the opening fell in its final
// clause, with that clause's estimated length: the estimate #465 makes for an
// armed closing line, by the clause and not the sentence (cutInFinalClause,
// #598), on the opening's rendered text. Only a cut of the opening line's own
// turn, made after it finished generating (its whole length is then known), is
// dated; any other cut is before the end.
func (s *Session) openingHeardToItsEnd(cut *openCut) (finalClause time.Duration, ok bool) {
	if !cut.isOpeningLine || cut.event.IsGenerating {
		return 0, false
	}
	return cutInFinalClause(s.cfg.Session.OpeningText, cut.event.Played, cut.event.Unplayed)
}

// openingReplayOf reports, for a cut, whether its verdict owes the opening once
// more (a greeting, words not known, or an acknowledgement before the final
// clause) and whether it has been asked for again on it.
func (s *Session) openingReplayOf(cut *openCut) (isOwed, isAsked bool) {
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	isOwed = cut.verdict == cutVerdictGreeting || cut.verdict == cutVerdictWordsUnknown ||
		(cut.verdict == cutVerdictAck && cut.isOpeningUnheard)
	return isOwed, cut.isReplayAsked
}

// verdictName is the verdict on a cut as the log names it.
func (s *Session) verdictName(cut *openCut) string {
	if cut == nil {
		return cutVerdictUnknown.String()
	}
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	return cut.verdict.String()
}

// refusesMove says a tool that could move the call is refused for this verdict.
func (v cutVerdict) refusesMove() bool {
	return v == cutVerdictAck || v == cutVerdictGreeting || v == cutVerdictWordsUnknown
}

// cutVerdictOf is the verdict on the cut, waiting up to cutVerdictWait for it,
// and how long it waited. No verdict in time is cutVerdictUnknown, and the call
// runs as it always did, except on a cut of an opening that can still be said
// again: its words are not known, and isExpired says this wait settled it so
// (cutVerdictWordsUnknown, #610). No transcript will bring that cut to the
// orchestrator, so the caller says the opening again itself.
func (s *Session) cutVerdictOf(cut *openCut) (verdict cutVerdict, waited time.Duration, isExpired bool) {
	select {
	case <-cut.done:
	default:
		began := time.Now()
		select {
		case <-cut.done:
			waited = time.Since(began)
		case <-s.cutWaitChan(cutVerdictWait):
			waited = time.Since(began)
			if s.expireOpeningCut(cut) {
				s.log.Info("no verdict on the cut of the opening within the wait; the callee's words are not known",
					"turn", cut.event.Turn, "waitedMs", waited.Milliseconds())
				// The wait settled it: a final that arrives later judges
				// nothing, so the cut is MISSING (#626). Any other cut stays
				// open to its final and is counted by it, by the cut that
				// replaces it or at the call's end.
				s.countCut(cut, obs.TurnCutWordsMissing)
				return cutVerdictWordsUnknown, waited, true
			}
			s.bc.mu.Lock()
			verdict = cut.verdict
			cut.isWaitOver = verdict == cutVerdictUnknown
			s.bc.mu.Unlock()
			if verdict != cutVerdictUnknown {
				// A final judged it as the wait ran out.
				return verdict, waited, false
			}
			s.log.Info("no verdict on the cut within the wait; the tool call runs as it always did")
			return cutVerdictUnknown, waited, false
		case <-s.done:
			return cutVerdictUnknown, time.Since(began), false
		}
	}
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	return cut.verdict, waited, false
}

// expireOpeningCut settles a cut of the opening that no final transcript
// judged within the wait, while the opening can still be said again: its words
// are not known (#610). A transcript that arrives later is the caller's words
// as ever and judges nothing. It reports false for any other cut, and for one a
// final judged meanwhile.
func (s *Session) expireOpeningCut(cut *openCut) bool {
	// Asked before bc.mu, and with no lock held (isOpeningReplayable).
	isReplayable := s.isOpeningReplayable()
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	if cut.verdict != cutVerdictUnknown || !cut.isOpening || !isReplayable {
		return false
	}
	cut.verdict = cutVerdictWordsUnknown
	close(cut.done)
	return true
}

// onGreetingCap handles the cap of a held utterance that is a greeting so far
// over the opening. The first cap does not cut the opening: it arms one more
// cap and the final transcript decides, words arriving in the meantime take the
// floor as ever. The provider does not report a failed transcription, so a
// final may never come; the second cap declines the utterance as a greeting
// (the opening is not cut, the item is deleted) rather than hold it for good.
func (s *Session) onGreetingCap(u *utterance) {
	s.bc.mu.Lock()
	if u.state != utteranceHeld {
		s.bc.mu.Unlock()
		return
	}
	if !u.isCapExtended {
		u.isCapExtended = true
		u.stopCap = s.holdTimer(holdCap, func() { s.onHoldCap(u) })
		s.bc.mu.Unlock()
		s.log.Info("a held utterance over the opening is a greeting so far at the cap; the opening goes on",
			"itemId", u.itemID, "capMs", holdCap.Milliseconds())
		return
	}
	u.state = utteranceDeclined
	u.stopCap = nil
	s.bc.mu.Unlock()
	s.noteCallerDeclined(u.itemID)
	s.log.Info("a greeting over the opening had no final transcript by the second cap; declined, the opening goes on",
		"itemId", u.itemID, "capMs", holdCap.Milliseconds())
	if err := s.turns.DeclineCallerTurn(u.itemID); err != nil {
		s.log.Warn("could not decline a greeting", "itemId", u.itemID, "error", err)
	}
}
