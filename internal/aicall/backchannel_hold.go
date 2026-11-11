// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"sync"
	"time"

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
// judges the utterance afterwards, and its verdict limits the damage
// (openCut): an acknowledgement does not move the flow, and does not count as
// the caller answering an armed closing line.

// holdCap is how long an utterance is held without a verdict before it takes
// the floor. Owner decision, 2026-11-11.
const holdCap = 2000 * time.Millisecond

// cutVerdictWait is how long a tool call that could move the flow waits for the
// verdict on the cut that preceded its response before running as it always did.
const cutVerdictWait = 1000 * time.Millisecond

type utteranceState int

const (
	// utteranceHeld: speech started over audible playback; no verdict yet.
	utteranceHeld utteranceState = iota
	// utteranceFloor: the utterance has the floor (it was not held, or the
	// hold ended).
	utteranceFloor
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
}

// cutVerdict is the verdict on the utterance that cut the bot on an engine
// that cuts by itself.
type cutVerdict int

const (
	cutVerdictUnknown cutVerdict = iota
	cutVerdictAck
	cutVerdictContent
)

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
	verdict    cutVerdict
	done       chan struct{}
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
	isHeld := s.isAudible()
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
	s.bc.mu.Unlock()
	if !isHeld {
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
	if err := s.turns.AnswerCallerTurn(u.itemID); err != nil {
		s.log.Warn("could not ask for the caller's turn", "itemId", u.itemID, "error", err)
	}
}

// onSpeechStopped handles the provider reporting the end of an utterance.
func (s *Session) onSpeechStopped(itemID string) {
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
	if s.isHolding() {
		isBackchannel = s.holdFinal(event)
	} else {
		s.noteCutVerdict(event)
	}
	if isBackchannel {
		// Recorded as the caller's words, flagged; nothing else hears it: not
		// the turn timer (#448's cue), not the flow, not the dead-air watch.
		s.emit(Event{Type: EventTypeCustomerSaid, Text: event.Text, IsFinal: true, IsBackchannel: true})
		return
	}
	s.mu.Lock()
	s.timer.onCallerWords(event.Text)
	s.mu.Unlock()
	s.emit(Event{Type: EventTypeCustomerSaid, Text: event.Text, IsFinal: true})
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
	if classifyBackchannel(s.cfg.Session.Language, text) == verdictContent {
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

	s.bc.mu.Lock()
	u := s.bc.find(event.ItemID)
	if event.ItemID == s.bc.lastItem {
		s.bc.isLastItemFinal = true
	}
	if u == nil {
		s.bc.mu.Unlock()
		return false
	}
	if u.state != utteranceHeld {
		s.bc.remove(u)
		s.bc.mu.Unlock()
		return false
	}
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
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	next := &openCut{event: cut, done: make(chan struct{})}
	// The utterance that cut is the latest one the provider named, unless its
	// transcript is already in (then it was an earlier utterance, and the next
	// speech start is this one).
	if s.bc.lastItem != "" && !s.bc.isLastItemFinal {
		next.itemID = s.bc.lastItem
	} else {
		next.isAdopting = true
	}
	s.bc.cut = next
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
// replaces the cut).
func (s *Session) noteCutVerdict(event provider.Event) {
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
		return
	}
	text := strings.TrimSpace(event.Text)
	isAck := text != "" && classifyBackchannel(s.cfg.Session.Language, text) == verdictAck
	if isAck {
		cut.verdict = cutVerdictAck
	} else {
		cut.verdict = cutVerdictContent
	}
	close(cut.done)
	s.bc.mu.Unlock()
	if !isAck {
		return
	}
	s.log.Info("the engine cut the bot for an acknowledgement; limiting the damage",
		"text", text, "turn", cut.event.Turn)
	if s.cfg.OnCutAcknowledged != nil {
		s.cfg.OnCutAcknowledged(cut.event)
	}
}

// cutWasAcknowledgement reports whether the cut was an acknowledgement, waiting up
// to cutVerdictWait for the verdict. No verdict in time is false: the call runs
// as it always did.
func (s *Session) cutWasAcknowledgement(cut *openCut) bool {
	select {
	case <-cut.done:
	default:
		select {
		case <-cut.done:
		case <-s.cutWaitChan(cutVerdictWait):
			s.log.Info("no verdict on the cut within the wait; the tool call runs as it always did")
			return false
		case <-s.done:
			return false
		}
	}
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	return cut.verdict == cutVerdictAck
}
