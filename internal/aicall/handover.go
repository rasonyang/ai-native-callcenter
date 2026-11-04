// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// A handover the switch did not confirm (#303).
//
// transfer_to_agent ends in a uuid_transfer of the caller's channel to the
// queue extension. When its reply does not come (esl.ErrTimeout) the switch
// may be stalled with the command on its socket, and runs it when it
// recovers. It used to be logged as a failed transfer and the bot leg closed
// at once; the switch then moved the caller a moment later, or the bot leg's
// end reached the dialplan first and the caller, whose teardown rule had just
// been put back, was hung up.
//
// So the transfer is pending, and the call waits for the switch to settle it:
//
//   - the bot leg ends (the switch hangs it up as it moves the caller, or the
//     caller hangs up): settled, as any transfer is;
//   - after handoverWait the switch is asked whether the caller's channel is
//     there. On one link the switch answers commands in the order they were
//     written, so an answer means the transfer has been read and run if it
//     was ever going to be, and a bot leg still up means it was not. The
//     handover failed, and the caller is told so (below). A caller who is
//     gone leaves nothing to settle; the bot leg is closed;
//   - a switch that still cannot answer is asked again, handoverChecks times
//     in all, and after that the bot leg is closed and the caller left to the
//     dialplan, since a switch that answers nothing cannot carry a
//     conversation either.
//
// While it waits the session is hushed: the caller heard that they are being
// put through, and the bot neither talks to them nor hears anything to answer.
// The ending is locked, so nothing the model or the platform does in the
// meantime arms another.
//
// A handover that never ran is a refusal, and a built-in's refusal is a
// conversation: the bot tells the caller it could not put them through and
// offers what it can still do (a message). The conversation goes on in the
// phase the transfer was made from, not in the one the transfer led to: that
// phase is terminal ("you are being put through, ask nothing more"), and in it
// no rule fires and no guard runs (the time limit, the closing target on
// silence, the turns-without-a-tool wall), so the call would have no way to
// end but the caller's. The settling goroutine must not touch the flow, which
// belongs to the call's own goroutine, so it hands the failure to drive
// (resumeAfterFailedHandover), which moves the flow back, re-pins the
// standing instructions and only then lets the conversation go on.
//
// The transfer is never sent again, and transfer_to_agent is refused for the
// rest of the call. Neither is the rescue a provider failure would otherwise
// send: the bot leg goes and the dialplan's fallback takes the caller, as for
// any bot that vanished (drive, EventTypeFailed). Sending it again
// would race the first if the switch ran it after all (it can, when the link
// was replaced while the command sat unread on the old one): the caller would
// be moved twice, and the second move pulls them out of the queue, or away
// from the agent who has just answered. A late first transfer during the
// resumed conversation only puts the caller where they asked to go.
//
// The platform's own handover at the time limit cannot be refused into a
// conversation, because the limit has already decided the call ends: one that
// never ran ends the call as the limit's other ending does.

const (
	// handoverWait is how long a handover the switch did not confirm is given
	// to settle by itself before the switch is asked. The caller hears silence
	// all this time, after the command's own wait: twenty seconds is long for
	// them and enough for a switch that has caught up to have moved them.
	handoverWait = 20 * time.Second
	// handoverChecks is how many times a switch that does not answer is
	// asked, handoverWait apart.
	handoverChecks = 3
)

// isUnconfirmed reports a command the switch did not answer: it was written,
// and may still run.
func isUnconfirmed(err error) bool { return errors.Is(err, esl.ErrTimeout) }

// failedHandover is a handover the switch never ran, on its way to drive.
type failedHandover struct {
	queue catalog.Queue
	// fromPhase is the phase transfer_to_agent was called in, where the
	// conversation resumes.
	fromPhase string
}

// awaitHandover holds the call while the switch settles a transfer it did not
// confirm. fromPhase is where the conversation resumes if it never ran.
func (a *callActions) awaitHandover(queue catalog.Queue, fromPhase string) {
	a.mu.Lock()
	a.isHandingOver = true
	wasLocked := a.isEndingLocked
	a.isEndingLocked = true
	a.mu.Unlock()
	a.session.hush(true)
	a.log.Warn("transfer sent and not confirmed: the switch may still run it; "+
		"holding the call silent until it settles",
		"queue", queue.Name, "ext", queue.ExtNumber, "waitSec", int(handoverWait.Seconds()))
	go a.settleHandover(failedHandover{queue: queue, fromPhase: fromPhase}, wasLocked)
}

// settleHandover waits for the switch to settle a handover (above).
func (a *callActions) settleHandover(handover failedHandover, wasLocked bool) {
	queue := handover.queue
wait:
	for check := 1; ; check++ {
		select {
		case <-a.session.done:
			a.log.Info("transfer settled: the bot leg ended", "queue", queue.Name)
			return
		case <-a.handoverTimer(handoverWait):
		}
		isUp, err := a.orchestrator.cfg.Switch.ChannelExists(a.callerChannel)
		switch {
		case err != nil && check < handoverChecks:
			a.log.Warn("transfer still unconfirmed: the switch cannot say where the caller is; asking again",
				"queue", queue.Name, "check", check, "error", err)
			continue
		case err != nil:
			a.log.Warn("transfer never confirmed: the switch still answers nothing; "+
				"closing the bot leg and leaving the caller to the dialplan",
				"queue", queue.Name, "checks", check, "error", err)
			a.session.Close(context.Background())
			return
		case !isUp:
			a.log.Info("transfer settled: the caller has gone", "queue", queue.Name)
			a.session.Close(context.Background())
			return
		}
		break wait
	}
	if a.session.isClosed() {
		return
	}
	a.handoverFailed(handover, wasLocked)
}

// handoverFailed goes on with a call whose transfer the switch never ran. It
// runs on the settling goroutine: what it does here is the switch's and the
// ledger's, and the conversation is handed to drive.
func (a *callActions) handoverFailed(handover failedHandover, wasLocked bool) {
	queue := handover.queue
	a.log.Warn("transfer never ran: the caller is still on the bot leg; telling them",
		"queue", queue.Name, "isAtTimeLimit", wasLocked, "resumeIn", handover.fromPhase)
	// The caller is still the bot's: the teardown rule goes back to what the
	// bridge to the bot set. An inbound call's dialplan turns it off, so a
	// bot leg that dies from here on sends them to the fallback rather than
	// hanging them up. A call the platform placed to a customer bridges with
	// it on, so a bot that ends normally ends the call and only a bridge that
	// never came up reaches the inline continuation (telephony.BridgeToBot):
	// "false" would run that continuation after a bot's ordinary hangup.
	rule := "false"
	if a.facts != nil && a.facts.isPlacedToCustomer {
		rule = "true"
	}
	if err := a.orchestrator.cfg.Switch.SetVariable(a.callerChannel, "hangup_after_bridge", rule); err != nil {
		a.log.Warn("could not restore the caller's teardown rule", "error", err)
	}
	if a.recorder != nil {
		a.recorder.markHandoverFailed()
	}
	a.mu.Lock()
	a.isTransferFailed = true
	a.mu.Unlock()

	if wasLocked {
		// The time limit chose this transfer and the call is over either way.
		// The flow is not touched: the session closes and drive returns.
		a.mu.Lock()
		a.isHandingOver = false
		a.mu.Unlock()
		a.markFinished(hangupCauseSessionLimit)
		a.session.Close(context.Background())
		return
	}
	// The call stays hushed, with its ending locked, until drive has moved
	// the flow back: until then it is in the terminal phase, where a silence
	// or a tool would be judged as in a call that is ending.
	select {
	case a.handoversFailed() <- handover:
	default:
		// Only one handover per call can fail: the next transfer_to_agent
		// is refused before anything is sent.
		a.log.Error("a second failed handover on one call; dropped", "queue", queue.Name)
	}
}

// handoversFailed is the channel a handover that never ran reaches drive on.
// One slot is enough: a call has at most one (above).
func (a *callActions) handoversFailed() chan failedHandover {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failedHandovers == nil {
		a.failedHandovers = make(chan failedHandover, 1)
	}
	return a.failedHandovers
}

// resumeAfterFailedHandover goes on with the conversation of a call whose
// transfer the switch never ran, on the call's own goroutine (drive). In
// order: the flow goes back to the phase the transfer was made from, the
// standing instructions are re-pinned to it (instructions only, which every
// engine takes mid-call), and only then is the call let go and the model told,
// so the turn the cue asks for runs under that phase's instruction and not
// under "the caller is connected right after this line".
func (o *Orchestrator) resumeAfterFailedHandover(failed failedHandover, session *Session,
	runtime *flow.Runtime, actions *callActions, log *slog.Logger) {

	if session.isClosed() {
		return
	}
	if !runtime.ReturnTo(failed.fromPhase) {
		// Only a transfer the runtime did not make has no phase to go back
		// to, and the one such (the time limit's) never comes here.
		log.Warn("transfer never ran and the phase it was made from cannot be resumed; "+
			"the conversation goes on where it is", "phase", failed.fromPhase,
			"node", runtime.Engine().NodeID())
	}
	if err := session.Reinstruct(runtime.Instructions()); err != nil {
		log.Warn("could not update instructions", "error", err)
	}
	actions.mu.Lock()
	actions.isHandingOver = false
	actions.isEndingLocked = false
	actions.mu.Unlock()
	session.hush(false)
	log.Info("transfer never ran; the conversation goes on",
		"queue", failed.queue.Name, "node", runtime.Engine().NodeID())
	if err := session.SendCue(actions.handoverFailedCue()); err != nil {
		log.Warn("could not tell the model the transfer failed; ending the call", "error", err)
		actions.markFinished("TRANSFER_FAILED")
		session.Close(context.Background())
	}
}

// hasTransferFailed reports whether a transfer on this call never ran.
func (a *callActions) hasTransferFailed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isTransferFailed
}

// handoverTimer is the wait between checks; tests replace it.
func (a *callActions) handoverTimer(d time.Duration) <-chan time.Time {
	if a.handoverAfter != nil {
		return a.handoverAfter(d)
	}
	return time.After(d)
}

// isSettlingHandover reports whether a handover the switch did not confirm is
// being settled.
func (a *callActions) isSettlingHandover() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isHandingOver
}

// transferFailed is what transfer_to_agent answers once a handover on this
// call never ran: a second one is never sent (above).
func (a *callActions) transferFailed() (flow.Result, bool) {
	a.mu.Lock()
	isFailed := a.isTransferFailed
	a.mu.Unlock()
	if !isFailed {
		return flow.Result{}, false
	}
	return flow.Failed("TRANSFER_FAILED", a.refusalHint()), true
}

// handoverFailedCue tells the model the caller was not put through, in the
// call's language.
func (a *callActions) handoverFailedCue() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "（转接人工没有成功，来电者仍在线。请向来电者道歉，说明现在无法转接，" +
			"并主动提出为他们留言。不要再尝试转接。）"
	}
	return "(The transfer to a person did not go through and the caller is still on the line. " +
		"Apologise, explain that you cannot put them through right now, and offer to take a " +
		"message instead. Do not try to transfer them again.)"
}
