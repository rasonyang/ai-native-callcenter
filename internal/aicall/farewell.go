// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"log/slog"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// The model's own goodbye (#453). A model that closes the conversation in words
// and never calls hangup used to be answered twice: the dead-air backstop
// waited for the full silence timer and then said the flow's closing line on
// top of it, and a caller who answered the goodbye re-opened a conversation the
// model could only close again, in words. Two rules end such a call, both
// without the closing target's announce (the model's goodbye was heard; a
// second one is the defect):
//
//   - Shape A. An unhung FAREWELL that played in full and was not talked over,
//     followed by ownFarewellGrace of caller silence, is the call's goodbye.
//   - Shape C. A second unhung FAREWELL in a row that played in full ends the
//     call as soon as its playback does, whatever the caller does next.
//
// The 8 s dead-air backstop (handleDeadAir) stays as the fallback for what the
// grace did not catch, and closes the same way.
//
// An armed ending, a handover settling, a time limit waiting for the floor and
// a promise the bot still owes (a standing lookup, transfer or message claim)
// keep their own behaviour: none of these rules acts on them.

// ownFarewellGrace is how long the caller must stay quiet after the bot's own
// goodbye finished playing for the call to end. Long enough for a caller to
// answer a goodbye ("bye", "okay"), short enough that the goodbye is not
// followed by an unexplained silence.
const ownFarewellGrace = 2 * time.Second

// farewellWatch follows the bot's goodbyes on the call's own goroutine, next
// to claimWatch, which knows what each turn said and whether it played.
type farewellWatch struct {
	// streak is the unhung goodbyes played in full in a row. A tool call, or a
	// finished bot turn that is no goodbye, resets it; a goodbye the caller
	// talked over neither counts nor resets.
	streak int
	// countedTurn is the goodbye already counted, so the two halves that
	// decide it (its playback and its transcript, in either order) count it
	// once.
	countedTurn int
	// undecided is the latest finished turn not known to be a goodbye: its
	// transcript may still land. The next finished turn settles it as a turn
	// that was none, and that resets the streak.
	undecided int
	// isCallerHeard is set by the caller's speech, a keypress or a transcript
	// since the last playback ended, and read only when a late transcript
	// decides a goodbye (the speech then already cancelled the grace it would
	// have started).
	isCallerHeard bool
	// due is the running grace; nil when none.
	due <-chan time.Time
	// graceTurn is the goodbye the running grace was started for.
	graceTurn int
}

// onTurnDone accounts for a finished, non-aside turn. isFarewellUnhung is the
// claim watch's verdict so far; a transcript still to come settles it later
// (onBotSaid).
func (w *farewellWatch) onTurnDone(turn int, isInterrupted, isFarewellUnhung bool) {
	// A new bot turn ends any grace: the conversation did not stop there.
	w.due = nil
	w.isCallerHeard = false
	if w.undecided != 0 {
		w.streak = 0
	}
	w.undecided = 0
	if isInterrupted || isFarewellUnhung {
		return
	}
	w.undecided = turn
}

// onBotSaid settles a turn whose transcript landed after the turn finished.
// isFarewell is whether the words were a goodbye, not whether the call may be
// left on them: a goodbye the caller cut still neither counts nor resets the
// streak.
func (w *farewellWatch) onBotSaid(turn int, isFarewell bool) {
	if isFarewell && w.undecided == turn {
		w.undecided = 0
	}
}

// onToolCall takes everything back: the model acts, so it did not close the
// conversation in words.
func (w *farewellWatch) onToolCall() {
	w.streak, w.undecided, w.due = 0, 0, nil
}

// onCallerSpoke cancels the grace. It is fed by the provider's speech start
// (EventTypeCallerSpeech), a keypress and a final transcript, so a caller who
// has begun a sentence is never hung up on while the words are still being
// transcribed. The conversation goes on from there as it would have.
func (w *farewellWatch) onCallerSpoke() {
	w.due = nil
	w.isCallerHeard = true
}

// onAudioPlayed notes that a turn's playback ended: what the caller says from
// here is an answer to it.
func (w *farewellWatch) onAudioPlayed() { w.isCallerHeard = false }

// checkOwnFarewell decides, after a goodbye's playback or its late transcript,
// whether the call ends now (shape C), after the grace (shape A), or goes on.
// It reports whether the call was ended. isOwed says the bot still owes an
// action it promised in words; isLimitWaiting that a time limit waits for the
// floor.
//
// The turns-without-a-tool wall and the re-ask run before this at a
// PLAYBACK_DONE (drive): the wall closing first arms an ending, which this then
// leaves alone, so the two never act twice on one turn.
func (o *Orchestrator) checkOwnFarewell(fw *farewellWatch, claims *claimWatch, isOwed, isLimitWaiting bool,
	session *Session, runtime *flow.Runtime, actions *callActions, log *slog.Logger) bool {

	turn := claims.farewellPlayedInFull()
	if turn == 0 || turn == fw.countedTurn {
		return false
	}
	fw.countedTurn = turn
	fw.streak++
	if actions.isArmed() || actions.isSettlingHandover() || isLimitWaiting || isOwed {
		fw.due = nil
		return false
	}
	if fw.streak >= 2 {
		o.endAtOwnFarewell("the bot said goodbye twice without hanging up; hanging up after the second one",
			"TWO_GOODBYES", turn, session, runtime, actions, log)
		return true
	}
	if fw.isCallerHeard {
		return false
	}
	fw.graceTurn = turn
	fw.due = o.after(ownFarewellGrace)
	return false
}

// onOwnFarewellGraceDue ends the call when the grace ran out and nothing has
// changed since it started. It reports whether it did.
func (o *Orchestrator) onOwnFarewellGraceDue(fw *farewellWatch, claims *claimWatch, isLimitWaiting bool,
	session *Session, runtime *flow.Runtime, actions *callActions, log *slog.Logger) bool {

	fw.due = nil
	if claims.farewellPlayedInFull() != fw.graceTurn || actions.isArmed() ||
		actions.isSettlingHandover() || isLimitWaiting {
		return false
	}
	o.endAtOwnFarewell("the bot's own goodbye played in full and the caller stayed quiet; "+
		"hanging up without the closing line", "GRACE", fw.graceTurn, session, runtime, actions, log)
	return true
}

// endAtOwnFarewell hangs up on the strength of the model's own goodbye. The
// engine still moves to global.closingTarget, so the flow's last node and the
// record are what they are after any engine-decided ending (containment,
// FLOW_END), but the target's announce is not said and no goodbye is asked
// for: the caller has just heard one. A flow with no closing target, or one
// already in a terminal phase, hangs up all the same.
func (o *Orchestrator) endAtOwnFarewell(msg, rule string, turn int, session *Session,
	runtime *flow.Runtime, actions *callActions, log *slog.Logger) {

	closing := runtime.CloseAtUnhungFarewell()
	finished := "HANGUP"
	if closing != "" {
		finished = "FLOW_END"
	}
	log.Info(msg, "rule", rule, "turn", turn, "node", runtime.Engine().NodeID(),
		"closingTarget", closing)
	if actions.recorder != nil {
		actions.recorder.markHangup()
	}
	actions.markFinished(finished)
	session.Close(context.Background())
}
