// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"sync"
	"time"
)

// This file is the session's half of the lead guard (#612, lead_guard.go): for
// a tool call that would record the customer's interest, which caller utterance
// it answers, whether that utterance began before the caller had heard the
// opening to its final clause (#598), and the utterance's final transcript.
//
// The link is the same on every engine, because no provider names on a tool
// call the caller item it answers. A response answers what the caller had said
// when it began: the latest utterance that had ended by then (speech_stopped,
// which the Realtime engines send before the commit their response follows, and
// doubao sends right after the final transcript), or, on a session that holds
// speech (openai, backchannel_hold.go), the latest one whose turn was asked for
// (AnswerCallerTurn): a held utterance is not answered until its words are
// known, and a declined acknowledgement never is. The snapshot is taken as the
// response starts (beginTurn) and rides on each of its TOOL_CALLs, as the cut
// of #403 does, so a later response answering a tool result still answers the
// caller's words that led to it.
//
// The final transcript is matched to the utterance by item id where the wire
// gives one and the ids agree; otherwise, as for a dialect whose speech events
// carry none, it is the oldest utterance still waiting for one. A final that
// matches no utterance (an engine that reports transcripts and no speech) is an
// utterance of its own, already over.

// maxCallerTurns is how many recent utterances the session keeps. A tool call
// answers the latest; a few more cover finals that arrive out of order.
const maxCallerTurns = 8

// openingCutWindow is how long before a speech start an engine's own cut of the
// opening may come and still be that speech's. An engine with turn detection of
// its own (qwen, grok, doubao, gateway) cancels the turn the moment it hears the
// caller and reports the speech after it, in the same moment.
const openingCutWindow = time.Second

// callerTurn is one caller utterance as the lead guard knows it. Its fields
// are guarded by callerTurns.mu; seq and done never change.
type callerTurn struct {
	// seq numbers the utterances of the call from 1.
	seq    int
	itemID string
	// isStopped says the speech ended (speech_stopped). isAnswerAsked says,
	// on a session that holds speech, that its turn was asked for.
	isStopped     bool
	isAnswerAsked bool
	// isDeclined says it was an acknowledgement or a greeting the session held
	// and declined: no turn answers it.
	isDeclined bool
	// isOpeningUnheard says it began over the call's opening line, its replay,
	// or while a cut opening was owed, before the caller had heard the
	// opening's final clause (isOverUnheardOpening, noteOpeningCut).
	isOpeningUnheard bool
	// The final transcript, when it came, and when.
	isFinal bool
	text    string
	finalAt time.Time
	// isWaitSpent says a tool call has already waited leadFinalWait for the
	// final and it did not come: a later one does not wait again.
	isWaitSpent bool
	// done closes when the final arrives.
	done chan struct{}
}

// callerTurns is the session's record of the call's recent utterances.
// mu is a leaf: nothing else is locked while it is held.
type callerTurns struct {
	mu   sync.Mutex
	list []*callerTurn
	seq  int
	// openingCutAt is when an engine's own cut of the opening, before its
	// final clause, came with no utterance under way: the speech start that
	// follows within openingCutWindow began over the unheard opening.
	openingCutAt time.Time
	// machineCategory is the kind of machine whose wording marked the call
	// as answered by one (obs.Machine*, machine_wording.go), empty while it is
	// not marked. Set once and never cleared.
	machineCategory string
	// isOpeningHeard says the caller has heard the call's opening line, or its
	// replay, to its final clause at least once (#715): one of them played to
	// the caller in full, with audio and not muted (noteOpeningHeard), or was
	// cut inside its final clause (cutInFinalClause: noteOpeningCut, and a
	// speech start dated there, isOverUnheardOpening). No other bot line sets
	// it and no caller transcript does. False at the start; set once and never
	// cleared.
	isOpeningHeard bool
}

// markOpeningHeard records that the caller has heard the opening to its final
// clause (isOpeningHeard). It takes only mu, a leaf, so it may be called with
// Session.mu or bc.mu held.
func (c *callerTurns) markOpeningHeard() {
	c.mu.Lock()
	c.isOpeningHeard = true
	c.mu.Unlock()
}

// isOpeningHeard reports whether the caller has heard the opening, or its
// replay, to its final clause at least once (callerTurns.isOpeningHeard).
// A call with no opening line never has.
func (s *Session) isOpeningHeard() bool {
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	return s.ct.isOpeningHeard
}

// noteCallerStarted records a caller utterance starting. It is called before
// anything acts on the speech (a barge-in flushes the audio it is dated by).
func (s *Session) noteCallerStarted(itemID string) {
	isUnheard := s.isOverUnheardOpening()
	now := time.Now()
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	if !s.ct.openingCutAt.IsZero() {
		isUnheard = isUnheard || now.Sub(s.ct.openingCutAt) <= openingCutWindow
		s.ct.openingCutAt = time.Time{}
	}
	s.ct.seq++
	s.ct.add(&callerTurn{seq: s.ct.seq, itemID: itemID, isOpeningUnheard: isUnheard,
		done: make(chan struct{})})
}

// isOverUnheardOpening reports whether caller speech starting now begins
// before the caller has heard the opening to its final clause: the opening
// line or its replay is being made or heard and has not reached its final
// clause, dated as #610 dates a cut of it (cutInFinalClause on the rendered
// line, from the played and unplayed audio; a line still being made, or not
// yet audible, is never in its final clause). The clause and not the sentence
// (#598): an opening of a short greeting and then one long sentence that
// carries the offer and ends in the question has its final sentence start
// after the greeting, and words there have heard none of the offer. On an
// engine that cuts by itself, speech while a cut opening is owed
// (bc.isOpeningOwed: it was cut before it played in full and nothing has
// answered it since) begins over it too, as any cut then is judged over it.
// The cut itself dates the same utterance by the same clause
// (noteOpeningCut), so both of rule A's inputs agree. Speech that starts in the
// final clause finds the opening heard to it, and the call records so
// (isOpeningHeard, #715).
func (s *Session) isOverUnheardOpening() bool {
	s.mu.Lock()
	isLine := s.isOpeningTurnLocked(s.turnSeq)
	pending := s.leg.Pending()
	isLive := isLine && (s.isResponding || s.isBotSpeaking || (s.framesQueued > 0 && pending > 0))
	isDated := !s.isResponding && s.framesQueued > 0
	played := time.Duration(max(s.framesQueued-pending, 0)*frameDurationMs) * time.Millisecond
	unplayed := time.Duration(pending*frameDurationMs) * time.Millisecond
	s.mu.Unlock()
	if isLive {
		if !isDated {
			return true
		}
		_, isHeard := cutInFinalClause(s.cfg.Session.OpeningText, played, unplayed)
		if isHeard {
			s.ct.markOpeningHeard()
		}
		return !isHeard
	}
	if s.isHolding() {
		return false
	}
	s.bc.mu.Lock()
	defer s.bc.mu.Unlock()
	return s.bc.isOpeningOwed
}

// noteOpeningCut is told of a cut over the opening (openCut, an engine that
// cuts by itself), with bc.mu held. A cut before the opening's final clause
// (openingHeardToItsEnd, as isOverUnheardOpening dates a speech start) marks
// the utterance under way as begun over the unheard opening; with none under
// way, the engine cut before it reported the speech, and the next speech start
// is marked (openingCutWindow). A cut inside the final clause finds the opening
// heard to it, and the call records so (isOpeningHeard, #715); nothing clears
// that.
func (s *Session) noteOpeningCut(cut *openCut) {
	if _, isHeard := s.openingHeardToItsEnd(cut); isHeard {
		s.ct.markOpeningHeard()
		return
	}
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	if n := len(s.ct.list); n > 0 && !s.ct.list[n-1].isStopped {
		s.ct.list[n-1].isOpeningUnheard = true
		return
	}
	s.ct.openingCutAt = time.Now()
}

// noteCallerStopped records the end of a caller utterance.
func (s *Session) noteCallerStopped(itemID string) {
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	t := s.ct.byID(itemID)
	if t == nil {
		t = s.ct.oldest(func(t *callerTurn) bool { return !t.isStopped })
	}
	if t != nil {
		t.isStopped = true
	}
}

// noteCallerAnswerAsked records, on a session that holds speech, that the
// caller's turn for the utterance was asked for (AnswerCallerTurn). Speech the
// session never saw start is an utterance of its own.
func (s *Session) noteCallerAnswerAsked(itemID string) {
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	t := s.ct.byID(itemID)
	if t == nil && itemID == "" {
		t = s.ct.oldest(func(t *callerTurn) bool { return !t.isAnswerAsked && !t.isDeclined })
	}
	if t == nil {
		s.ct.seq++
		t = &callerTurn{seq: s.ct.seq, itemID: itemID, done: make(chan struct{})}
		s.ct.add(t)
	}
	t.isStopped, t.isAnswerAsked = true, true
}

// noteCallerDeclined records that the session declined an utterance it held
// (an acknowledgement, or a greeting over the opening): no turn answers it.
func (s *Session) noteCallerDeclined(itemID string) {
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	t := s.ct.byID(itemID)
	if t == nil && itemID == "" {
		t = s.ct.oldest(func(t *callerTurn) bool { return !t.isDeclined && !t.isAnswerAsked })
	}
	if t != nil {
		t.isDeclined = true
	}
}

// noteCallerFinal records an utterance's final transcript; isDeclined says the
// session declined it as an acknowledgement. A final that matches no utterance
// still waiting for one is an utterance of its own, already over; a second
// final for an utterance that has one changes nothing. Every final is first
// judged for a machine's wording (#409), so the call is marked before a wait
// for this final ends.
func (s *Session) noteCallerFinal(itemID, text string, isDeclined bool) {
	s.noteMachineWording(text)
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	t := s.ct.byID(itemID)
	if t != nil && t.isFinal {
		return
	}
	if t == nil {
		t = s.ct.oldest(func(t *callerTurn) bool { return !t.isFinal })
	}
	if t == nil {
		s.ct.seq++
		t = &callerTurn{seq: s.ct.seq, itemID: itemID, isStopped: true, isAnswerAsked: !isDeclined,
			done: make(chan struct{})}
		s.ct.add(t)
	}
	t.isFinal, t.text, t.finalAt = true, text, time.Now()
	t.isDeclined = t.isDeclined || isDeclined
	close(t.done)
}

// answeredCallerTurnLocked is the utterance a response starting now answers
// (see the top of this file), nil when the caller has said nothing it could.
// The caller holds mu.
func (s *Session) answeredCallerTurnLocked() *callerTurn {
	isHolding := s.isHolding()
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	for i := len(s.ct.list) - 1; i >= 0; i-- {
		t := s.ct.list[i]
		if t.isDeclined {
			continue
		}
		if (isHolding && t.isAnswerAsked) || (!isHolding && t.isStopped) {
			return t
		}
	}
	return nil
}

// callerTurnAnswered is the utterance the turn in progress answers
// (beginTurn's snapshot).
func (s *Session) callerTurnAnswered() *callerTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnAnswers
}

// callerFinal is what is known of an utterance's final transcript now, and
// whether it began over the unheard opening.
func (s *Session) callerFinal(t *callerTurn) (text string, isFinal bool, finalAt time.Time, isOpeningUnheard bool) {
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	return t.text, t.isFinal, t.finalAt, t.isOpeningUnheard
}

// awaitCallerFinal waits for an utterance's final transcript, at most
// leadFinalWait, and reports how long it waited. It does not wait when the
// final is in, or when a wait for it has already run out: the caller's words
// are then not coming in time for anything. A wait that runs out is recorded
// so; a session closing ends the wait.
func (s *Session) awaitCallerFinal(t *callerTurn) (waited time.Duration) {
	s.ct.mu.Lock()
	isSettled := t.isFinal || t.isWaitSpent
	s.ct.mu.Unlock()
	if isSettled {
		return 0
	}
	began := time.Now()
	select {
	case <-t.done:
	case <-s.finalWaitChan(leadFinalWait):
		s.ct.mu.Lock()
		t.isWaitSpent = true
		s.ct.mu.Unlock()
	case <-s.done:
	}
	return time.Since(began)
}

// spendFinalWaitAfterCut records that the cut the utterance's response
// answers was already waited for, cutVerdictWait, and no final came: the
// words are not coming in time, and the guard does not wait for them again.
func (s *Session) spendFinalWaitAfterCut(t *callerTurn, cut *openCut) {
	s.bc.mu.Lock()
	isOver := cut.isWaitOver
	s.bc.mu.Unlock()
	if !isOver {
		return
	}
	s.ct.mu.Lock()
	t.isWaitSpent = true
	s.ct.mu.Unlock()
}

// finalWaitChan is the final wait's clock: time.After, replaced by a test.
func (s *Session) finalWaitChan(d time.Duration) <-chan time.Time {
	if s.finalWait != nil {
		return s.finalWait(d)
	}
	return time.After(d)
}

// add appends an utterance and forgets the oldest past maxCallerTurns.
func (c *callerTurns) add(t *callerTurn) {
	c.list = append(c.list, t)
	if n := len(c.list); n > maxCallerTurns {
		c.list = append(c.list[:0], c.list[n-maxCallerTurns:]...)
	}
}

// byID is the latest utterance with the item id, nil for an empty id or none.
// An id the session knows is that utterance's and no other's.
func (c *callerTurns) byID(itemID string) *callerTurn {
	if itemID == "" {
		return nil
	}
	for i := len(c.list) - 1; i >= 0; i-- {
		if c.list[i].itemID == itemID {
			return c.list[i]
		}
	}
	return nil
}

// oldest is the oldest utterance an event with no usable id may apply to (ok):
// a wire with no ids, or ids that do not agree. Nil when none does.
func (c *callerTurns) oldest(ok func(*callerTurn) bool) *callerTurn {
	for _, t := range c.list {
		if ok(t) {
			return t
		}
	}
	return nil
}
