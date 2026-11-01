// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// The flow's time limit (global.maxDurationSec, 02 §7, issue #10): a wrap-up
// steer at 80% of the budget, then the platform ends the call — to the
// number's queue when it is open, otherwise through the flow's own goodbye.
//

// limitFlow closes into a terminal phase with a line; CLOSING is substituted
// so a test can drop the closing target.
const limitFlow = `{
	"id": "limit-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone." CLOSING
	},
	"nodes": {
		"welcome": {"instruction": "Answer questions.", "tools": []},
		"farewell": {"instruction": "Say goodbye.",
			"announce": "Thank you for calling, goodbye.",
			"tools": [], "isTerminal": true}
	}
}`

type limitHarness struct {
	o        *Orchestrator
	session  *Session
	model    *fakeModel
	engine   *flow.Engine
	runtime  *flow.Runtime
	actions  *callActions
	recorder *callRecorder
	sw       *fakeSwitch
	log      *slog.Logger
}

type limitSetup struct {
	// queue names the number's fallback queue: "support" is open,
	// "after-hours" is closed, "" is none.
	queue string
	// hasClosingTarget sets global.closingTarget to the farewell phase.
	hasClosingTarget bool
	profile          provider.Profile
}

func newLimitHarness(t *testing.T, setup limitSetup) *limitHarness {
	t.Helper()
	if setup.profile.Name == "" {
		setup.profile = provider.OpenAIProfile()
	}
	sw := &fakeSwitch{}
	session, _, model := startBridge(t, setup.profile)
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)
	o.cfg.Profile = setup.profile
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	closing := ""
	if setup.hasClosingTarget {
		closing = `, "closingTarget": "farewell"`
	}
	spec, err := flow.Load([]byte(strings.Replace(limitFlow, "CLOSING", closing, 1)))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	recorder := newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
		recorder: recorder,
	}
	if setup.queue != "" {
		queues, _ := o.cfg.Catalog.Queues(t.Context())
		for _, queue := range queues {
			if queue.Name == setup.queue {
				actions.fallbackQueue = &queue.ID
			}
		}
	}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)
	model.answerLinesWithATurn(session)
	return &limitHarness{o: o, session: session, model: model, engine: engine,
		runtime: runtime, actions: actions, recorder: recorder, sw: sw, log: log}
}

func (h *limitHarness) enforce(how limitEnforcement) {
	h.o.enforceSessionLimit(context.Background(), h.session, h.runtime, h.actions,
		h.recorder, how, h.log)
}

func (h *limitHarness) instructions() []string {
	h.model.mu.Lock()
	defer h.model.mu.Unlock()
	return append([]string(nil), h.model.instructions...)
}

// The wrap-up steer is a change to the standing instructions and nothing else:
// no turn is asked for at an arbitrary moment, and the steer survives a phase
// change because every later re-pin carries it.
func TestTheWrapUpSteerChangesOnlyTheInstructions(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})

	h.o.handleWrapUpDue(t.Context(), h.session, h.runtime, h.actions, time.Now().Add(3*time.Minute), h.log)

	got := h.instructions()
	if len(got) != 1 || !strings.Contains(got[0], "close to its time limit") ||
		!strings.Contains(got[0], "about 3 minutes") {
		t.Fatalf("instructions = %q, want one re-pin carrying the wrap-up steer", got)
	}
	if !strings.Contains(got[0], "Answer questions.") {
		t.Error("the wrap-up re-pin dropped the phase's own instruction")
	}
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues sent = %v, want none", cues)
	}
	if lines := h.model.spokenLines(); len(lines) != 0 {
		t.Errorf("lines spoken = %v, want none", lines)
	}
	if !h.runtime.IsWrappingUp() {
		t.Error("the runtime does not know it is wrapping up")
	}
}

// The steer invites a transfer only when the limit can deliver one: with no
// fallback queue, or a disabled one, the limit hangs up, and a caller told they
// will be put through would be misled.
func TestTheWrapUpSteerOffersAPersonOnlyWhenTheQueueIsOpen(t *testing.T) {
	for _, tc := range []struct {
		queue     string
		wantOffer bool
	}{
		{"support", true},
		{"after-hours", false},
		{"", false},
	} {
		h := newLimitHarness(t, limitSetup{queue: tc.queue, hasClosingTarget: true})
		h.o.handleWrapUpDue(t.Context(), h.session, h.runtime, h.actions,
			time.Now().Add(3*time.Minute), h.log)
		got := h.instructions()
		if len(got) != 1 {
			t.Fatalf("queue %q: instructions = %q, want one re-pin", tc.queue, got)
		}
		if has := strings.Contains(got[0], "put the caller through to a person"); has != tc.wantOffer {
			t.Errorf("queue %q: transfer offered = %v, want %v:\n%s", tc.queue, has, tc.wantOffer, got[0])
		}
	}
}

func TestTheWrapUpSteerLeavesAnEndingCallAlone(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	if _, err := h.actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil {
		t.Fatal(err)
	}

	h.o.handleWrapUpDue(t.Context(), h.session, h.runtime, h.actions, time.Now().Add(time.Minute), h.log)

	if got := h.instructions(); len(got) != 0 {
		t.Errorf("instructions = %q, want none: the call is already ending", got)
	}
}

// With the number's queue open, the limit puts the caller through with the
// SESSION_LIMIT reason and a summary — without one the reason never reaches
// the human path's CDR — once the handover line has been heard.
func TestTheLimitTransfersToAnOpenQueueAfterTheHandoverLine(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	turnBefore := h.session.currentTurn()

	h.enforce(limitFloorFree)

	if got := h.sw.variable("aicc_bot_reason"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_reason = %q, want SESSION_LIMIT", got)
	}
	if got := h.sw.variable("aicc_bot_summary"); got == "" {
		t.Error("no summary stamped; the reason would never reach the CDR")
	}
	if !h.actions.isArmed() || h.actions.armedInTurn != turnBefore {
		t.Fatalf("armed=%v in turn %d, want the transfer armed before the handover line",
			h.actions.isArmed(), h.actions.armedInTurn)
	}
	if cues := h.model.recordedUserText(); len(cues) != 1 || !strings.Contains(cues[0], "putting them through") {
		t.Fatalf("cues = %v, want the handover cue once", cues)
	}
	if got := h.sw.recordedTransfers(); len(got) != 0 {
		t.Fatalf("transferred before the handover line was heard: %v", got)
	}
	// A caller who hangs up during the handover line leaves the bot's
	// provisional row as the final one: the bot ended the call, so it says
	// SESSION_LIMIT, with the reason beside the summary and no queue, since
	// the caller never reached one. aicc_bot_sec is not stamped yet, so the
	// human path declines to write the call (telephony BotShare.HandedOver).
	if got := h.sw.variable("aicc_bot_sec"); got != "" {
		t.Errorf("aicc_bot_sec = %q before the handover; the human path would claim the row", got)
	}
	provisional := newFakeLedger()
	h.recorder.finish(provisional, testFacts(), discard())
	if cdr := provisional.cdrs[0]; cdr.HangupCause != "SESSION_LIMIT" || cdr.IsContained ||
		cdr.QueueID != nil || cdr.UserData["botReason"] != "SESSION_LIMIT" {
		t.Errorf("row before the handover: cause=%q contained=%v queue=%v botReason=%v, "+
			"want SESSION_LIMIT, not contained, no queue, botReason SESSION_LIMIT",
			cdr.HangupCause, cdr.IsContained, cdr.QueueID, cdr.UserData["botReason"])
	}

	// The model answers the cue by hanging up; the transfer stands.
	if result, _ := h.actions.Hangup(t.Context(), flow.HangupRequest{}); !result.IsOK {
		t.Errorf("hangup after the limit = %+v, want a quiet success", result)
	}

	h.actions.onPlaybackDone(turnBefore+1, true)
	if got := h.sw.recordedTransfers(); len(got) != 1 || got[0] != "chan-9→7001" {
		t.Fatalf("transfers = %v, want the caller put through to 7001", got)
	}
	if got := h.sw.variable("aicc_bot_finished"); got != "" {
		t.Errorf("aicc_bot_finished = %q; a model hangup replaced the transfer", got)
	}
	// Handed over, the call is the human path's: its row replaces the bot's
	// provisional one, with the final leg's cause and botReason=SESSION_LIMIT
	// (TestASessionLimitTransferKeepsTheFinalLegsCause). The handover itself
	// is what stamps aicc_bot_sec, which hands the row to that path.
	if h.recorder.endReason != "TRANSFER" || h.recorder.transferQueue == nil {
		t.Errorf("recorder end=%q queue=%v, want TRANSFER to the queue",
			h.recorder.endReason, h.recorder.transferQueue)
	}
	if h.sw.variable("aicc_bot_sec") == "" {
		t.Error("no aicc_bot_sec stamped at the handover; the human path would not write the row")
	}
}

// With no queue open, the limit closes the call through the flow's own
// goodbye, exactly as the turns-without-a-tool wall does, and the CDR says why
// and does not count the call as contained.
func TestTheLimitClosesThroughTheClosingPhaseWhenNoQueueIsOpen(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "after-hours", hasClosingTarget: true})
	turnBefore := h.session.currentTurn()

	h.enforce(limitFloorFree)

	if !h.engine.IsTerminal() || h.engine.NodeID() != "farewell" {
		t.Fatalf("node = %q, want the closing phase", h.engine.NodeID())
	}
	if got := h.model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken = %v, want the closing announce", got)
	}
	if got := h.model.spokenLinesClosing(); len(got) != 1 || !got[0] {
		t.Errorf("the announce was not asked for as a closing line: %v", got)
	}
	if got := h.sw.variable("aicc_bot_reason"); got != "" {
		t.Errorf("aicc_bot_reason = %q, want no transfer to a closed queue", got)
	}

	h.actions.onPlaybackDone(turnBefore+1, true)
	if got := h.sw.variable("aicc_bot_finished"); got != "FLOW_END" {
		t.Errorf("aicc_bot_finished = %q, want FLOW_END", got)
	}
	if len(h.sw.recordedTransfers()) != 0 {
		t.Error("the caller was transferred to a closed queue")
	}

	ledger := newFakeLedger()
	h.recorder.finish(ledger, testFacts(), discard())
	cdr := ledger.cdrs[0]
	if cdr.HangupCause != "SESSION_LIMIT" || cdr.IsContained {
		t.Errorf("cdr cause=%q contained=%v, want SESSION_LIMIT and not contained",
			cdr.HangupCause, cdr.IsContained)
	}
}

// A flow with no closing phase still ends: a plain goodbye, then the hangup.
func TestTheLimitSaysGoodbyeWhereTheFlowHasNoClosingPhase(t *testing.T) {
	h := newLimitHarness(t, limitSetup{})
	turnBefore := h.session.currentTurn()

	h.enforce(limitFloorFree)

	if cues := h.model.recordedUserText(); len(cues) != 1 || !strings.Contains(cues[0], "goodbye") {
		t.Fatalf("cues = %v, want the goodbye cue", cues)
	}
	h.actions.onPlaybackDone(turnBefore+1, true)
	if got := h.sw.variable("aicc_bot_finished"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_finished = %q, want SESSION_LIMIT", got)
	}
}

// A bot that kept the floor past the backstop cannot be asked for a goodbye:
// the cue asks for a turn over its own, which the providers refuse. With no line of the flow's own the hangup runs at once.
func TestAForcedLimitHangsUpAtOnceWithoutAskingForAGoodbye(t *testing.T) {
	h := newLimitHarness(t, limitSetup{})

	h.enforce(limitForced)

	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues = %v, want none: the bot holds the floor", cues)
	}
	if h.actions.isArmed() {
		t.Error("the hangup is still armed; it should have run at once")
	}
	if !h.session.isClosed() {
		t.Error("the session is still open after a forced limit")
	}
	if got := h.sw.variable("aicc_bot_finished"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_finished = %q, want SESSION_LIMIT", got)
	}
	if h.recorder.hangupCause != "SESSION_LIMIT" {
		t.Errorf("hangup cause = %q, want SESSION_LIMIT", h.recorder.hangupCause)
	}
}

// A closing phase's own line is still spoken on a forced limit: a line
// pre-empts, and needs no turn asked for.
func TestAForcedLimitStillSpeaksTheClosingPhasesLine(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "after-hours", hasClosingTarget: true})

	h.enforce(limitForced)

	if got := h.model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken = %v, want the closing announce", got)
	}
	if got := h.model.spokenLinesClosing(); len(got) != 1 || !got[0] {
		t.Errorf("the announce was not spoken as a closing line: %v", got)
	}
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues = %v, want none", cues)
	}
	if !h.actions.isArmed() {
		t.Error("the ending was not left armed for the line to be heard")
	}
}

// An ending already armed is bounded by its own grace cap and says more than
// the limit does; the limit leaves it alone.
func TestTheLimitNeverReplacesAnArmedEnding(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	if _, err := h.actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil {
		t.Fatal(err)
	}
	turnBefore := h.session.currentTurn()

	h.enforce(limitFloorFree)

	if got := h.sw.variable("aicc_bot_reason"); got != "" {
		t.Errorf("aicc_bot_reason = %q, want the armed hangup kept", got)
	}
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues = %v, want none", cues)
	}
	if h.recorder.hangupCause != "" {
		t.Errorf("hangup cause = %q, want none: the limit did not end this call", h.recorder.hangupCause)
	}
	h.actions.onPlaybackDone(turnBefore+1, true)
	if got := h.sw.variable("aicc_bot_finished"); got != "HANGUP" {
		t.Errorf("aicc_bot_finished = %q, want HANGUP", got)
	}
}

// Doubao takes no text cue, so nothing can ask for a handover line; the
// transfer runs at once instead of after ten seconds of silence.
func TestTheLimitTransfersAtOnceWhereNoCueCanBeSent(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true,
		profile: provider.DoubaoProfile()})

	h.enforce(limitFloorFree)

	if got := h.sw.recordedTransfers(); len(got) != 1 || got[0] != "chan-9→7001" {
		t.Fatalf("transfers = %v, want the caller put through at once", got)
	}
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues = %v, want none on a client that takes no cue", cues)
	}
}

// driveHarness runs drive with a budget, as runCall does.
func driveHarness(t *testing.T, budget sessionBudget) (*limitHarness, chan struct{}) {
	t.Helper()
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.o.drive(t.Context(), h.session, h.runtime, h.actions, h.recorder, budget, h.log)
	}()
	return h, done
}

func waitFor(t *testing.T, what string, isDone func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !isDone() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A limit that comes due while the bot is mid-turn waits for the caller to
// hear the turn, rather than asking for a line over it.
func TestTheLimitWaitsForTheBotToFinishItsTurn(t *testing.T) {
	h, _ := driveHarness(t, sessionBudget{
		wrapUpAfter: 10 * time.Millisecond, limitAfter: 60 * time.Millisecond,
		floorWait: time.Minute,
	})

	// The wrap-up steer arrives first, as instructions.
	waitFor(t, "the wrap-up steer", func() bool {
		got := h.instructions()
		return len(got) > 0 && strings.Contains(got[len(got)-1], "close to its time limit")
	})

	// The bot takes the floor and holds it past the limit.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples)}
	time.Sleep(150 * time.Millisecond)
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Fatalf("cues = %v, want none while the bot holds the floor", cues)
	}

	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	waitFor(t, "the handover cue", func() bool { return len(h.model.recordedUserText()) == 1 })
	if got := h.sw.variable("aicc_bot_reason"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_reason = %q, want SESSION_LIMIT", got)
	}
}

// A bot that never lets go of the floor does not keep the call past the limit
// for long: at the backstop the transfer runs with no line.
func TestTheLimitBackstopTransfersABotThatKeepsTheFloor(t *testing.T) {
	h, _ := driveHarness(t, sessionBudget{
		limitAfter: 20 * time.Millisecond, floorWait: 50 * time.Millisecond,
	})
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}

	waitFor(t, "the transfer", func() bool { return len(h.sw.recordedTransfers()) == 1 })
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues = %v, want none: the bot never gave up the floor", cues)
	}
}

// The budget's clocks die with drive: a call that ended before its limit is
// never acted on afterwards.
func TestTheBudgetStopsWhenTheCallEnds(t *testing.T) {
	h, done := driveHarness(t, sessionBudget{
		wrapUpAfter: 40 * time.Millisecond, limitAfter: 60 * time.Millisecond,
		floorWait: time.Second,
	})
	h.session.Close(context.Background())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not return when the call ended")
	}
	time.Sleep(100 * time.Millisecond)
	if got := h.instructions(); len(got) != 0 {
		t.Errorf("instructions = %q after the call ended", got)
	}
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues = %v after the call ended", cues)
	}
}

// The budget runs from the bot answering, the same clock as the CDR's botSec;
// what has already come due fires at once and never reads as "off".
func TestTheBudgetCountsFromTheAnswer(t *testing.T) {
	seconds := 100
	spec := &flow.Spec{Global: flow.Global{MaxDurationSec: &seconds}}
	budget := budgetFor(spec)
	if budget.limitAfter != 100*time.Second || budget.wrapUpAfter != 80*time.Second {
		t.Fatalf("budget = %+v, want the limit at 100s and the wrap-up at 80s", budget)
	}

	answered := time.Now()
	shifted := budget.since(answered, answered.Add(90*time.Second))
	if shifted.limitAfter != 10*time.Second || shifted.wrapUpAfter <= 0 || shifted.wrapUpAfter > time.Millisecond {
		t.Errorf("shifted = %+v, want the limit in 10s and the wrap-up due now", shifted)
	}

	off := 0
	if got := budgetFor(&flow.Spec{Global: flow.Global{MaxDurationSec: &off}}); got != (sessionBudget{}) {
		t.Errorf("budget with the limit off = %+v, want none", got)
	}
	if got := budgetFor(&flow.Spec{}).limitAfter; got != 900*time.Second {
		t.Errorf("default limit = %v, want 900s", got)
	}
}

// Once the limit has chosen the ending, a model hangup or transfer is answered
// as a success and changes nothing: the armed ending still runs afterwards.
func TestALockedTransferEndingSurvivesAModelHangupAndTransfer(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	turnBefore := h.session.currentTurn()
	h.enforce(limitFloorFree)

	if result, err := h.actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil || !result.IsOK {
		t.Fatalf("hangup after the limit = %+v, %v", result, err)
	}
	if result, err := h.actions.TransferToAgent(t.Context(),
		flow.TransferRequest{Queue: "after-hours", Reason: "CALLER_REQUEST"}); err != nil || !result.IsOK {
		t.Fatalf("transfer after the limit = %+v, %v", result, err)
	}

	h.actions.onPlaybackDone(turnBefore+1, true)
	if got := h.sw.recordedTransfers(); len(got) != 1 || got[0] != "chan-9→7001" {
		t.Fatalf("transfers = %v, want the limit's own transfer to 7001", got)
	}
	if got := h.sw.variable("aicc_bot_reason"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_reason = %q, want SESSION_LIMIT kept", got)
	}
	if got := h.sw.variable("aicc_bot_finished"); got != "" {
		t.Errorf("aicc_bot_finished = %q; the model's hangup replaced the transfer", got)
	}
}

func TestALockedClosingEndingSurvivesAModelHangupAndTransfer(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "after-hours", hasClosingTarget: true})
	turnBefore := h.session.currentTurn()
	h.enforce(limitFloorFree)

	if result, err := h.actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil || !result.IsOK {
		t.Fatalf("hangup after the limit = %+v, %v", result, err)
	}
	if result, err := h.actions.TransferToAgent(t.Context(),
		flow.TransferRequest{Queue: "support", Reason: "CALLER_REQUEST"}); err != nil || !result.IsOK {
		t.Fatalf("transfer after the limit = %+v, %v", result, err)
	}

	h.actions.onPlaybackDone(turnBefore+1, true)
	if got := h.sw.variable("aicc_bot_finished"); got != "FLOW_END" {
		t.Errorf("aicc_bot_finished = %q, want the closing phase's FLOW_END", got)
	}
	if got := h.sw.recordedTransfers(); len(got) != 0 {
		t.Errorf("transfers = %v, want none: the model's transfer was ignored", got)
	}
}

// A transfer the queue check allowed but the action then refused (here: the
// caller's channel is unknown) falls through to the closing path rather than
// leaving the call with no ending at all.
func TestARefusedLimitTransferFallsThroughToTheClosingPath(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	h.actions.callerChannel = ""
	turnBefore := h.session.currentTurn()

	h.enforce(limitFloorFree)

	if got := h.engine.NodeID(); got != "farewell" {
		t.Fatalf("node = %q, want the closing phase after the refused transfer", got)
	}
	if got := h.model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken = %v, want the closing announce", got)
	}
	if !h.actions.isEndingDecided() {
		t.Error("the closing ending was not locked")
	}
	// With no channel to stamp, the closing action's proof is the session
	// ending once the line has been heard.
	h.actions.onPlaybackDone(turnBefore+1, true)
	if !h.session.isClosed() {
		t.Error("the closing ending did not run")
	}
	if got := h.sw.recordedTransfers(); len(got) != 0 {
		t.Errorf("transfers = %v, want none", got)
	}
	if h.recorder.hangupCause != "SESSION_LIMIT" {
		t.Errorf("hangup cause = %q, want SESSION_LIMIT", h.recorder.hangupCause)
	}
}

// Doubao takes no text cue, so with no closing phase there is no goodbye to
// ask for: the hangup runs at once, and says why.
func TestTheLimitHangsUpDoubaoAtOnceWhenTheFlowHasNoClosingPhase(t *testing.T) {
	h := newLimitHarness(t, limitSetup{profile: provider.DoubaoProfile()})

	h.enforce(limitFloorFree)

	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Errorf("cues = %v, want none on a client that takes no cue", cues)
	}
	if h.actions.isArmed() || !h.session.isClosed() {
		t.Error("the hangup did not run at once")
	}
	if got := h.sw.variable("aicc_bot_finished"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_finished = %q, want SESSION_LIMIT", got)
	}
}

// After a tool result is sent the answer turn begins one round trip later; in
// that gap the bot still has the floor, and a cue would be refused.
func TestASentToolResultHoldsTheFloorUntilTheAnswerTurnBegins(t *testing.T) {
	h := newLimitHarness(t, limitSetup{})
	if h.session.isHoldingTheFloor() {
		t.Fatal("holding the floor before anything was asked")
	}

	if err := h.session.AnswerTool("call-1", `{"ok":1}`); err != nil {
		t.Fatal(err)
	}
	if !h.session.isHoldingTheFloor() {
		t.Fatal("not holding the floor between the tool result and the answer turn")
	}

	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	awaitBridgeEvent(t, h.session, EventTypeTurnDone)
	h.session.mu.Lock()
	owed := h.session.turnOwedUntil
	h.session.mu.Unlock()
	if !owed.IsZero() {
		t.Error("the owed turn was not cleared when the answer turn began")
	}
}

// The claim on the floor cannot stick: a turn that never comes stops counting
// after its expiry, and closing the session drops it at once.
func TestAnOwedTurnThatNeverComesStopsHoldingTheFloor(t *testing.T) {
	h := newLimitHarness(t, limitSetup{})
	if err := h.session.SendCue("hello"); err != nil {
		t.Fatal(err)
	}
	if !h.session.isHoldingTheFloor() {
		t.Fatal("a cue's turn is not counted")
	}
	h.session.mu.Lock()
	h.session.turnOwedUntil = time.Now().Add(-time.Millisecond)
	h.session.mu.Unlock()
	if h.session.isHoldingTheFloor() {
		t.Error("an expired claim still holds the floor")
	}

	if err := h.session.SendCue("again"); err != nil {
		t.Fatal(err)
	}
	h.session.Close(context.Background())
	if h.session.isHoldingTheFloor() {
		t.Error("a closed session still holds the floor")
	}
}

// A limit that falls due right after a tool result waits for the answer turn,
// and acts only once the caller has heard it.
func TestTheLimitWaitsThroughTheGapAfterAToolResult(t *testing.T) {
	h, _ := driveHarness(t, sessionBudget{limitAfter: 40 * time.Millisecond, floorWait: time.Minute})
	if err := h.session.AnswerTool("call-1", `{"ok":1}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Fatalf("cues = %v, want none: the answer turn is still to come", cues)
	}
	if got := h.sw.recordedTransfers(); len(got) != 0 {
		t.Fatalf("transfers = %v before the answer turn was heard", got)
	}

	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples)}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	waitFor(t, "the handover cue", func() bool { return len(h.model.recordedUserText()) == 1 })
	if got := h.sw.variable("aicc_bot_reason"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_reason = %q, want SESSION_LIMIT", got)
	}
}

// A PLAYBACK_DONE already in the channel when the next turn began is stale:
// consumed while the floor is held again, it does not enforce the limit.
func TestAStalePlaybackDoneDoesNotEnforceTheLimitOverANewTurn(t *testing.T) {
	h, _ := driveHarness(t, sessionBudget{limitAfter: 30 * time.Millisecond, floorWait: time.Minute})
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(120 * time.Millisecond) // the limit is due and waiting

	h.session.emit(Event{Type: EventTypePlaybackDone, Turn: 0})
	time.Sleep(100 * time.Millisecond)
	if cues := h.model.recordedUserText(); len(cues) != 0 {
		t.Fatalf("cues = %v, want none: a new turn holds the floor", cues)
	}

	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	waitFor(t, "the handover cue", func() bool { return len(h.model.recordedUserText()) == 1 })
}
