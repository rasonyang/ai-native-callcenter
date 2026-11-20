// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"log/slog"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// sessionBudget is how long one AI call may run (flow global.maxDurationSec,
// bounded by the deployment's AICC_BOT_MAX_CALL_DURATION; 02 §7, #637): a
// wrap-up steer once most of it is used, then the platform ends the call
// itself. Without it a call that loops, or a caller who simply keeps talking,
// runs until the provider cuts the session — at the provider's duration and
// the provider's cost, and with the caller rescued mid-sentence.
//
// Every call has one: budgetFor never returns the zero value, which is no
// budget and is left to tests that drive a call without a clock.
type sessionBudget struct {
	// wrapUpAfter and limitAfter are measured from now; zero is never.
	wrapUpAfter time.Duration
	limitAfter  time.Duration
	// floorWait is how long a limit that came due while the bot held the
	// floor waits for it to be free before acting anyway.
	floorWait time.Duration
}

// wrapUpShare is the part of the budget used before the model is told to wrap
// up (02 §7: at 80%).
const wrapUpShare = 0.8

// budgetFor is a flow's time budget under the deployment's cap, measured from
// the bot answering. There is no off: a flow that sets 0, or more than the
// cap, runs at the cap (flow.Global.EffectiveMaxDuration).
func budgetFor(spec *flow.Spec, deploymentCap time.Duration) sessionBudget {
	limit := spec.Global.EffectiveMaxDuration(deploymentCap)
	return sessionBudget{
		wrapUpAfter: time.Duration(float64(limit) * wrapUpShare),
		limitAfter:  limit,
		floorWait:   actionGraceCap,
	}
}

// since shifts the budget to start at answeredAt rather than now, so it is the
// same clock as the CDR's botSec. Whatever has already come due fires at once,
// and never counts as off.
func (b sessionBudget) since(answeredAt, now time.Time) sessionBudget {
	if b.limitAfter <= 0 {
		return b
	}
	elapsed := max(now.Sub(answeredAt), 0)
	b.wrapUpAfter = max(b.wrapUpAfter-elapsed, time.Nanosecond)
	b.limitAfter = max(b.limitAfter-elapsed, time.Nanosecond)
	return b
}

// handleWrapUpDue tells the model the call is close to its time limit.
//
// Through its standing instructions, not a turn of its own: the moment is
// arbitrary, and a cue asking for a turn would collide with one in progress
// (refused on the Realtime providers and refused outright on doubao). The
// instructions carry it from here on, so a later phase change keeps it.
func (o *Orchestrator) handleWrapUpDue(ctx context.Context, session *Session, runtime *flow.Runtime,
	actions *callActions, deadline time.Time, log *slog.Logger) {

	if actions.isArmed() || runtime.Engine().IsTerminal() {
		log.Info("call nearing its time limit, but it is already ending")
		return
	}
	// The steer offers a person only when the limit can deliver one: the same
	// queue check the limit itself makes.
	_, isTransferOpen := actions.openFallbackQueue(ctx)
	runtime.BeginWrapUp(deadline, isTransferOpen)
	if err := session.Reinstruct(runtime.Instructions()); err != nil {
		log.Warn("could not ask the model to wrap up", "error", err)
		return
	}
	log.Info("call nearing its time limit; asked the model to wrap up",
		"remaining", time.Until(deadline), "isTransferOpen", isTransferOpen)
}

// limitEnforcement is the moment the limit is acted on, which decides whether
// a line can still be asked for.
type limitEnforcement int

const (
	// limitFloorFree: nobody is speaking; a line can be asked for.
	limitFloorFree limitEnforcement = iota
	// limitCallerSilent: the caller has gone quiet; the goodbye is said as
	// to someone who may have gone.
	limitCallerSilent
	// limitForced: the bot never let go of the floor; nothing more is asked
	// of it before a transfer.
	limitForced
)

// enforceSessionLimit ends a call that has reached its time limit (02 §7).
//
// To a person when the number's queue is open: the transfer is armed with the
// SESSION_LIMIT reason and a platform summary, and the model is asked to say
// it is putting the caller through. Otherwise the flow's own goodbye
// (global.closingTarget), exactly as the turns-without-a-tool wall reaches it,
// or a plain goodbye when the flow has none; a bot that kept the floor past
// the backstop is not asked for one, and the hangup runs at once. Either way
// the ending is then locked, so the model cannot replace it with one of its
// own.
//
// It never overrides an ending already armed or a phase that already ends the
// call: those are bounded by the grace cap and say more than the limit does.
func (o *Orchestrator) enforceSessionLimit(ctx context.Context, session *Session,
	runtime *flow.Runtime, actions *callActions, recorder *callRecorder,
	how limitEnforcement, log *slog.Logger) {

	if session.isClosed() || actions.isArmed() || actions.isEndingDecided() ||
		runtime.Engine().IsTerminal() {
		log.Info("call reached its time limit, but it is already ending")
		return
	}
	lang := runtime.Engine().Lang()
	if recorder != nil {
		recorder.markSessionLimit()
	}

	if queue, ok := actions.openFallbackQueue(ctx); ok {
		result, err := actions.TransferToAgent(ctx, flow.TransferRequest{
			Queue:     queue.Name,
			Reason:    hangupCauseSessionLimit,
			Summary:   sessionLimitSummary(lang),
			FromPhase: runtime.Engine().NodeID(),
		})
		if err == nil && result.IsOK {
			log.Warn("call reached its time limit; transferring the caller",
				"queue", queue.Name)
			actions.lockEnding()
			obs.RecordSessionLimit(obs.SessionLimitOutcomeTransfer)
			o.askForTheHandover(session, actions, lang, how, log)
			return
		}
		log.Warn("call reached its time limit and the transfer was refused; closing it",
			"queue", queue.Name, "error", err, "result", result.Error)
	}

	log.Warn("call reached its time limit; closing it")
	if moved := runtime.CloseAtSessionLimit(); moved != "" {
		if !o.afterMove(moved, session, runtime, actions, log) {
			o.askForTheGoodbye(session, actions, lang, how, log)
		}
	} else {
		if recorder != nil {
			recorder.markHangup()
		}
		actions.arm(ctx, func() {
			actions.markFinished(hangupCauseSessionLimit)
			session.Close(context.Background())
		})
		o.askForTheGoodbye(session, actions, lang, how, log)
	}
	actions.lockEnding()
	obs.RecordSessionLimit(obs.SessionLimitOutcomeHangup)
}

// askForTheHandover asks the model to tell the caller they are being put
// through, before the armed transfer runs. A client that cannot be asked
// (doubao takes no text cue), or a bot that never gave up the floor, gets the
// transfer at once: ten seconds of silence on the grace cap serves nobody.
func (o *Orchestrator) askForTheHandover(session *Session, actions *callActions,
	lang string, how limitEnforcement, log *slog.Logger) {

	if how == limitForced || o.cfg.Profile.RequiresTerminalAnnounce {
		actions.runArmedNow()
		return
	}
	if err := session.SendCue(handoverCue(lang)); err != nil {
		log.Warn("could not ask for the handover line; transferring now", "error", err)
		actions.runArmedNow()
	}
}

// askForTheGoodbye asks the model for the goodbye of a call the limit closed
// with no line of its own. A client that cannot be asked ends it at once, and
// so does a bot that never gave up the floor: the cue asks for a turn the
// provider refuses, so the armed hangup runs now. A closing phase's announce is not this: a line pre-empts.
//
// Doubao is the same: it takes no text cue, so a flow with no closing phase
// (global.closingTarget) is hung up with no line at the limit. Doubao flows
// should set one; docs/provider-extension.md.
func (o *Orchestrator) askForTheGoodbye(session *Session, actions *callActions,
	lang string, how limitEnforcement, log *slog.Logger) {

	if o.cfg.Profile.RequiresTerminalAnnounce {
		log.Info("this provider takes no goodbye cue and the flow has no closing phase; ending now")
		actions.runArmedNow()
		return
	}
	if how == limitForced {
		actions.runArmedNow()
		return
	}
	isCallerSilent := how == limitCallerSilent
	if err := session.SendCue(goodbyeCue(lang, isCallerSilent)); err != nil {
		log.Warn("could not ask for the goodbye; ending now", "error", err)
		actions.runArmedNow()
	}
}

// sessionLimitSummary is the transfer summary the agent reads when the
// platform, not the model, handed the call over. It must not be empty: the
// reason reaches the human path's CDR only beside a summary.
func sessionLimitSummary(lang string) string {
	if lang == flow.LangZH {
		return "通话已达到时长上限，由系统转接人工。"
	}
	return "The call reached its time limit; the platform transferred it."
}

// handoverCue asks the model to tell the caller they are being put through. It
// gives no reason: the time limit is the platform's business, not the
// caller's.
func handoverCue(lang string) string {
	if lang == flow.LangZH {
		return "（现在需要把来电转给人工同事。请简短告诉来电者正在为其转接人工，不要调用任何工具。）"
	}
	return "(It is time to hand this call to a colleague. Tell the caller briefly " +
		"that you are putting them through to a person now. Do not call any tool.)"
}
