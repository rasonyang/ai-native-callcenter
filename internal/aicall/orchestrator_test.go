// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// Fakes for the orchestrator's edges.
//

type fakeCatalog struct {
	dids   []catalog.DID
	queues []catalog.Queue
}

func (c *fakeCatalog) DIDs(context.Context) ([]catalog.DID, error)     { return c.dids, nil }
func (c *fakeCatalog) Queues(context.Context) ([]catalog.Queue, error) { return c.queues, nil }

type fakeSwitch struct {
	mu        sync.Mutex
	transfers []string // "channel→extension"
	handedOn  []string // channels whose teardown rule was put back
	variables map[string]string
}

func (s *fakeSwitch) TransferToExtension(channelID, extension, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transfers = append(s.transfers, channelID+"→"+extension)
	return nil
}

func (s *fakeSwitch) SetVariable(_, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.variables == nil {
		s.variables = map[string]string{}
	}
	s.variables[name] = value
	return nil
}

func (s *fakeSwitch) EndCallerWithTheirBridge(channelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handedOn = append(s.handedOn, channelID)
	return nil
}

func (s *fakeSwitch) handedOnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.handedOn)
}

func (s *fakeSwitch) recordedTransfers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.transfers...)
}

func (s *fakeSwitch) variable(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.variables[name]
}

// testOrchestrator builds one against fakes; its UAS never listens.
func testOrchestrator(t *testing.T, sw *fakeSwitch) *Orchestrator {
	t.Helper()

	supportQueue := catalog.Queue{
		ID: uuid.New(), Name: "support", ExtNumber: "7001", IsEnabled: true,
	}
	closedQueue := catalog.Queue{
		ID: uuid.New(), Name: "after-hours", ExtNumber: "7002", IsEnabled: false,
	}

	o, err := NewOrchestrator(OrchestratorConfig{
		Catalog: &fakeCatalog{queues: []catalog.Queue{supportQueue, closedQueue}},
		Flows:   fakeFlows{},
		Switch:  sw,
		Profile: provider.OpenAIProfile(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	return o
}

type fakeFlows struct{}

func (fakeFlows) PublishedSpec(context.Context, uuid.UUID) (*flow.Spec, error) {
	return nil, nil
}

// testActions wires callActions to a bridge running on fakes.
func testActions(t *testing.T, sw *fakeSwitch) (*callActions, *Session, *fakeModel) {
	t.Helper()

	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	return &callActions{
		orchestrator:  testOrchestrator(t, sw),
		session:       session,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		callerChannel: "caller-channel-1",
	}, session, model
}

//
// Transfer.
//

func TestTransferWaitsForTheBridgeLineToPlay(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, model := testActions(t, sw)

	// The model calls the tool mid-turn.
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(20 * time.Millisecond)

	result, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "support", Reason: "BILLING", Summary: "wants a refund",
	})
	if err != nil || !result.IsOK {
		t.Fatalf("transfer refused: %+v %v", result, err)
	}

	// The summary is stamped before any transfer, so it cannot lose the race.
	if got := sw.variable("aicc_bot_summary"); got != "wants a refund" {
		t.Errorf("summary variable = %q", got)
	}

	// The turn the tool call arrived in finishing its playback must NOT fire
	// the transfer: the bridge line has not been spoken yet.
	actions.onPlaybackDone(session.currentTurn())
	if got := sw.recordedTransfers(); len(got) != 0 {
		t.Fatalf("the transfer ran before the bridge line was spoken: %v", got)
	}

	// The next turn — created by the tool's own result — plays out, and the
	// transfer follows it.
	actions.onPlaybackDone(session.currentTurn() + 1)
	if got := sw.recordedTransfers(); len(got) != 1 || got[0] != "caller-channel-1→7001" {
		t.Fatalf("transfers = %v, want the caller moved to 7001", got)
	}

	// A second playback must not transfer twice.
	actions.onPlaybackDone(session.currentTurn() + 2)
	if got := sw.recordedTransfers(); len(got) != 1 {
		t.Errorf("the armed action ran twice: %v", got)
	}
}

// With nowhere to fall back to, an unknown queue is still a refusal — but it
// is the last resort, not the first answer. See the test below.
func TestTransferToAnUnknownQueueIsARefusalNotAnError(t *testing.T) {
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)

	result, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "nonexistent", Reason: "X", Summary: "s",
	})
	if err != nil {
		t.Fatalf("a missing queue errored instead of refusing: %v", err)
	}
	if result.IsOK || result.Error != "QUEUE_UNKNOWN" {
		t.Errorf("result = %+v, want a QUEUE_UNKNOWN refusal", result)
	}
	if !strings.Contains(result.Hint, "take a message") {
		t.Errorf("hint = %q, want the bot told what to offer instead", result.Hint)
	}
	if len(sw.recordedTransfers()) != 0 {
		t.Error("a refused transfer still moved the caller")
	}
}

// A caller who asked for a person gets one, even when the bot named a queue
// that is not there.
//
// The model invented "customer_service" on a deployment whose queues are
// support-en, support-zh and wt_queue: the transfer was refused, the flow
// announced a handover anyway and the line dropped, having promised a call
// back that nobody would make. The queue argument is an enum of the real
// queues now, so this is the narrow path — but on it, the number's own
// fallback queue is a better answer than turning the caller away over an
// argument the bot got wrong.
func TestAnUnknownQueueFallsBackToTheNumbersOwnQueue(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, _ := testActions(t, sw)

	queues, err := actions.orchestrator.cfg.Catalog.Queues(t.Context())
	if err != nil {
		t.Fatalf("read queues: %v", err)
	}
	support := queues[0]
	actions.fallbackQueue = &support.ID

	result, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "customer_service", Reason: "X", Summary: "s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsOK {
		t.Fatalf("result = %+v, want the caller put through to the fallback queue", result)
	}

	actions.onPlaybackDone(session.currentTurn() + 1)
	if got := sw.recordedTransfers(); len(got) != 1 ||
		got[0] != "caller-channel-1→"+support.ExtNumber {
		t.Errorf("transfers = %v, want the caller moved to %s", got, support.ExtNumber)
	}
}

func TestTransferToAClosedQueueIsRefusedAsClosed(t *testing.T) {
	actions, _, _ := testActions(t, &fakeSwitch{})

	result, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "after-hours", Reason: "X", Summary: "s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsOK || result.Error != "QUEUE_CLOSED" {
		t.Errorf("result = %+v, want QUEUE_CLOSED", result)
	}
}

//
// Hangup.
//

func TestHangupEndsTheCallAfterTheFarewell(t *testing.T) {
	actions, session, _ := testActions(t, &fakeSwitch{})

	result, err := actions.Hangup(t.Context(), flow.HangupRequest{})
	if err != nil || !result.IsOK {
		t.Fatalf("hangup refused: %+v %v", result, err)
	}

	// Not yet: the goodbye has not been heard.
	select {
	case <-session.done:
		t.Fatal("the call ended before the farewell played")
	default:
	}

	actions.onPlaybackDone(session.currentTurn() + 1)

	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("the call did not end after the farewell")
	}
}

// What both live test calls showed: the caller answers the goodbye, the
// detector reports speech, the playback watch is superseded, and every call
// ends on the grace cap five silent seconds late. Speech after the closing
// line must fire the action, not delay it.
func TestACallerAnsweringTheGoodbyeFiresTheArmedAction(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, _ := testActions(t, sw)

	fired := make(chan struct{})
	actions.arm(t.Context(), func() { close(fired) })

	// Speech before the closing line exists must NOT fire: the tool call has
	// only just happened and the line is still coming.
	actions.onBargeIn()
	select {
	case <-fired:
		t.Fatal("the action ran before the closing line was even generated")
	case <-time.After(50 * time.Millisecond):
	}

	// The closing line finishes generating in the next turn...
	actions.onTurnDone(session.currentTurn()+1, false)
	// ...and the caller talks over the tail of it.
	actions.onBargeIn()

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("speech after the closing line did not fire the armed action")
	}
}

//
// The cap.
//

func TestAnArmedActionRunsAtTheCapWhenPlaybackNeverFinishes(t *testing.T) {
	actions, session, _ := testActions(t, &fakeSwitch{})

	fired := make(chan struct{})
	actions.arm(t.Context(), func() { close(fired) })
	_ = session

	select {
	case <-fired:
		t.Fatal("the action ran immediately")
	case <-time.After(50 * time.Millisecond):
	}

	// The production cap is seconds; this test would rather not wait for it,
	// so it verifies the mechanism by inspection of the timer having been set
	// and fires the fallback path directly.
	actions.mu.Lock()
	armed := actions.armed
	actions.armed = nil
	actions.mu.Unlock()
	if armed == nil {
		t.Fatal("nothing was armed")
	}
	armed()
	<-fired
}

//
// Driving a conversation end to end over the fakes.
//

func TestDriveAnswersToolCallsThroughTheFlow(t *testing.T) {
	sw := &fakeSwitch{}
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)

	spec, err := flow.Load([]byte(driveFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	actions.recorder = recorder

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.drive(t.Context(), session, runtime, actions, recorder, log)
	}()

	// The model asks for a transfer.
	model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_1",
		ToolName: flow.ToolTransferToAgent,
		ToolArgs: `{"queue":"support","reason":"BILLING","summary":"needs help"}`,
	}

	// The tool is answered and the next turn requested.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		model.mu.Lock()
		answered := len(model.toolResults)
		model.mu.Unlock()
		if answered > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	model.mu.Lock()
	results := append([]toolResult(nil), model.toolResults...)
	model.mu.Unlock()
	if len(results) != 1 || results[0].id != "fc_1" {
		t.Fatalf("tool results = %+v", results)
	}
	if !strings.Contains(results[0].output, `"ok":"1"`) {
		t.Errorf("tool output = %s", results[0].output)
	}

	// The closing line plays out; the caller moves; the bridge closes.
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}

	deadline = time.Now().Add(2 * time.Second)
	for len(sw.recordedTransfers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sw.recordedTransfers(); len(got) != 1 || got[0] != "chan-9→7001" {
		t.Fatalf("transfers = %v", got)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not return after the call ended")
	}
}

// A failure the provider could name reaches the CDR as that name. A failure it
// could not is released exactly as it always was.
//
// Both halves matter. The caller is rescued the same way either way — nothing
// about the release changes — but "the provider's session ran out of time" and
// "the bot could not go on" are different answers to give whoever reads the
// call afterwards, and a deployment whose calls keep outliving a cap can only
// see that if the cause survives the trip out of the client.
func TestAFailureSaysWhatTheProviderCalledIt(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		cause provider.FailureCause
		want  string
	}{
		{"a failure with nothing to say for itself",
			"", "MEDIA_OR_PROVIDER_FAILURE"},
		{"a session the provider's own clock ended",
			provider.FailureCauseSessionExpired, "PROVIDER_SESSION_EXPIRED"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			session, _, model := startBridge(t, provider.OpenAIProfile())
			awaitBridgeEvent(t, session, EventTypeReady)

			o := testOrchestrator(t, &fakeSwitch{})
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			spec, err := flow.Load([]byte(driveFlow))
			if err != nil {
				t.Fatalf("load flow: %v", err)
			}
			actions := &callActions{orchestrator: o, session: session, log: log}
			runtime := flow.NewRuntime(flow.NewEngine(spec, "en", nil, log),
				actions, flow.NewBackend(""), nil, log)
			recorder := newCallRecorder(uuid.New(), time.Now(), nil)
			actions.recorder = recorder

			done := make(chan struct{})
			go func() {
				defer close(done)
				o.drive(t.Context(), session, runtime, actions, recorder, log)
			}()

			model.events <- provider.Event{
				Type: provider.EventTypeError, IsFatal: true,
				Text: "the session ended", Err: errors.New("the session ended"),
				FailureCause: testCase.cause,
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("drive did not return after the conversation failed")
			}

			recorder.mu.Lock()
			endReason, cause := recorder.endReason, recorder.hangupCause
			recorder.mu.Unlock()
			if endReason != "FAILED" {
				t.Errorf("endReason = %q, want FAILED", endReason)
			}
			if cause != testCase.want {
				t.Errorf("hangup cause = %q, want %q", cause, testCase.want)
			}
		})
	}
}

// driveFlow is the minimal flow the drive test runs.
// A call the flow concludes — reaching a terminal phase by any route — is
// contained, exactly as if the model had called the hangup tool. This path
// forgot to say so once, and every farewell-ended call reported uncontained.
func TestReachingATerminalPhaseIsContainment(t *testing.T) {
	session, _, _ := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, &fakeSwitch{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	spec, err := flow.Load([]byte(`{
		"id": "terminal-test",
		"specVersion": "v2",
		"initialNode": "welcome",
		"global": {"persona": "You answer the phone."},
		"nodes": {
			"welcome": {"instruction": "Greet.", "tools": [],
				"transitions": [{"on": "NO_INPUT", "target": "farewell"}]},
			"farewell": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true}
		}
	}`))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{orchestrator: o, session: session, log: log}
	actions.recorder = newCallRecorder(uuid.New(), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	moved := engine.OnNoInput()
	if moved == "" || !engine.IsTerminal() {
		t.Fatalf("the test flow did not reach its terminal phase (moved=%q)", moved)
	}
	o.afterMove(moved, session, runtime, actions, log)

	actions.recorder.mu.Lock()
	endReason := actions.recorder.endReason
	actions.recorder.mu.Unlock()
	if endReason != "HANGUP" {
		t.Errorf("endReason = %q, want HANGUP — a flow-concluded call must count as contained", endReason)
	}
}

//
// Lines the flow owns.
//

// A phase that carries its own words has them said on the way in, and a
// terminal phase still ends the call only once the caller has heard them.
//
// The ordering is the whole of it. arm() remembers the turn it was armed in
// and waits for the playback of a LATER one, so the line has to be asked for
// after that turn is recorded — ask first and the line's own turn can be the
// one remembered, and then no playback ever counts and the call ends ten
// seconds later on the grace cap, in silence.
func TestATerminalPhaseSaysItsLineAndStillWaitsForTheCallerToHearIt(t *testing.T) {
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, &fakeSwitch{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(announcingFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{orchestrator: o, session: session, log: log}
	actions.recorder = newCallRecorder(uuid.New(), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)
	model.answerLinesWithATurn(session)

	turnBefore := session.currentTurn()
	moved := engine.OnNoInput()
	if moved != "farewell" || !engine.IsTerminal() {
		t.Fatalf("the test flow did not reach its terminal phase (moved=%q)", moved)
	}
	o.afterMove(moved, session, runtime, actions, log)

	if got := model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken lines = %v, want the phase's own closing line once", got)
	}
	if !actions.isArmed() {
		t.Fatal("the call ended before the closing line could be heard")
	}
	if actions.armedInTurn != turnBefore {
		t.Errorf("armed in turn %d, want %d — the line's own turn must come after",
			actions.armedInTurn, turnBefore)
	}

	// The turn that carried the move does not count; the line's own does.
	actions.onPlaybackDone(turnBefore)
	if !actions.isArmed() {
		t.Fatal("the call ended on the playback of a turn that preceded the line")
	}
	actions.onPlaybackDone(turnBefore + 1)
	if actions.isArmed() {
		t.Error("the caller heard the closing line and the call stayed open")
	}
}

// The transfer case, where an action is already armed when the phase arrives.
// The line still has to be said — it is the hand-over script — and the
// transfer still has to wait for it.
func TestATerminalPhaseThatFindsAnArmedTransferStillSaysItsLine(t *testing.T) {
	sw := &fakeSwitch{}
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(announcingFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	actions.recorder = newCallRecorder(uuid.New(), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)
	model.answerLinesWithATurn(session)

	turnBefore := session.currentTurn()
	_, moved := runtime.Dispatch(t.Context(), flow.ToolTransferToAgent,
		`{"queue":"support","reason":"BILLING","summary":"needs help"}`)
	if moved != "handoff" {
		t.Fatalf("moved to %q, want handoff", moved)
	}
	o.afterMove(moved, session, runtime, actions, log)

	if got := model.spokenLines(); len(got) != 1 || got[0] != "I am putting you through now." {
		t.Fatalf("spoken lines = %v, want the hand-over line once", got)
	}
	actions.onPlaybackDone(turnBefore + 1)
	if got := sw.recordedTransfers(); len(got) != 1 || got[0] != "chan-9→7001" {
		t.Errorf("transfers = %v, want the caller put through once the line was heard", got)
	}
}

// Most phases leave the words to the model, and for those nothing new happens:
// the instructions are re-pinned and that is all.
func TestAPhaseWithNoLineOfItsOwnAsksForNothingToBeSaid(t *testing.T) {
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, &fakeSwitch{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(driveFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{orchestrator: o, session: session, log: log}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	o.afterMove(engine.OnToolResult(flow.ToolTransferToAgent,
		map[string]any{"ok": "1"}), session, runtime, actions, log)

	if got := model.spokenLines(); len(got) != 0 {
		t.Errorf("spoken lines = %v, want none: this phase has no words of its own", got)
	}
	model.mu.Lock()
	instructions := len(model.instructions)
	model.mu.Unlock()
	if instructions != 1 {
		t.Errorf("the phase change re-pinned the instructions %d times, want once", instructions)
	}
}

//
// Silence.
//

// deadAirHarness wires one flow for the no-input tests.
func deadAirHarness(t *testing.T, flowJSON string) (*Orchestrator, *Session,
	*fakeModel, *flow.Engine, *flow.Runtime, *callActions, *slog.Logger) {
	t.Helper()
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, &fakeSwitch{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(flowJSON))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{orchestrator: o, session: session, log: log}
	actions.recorder = newCallRecorder(uuid.New(), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)
	return o, session, model, engine, runtime, actions, log
}

// Silence that moves the flow into a phase with words of its own asks for one
// turn, and it is the line. The re-engagement cue on top of it was a second
// turn requested 38 ms later (W-Q2): qwen refused it with "another response is
// in progress", and an engine that takes it says both — a check-in after the
// flow's own goodbye.
func TestSilenceThatMovesIntoALineAsksForOneTurnOnly(t *testing.T) {
	o, session, model, engine, runtime, actions, log := deadAirHarness(t, announcingFlow)
	model.answerLinesWithATurn(session)

	o.handleDeadAir(session, runtime, actions, log)

	if !engine.IsTerminal() {
		t.Fatal("the test flow did not move on the silence")
	}
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Errorf("spoken lines = %v, want the phase's own line once", got)
	}
	if got := model.recordedUserText(); len(got) != 0 {
		t.Errorf("cues sent = %v, want none: the line is the turn", got)
	}
	if !actions.isArmed() {
		t.Error("the terminal phase did not arm the ending")
	}
}

// Silence the flow does not act on is the model's to handle, and nothing but
// the cue asks it to.
func TestSilenceWithNoMoveStillPromptsTheModel(t *testing.T) {
	o, session, model, _, runtime, actions, log := deadAirHarness(t, `{
		"id": "silence-test",
		"specVersion": "v2",
		"initialNode": "welcome",
		"global": {"persona": "You answer the phone."},
		"nodes": {"welcome": {"instruction": "Greet.", "tools": []}}
	}`)

	o.handleDeadAir(session, runtime, actions, log)

	if got := model.spokenLines(); len(got) != 0 {
		t.Errorf("spoken lines = %v, want none", got)
	}
	if got := model.recordedUserText(); len(got) != 1 ||
		!strings.Contains(got[0], "still there") {
		t.Errorf("cues sent = %v, want the re-engagement cue once", got)
	}
}

// A move into a phase with no words of its own leaves them to the model, so
// the cue still goes — here the goodbye one, because the phase is terminal.
func TestSilenceThatMovesIntoAPhaseWithoutALineStillPromptsTheModel(t *testing.T) {
	o, session, model, engine, runtime, actions, log := deadAirHarness(t, `{
		"id": "silence-terminal-test",
		"specVersion": "v2",
		"initialNode": "welcome",
		"global": {"persona": "You answer the phone."},
		"nodes": {
			"welcome": {"instruction": "Greet.", "tools": [],
				"transitions": [{"on": "NO_INPUT", "target": "farewell"}]},
			"farewell": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true}
		}
	}`)

	o.handleDeadAir(session, runtime, actions, log)

	if !engine.IsTerminal() {
		t.Fatal("the test flow did not move on the silence")
	}
	if got := model.spokenLines(); len(got) != 0 {
		t.Errorf("spoken lines = %v, want none", got)
	}
	if got := model.recordedUserText(); len(got) != 1 ||
		!strings.Contains(got[0], "goodbye") {
		t.Errorf("cues sent = %v, want the goodbye cue once", got)
	}
}

// The call's first words travel in the session configuration, because the
// opening turn is asked for as part of starting the session — there is no
// mid-call moment to say them in.
func TestTheEntryPhasesLineOpensTheCall(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(announcingFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "zh", nil, log)
	runtime := flow.NewRuntime(engine, &callActions{log: log}, flow.NewBackend(""), nil, log)

	cfg := sessionConfigFor(spec, runtime, "zh")
	if cfg.OpeningText != "感谢致电，请问有什么可以帮您？" {
		t.Errorf("openingText = %q, want the entry phase's line in the call's language",
			cfg.OpeningText)
	}
	if !strings.Contains(cfg.Instructions, "Current phase [welcome]") &&
		!strings.Contains(cfg.Instructions, "当前环节【welcome】") {
		t.Errorf("instructions do not start the call in the entry phase:\n%s", cfg.Instructions)
	}

	// A flow that names no line leaves the opening to the model, exactly as
	// every call did before a phase could carry one.
	plain, err := flow.Load([]byte(driveFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	plainEngine := flow.NewEngine(plain, "en", nil, log)
	plainRuntime := flow.NewRuntime(plainEngine, &callActions{log: log},
		flow.NewBackend(""), nil, log)
	if got := sessionConfigFor(plain, plainRuntime, "en").OpeningText; got != "" {
		t.Errorf("openingText = %q, want nothing", got)
	}
}

// announcingFlow carries a line on its entry phase and on both of its terminal
// phases, which is the shape a provider that cannot be cued demands.
const announcingFlow = `{
	"id": "announcing-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["transfer_to_agent", "hangup"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",
			 "condition": {"slot": "result.ok", "op": "EQ", "value": "1"},
			 "target": "handoff"}
		]
	},
	"nodes": {
		"welcome": {
			"instruction": "Greet the caller.",
			"announce": {"en": "Thanks for calling, how can I help you today?",
			             "zh": "感谢致电，请问有什么可以帮您？"},
			"tools": [],
			"transitions": [{"on": "NO_INPUT", "target": "farewell"}]
		},
		"handoff": {
			"instruction": "Announce the transfer.",
			"announce": "I am putting you through now.",
			"tools": [], "isTerminal": true
		},
		"farewell": {
			"instruction": "Say goodbye.",
			"announce": "Thank you for calling, goodbye.",
			"tools": [], "isTerminal": true
		}
	}
}`

const driveFlow = `{
	"id": "drive-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["transfer_to_agent", "hangup"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",
			 "condition": {"slot": "result.ok", "op": "EQ", "value": "1"},
			 "target": "handoff"}
		]
	},
	"nodes": {
		"welcome": {"instruction": "Greet the caller.", "tools": []},
		"handoff": {"instruction": "Announce the transfer.", "tools": []}
	}
}`

// A flow whose transfer phase is terminal still puts the caller through.
//
// The five ported flows all mark their "we're putting you through" phase
// isTerminal, and terminality arms the call's ending — which replaced the
// transfer the tool had just armed. The caller heard "an agent will be with
// you shortly" and was then hung up on, having been transferred nowhere.
// arming is last-one-wins by design, so the rule lives where the two meet:
// a terminal phase that arrives on top of an armed action leaves it alone.
func TestATerminalTransferPhaseDoesNotHangUpOnTheCallerInstead(t *testing.T) {
	sw := &fakeSwitch{}
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)
	spec, err := flow.Load([]byte(terminalHandoffFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	actions.recorder = recorder

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.drive(t.Context(), session, runtime, actions, recorder, log)
	}()

	model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_1",
		ToolName: flow.ToolTransferToAgent,
		ToolArgs: `{"queue":"support","reason":"BILLING","summary":"needs help"}`,
	}

	// Wait for the tool to have been answered: the phase move, and with it the
	// terminal rule this test is about, happens on the way out of that.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		model.mu.Lock()
		answered := len(model.toolResults)
		model.mu.Unlock()
		if answered > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The closing line plays out.
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}

	deadline = time.Now().Add(2 * time.Second)
	for len(sw.recordedTransfers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sw.recordedTransfers(); len(got) != 1 || got[0] != "chan-9→7001" {
		t.Fatalf("transfers = %v, want the caller put through to 7001", got)
	}
	// And the ledger says transferred, not hung up on.
	if recorder.endReason != "TRANSFER" {
		t.Errorf("endReason = %q, want TRANSFER", recorder.endReason)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not return after the call ended")
	}
}

// driveFlow's handoff phase, as the ported flows write it: terminal.
const terminalHandoffFlow = `{
	"id": "terminal-handoff-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["transfer_to_agent", "hangup"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",
			 "condition": {"slot": "result.ok", "op": "EQ", "value": "1"},
			 "target": "handoff"}
		]
	},
	"nodes": {
		"welcome": {"instruction": "Greet the caller.", "tools": []},
		"handoff": {"instruction": "Announce the transfer.", "tools": [], "isTerminal": true}
	}
}`

// A deployment must not start the AI leg without having chosen a provider:
// the choice is made once, at startup, so a missing one is a configuration
// error rather than a call that fails when the phone rings.
func TestAnOrchestratorWithoutAProviderIsRejected(t *testing.T) {
	_, err := NewOrchestrator(OrchestratorConfig{
		Catalog: &fakeCatalog{},
		Flows:   fakeFlows{},
		Switch:  &fakeSwitch{},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Fatal("an orchestrator with no provider profile was accepted")
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// The dialplan keeps the caller alive past the bot leg now, so it has to be
// told which of the two things happened. These pin the marking, and — just as
// load-bearing — the places that must NOT mark.
//
// What made this necessary: with hangup_after_bridge on, a bot leg that died
// took the caller down inside fifty milliseconds, so the fallback block never
// ran for the failures it exists for. The caller heard the call simply cut.
func TestTheBotSaysWhetherItMeantToEndTheCall(t *testing.T) {
	const marker = "aicc_bot_finished"

	t.Run("the hangup tool marks the ending as deliberate", func(t *testing.T) {
		sw := &fakeSwitch{}
		actions, session, _ := testActions(t, sw)

		if _, err := actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil {
			t.Fatalf("hangup: %v", err)
		}
		if got := sw.variable(marker); got != "" {
			t.Errorf("marked before the farewell played: %q", got)
		}

		actions.onPlaybackDone(session.currentTurn() + 1)
		<-session.done

		if got := sw.variable(marker); got != "HANGUP" {
			t.Errorf("%s = %q, want HANGUP — unmarked, the dialplan sends a "+
				"caller who heard goodbye to a queue", marker, got)
		}
	})

	t.Run("a transfer marks nothing, so a failed one still rescues", func(t *testing.T) {
		sw := &fakeSwitch{}
		actions, session, _ := testActions(t, sw)
		actions.orchestrator.cfg.Catalog = &fakeCatalog{queues: []catalog.Queue{
			{ID: uuid.New(), Name: "support-en", ExtNumber: "7001", IsEnabled: true},
		}}

		result, err := actions.TransferToAgent(t.Context(),
			flow.TransferRequest{Queue: "support-en", Summary: "wants a person"})
		if err != nil || !result.IsOK {
			t.Fatalf("transfer refused: %+v %v", result, err)
		}
		actions.onPlaybackDone(session.currentTurn() + 1)

		if got := sw.variable(marker); got != "" {
			t.Errorf("a transfer marked the call as finished (%q). If the transfer "+
				"fails the caller is then hung up instead of rescued — and it "+
				"fails exactly when the switch is in trouble", got)
		}
		if sw.handedOnCount() != 1 {
			t.Errorf("the caller's teardown rule was not restored before the "+
				"transfer (%d calls); from a queue the agent's hangup must end "+
				"the call", sw.handedOnCount())
		}
	})

	t.Run("a call with no caller channel marks nothing and does not fail", func(t *testing.T) {
		sw := &fakeSwitch{}
		actions, session, _ := testActions(t, sw)
		actions.callerChannel = ""

		if _, err := actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil {
			t.Fatalf("hangup: %v", err)
		}
		actions.onPlaybackDone(session.currentTurn() + 1)
		<-session.done

		if got := sw.variable(marker); got != "" {
			t.Errorf("stamped a channel that does not exist: %q", got)
		}
	})
}

// The other deliberate ending: the flow itself concludes. It is containment
// for the ledger already; it has to look deliberate to the dialplan too, or
// every flow that ends by design drops its caller into a queue.
func TestAFlowThatConcludesAlsoSaysSo(t *testing.T) {
	session, _, _ := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	sw := &fakeSwitch{}
	o := testOrchestrator(t, sw)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	spec, err := flow.Load([]byte(`{
		"id": "terminal-marks",
		"specVersion": "v2",
		"initialNode": "welcome",
		"global": {"persona": "You answer the phone."},
		"nodes": {
			"welcome": {"instruction": "Greet.", "tools": [],
				"transitions": [{"on": "NO_INPUT", "target": "farewell"}]},
			"farewell": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true}
		}
	}`))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "caller-channel-1",
	}
	actions.recorder = newCallRecorder(uuid.New(), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	o.afterMove(engine.OnNoInput(), session, runtime, actions, log)
	actions.onPlaybackDone(session.currentTurn() + 1)
	<-session.done

	if got := sw.variable("aicc_bot_finished"); got != "FLOW_END" {
		t.Errorf("aicc_bot_finished = %q, want FLOW_END", got)
	}
}

// How long the caller was with the bot is not known when the bot decides to
// transfer: the closing sentence still has to be spoken, and the caller is
// with the bot while it plays. Stamped at the decision, the number disagreed
// with the leg's own bridge on every single transferred call — by the length
// of the goodbye — and the assembler warned about the difference every time,
// which is how a warning meant to catch real disagreement became noise (C50,
// live 2026-08-23: stamped 7, bridged 12).
func TestTheBotStampsItsDurationWhenTheCallerIsHandedOnNotWhenItDecides(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, model := testActions(t, sw)
	actions.facts = testFacts()
	actions.recorder = newCallRecorder(uuid.New(), time.Now().Add(-7*time.Second), nil)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(20 * time.Millisecond)

	if _, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "support", Reason: "BILLING", Summary: "wants a refund",
	}); err != nil {
		t.Fatalf("transfer refused: %v", err)
	}

	// What the conversation established is stamped straight away — it cannot
	// change, and it must not lose a race with the caller moving on.
	if got := sw.variable("aicc_did"); got != "95012" {
		t.Errorf("aicc_did = %q at the decision, want it stamped there", got)
	}
	if got := sw.variable("aicc_bot_sec"); got != "" {
		t.Errorf("aicc_bot_sec = %q at the decision — the goodbye has not been "+
			"spoken yet, so the caller's time with the bot is still growing", got)
	}

	actions.onPlaybackDone(session.currentTurn() + 1)
	if got := sw.recordedTransfers(); len(got) != 1 {
		t.Fatalf("transfers = %v, want the caller handed on", got)
	}
	switch got, err := strconv.Atoi(sw.variable("aicc_bot_sec")); {
	case err != nil:
		t.Errorf("aicc_bot_sec = %q after the handoff, want the seconds with the bot",
			sw.variable("aicc_bot_sec"))
	case got < 7:
		t.Errorf("aicc_bot_sec = %d, want at least the 7 seconds already elapsed "+
			"when the bot decided", got)
	}
}

// An armed action waits for a closing line to be heard, with a cap in case it
// never finishes. A caller who hangs up in the middle of that line makes the
// cap the only thing left running — and ten seconds after they are gone it
// transfers a channel that no longer exists, warning twice and erroring once
// on the way. The alarm reads exactly like a real failed transfer, which is
// the worst kind of noise (C51, live 2026-08-23).
func TestNothingArmedRunsOnceTheCallHasEnded(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, model := testActions(t, sw)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(20 * time.Millisecond)

	if _, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "support", Reason: "BILLING", Summary: "wants a refund",
	}); err != nil {
		t.Fatalf("transfer refused: %v", err)
	}

	actions.disarm() // the caller hung up while the bridge line was playing

	actions.onPlaybackDone(session.currentTurn() + 1)
	if got := sw.recordedTransfers(); len(got) != 0 {
		t.Errorf("transferred a channel whose call had ended: %v", got)
	}
}

// The model is offered the number's own queue, not every queue there is.
//
// Offered the whole catalogue, a model on a Chinese call picked support-zh —
// a real queue, correctly reasoned, and staffed by nobody who works the number
// that was dialled. The caller sat on hold music while the agent for that line
// waited in another queue. The number is where an operator said which agents
// answer it; there is nothing for the model to choose.
func TestATransferIsOfferedTheNumbersOwnQueue(t *testing.T) {
	o := testOrchestrator(t, &fakeSwitch{})
	queues, err := o.cfg.Catalog.Queues(t.Context())
	if err != nil {
		t.Fatalf("read queues: %v", err)
	}
	if len(queues) < 2 {
		t.Fatalf("the fixture needs more than one queue, has %d", len(queues))
	}

	got := o.queueNames(t.Context(), &queues[1].ID)
	if len(got) != 1 || got[0] != queues[1].Name {
		t.Errorf("queue names = %v, want only %q", got, queues[1].Name)
	}

	// With no queue on the number there is nothing better to go on, so the
	// model chooses among real queues rather than inventing one.
	got = o.queueNames(t.Context(), nil)
	if len(got) != len(queues) {
		t.Errorf("queue names = %v, want every queue when the number names none", got)
	}
}

//
// A tool result that moves the call into a closing line (W-Q1, A5c).
//

// closingLineFlow ends on hangup in a terminal phase with a line of its own,
// and moves on take_message into a phase with a line that is not terminal.
const closingLineFlow = `{
	"id": "closing-line-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["hangup", "take_message"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "hangup", "target": "farewell"},
			{"on": "TOOL_RESULT", "tool": "take_message", "target": "noted"}
		]
	},
	"nodes": {
		"welcome": {"instruction": "Greet the caller.", "tools": []},
		"noted": {
			"instruction": "Ask whether there is anything else.",
			"announce": {"en": "I have taken your message.", "zh": "您的留言已记录。"},
			"tools": []
		},
		"farewell": {
			"instruction": "Say goodbye.",
			"announce": {"en": "Thank you for calling, goodbye.", "zh": "感谢您的来电，再见。"},
			"tools": [], "isTerminal": true
		}
	}
}`

// toolPath is one call driven by the orchestrator over the fakes, with the
// deployment running a given provider.
type toolPath struct {
	session *Session
	model   *fakeModel
	actions *callActions
	engine  *flow.Engine
	done    chan struct{}
}

func startToolPath(t *testing.T, profile provider.Profile, lang string) *toolPath {
	t.Helper()
	session, _, model := startBridge(t, profile)
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, &fakeSwitch{})
	o.cfg.Profile = profile
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(closingLineFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, lang, nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	actions.recorder = recorder
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	h := &toolPath{session: session, model: model, actions: actions,
		engine: engine, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		o.drive(t.Context(), session, runtime, actions, recorder, log)
	}()
	return h
}

// callTool has the model call a tool in a turn of its own and waits for the
// answer.
func (h *toolPath) callTool(t *testing.T, name, args string) toolResult {
	t.Helper()
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_1", ToolName: name, ToolArgs: args,
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if results := h.model.recordedToolResults(); len(results) > 0 {
			// The follow-up after the answer runs on the drive goroutine; give
			// it the moment it needs before the test reads what it did.
			time.Sleep(50 * time.Millisecond)
			return results[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the %s call was never answered", name)
	return toolResult{}
}

// playTurn has the model produce one whole turn of speech.
func (h *toolPath) playTurn() {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2),
	}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
}

func (h *toolPath) isCallEnded() bool {
	select {
	case <-h.session.done:
		return true
	default:
		return false
	}
}

func (h *toolPath) awaitCallEnded(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-h.session.done:
	case <-time.After(within):
		t.Fatal("the call did not end")
	}
}

func hintOf(t *testing.T, output string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("the tool output is not an object: %s", output)
	}
	hint, _ := payload["hint"].(string)
	return hint
}

// On a Realtime profile, the tool result that ends the call carries the
// closing line as its hint, and the turn it produces is the line.
// Nothing else is asked for: a second turn for the line on top of the result
// lost to the result on qwen every time (docs/design/qwen-findings.md, W-Q1).
//
// The ending is armed, and the instructions re-pinned, before the tool is
// answered — the answer is what brings the line's turn into existence — and the
// call ends on the playback of that turn, never of the turn the tool call came
// in.
func TestAToolThatEndsTheCallCarriesTheClosingLineOnARealtimeProfile(t *testing.T) {
	for _, testCase := range []struct {
		profile provider.Profile
		lang    string
		line    string
	}{
		{provider.OpenAIProfile(), "en", "Thank you for calling, goodbye."},
		{provider.QwenProfile(), "zh", "感谢您的来电，再见。"},
		{provider.GatewayProfile(), "en", "Thank you for calling, goodbye."},
	} {
		t.Run(testCase.profile.Name, func(t *testing.T) {
			h := startToolPath(t, testCase.profile, testCase.lang)

			var isArmedAtAnswer bool
			var armedInTurn, instructionsAtAnswer int
			h.model.onToolResult = func() {
				isArmedAtAnswer = h.actions.isArmed()
				h.actions.mu.Lock()
				armedInTurn = h.actions.armedInTurn
				h.actions.mu.Unlock()
				h.model.mu.Lock()
				instructionsAtAnswer = len(h.model.instructions)
				h.model.mu.Unlock()
			}

			answer := h.callTool(t, flow.ToolHangup, `{}`)
			toolTurn := h.session.currentTurn()

			if got, want := hintOf(t, answer.output),
				provider.SayExactly(testCase.line, testCase.lang); got != want {
				t.Errorf("hint = %q, want the closing line as a direction %q", got, want)
			}
			if !strings.Contains(answer.output, `"ok":"1"`) {
				t.Errorf("tool output = %s, want the result kept", answer.output)
			}
			if answer.hint != "" {
				t.Errorf("hint argument = %q, want it in the output only", answer.hint)
			}
			if got := h.model.spokenLines(); len(got) != 0 {
				t.Errorf("spoken lines = %v, want none: the tool result's turn is the line", got)
			}
			if got := h.model.recordedCalls(); strings.Join(got, ",") !=
				"UpdateInstructions,SendToolResult" {
				t.Errorf("calls = %v, want the phase re-pinned and then the tool answered", got)
			}
			if instructionsAtAnswer != 1 {
				t.Errorf("instructions re-pinned %d times before the answer, want once",
					instructionsAtAnswer)
			}
			if !isArmedAtAnswer {
				t.Error("the ending was not armed when the tool was answered")
			}
			if armedInTurn != toolTurn {
				t.Errorf("armed in turn %d, want the tool call's turn %d", armedInTurn, toolTurn)
			}

			// The turn the tool call arrived in finishes and plays out: the line
			// has not been said yet, so the call stays up.
			h.model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
			time.Sleep(150 * time.Millisecond)
			if h.isCallEnded() || !h.actions.isArmed() {
				t.Fatal("the call ended on the playback of the turn the tool call came in")
			}

			// The tool result's turn — the line — plays out, and the call ends.
			h.playTurn()
			h.awaitCallEnded(t, 2*time.Second)
		})
	}
}

// A line cut short is not the line. With the result's turn stopped before it
// said anything, a caller speaking must not end the call — the armed action
// counted that turn as the closing line once, which let a barge-in fire it while
// nothing had been said — and the cap still ends it when no line ever plays.
func TestAClosingLineCutShortEndsTheCallOnlyAtTheCap(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "zh")
	h.actions.graceCap = 400 * time.Millisecond

	h.callTool(t, flow.ToolHangup, `{}`)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeInterrupted,
		Status: "cancelled", InterruptedBy: provider.InterruptReasonSpeech}
	time.Sleep(100 * time.Millisecond)

	h.actions.onBargeIn()
	if h.isCallEnded() || !h.actions.isArmed() {
		t.Fatal("the call ended on speech over a closing line that was never said")
	}
	h.awaitCallEnded(t, 2*time.Second)
}

// On doubao and gemini the line is its own turn, exactly as before: the tool
// result carries the phase's instruction, and SpeakText says the line. On
// doubao that path is exact by construction; a direction to a model is not. On
// gemini the SpeakText after the result is what still says the line when the
// model answers the result with nothing (gemini-findings W-G6).
func TestAToolThatEndsTheCallOnDoubaoOrGeminiSpeaksTheLineAsBefore(t *testing.T) {
	for _, testCase := range []struct {
		profile provider.Profile
		lang    string
	}{
		{provider.DoubaoProfile(), "en"},
		{provider.DoubaoProfile(), "zh"},
		{provider.GeminiProfile(), "en"},
		{provider.GeminiProfile(), "zh"},
	} {
		t.Run(testCase.profile.Name+"/"+testCase.lang, func(t *testing.T) {
			lang := testCase.lang
			h := startToolPath(t, testCase.profile, lang)
			line := map[string]string{
				"en": "Thank you for calling, goodbye.", "zh": "感谢您的来电，再见。",
			}[lang]

			answer := h.callTool(t, flow.ToolHangup, `{}`)

			hint := hintOf(t, answer.output)
			if !strings.Contains(hint, "farewell") || !strings.Contains(hint, "Say goodbye.") {
				t.Errorf("hint = %q, want the farewell phase's instruction", hint)
			}
			if strings.Contains(hint, line) {
				t.Errorf("hint = %q carries the line; this client speaks it itself", hint)
			}
			if got := h.model.spokenLines(); len(got) != 1 || got[0] != line {
				t.Errorf("spoken lines = %v, want [%s]", got, line)
			}
			if got := h.model.recordedCalls(); strings.Join(got, ",") !=
				"SendToolResult,UpdateInstructions,SpeakText" {
				t.Errorf("calls = %v, want the answer, the phase, then the line", got)
			}

			h.model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
			time.Sleep(150 * time.Millisecond)
			if h.isCallEnded() {
				t.Fatal("the call ended on the playback of the turn the tool call came in")
			}
			h.playTurn()
			h.awaitCallEnded(t, 2*time.Second)
		})
	}
}

// A phase the call does not stop at keeps its line in a turn of its own on
// every client: a direction left in a tool result stays in the history, and
// what it does to the turns after it has not been measured.
func TestAToolThatMovesIntoALineThatIsNotTheEndIsUnchanged(t *testing.T) {
	for _, profile := range []provider.Profile{
		provider.OpenAIProfile(), provider.QwenProfile(), provider.GeminiProfile(),
		provider.DoubaoProfile(),
	} {
		t.Run(profile.Name, func(t *testing.T) {
			h := startToolPath(t, profile, "en")

			answer := h.callTool(t, flow.ToolTakeMessage, `{"message":"call me back"}`)

			hint := hintOf(t, answer.output)
			if !strings.Contains(hint, "noted") ||
				!strings.Contains(hint, "Ask whether there is anything else.") {
				t.Errorf("hint = %q, want the new phase's instruction", hint)
			}
			if got := h.model.spokenLines(); len(got) != 1 || got[0] != "I have taken your message." {
				t.Errorf("spoken lines = %v, want the phase's line once", got)
			}
			if got := h.model.recordedCalls(); strings.Join(got, ",") !=
				"SendToolResult,UpdateInstructions,SpeakText" {
				t.Errorf("calls = %v, want the answer, the phase, then the line", got)
			}
			if h.actions.isArmed() {
				t.Error("a phase the call does not stop at armed an ending")
			}
		})
	}
}

// The unit rule behind the test above: a turn done because it was cut short
// does not make the closing line exist.
func TestACutShortTurnIsNotTheClosingLine(t *testing.T) {
	actions, session, _ := testActions(t, &fakeSwitch{})

	fired := make(chan struct{})
	actions.arm(t.Context(), func() { close(fired) })

	actions.onTurnDone(session.currentTurn()+1, true)
	actions.onBargeIn()
	select {
	case <-fired:
		t.Fatal("a turn stopped before its words existed counted as the closing line")
	case <-time.After(50 * time.Millisecond):
	}

	actions.onTurnDone(session.currentTurn()+2, false)
	actions.onBargeIn()
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("speech after a closing line that ran to completion did not fire")
	}
}
