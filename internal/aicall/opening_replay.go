// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// This file is the session's half of the greeting rules in
// docs/design/11-backchannels.md: what happens when the first thing a callee
// says, "喂" or "hello?", lands on the bot's opening line.
//
// On an engine that cuts by itself the opening is cut and the model answers the
// greeting. The opening is the line the flow wrote for the call's start, so it
// is said again once, from its start and word for word, in place of that
// answer. Words that cut it and that no transcript made known (a blank final,
// or none in time) are treated the same way (#610): the callee has not heard
// the opening, and nothing says they answered it. On an engine whose client
// holds speech over audible playback (backchannel_hold.go) the greeting never
// cuts it.

// openingReplayMute is the name MuteTurn is given for the reply it mutes.
const openingReplayMute = "opening replay"

// replayWaitCap is how long a replay waits for the model's own reply to the
// greeting to start, before saying the line anyway. A line requested while that
// reply is about to be created is taken for it (Realtime.onResponseCreated), so
// the line goes out after the reply has begun and is muted; the cap is for a
// reply that never comes.
const replayWaitCap = 1000 * time.Millisecond

// isOpeningTurn reports whether a turn is the call's opening line or its
// replay. The opening is turn 1 when the call has one: it is asked for as the
// session starts, before the caller is heard.
func (s *Session) isOpeningTurn(turn int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isOpeningTurnLocked(turn)
}

func (s *Session) isOpeningTurnLocked(turn int) bool {
	if turn == 0 {
		return false
	}
	return (turn == 1 && s.cfg.Session.OpeningText != "") || turn == s.replayTurn
}

// isOpeningLive reports whether the opening line (or its replay) is the turn in
// progress and is being made or heard: the moment a greeting lands on it.
func (s *Session) isOpeningLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isOpeningTurnLocked(s.turnSeq) {
		return false
	}
	return s.isResponding || s.isBotSpeaking || (s.framesQueued > 0 && s.leg.Pending() > 0)
}

// noteUnqueuedOpeningCut opens a cut for the opening when the provider cut it
// before any of its audio was queued, which the interrupted path does not do
// (it opens one only when something was queued). A cut the session's own
// barge-in already recorded for the turn is left as it is: one cut, once.
func (s *Session) noteUnqueuedOpeningCut(turn int) {
	if s.isHolding() {
		return
	}
	s.bc.mu.Lock()
	isOwed := s.bc.isOpeningOwed
	s.bc.mu.Unlock()
	if !isOwed && !s.isOpeningTurn(turn) {
		return
	}
	s.bc.mu.Lock()
	if cut := s.bc.cut; cut != nil && cut.event.Turn == turn {
		s.bc.mu.Unlock()
		return
	}
	s.bc.mu.Unlock()
	s.openCut(Event{Type: EventTypeBargeIn, Text: string(provider.InterruptReasonSpeech), Turn: turn,
		Played: -1, IsGenerating: true, IsByProvider: true})
}

// SayOpeningAgain says the call's opening line once more, from its start, in
// place of the model's own reply to a greeting that cut it, or to words that
// cut it and that no transcript made known (#610). It is for the
// orchestrator, which has judged that the flow is still in the entry phase and
// no ending is under way. It reports whether the line is on its way: false
// when the call has no opening or has already said it again (once per call).
//
// Like any line it pre-empts and never queues. The model's reply to the cut is
// muted when it has started, so the caller does not hear it; one
// that has not started yet is waited for (replayWaitCap), because the line
// would be taken for it.
func (s *Session) SayOpeningAgain(cut *openCut) bool {
	text := s.cfg.Session.OpeningText
	s.mu.Lock()
	if text == "" || s.isOpeningReplayed {
		s.mu.Unlock()
		return false
	}
	s.isOpeningReplayed = true
	s.clearOpeningOwed()
	// Read with mu held: beginTurn, which sets the cut's answer turn, holds it.
	s.bc.mu.Lock()
	answerTurn, verdict := 0, cutVerdictUnknown
	if cut != nil {
		answerTurn, verdict = cut.answerTurn, cut.verdict
		cut.isReplayAsked = true
	}
	s.bc.mu.Unlock()
	if answerTurn == 0 {
		s.isReplayWaiting = true
		s.mu.Unlock()
		s.log.Info("the opening was cut and is owed; saying it again once the model's reply to the cut starts",
			"verdict", verdict.String())
		stop := s.holdTimer(replayWaitCap, s.onReplayWaitOver)
		s.mu.Lock()
		if s.isReplayWaiting {
			s.replayStop = stop
			s.mu.Unlock()
			return true
		}
		s.mu.Unlock()
		stop()
		return true
	}
	isLive := s.turnSeq == answerTurn &&
		(s.isResponding || (s.framesQueued > 0 && s.leg.Pending() > 0))
	s.mu.Unlock()
	s.log.Info("the opening was cut and is owed; saying it again", "verdict", verdict.String(),
		"replyTurn", answerTurn, "isReplyLive", isLive)
	if isLive {
		s.MuteTurn(answerTurn, openingReplayMute)
	}
	s.sayOpening(text)
	return true
}

// isOpeningSaidAgain reports whether the opening has been asked for again
// (once per call).
func (s *Session) isOpeningSaidAgain() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isOpeningReplayed
}

// isOpeningReplayable reports whether, as far as the session can tell, the
// opening could still be said again: it has not been, and no ending is armed
// (Config.IsEndingArmed). The flow's phase and an ending decided otherwise are
// the orchestrator's to judge (canSayOpeningAgain). Called with no lock held:
// the ending's own lock is taken before the session's elsewhere.
func (s *Session) isOpeningReplayable() bool {
	if s.isOpeningSaidAgain() {
		return false
	}
	return s.cfg.IsEndingArmed == nil || !s.cfg.IsEndingArmed()
}

// onReplyStartedForReplay is called as a turn starts. A replay waiting for the
// model's reply to the greeting mutes it and says the opening.
func (s *Session) onReplyStartedForReplay() {
	s.mu.Lock()
	if !s.isReplayWaiting {
		s.mu.Unlock()
		return
	}
	s.isReplayWaiting = false
	stop := s.replayStop
	s.replayStop = nil
	turn := s.turnSeq
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.log.Info("the model's reply to the cut of the opening started; muting it and saying the opening",
		"replyTurn", turn)
	s.MuteTurn(turn, openingReplayMute)
	s.sayOpening(s.cfg.Session.OpeningText)
}

// onReplayWaitOver says the opening when the model's reply to the greeting did
// not start in time: the floor is free.
func (s *Session) onReplayWaitOver() {
	s.mu.Lock()
	if !s.isReplayWaiting {
		s.mu.Unlock()
		return
	}
	s.isReplayWaiting = false
	s.replayStop = nil
	s.mu.Unlock()
	if s.isClosed() {
		return
	}
	s.log.Info("no reply to the cut of the opening started in time; saying the opening")
	s.sayOpening(s.cfg.Session.OpeningText)
}

// sayOpening sends the opening line as a line, and marks the next spoken-line
// turn as its replay.
func (s *Session) sayOpening(text string) {
	s.mu.Lock()
	s.isReplayOwed = true
	s.mu.Unlock()
	if err := s.Speak(text, false); err != nil {
		s.mu.Lock()
		s.isReplayOwed = false
		s.mu.Unlock()
		s.log.Warn("could not say the opening again", "error", err)
	}
}

// clearOpeningOwed says the opening is no longer owed: it has been asked for
// again, the caller answered with words, a later turn was heard in full, or the
// call left the entry phase or is ending.
func (s *Session) clearOpeningOwed() {
	s.bc.mu.Lock()
	s.bc.isOpeningOwed = false
	s.bc.mu.Unlock()
}

// noteOpeningHeard is told that a turn finished playing to the caller in full.
// A turn after the cut that made the opening owed, with audio, not muted,
// clears it: the caller has been answered since.
func (s *Session) noteOpeningHeard(turn int, hasAudio bool) {
	if !hasAudio || s.isMutedTurn(turn) {
		return
	}
	s.bc.mu.Lock()
	if s.bc.isOpeningOwed && turn > s.bc.owedAfterTurn {
		s.bc.isOpeningOwed = false
	}
	s.bc.mu.Unlock()
}
