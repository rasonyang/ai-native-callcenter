// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// callActions is what one call's flow tools actually do.
//
// The invariant all of them share: an action that ends or moves the call never
// runs while the bot is mid-sentence. The tool returns immediately — the model
// needs the result to speak its closing line — and the action itself is armed,
// to run when that line has finished playing, with a cap so a stalled provider
// cannot hold the caller hostage.
type callActions struct {
	orchestrator *Orchestrator
	session      *Session
	log          *slog.Logger

	// callerChannel is the caller's leg on the switch — the one that survives
	// this conversation and goes on to a queue.
	callerChannel string
	fallbackQueue *uuid.UUID
	// campaignID and attemptID come from X-AICC-Campaign-ID / X-AICC-Attempt-ID
	// on the bot leg; a non-empty campaignID marks a campaign call, which has no
	// way to a person (isCampaign). Zero on every other call.
	campaignID string
	attemptID  string
	// recorder and facts feed the ledger; both may be nil in tests.
	recorder *callRecorder
	facts    *callFacts

	// graceCap overrides actionGraceCap; zero keeps it. Tests only.
	graceCap time.Duration

	mu    sync.Mutex
	armed func()
	// isArmedTransfer says the armed ending is a transfer to an agent.
	isArmedTransfer bool
	// armedInTurn is the model turn in progress when the action was armed —
	// the turn the arming tool call arrived in. A tool arms inside its own
	// dispatch, and a terminal phase whose line the tool result carries arms
	// before the result is sent, so in both cases the turn the answer produces
	// is a later one. The
	// action waits for the playback of a LATER turn: the closing line is spoken
	// in the turn created by the tool's own result (or, on a client that speaks
	// text outright, in the line's own turn after it), and the turn that
	// carried the tool call may finish playing after arming — acting on that
	// one would cut in before the line is spoken.
	armedInTurn int
	// isLineSpoken records that a later turn has finished generating — the
	// closing line exists and is playing out or already played. Only a turn
	// that ran to completion counts: a turn cut short before its words existed
	// (a tool-result turn stopped to make room for a line, or one the caller
	// talked over) is not the line, and counting it let a barge-in fire the
	// action while the real line was still being generated. A caller cutting
	// in early on the line resets it (onBargeIn). While it is clear no barge-in
	// runs the action; a cut of a later turn still being generated is counted
	// as an interruption, but never dated inside the line (#493).
	isLineSpoken bool
	// isInterrupted says the caller cut in on the armed closing line with more
	// than closingTailAck of it unplayed, or while it was still being generated
	// (onBargeIn). From then on the ending
	// waits for a turn that plays audio (onPlaybackDone), and the model's first
	// bare hangup is refused (Hangup).
	isInterrupted bool
	// interruptions counts the cuts over the armed ending (onBargeIn): over the
	// line, over a re-said line, over an answer turn. A turn that finished
	// generating counts when more than closingTailAck of the audio was still
	// unplayed; a later turn cut while still being generated counts whenever
	// something audible was cut (#493). The session's barge-in and the
	// provider's own cancellation are counted alike, one cut once. The first is
	// handled as an interruption always was; from the second on the line counts
	// as said and the cap is frozen (startCapLocked), so a caller who keeps
	// talking over a model that keeps saying the line again cannot stretch the
	// call (#465). Reset by armAs.
	interruptions int
	// isLineSaidByRule says the closing line counts as said by a rule and not by
	// having played: the cut fell in the final sentence of a line that had
	// finished generating (rule A) or this is the second interruption or later,
	// whichever side reported it and whether or not the line had finished
	// generating (rule D), both set in onBargeIn. Only then does a hangup from
	// the model over a speaking answer end the call on that answer
	// (sayGoodbyeOnHangup). A first interruption mid-line, or any first cut of a
	// line still being generated (#493), leaves it clear: the line is still owed
	// and the answer may take two turns (#147, #262). Reset by armAs.
	isLineSaidByRule bool
	// goodbyeTurn is the model turn that spoke the goodbye and called hangup
	// while the line counted as said (sayGoodbyeOnHangup); 0 is none. The
	// ending runs on that turn's playback, also through the playback of a later
	// turn with no audio of its own (onPlaybackDone): the turn the hangup's
	// result brings is muted, and its playback ends when the goodbye's queued
	// audio has drained. A further interruption clears it (onBargeIn). Reset by
	// armAs.
	goodbyeTurn int
	// audioTurnsAtInterrupt is Session.audioProgress's finished-turn count when
	// the interruption was seen; a larger count later means the model has
	// answered aloud since.
	audioTurnsAtInterrupt int
	// isHangupRefused says the one refusal has been spent; a second hangup
	// succeeds.
	isHangupRefused bool
	// isLineOwed says the armed ending is a hangup into a terminal phase that
	// has a closing line of its own, and no uninterrupted turn with audio has
	// finished since: the caller has not heard the line (expectClosingLine,
	// #207). While it holds, a turn with no audio is not the line: it does not
	// run the ending (onPlaybackDone) and does not count as the line spoken
	// (onTurnDone). Nor is a turn whose words do not say the line (#262): it is
	// answered by asking for the line once more. It is cleared when the model insists on hangup after its
	// one refusal, and by lockEnding.
	isLineOwed bool
	// closingLine is the rendered line the caller is owed while isLineOwed
	// holds, in the language the flow wrote it in (expectClosingLine). The
	// words of the turns after the arming one are compared with it (onTurnDone,
	// #262).
	closingLine string
	// said holds the words the model has spoken, by turn, in the turns after
	// the arming one while the line is owed (onBotSaid). A turn's entry is
	// consumed when the turn finishes.
	said map[int]string
	// isLineAskedAgain says the one transcript-driven request for the line has
	// been spent (onTurnDone): a turn after the arming one did not say it.
	isLineAskedAgain bool
	// notLineThrough is the latest turn judged not to be the line. The ending
	// does not run on the playback of that turn or any before it
	// (onPlaybackDone): the caller has heard something else, and the line is
	// still to come. 0 is none.
	notLineThrough int
	// isLineRefused says the one refusal of a hangup made while the line is
	// owed has been spent (refuseHangupWhileLineOwed).
	isLineRefused bool
	// armedGeneration lets a cap timer recognise the action it belongs to, and
	// retires the timer when the cap is moved.
	armedGeneration int
	// capDue is when the running cap fires.
	capDue time.Time
	// isEndingLocked says the call's ending has been decided by the platform
	// (the flow's time limit) and nothing the model does may replace it: arm
	// keeps what is armed, and the ending tools report success without acting.
	// Unlike an armed action it outlives the running of that action, so a
	// model answering the handover cue with hangup after the transfer has run
	// cannot arm a second ending.
	isEndingLocked bool
	// isHandingOver says a transfer the switch did not confirm is being
	// settled (handover.go): the session is hushed and the ending locked.
	isHandingOver bool
	// isTransferFailed says a transfer on this call never ran; no other is
	// sent (handover.go).
	isTransferFailed bool
	// failedHandovers carries a handover that never ran from the goroutine
	// that settled it to the call's own (drive), which alone may move the
	// flow (handoversFailed).
	failedHandovers chan failedHandover
	// handoverAfter replaces time.After for the handover's waits. Tests only.
	handoverAfter func(time.Duration) <-chan time.Time
}

// TransferToAgent checks the queue can take the caller, then arms the
// transfer to run once the bridge line has been heard.
func (a *callActions) TransferToAgent(ctx context.Context, request flow.TransferRequest) (flow.Result, error) {
	if a.isEndingDecided() {
		a.log.Info("transfer ignored: the call's time limit has already decided how it ends",
			"asked", request.Queue)
		return flow.Succeeded(nil, ""), nil
	}
	if refused, isFailed := a.transferFailed(); isFailed {
		a.log.Info("transfer refused: a transfer on this call already never ran", "asked", request.Queue)
		return refused, nil
	}
	if isArmed, isTransfer := a.armedState(); isArmed {
		a.log.Info("transfer ignored: the call's ending is already armed",
			"asked", request.Queue, "isArmedTransfer", isTransfer)
		if isTransfer {
			// The model repeating itself: the transfer it wants is already on
			// its way, and a second one would restamp the channel and restart
			// the wait.
			return flow.Succeeded(nil, ""), nil
		}
		// The armed ending is a hangup. Reporting success here had the model
		// promise a transfer that never came (#147: "You'll be connected to an
		// agent", then the line dropped). A refusal is a conversation.
		return flow.Failed("CALL_ENDING", a.endingHint(false)), nil
	}
	queue, ok := a.queueForTransfer(ctx, request.Queue)
	if !ok {
		// The refusal is a conversation, not an error: the bot explains and
		// offers what it can still do.
		a.log.Warn("transfer refused: no queue to send the caller to",
			"asked", request.Queue, "hasFallback", a.fallbackQueue != nil)
		return flow.Failed("QUEUE_UNKNOWN", a.refusalHint()), nil
	}
	if !queue.IsEnabled {
		a.log.Warn("transfer refused: the queue is disabled", "queue", queue.Name)
		return flow.Failed("QUEUE_CLOSED", a.refusalHint()), nil
	}
	if a.callerChannel == "" {
		return flow.Result{}, errNoCallerChannel
	}

	// The summary rides the caller's channel into the queue, so the agent who
	// answers already knows what the conversation established.
	a.stampChannel("aicc_bot_summary", request.Summary)
	a.stampChannel("aicc_bot_reason", request.Reason)
	if len(request.Slots) > 0 {
		if encoded, err := json.Marshal(request.Slots); err == nil {
			a.stampChannel("aicc_bot_slots", string(encoded))
		}
	}
	// The ledger follows the call: after a transfer the human path writes the
	// one CDR, and the bot's share of the story goes with the caller.
	if a.recorder != nil {
		a.recorder.markTransferred(request.Summary, request.Reason)
		if a.facts != nil {
			a.stampBotShare(a.facts)
		}
	}

	a.armAs(ctx, true, func() {
		a.log.Info("transferring the caller", "queue", queue.Name, "ext", queue.ExtNumber)
		// Last, because they are the only ones of the bot's stamps still
		// changing.
		if a.recorder != nil {
			a.recorder.markHandedOver(queue.ID)
			a.stampUnbackedClaims(a.recorder)
			a.stampBotSec(a.recorder)
		}
		a.handOnCaller()
		// No context: the switch knows which dialplan this deployment runs in,
		// and naming one here is how the bot came to hand its callers to a
		// context where the queue extension does not exist. They heard hold
		// music and joined nothing.
		err := a.orchestrator.cfg.Switch.TransferToExtension(a.callerChannel, queue.ExtNumber, "")
		if isUnconfirmed(err) {
			// Sent and not answered: the switch may still move the caller,
			// and closing our leg now would race it (handover.go).
			a.awaitHandover(queue, request.FromPhase)
			return
		}
		if err != nil {
			a.log.Error("transfer failed", "queue", queue.Name, "error", err)
		}
		// Our SIP leg's job is done either way; the caller's leg has moved on.
		a.session.Close(context.Background())
	})

	return flow.Succeeded(map[string]any{"queue": queue.Name}, ""), nil
}

// queueForTransfer resolves where the caller is being put through.
//
// The name the model gave, when the deployment has it. Otherwise the number's
// own fallback queue: the caller asked for a person, and refusing them over an
// argument the bot itself got wrong serves nobody. The queue argument is an
// enum of the real queues now, so this is the narrow path — a flow written
// against another deployment's queue names, or a queue deleted mid-call — but
// it is the path where a caller would otherwise be dropped.
func (a *callActions) queueForTransfer(ctx context.Context, name string) (catalog.Queue, bool) {
	if queue, ok := a.orchestrator.findQueue(ctx, name); ok {
		return queue, true
	}
	if a.fallbackQueue == nil {
		return catalog.Queue{}, false
	}
	queues, err := a.orchestrator.cfg.Catalog.Queues(ctx)
	if err != nil {
		a.log.Error("read queues", "error", err)
		return catalog.Queue{}, false
	}
	for _, queue := range queues {
		if queue.ID != *a.fallbackQueue {
			continue
		}
		a.log.Warn("the model named a queue this deployment does not have; "+
			"using the number's fallback queue",
			"asked", name, "using", queue.Name)
		return queue, true
	}
	return catalog.Queue{}, false
}

// TakeMessage records what the caller wants passed on as an OPEN callback,
// which is the work queue an agent later rings through.
//
// The number called back is the caller's own unless the model gave one that is
// a telephone number (callbackNumber). Neither the message nor a number reaches
// the log: the message is what a customer said, and the number is theirs.
func (a *callActions) TakeMessage(ctx context.Context, request flow.MessageRequest) (flow.Result, error) {
	phoneNumber, source := a.callbackNumber(request.CallbackNumber)
	a.log.Info("message taken",
		"messageRunes", utf8.RuneCountInString(request.Message),
		"callbackNumberSource", source,
		"callbackNumber", maskNumber(phoneNumber))

	if ledger := a.orchestrator.cfg.Ledger; ledger != nil && a.recorder != nil {
		callID := a.recorder.callID
		callback, err := ledger.InsertCallback(ctx, &callID, a.fallbackQueue,
			phoneNumber, request.Message)
		if err != nil {
			a.log.Error("could not save the callback", "error", err)
			return flow.Failed("SAVE_FAILED", a.refusalHint()), nil
		}
		if announce := a.orchestrator.cfg.AnnounceCallback; announce != nil {
			announce(callback)
		}
	}
	return flow.Succeeded(nil, ""), nil
}

// Where a callback number came from, as the log names it.
const (
	callbackNumberCaller = "CALLER" // the caller's own number (the ANI)
	callbackNumberSpoken = "SPOKEN" // a number the caller gave
)

// callbackNumber decides the number a callback rings. A number the model gave
// is kept when it is a plausible telephone number, written the way this
// deployment dials (phonenum.Callback). Anything else it wrote — a description
// of the caller's own number ("当前来电号码"), digits inside words, a number a
// digit short — is discarded for the caller's number, and the call goes on
// (#311): the model is not asked to try again, because an agent working the
// queue needs something to dial, and the caller is reachable on the line they
// called from. The discarded value is not logged, only its length.
func (a *callActions) callbackNumber(given string) (number, source string) {
	if given != "" {
		cfg := a.orchestrator.cfg
		if number, ok := phonenum.Callback(given, cfg.DefaultCountryCode, cfg.DialForm); ok {
			return number, callbackNumberSpoken
		}
		a.log.Warn("callback number discarded: not a telephone number; calling back the caller's number",
			"givenRunes", utf8.RuneCountInString(given))
	}
	if a.facts == nil {
		return "", callbackNumberCaller
	}
	return a.facts.fromNumber, callbackNumberCaller
}

// maskNumber keeps at most the last four characters of a number, which is
// enough to tell two callbacks apart in a log and not enough to dial.
func maskNumber(number string) string {
	if number == "" {
		return ""
	}
	if len(number) <= 4 {
		return "***"
	}
	return "***" + number[len(number)-4:]
}

// RecordLead stores the lead on the call's attempt, overwriting an earlier one.
// Nothing else happens now: the DNC row and the callback are written once, from
// the final value, when the attempt finishes (design 10 section 11), so a model
// that changes its mind leaves nothing to undo and the call is not ended.
func (a *callActions) RecordLead(ctx context.Context, request flow.LeadRequest) (flow.Result, error) {
	if !a.isCampaign() || a.orchestrator.cfg.Leads == nil || a.recorder == nil {
		// The runtime offers the tool on a campaign call only, so this is a
		// wiring gap; the model is told the lead was not saved.
		a.log.Error("lead not recorded: no place to record it", "intent", request.Intent)
		return flow.Failed("NOT_RECORDED", a.leadHint()), nil
	}
	ok, err := a.orchestrator.cfg.Leads.SetLead(ctx, a.recorder.callID,
		campaign.LeadIntent(request.Intent), request.Notes)
	if err != nil {
		a.log.Error("could not save the lead", "intent", request.Intent, "error", err)
		return flow.Failed("SAVE_FAILED", a.leadHint()), nil
	}
	if !ok {
		a.log.Warn("lead not saved: the attempt is no longer in flight", "intent", request.Intent)
		return flow.Failed("NOT_RECORDED", a.leadHint()), nil
	}
	a.log.Info("lead recorded", "intent", request.Intent,
		"notesRunes", len([]rune(request.Notes)))
	return flow.Succeeded(nil, ""), nil
}

// leadHint steers the model after a lead could not be saved: it carries on and
// says nothing of it, because the customer has no use for our bookkeeping.
func (a *callActions) leadHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "记录未保存。不要向客户提及，继续当前对话；如有必要稍后可再次调用。"
	}
	return "The record was not saved. Do not mention it to the customer; carry on " +
		"with the conversation, and you may try again later if it still matters."
}

// Hangup ends the call once the goodbye has been heard.
func (a *callActions) Hangup(ctx context.Context, _ flow.HangupRequest) (flow.Result, error) {
	if a.isEndingDecided() {
		a.log.Info("hangup ignored: the call's time limit has already decided how it ends")
		return flow.Succeeded(nil, ""), nil
	}
	if isArmed, isTransfer := a.armedState(); isArmed {
		// The ending already armed stands, and the ledger keeps its outcome.
		a.log.Info("hangup ignored: the call's ending is already armed",
			"isArmedTransfer", isTransfer)
		if isTransfer {
			// The caller is being put through; saying the call is over would
			// contradict that. Told so, the model says at most a short line.
			return flow.Failed("CALL_ENDING", a.endingHint(true)), nil
		}
		if a.refuseBareHangup() {
			return flow.Failed("CALL_ENDING", a.interruptedHangupHint()), nil
		}
		if a.refuseHangupWhileLineOwed() {
			return flow.Failed("CALL_ENDING", a.lineOwedHint()), nil
		}
		// A repeated hangup is the model's intent exactly; the ending runs on
		// the playback of a turn that played audio (onPlaybackDone), or, when
		// it insisted after being asked for the closing line, on the playback
		// of the turn that carries this call (insistedOnHangup).
		a.insistedOnHangup()
		a.sayGoodbyeOnHangup()
		return flow.Succeeded(nil, ""), nil
	}
	if a.recorder != nil {
		a.recorder.markHangup()
	}
	a.arm(ctx, func() {
		a.log.Info("hanging up after the farewell")
		a.markFinished("HANGUP")
		a.session.Close(context.Background())
	})
	return flow.Succeeded(nil, ""), nil
}

// arm schedules an action for the end of the closing line the model is about
// to speak, with a cap in case that line never finishes.
//
// The first ending armed is the call's ending: arming again while it waits
// changes nothing. Replacing it moved armedInTurn and restarted the cap on
// every repeat, so a model that called transfer_to_agent three times waited
// for a turn after the last call and hit the cap ten seconds after it (#25).
func (a *callActions) arm(ctx context.Context, action func()) {
	a.armAs(ctx, false, action)
}

// armAs is arm, recording whether the ending is a transfer to an agent, which
// decides how a later hangup or transfer from the model is answered.
func (a *callActions) armAs(_ context.Context, isTransfer bool, action func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isEndingLocked {
		a.log.Info("not arming: the call's time limit has already decided how it ends")
		return
	}
	if a.armed != nil {
		a.log.Info("not arming: the call's ending is already armed")
		return
	}
	a.armed = action
	a.isArmedTransfer = isTransfer
	a.armedInTurn = a.session.currentTurn()
	a.isLineSpoken = false
	a.isInterrupted = false
	a.isHangupRefused = false
	a.isLineOwed = false
	a.closingLine = ""
	a.said = nil
	a.isLineAskedAgain = false
	a.notLineThrough = 0
	a.isLineRefused = false
	// Before the cap starts: a fresh arming is never frozen.
	a.interruptions = 0
	a.isLineSaidByRule = false
	a.goodbyeTurn = 0
	graceCap := a.graceCap
	if graceCap <= 0 {
		graceCap = actionGraceCap
	}
	a.startCapLocked(graceCap)
}

// startCapLocked (re)starts the cap, retiring any timer already running.
//
// From the second interruption over the closing line on it does nothing: the
// running timer and capDue stay as they are, so the call ends within the cap
// that was running then, whatever the caller does. Every path that restarts or
// moves out the cap (onBargeIn, onTurnDone, refuseBareHangup, the line ask)
// goes through here, so none of them can undo the bound (#465).
func (a *callActions) startCapLocked(after time.Duration) {
	if a.interruptions >= 2 {
		return
	}
	a.armedGeneration++
	generation := a.armedGeneration
	a.capDue = time.Now().Add(after)
	time.AfterFunc(after, func() { a.runAtCap(generation) })
}

// runAtCap runs the armed action when its closing line has not been heard in
// time. The two ways to get here are told apart: a line that was never
// generated is a provider that went quiet, while a line generated but not
// heard is playback that ran past the cap's estimate.
//
// A third way is no fault: a cap frozen at the second interruption (#465) is
// the bound that rule sets on a caller who keeps talking over the line, and
// reaching it is the rule working. Neither warning fits there (the line may
// have been generated several times, and isLineSpoken only says whether the
// latest answer had finished generating), so it is logged as the rule.
func (a *callActions) runAtCap(generation int) {
	a.mu.Lock()
	armed := a.armed
	isStillArmed := armed != nil && a.armedGeneration == generation
	isLineSpoken := a.isLineSpoken
	interruptions := a.interruptions
	if isStillArmed {
		a.armed = nil
	}
	a.mu.Unlock()
	if !isStillArmed {
		return
	}
	switch {
	case interruptions >= 2:
		a.log.Info("the cap frozen at the second interruption ran out; running the armed action",
			"interruptions", interruptions)
	case isLineSpoken:
		a.log.Warn("running an armed action at the cap; the closing line was generated " +
			"but its playback had not finished")
	default:
		a.log.Warn("running an armed action at the cap; no closing line was generated " +
			"after the action was armed")
	}
	armed()
}

// isArmed reports whether an action is already waiting for the closing line.
func (a *callActions) isArmed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed != nil
}

// armedState reports whether an ending is armed and whether it is a transfer.
func (a *callActions) armedState() (isArmed, isTransfer bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed != nil, a.isArmedTransfer
}

// endingHint steers the model after it asked for a transfer or a hangup the
// armed ending already settles. The call is ending, the answer to the caller
// is short, and no transfer is promised unless one is armed.
func (a *callActions) endingHint(isHangupAsked bool) string {
	zh := a.session != nil && a.session.cfg.Session.Language == "zh"
	if isHangupAsked {
		if zh {
			return "来电者正在被转接给人工,通话不会挂断。不要道别,最多简短说明正在为他们转接。"
		}
		return "The caller is already being transferred to an agent, so the call will not " +
			"be hung up. Do not say goodbye; at most say briefly that they are being connected."
	}
	if zh {
		return "通话已经在结束,不会转接人工。不要承诺转接;简短回答对方,或直接道别。"
	}
	return "The call is already ending and will not be transferred. Do not promise a " +
		"transfer; answer the caller briefly or just say goodbye."
}

// refuseBareHangup reports whether this hangup is the one to refuse: the caller
// interrupted the armed closing line, the model has said nothing aloud since
// (in an earlier turn or so far in this one), and the refusal is unspent.
// Without it the model answered a caller's question with a bare hangup and the
// call ended on the audio-less turn, the question unanswered (#147). It refuses
// once, so a model that insists gets its hangup and there is no loop. The cap,
// restarted here, bounds the wait for the answer: a model that never speaks
// still ends the call. After the second interruption the cap is frozen
// (startCapLocked) and the refusal does not move it.
func (a *callActions) refuseBareHangup() bool {
	finished, isSpeaking := a.session.audioProgress()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || !a.isInterrupted || a.isHangupRefused ||
		isSpeaking || finished > a.audioTurnsAtInterrupt {
		return false
	}
	a.isHangupRefused = true
	graceCap := a.graceCap
	if graceCap <= 0 {
		graceCap = actionGraceCap
	}
	a.startCapLocked(graceCap)
	return true
}

// interruptedHangupHint tells the model to answer the caller who spoke over
// its closing line, and that the call ends on its own after the answer.
func (a *callActions) interruptedHangupHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "来电者打断了你的结束语。请用一句简短的话回应他们刚才说的内容,通话会在这句话之后自动结束。不要调用 hangup。"
	}
	return "The caller spoke over your closing line. Reply to what they said in one short " +
		"sentence; the call ends by itself right after it. Do not call hangup."
}

// expectClosingLine records that the armed hangup leads into a terminal phase
// with a line of its own, which the caller must hear before the call ends
// (#207). It does nothing for a transfer, for an ending the time limit has
// decided, or when nothing is armed. The orchestrator calls it after the phase
// has been entered, on profiles that do not speak the line themselves
// (doubao's RequiresTerminalAnnounce), with the line as rendered for the call.
//
// The line is kept so the words of the turn that follows can be held against
// it (onTurnDone, #262): a model that answers a tool batch ending the call
// with its own confirmation, not the line, has not said the line. An empty line
// cannot be compared, and any turn with audio counts as it.
func (a *callActions) expectClosingLine(line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || a.isArmedTransfer || a.isEndingLocked || a.isLineSpoken {
		return
	}
	a.isLineOwed = true
	a.closingLine = line
}

// onBotSaid collects the words of a final transcript of the model's, for the
// turn it belongs to, while the closing line is owed. A turn can carry several;
// they are joined. The arming turn's own words are not kept: it spoke before the
// ending was armed and is not the line.
func (a *callActions) onBotSaid(turn int, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || !a.isLineOwed || a.closingLine == "" || turn <= a.armedInTurn ||
		turn <= a.notLineThrough || text == "" {
		return
	}
	if a.said == nil {
		a.said = map[int]string{}
	}
	a.said[turn] = strings.TrimSpace(a.said[turn] + " " + text)
}

// refuseHangupWhileLineOwed reports whether this hangup is the one to refuse:
// the closing line is owed, nothing has been said aloud in a turn after the one
// that armed the ending (finished, or so far in this one), and the refusal is unspent.
// A model that answers the tool result carrying the line with a second bare
// hangup would otherwise end the call on that audio-less turn, the line never
// said (#207). Like refuseBareHangup it refuses once and restarts the cap, so
// a model that never speaks still ends the call there.
func (a *callActions) refuseHangupWhileLineOwed() bool {
	a.mu.Lock()
	armedInTurn := a.armedInTurn
	a.mu.Unlock()
	// Only a turn after the one that armed the ending counts: that turn may
	// have spoken ("Great, noted.") before it called the tools.
	hasSpoken, isSpeaking := a.session.audioSince(armedInTurn)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || !a.isLineOwed || a.isLineRefused || a.isInterrupted ||
		isSpeaking || hasSpoken {
		return false
	}
	a.isLineRefused = true
	graceCap := a.graceCap
	if graceCap <= 0 {
		graceCap = actionGraceCap
	}
	a.startCapLocked(graceCap)
	return true
}

// insistedOnHangup drops the owed line when the model calls hangup again after
// its one refusal: it gets its hangup, which runs on the playback of the turn
// that carries the call (audio-less or not) instead of waiting out the cap in
// silence. No loop, as with refuseBareHangup.
func (a *callActions) insistedOnHangup() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || !a.isLineOwed || !a.isLineRefused {
		return
	}
	a.isLineOwed = false
	a.log.Warn("the model insisted on hangup after being asked for the closing line; " +
		"ending the call without it")
}

// sayGoodbyeOnHangup handles a hangup the model makes in an answer that is
// already speaking, after the caller talked over the closing line and a rule
// counted the line as said (isLineSaidByRule: a cut in its final sentence, or
// the second interruption). The answer is then the goodbye, and a hangup from
// it is the model saying so. A tool result is answered with one more turn, and
// on qwen that turn said the line yet again (#465: the line three times and a
// BYE 16.6 s after the interruption). So the turn that made
// the call is recorded as the goodbye (goodbyeTurn), the ending runs on its
// playback (onPlaybackDone), and the turn the result brings is not played and
// leaves no words in the transcript (Session.muteNextTurn).
//
// Nothing here applies to a first interruption that left the line owed (the
// answer may legitimately take two turns), to an answer that has not started
// speaking (the bare hangup refusals decide that), or to a transfer. A further
// interruption withdraws it (onBargeIn).
func (a *callActions) sayGoodbyeOnHangup() {
	_, isSpeaking := a.session.audioProgress()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || a.isArmedTransfer || !a.isLineSaidByRule || a.goodbyeTurn != 0 || !isSpeaking {
		return
	}
	turn := a.session.currentTurn()
	if turn <= a.armedInTurn {
		return
	}
	a.goodbyeTurn = turn
	a.session.muteNextTurn(true)
	a.log.Info("the model said goodbye and called hangup over a closing line counted as said; "+
		"the call ends after this turn, and the turn its answer brings is not played", "turn", turn)
}

// lineOwedHint asks the model to say the closing line it has not said yet.
func (a *callActions) lineOwedHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "你还没有对来电者说结束语。请现在就说出来,通话会在这句话之后自动结束。不要再调用 hangup。"
	}
	return "You have not said your closing line to the caller yet. Say it now, out loud; " +
		"the call ends by itself right after it. Do not call hangup again."
}

// lockEnding keeps the ending armed now — or already run — as the call's
// ending, whatever the model asks for afterwards.
func (a *callActions) lockEnding() {
	a.mu.Lock()
	a.isEndingLocked = true
	a.isLineOwed = false
	a.mu.Unlock()
}

// isEndingDecided reports whether lockEnding has been called.
func (a *callActions) isEndingDecided() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isEndingLocked
}

// runArmedNow runs the armed action without waiting for a line, for an ending
// that has no line to wait for.
func (a *callActions) runArmedNow() {
	a.mu.Lock()
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	if armed != nil {
		armed()
	}
}

// openFallbackQueue is the number's own queue when it exists and is taking
// calls: the one queue the platform may hand a caller to on its own.
func (a *callActions) openFallbackQueue(ctx context.Context) (catalog.Queue, bool) {
	if a.isCampaign() {
		return catalog.Queue{}, false
	}
	if a.fallbackQueue == nil {
		return catalog.Queue{}, false
	}
	queues, err := a.orchestrator.cfg.Catalog.Queues(ctx)
	if err != nil {
		a.log.Error("read queues", "error", err)
		return catalog.Queue{}, false
	}
	for _, queue := range queues {
		if queue.ID == *a.fallbackQueue {
			return queue, queue.IsEnabled
		}
	}
	return catalog.Queue{}, false
}

// disarm drops whatever was waiting to be spoken over, because the call it
// belonged to has ended. The cap that exists to rescue a closing line that
// never finishes cannot tell that case from a caller who hung up in the middle
// of one — and there it fired ten seconds after the caller was gone, onto a
// channel that no longer existed: two warnings and a "transfer failed" that
// reads exactly like a real one.
func (a *callActions) disarm() {
	a.mu.Lock()
	a.armed = nil
	a.armedGeneration++
	a.mu.Unlock()
}

// onPlaybackDone runs whatever was armed, now that the caller has heard the
// line that precedes it. turn says whose playback this was: the turn the tool
// call arrived in does not count, only a turn the tool's result created.
//
// hasAudio says the turn played something to the caller. It is required only
// once the caller has interrupted the closing line: a model that answers that
// interruption with a bare tool call produces an audio-less turn whose
// playback "ends" at once, and running the ending on it hung up on a question
// nobody had answered (#147). Before an interruption the rule stays as it was
// (the line's own turn carries audio, and an audio-less repeat of hangup in it
// must still end the call), and the "bye now" acknowledgement never reaches
// here (onBargeIn runs it directly). The cap bounds a model that never speaks.
//
// The same holds while the closing line is owed and unheard (isLineOwed, #207):
// the tool result that carried the line can be answered with a bare hangup, an
// audio-less turn that is not the line, and the caller was hung up on without
// a word. A turn with other words (a paraphrase) has audio, but onTurnDone has
// judged it by its words and asked for the line again (#262), so it does not
// end the call either. A flow with no closing line (or a hangup outside a terminal phase) owes
// nothing and keeps the rule above: an audio-less repeat of hangup ends it.
//
// A turn that onTurnDone judged not to be the line (notLineThrough) does not
// run the ending either, whatever it played: the line is on its way in a turn
// after it, and that turn's playback is the one the ending waits for.
//
// After a goodbye (goodbyeTurn, sayGoodbyeOnHangup) a later turn needs no audio
// of its own: it is the muted turn the hangup's result brought, and its
// playback ends when the goodbye's queued audio has drained, which is the
// goodbye's own playback end (the goodbye's PLAYBACK_DONE is superseded when
// that turn begins).
func (a *callActions) onPlaybackDone(turn int, hasAudio bool) {
	a.mu.Lock()
	isAfterGoodbye := a.goodbyeTurn != 0 && turn > a.goodbyeTurn
	if a.armed == nil || turn <= a.armedInTurn || (a.isInterrupted && !hasAudio && !isAfterGoodbye) ||
		(a.isLineOwed && !a.isLineSpoken && !hasAudio) || turn <= a.notLineThrough {
		a.mu.Unlock()
		return
	}
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	armed()
}

// onTurnDone records that the closing line has finished generating. A turn
// that was cut short is not the line, whichever turn it was.
//
// The line's length is known from here on, so the cap is moved to cover it.
// The cap started at arming, but the line plays behind whatever is still
// queued: the turn that carried the tool call can go on speaking for seconds
// after the call, and a cap counted from arming then cut the tail of the line
// off (issue #24). The cap only ever moves later; a line that fits inside it
// keeps the original.
//
// Every later turn does this, not only the first: a caller who interrupts the
// closing line gets an answer, and a model may answer in two turns (#147: a
// second answer clipped by a cap that only the first had moved).
//
// While the closing line is owed a turn with no audio is not the line either
// (hasAudio, #207): it neither marks the line spoken nor moves the cap.
//
// A turn with audio is still not the line unless its words say it (#262). A
// model that ends the call with record_lead and hangup in one response answers
// both results in one turn, and that turn followed the talk phase's instruction
// ("confirm what you noted") instead of the line, and the call ended as if the
// line had been heard. So the words of the turn (onBotSaid) are held against
// the line (saysTheClosingLine), and a turn that does not say it is answered
// by asking for the line once more, whether or not that turn said a goodbye of
// its own: the flow's exact words matter more than a double farewell. The
// caller hears that turn whole, then the line, because the request goes out
// now, with the turn finished, while its tail is still playing: the line
// generates behind the tail and queues after it, and nothing is flushed. A
// second turn that misses ends the call on its own playback, logged at WARN,
// since a third request would only drag the call out. A turn with no
// transcript cannot be judged and counts as the line, as before.
//
// The request is held back while a later turn is already being made
// (isTurnGenerating): asking over it would cut it off, and it may be the line.
// It is judged when it finishes, and the request not spent here is still
// there for it.
func (a *callActions) onTurnDone(turn int, isInterrupted, hasAudio bool) {
	a.mu.Lock()
	if a.armed == nil || turn <= a.armedInTurn || isInterrupted ||
		(a.isLineOwed && !hasAudio) {
		a.mu.Unlock()
		return
	}
	var ask string
	if a.isLineOwed {
		words := a.said[turn]
		for judged := range a.said {
			if judged <= turn {
				delete(a.said, judged)
			}
		}
		switch {
		case a.closingLine == "" || words == "":
			a.log.Debug("no words were transcribed for the turn after the closing move; "+
				"counting it as the closing line", "turn", turn)
		default:
			coverage, isSaid := saysTheClosingLine(a.closingLine, words)
			switch {
			case isSaid:
			case a.isLineAskedAgain:
				a.log.Warn("the closing line was not said; ending the call without it",
					"turn", turn, "coverage", coverage)
			default:
				a.log.Info("the turn after the closing move did not say the closing line; "+
					"the line is asked for once more", "turn", turn, "coverage", coverage)
				a.notLineThrough = turn
				// The line generates while this turn's tail plays and plays
				// behind it; the cap covers both.
				a.startCapLocked(a.capAfterLineAskLocked())
				if a.session.isTurnGenerating() {
					a.log.Info("a later turn is already being made; judging it before asking",
						"turn", turn)
					a.mu.Unlock()
					return
				}
				a.isLineAskedAgain = true
				ask = a.closingLine
			}
		}
	}
	if ask == "" {
		a.isLineSpoken = true
		playout := a.session.queuedPlayout() + lineDrainMargin
		if due := time.Now().Add(playout); due.After(a.capDue) {
			a.startCapLocked(playout)
		}
	}
	a.mu.Unlock()
	if ask != "" {
		if err := a.session.Speak(ask, true); err != nil {
			a.log.Warn("could not ask for the closing line again; the cap ends the call", "error", err)
		}
	}
}

// capAfterLineAskLocked is how long the wait for a requested line is bounded:
// the usual grace on top of the audio still queued, which the line plays behind.
// The caller holds mu.
func (a *callActions) capAfterLineAskLocked() time.Duration {
	graceCap := a.graceCap
	if graceCap <= 0 {
		graceCap = actionGraceCap
	}
	return graceCap + a.session.queuedPlayout()
}

// closingTailAck is how much of the closing line may still be unplayed when
// the caller speaks for that speech to count as answering it. Caller speech
// with at most this much of the line left ("bye now" over its last words) is
// the acknowledgement the call ends on; with more left it is an interruption
// and the line has not been said. 1.5 s is the last phrase or two of a
// goodbye: short enough that a question cannot have been cut off in the middle
// of the line, long enough to cover the lag between the caller starting to
// answer and the detector reporting it.
const closingTailAck = 1500 * time.Millisecond

// onBargeIn decides what the caller taking the floor means for an armed ending.
// cut is the BARGE_IN: Unplayed is the audio still queued for the caller when
// they spoke, read before the barge-in flushed it; Played is how much of the
// cut turn (Turn) they had heard (negative when not known); IsGenerating says
// that turn was still being generated; IsByProvider says the provider reported
// the cut (it cancelled the turn on hearing the caller) and not the session's
// own barge-in. Both reports are handled alike: what counts is that the caller
// cut in, not which side noticed first, and the session reports one cut once
// (#493).
//
// Over a line that has not finished generating (isLineSpoken clear), a cut of
// a turn after the arming one while that turn is still being generated, with
// something audible cut (Unplayed or Played above zero), is an interruption of
// the closing line (#493). On qwen the provider's own turn detection cancels
// such a turn before the session hears the caller, and the cut went uncounted:
// a caller saying "mm-hm" over every telling needed a third interruption
// before the cap froze. It is counted and handled like the second case below,
// with two differences, both because a line still being generated has no known
// length: it never runs the armed action (whatever is unplayed, it cannot be
// the caller answering the goodbye), and the final-sentence rule never applies
// to it (the owner's decision, 2026-11-06). So a first such cut leaves the line
// owed, and from the second on, D holds as for any interruption. Anything else
// over a line not yet generated changes nothing, and the ending waits for a
// line: a cut of the arming turn itself (the line has not started), of a turn
// that had finished (a tail that is not the line), or one with nothing audible
// to cut (nothing heard of the turn and nothing queued).
//
// Over a line that finished generating, there are two cases, told apart by how
// much of it the caller had left to hear:
//
//   - At most closingTailAck: the caller is answering the goodbye ("bye now").
//     That is what this exists for: the detector reports speech, the playback
//     watch is superseded, PLAYBACK_DONE never arrives, and every such call
//     used to end on the grace cap, five silent seconds late. Act now.
//   - More: the caller cut in on the line, with a question (#147: "wait, what's
//     the trial offer?" 2.3 s into a 9 s line, and the call was ended on it).
//     The flush and truncation the barge-in does proceed, the line no longer
//     counts as said, the ending stays armed, and the model answers in a later
//     turn whose playback (or, if the caller cuts that one short too, the same
//     rule) runs it. The cap starts over so that answer has the time to be made.
//
// The second case is counted (interruptions, #465), because it can repeat:
//
//   - The first interruption is as above, unless it fell in the line's final
//     sentence (cutInFinalSentence): the caller has then heard the line but for
//     its goodbye, and a decline over it is answered by a short goodbye that is
//     not the line, so asking for the line again said it twice. The line counts
//     as said (isLineOwed cleared); everything else is as for any first
//     interruption. Needs the line's text, so a profile that never owes the
//     line (doubao, closingLine empty) gets the count but never this rule.
//   - From the second on the line counts as said and is never asked for again,
//     and the cap is not restarted (and, in startCapLocked, never moved): the
//     ending runs on the playback of the next turn with audio, or at the cap
//     already running. Without this a caller saying "mm-hm" over every telling
//     restarted the cap each time and the model said the full line five times
//     in 20 s. The trade-off is accepted: a caller who interrupts twice may not
//     hear all of the line.
//
// Either rule sets isLineSaidByRule, which is what lets a hangup from the
// answer end the call on that answer (sayGoodbyeOnHangup). A caller cutting in
// on that goodbye with more than closingTailAck left withdraws it: the answer
// to them is played, and the ending runs on its playback or at the cap.
func (a *callActions) onBargeIn(cut Event) {
	unplayed, played := cut.Unplayed, cut.Played
	a.mu.Lock()
	if a.armed != nil && a.goodbyeTurn != 0 && unplayed > closingTailAck {
		// The caller cut in on the goodbye with more than a tail left: what
		// they say is answered and that answer is played, not muted
		// (sayGoodbyeOnHangup). The ending waits for it, or for the cap.
		a.goodbyeTurn = 0
		a.session.muteNextTurn(false)
		a.log.Info("the caller spoke over the goodbye; the answer to them is played",
			"unplayedMs", unplayed.Milliseconds())
	}
	if a.armed == nil {
		a.mu.Unlock()
		return
	}
	if !a.isLineSpoken {
		if !cut.IsGenerating || cut.Turn <= a.armedInTurn || (unplayed <= 0 && played <= 0) {
			a.mu.Unlock()
			return
		}
		// A cut over a line still being generated (#493): an interruption,
		// never the "bye now", never dated inside the line.
		count, capIn := a.interruptLocked()
		a.mu.Unlock()
		a.log.Info("the caller cut in on the closing line while it was still being generated; "+
			"counting it as an interruption, not as the line said",
			"interruptions", count, "turn", cut.Turn, "isByProvider", cut.IsByProvider,
			"unplayedMs", unplayed.Milliseconds(), "playedMs", played.Milliseconds())
		a.logLaterInterruption(count, cut, capIn)
		return
	}
	if unplayed > closingTailAck {
		count, capIn := a.interruptLocked()
		var finalSentence time.Duration
		isFinalSentence := false
		if count == 1 && a.closingLine != "" {
			finalSentence, isFinalSentence = cutInFinalSentence(a.closingLine, played, unplayed)
			if isFinalSentence {
				a.isLineOwed = false
				a.isLineSaidByRule = true
			}
		}
		a.mu.Unlock()
		if count == 1 {
			a.log.Info("caller spoke over the closing line; answering them before the armed action runs",
				"unplayedMs", unplayed.Milliseconds(), "interruptions", count, "isByProvider", cut.IsByProvider)
			if isFinalSentence {
				a.log.Info("the cut fell in the closing line's final sentence; counting it as said",
					"unplayedMs", unplayed.Milliseconds(), "playedMs", played.Milliseconds(),
					"finalSentenceMs", finalSentence.Milliseconds())
			}
		}
		a.logLaterInterruption(count, cut, capIn)
		return
	}
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	a.log.Info("caller spoke after the closing line; running the armed action now",
		"unplayedMs", unplayed.Milliseconds(), "isByProvider", cut.IsByProvider)
	armed()
}

// interruptLocked counts a cut over the armed ending as an interruption and
// handles it as one (onBargeIn): the line no longer counts as spoken, the
// ending waits for a turn that plays audio (isInterrupted), and the model's
// first bare hangup is refused. The first interruption restarts the cap so the
// answer has the time to be made; from the second on the line counts as said
// and is never asked for again (rule D), and the cap is not restarted, nor, in
// startCapLocked, ever moved again. It reports the count and how long the cap
// has left. The final-sentence rule (A) is not applied here: it needs a line
// that finished generating, and onBargeIn applies it only to a cut of one.
// The caller holds mu.
func (a *callActions) interruptLocked() (count int, capIn time.Duration) {
	a.isLineSpoken = false
	a.isInterrupted = true
	a.audioTurnsAtInterrupt, _ = a.session.audioProgress()
	a.interruptions++
	count = a.interruptions
	if count >= 2 {
		a.isLineOwed = false
		a.isLineSaidByRule = true
	} else {
		graceCap := a.graceCap
		if graceCap <= 0 {
			graceCap = actionGraceCap
		}
		a.startCapLocked(graceCap)
	}
	return count, time.Until(a.capDue)
}

// logLaterInterruption logs rule D for the second interruption and each one
// after it, whichever side reported it and whether or not the line had
// finished generating. The first is logged by the case it fell in.
func (a *callActions) logLaterInterruption(count int, cut Event, capIn time.Duration) {
	switch {
	case count == 2:
		a.log.Info("a second interruption over the closing line; not asking for it again, cap not restarted",
			"interruptions", count, "unplayedMs", cut.Unplayed.Milliseconds(), "capInMs", capIn.Milliseconds(),
			"isGenerating", cut.IsGenerating, "isByProvider", cut.IsByProvider)
	case count > 2:
		a.log.Info("a further interruption over the closing line; cap still not restarted",
			"interruptions", count, "unplayedMs", cut.Unplayed.Milliseconds(), "capInMs", capIn.Milliseconds(),
			"isGenerating", cut.IsGenerating, "isByProvider", cut.IsByProvider)
	}
}

// campaignDeniedTools are the built-ins a campaign call never offers: a
// campaign call has no way to a person, and callbacks come only from leads.
var campaignDeniedTools = []string{"transfer_to_agent", "take_message"}

// isCampaign reports whether this call was placed by a campaign.
func (a *callActions) isCampaign() bool { return a.campaignID != "" }

// rescueCaller moves the caller to the number's fallback queue after a failed
// conversation.
func (a *callActions) rescueCaller() {
	if a.callerChannel == "" || a.fallbackQueue == nil || a.isCampaign() {
		a.session.Close(context.Background())
		return
	}
	queues, err := a.orchestrator.cfg.Catalog.Queues(context.Background())
	if err == nil {
		for _, queue := range queues {
			if queue.ID == *a.fallbackQueue {
				a.log.Info("rescuing the caller to the fallback queue", "queue", queue.Name)
				a.handOnCaller()
				if err := a.orchestrator.cfg.Switch.TransferToExtension(
					a.callerChannel, queue.ExtNumber, ""); isUnconfirmed(err) {
					// The conversation is over either way; the switch may
					// still move the caller when it recovers.
					a.log.Warn("rescue transfer sent and not confirmed; the switch may still run it", "error", err)
				} else if err != nil {
					a.log.Error("rescue transfer failed", "error", err)
				}
				break
			}
		}
	}
	a.session.Close(context.Background())
}

// markFinished tells the dialplan that this call ended because the bot decided
// it had, and not because the bot disappeared.
//
// The inbound script cannot tell those apart on its own: both look like a
// bridge that ended. It used to be spared the question by hangup_after_bridge,
// which took the caller down with the bot leg — and took the fallback with it,
// for exactly the failures the fallback exists for (the application restarts,
// the process dies, a provider drops). Now the script keeps the caller and
// asks: marked means the conversation reached its end, so hang up; unmarked
// means the bot vanished mid-call, so find the caller a human.
//
// Only the two closes that ARE the deliberate ending carry it — the hangup
// tool and a flow reaching its terminal phase. A transfer must not: if the
// transfer fails, the absence of this mark is what sends the caller to the
// fallback queue instead of dropping them, and that failure is likeliest at
// the moment the mark would be wrong.
func (a *callActions) markFinished(how string) {
	if a.callerChannel == "" {
		return
	}
	// The bot is about to close its leg, and a call an agent placed is
	// written by the human path, which reads the bot's tally from the agent's
	// channel. Before the mark, because it is what the dialplan acts on, and
	// before the leg goes, because this is the last moment the bot's time is
	// known.
	if a.recorder != nil && a.facts.isPlacedByAgent() {
		a.stampUnbackedClaims(a.recorder)
		a.stampBotSec(a.recorder)
	}
	a.stampChannel("aicc_bot_finished", how)
}

// handOnCaller puts the switch's teardown rule back before the caller leaves
// us for a queue: from there an agent's hangup ends the call.
func (a *callActions) handOnCaller() {
	if a.callerChannel == "" {
		return
	}
	if err := a.orchestrator.cfg.Switch.EndCallerWithTheirBridge(a.callerChannel); err != nil {
		a.log.Warn("could not restore the caller's teardown rule", "error", err)
	}
}

// stampChannel writes one variable onto the caller's channel.
//
// Some values are the model's own words (the summary and the reason of a
// transfer), and a model writes line breaks. A line break would end the
// command on the event socket, so the switch adapter refuses it; the stamp is
// flattened to one line here instead, so the agent still gets the summary.
func (a *callActions) stampChannel(name, value string) {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	if strings.TrimSpace(value) == "" {
		return
	}
	if err := a.orchestrator.cfg.Switch.SetVariable(a.callerChannel, name, value); err != nil {
		a.log.Warn("could not stamp the caller's channel", "variable", name, "error", err)
	}
}

func (a *callActions) refusalHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "这个队列现在无法接听。请向来电者说明，并主动提出为他们留言。"
	}
	return "That queue cannot take the call right now. Explain that to the caller " +
		"and offer to take a message instead."
}

var errNoCallerChannel = errors.New(
	"the bot leg carried no caller channel header, so there is nothing to transfer")
