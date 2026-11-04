// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// transferErr is what a transfer answers, after it is recorded as sent;
	// exists and existsErr are what ChannelExists answers (nil exists: up).
	transferErr error
	exists      *bool
	existsErr   error
	existsAsked int
	// prompts are the bot-unavailable prompts ("channel language"), and ops
	// every rescue-related command in the order it was sent.
	prompts []string
	ops     []string
	// onTransfer runs, outside the lock, when a transfer or a prompt is sent.
	onTransfer func()
}

func (s *fakeSwitch) TransferToExtension(channelID, extension, _ string) error {
	s.mu.Lock()
	s.transfers = append(s.transfers, channelID+"→"+extension)
	s.ops = append(s.ops, "transfer "+channelID+"→"+extension)
	hook, err := s.onTransfer, s.transferErr
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (s *fakeSwitch) StampBotLegFailed(channelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.variables == nil {
		s.variables = map[string]string{}
	}
	s.variables["aicc_bot_reason"] = "BOT_LEG_FAILED"
	s.ops = append(s.ops, "stamp "+channelID)
	return nil
}

func (s *fakeSwitch) PlayBotUnavailable(channelID, language string) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, channelID+" "+language)
	s.ops = append(s.ops, "prompt "+channelID+" "+language)
	hook := s.onTransfer
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (s *fakeSwitch) recordedOps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

func (s *fakeSwitch) ChannelExists(string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existsAsked++
	if s.existsErr != nil {
		return false, s.existsErr
	}
	return s.exists == nil || *s.exists, nil
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
	s.ops = append(s.ops, "handOn "+channelID)
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
		ID: uuid.Must(uuid.NewV7()), Name: "support", ExtNumber: "7001", IsEnabled: true,
	}
	closedQueue := catalog.Queue{
		ID: uuid.Must(uuid.NewV7()), Name: "after-hours", ExtNumber: "7002", IsEnabled: false,
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
	actions.onPlaybackDone(session.currentTurn(), true)
	if got := sw.recordedTransfers(); len(got) != 0 {
		t.Fatalf("the transfer ran before the bridge line was spoken: %v", got)
	}

	// The next turn — created by the tool's own result — plays out, and the
	// transfer follows it.
	actions.onPlaybackDone(session.currentTurn()+1, true)
	if got := sw.recordedTransfers(); len(got) != 1 || got[0] != "caller-channel-1→7001" {
		t.Fatalf("transfers = %v, want the caller moved to 7001", got)
	}

	// A second playback must not transfer twice.
	actions.onPlaybackDone(session.currentTurn()+2, true)
	if got := sw.recordedTransfers(); len(got) != 1 {
		t.Errorf("the armed action ran twice: %v", got)
	}
}

// What a live call showed (#25): the model called transfer_to_agent three
// times, each call re-armed the transfer, and it ran on the cap ten seconds
// after the last one. The first ending armed is the call's ending: a repeat —
// or a hangup — in a later turn keeps its turn, its cap and its stamps, and
// the caller is put through exactly once.
func TestARepeatedEndingToolCallKeepsTheFirstArming(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, model := testActions(t, sw)
	const graceCap = 500 * time.Millisecond
	actions.graceCap = graceCap

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(20 * time.Millisecond)
	firstTurn := session.currentTurn()
	armedAt := time.Now()
	if result, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "support", Reason: "BILLING", Summary: "wants a refund",
	}); err != nil || !result.IsOK {
		t.Fatalf("transfer refused: %+v %v", result, err)
	}

	// The next turn calls the tool again, then hangs up for good measure.
	time.Sleep(200 * time.Millisecond)
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(50 * time.Millisecond)
	if session.currentTurn() <= firstTurn {
		t.Fatal("the fixture did not move on to a later turn")
	}
	if result, err := actions.TransferToAgent(t.Context(), flow.TransferRequest{
		Queue: "support", Reason: "OTHER", Summary: "asked again",
	}); err != nil || !result.IsOK {
		t.Fatalf("the repeated transfer was not answered as done: %+v %v", result, err)
	}
	// The caller is being put through: a hangup is refused, not promised.
	if result, err := actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil ||
		result.IsOK || result.Error != "CALL_ENDING" || result.Hint == "" {
		t.Fatalf("the hangup over an armed transfer was not refused: %+v %v", result, err)
	}

	actions.mu.Lock()
	armedInTurn := actions.armedInTurn
	actions.mu.Unlock()
	if armedInTurn != firstTurn {
		t.Errorf("armed in turn %d, want the first call's turn %d", armedInTurn, firstTurn)
	}
	if got := sw.variable("aicc_bot_summary"); got != "wants a refund" {
		t.Errorf("summary variable = %q, want the first call's", got)
	}

	// Nothing plays out, so the cap runs it — counted from the first call.
	deadline := time.Now().Add(2 * time.Second)
	for len(sw.recordedTransfers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if elapsed := time.Since(armedAt); elapsed >= graceCap+200*time.Millisecond {
		t.Errorf("the cap ran %v after the first call, want about %v: it was restarted",
			elapsed, graceCap)
	}
	time.Sleep(graceCap)
	if got := sw.recordedTransfers(); len(got) != 1 || got[0] != "caller-channel-1→7001" {
		t.Errorf("transfers = %v, want the caller put through once", got)
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

	actions.onPlaybackDone(session.currentTurn()+1, true)
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

	actions.onPlaybackDone(session.currentTurn()+1, true)

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
	actions.onBargeIn(0)
	select {
	case <-fired:
		t.Fatal("the action ran before the closing line was even generated")
	case <-time.After(50 * time.Millisecond):
	}

	// The closing line finishes generating in the next turn...
	actions.onTurnDone(session.currentTurn()+1, false, true)
	// ...and the caller talks over the tail of it.
	actions.onBargeIn(0)

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("speech after the closing line did not fire the armed action")
	}
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

	recorder := newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	actions.recorder = recorder

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.drive(t.Context(), session, runtime, actions, recorder, sessionBudget{}, log)
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
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}

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

// A fatal provider error releases the caller and records the call as FAILED
// with the media-or-provider cause.
func TestAFatalProviderErrorIsRecordedAsAMediaOrProviderFailure(t *testing.T) {
	for _, testCase := range []struct {
		name string
		want string
	}{
		{"a failure", "MEDIA_OR_PROVIDER_FAILURE"},
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
			recorder := newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
			actions.recorder = recorder

			done := make(chan struct{})
			go func() {
				defer close(done)
				o.drive(t.Context(), session, runtime, actions, recorder, sessionBudget{}, log)
			}()

			model.events <- provider.Event{
				Type: provider.EventTypeError, IsFatal: true,
				Text: "the session ended", Err: errors.New("the session ended"),
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
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
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
	actions.onPlaybackDone(turnBefore, true)
	if !actions.isArmed() {
		t.Fatal("the call ended on the playback of a turn that preceded the line")
	}
	actions.onPlaybackDone(turnBefore+1, true)
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
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
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
	actions.onPlaybackDone(turnBefore+1, true)
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

	o.afterMove(engine.OnToolResult(flow.ToolTransferToAgent, nil,
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
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
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

	o.handleDeadAir(session, runtime, actions, false, nil, log)

	if !engine.IsTerminal() {
		t.Fatal("the test flow did not move on the silence")
	}
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Errorf("spoken lines = %v, want the phase's own line once", got)
	}
	if got := model.spokenLinesClosing(); len(got) != 1 || !got[0] {
		t.Errorf("a terminal phase's line was not asked for as one that ends the call: %v", got)
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

	o.handleDeadAir(session, runtime, actions, false, nil, log)

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

	o.handleDeadAir(session, runtime, actions, false, nil, log)

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

//
// A goodbye in words that nothing hung up on (#147).
//

const farewellBackstopFlow = `{
	"id": "farewell-backstop-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {"persona": "You answer the phone.", "closingTarget": "finish"},
	"nodes": {
		"welcome": {"instruction": "Greet.", "tools": ["hangup"]},
		"finish": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true,
			"announce": {"en": "Sorry to have bothered you, goodbye.", "zh": "不好意思打扰了，再见。"}}
	}
}`

func TestAGoodbyeWithoutHangupThenSilenceClosesTheCallWithoutAskingTheModel(t *testing.T) {
	o, session, model, engine, runtime, actions, log := deadAirHarness(t, farewellBackstopFlow)
	model.answerLinesWithATurn(session)

	var claims claimWatch
	claims.onBotSaid(1, "Alright, thanks for your time. Goodbye.")
	if found := claims.onTurnDone(1, false, false); len(found) != 1 {
		t.Fatalf("claims = %+v, want the unbacked farewell", found)
	}

	o.handleDeadAir(session, runtime, actions, claims.hasFarewellWithoutHangup(), &claims, log)

	if !engine.IsTerminal() {
		t.Fatal("the call did not move to the closing target")
	}
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Sorry to have bothered you, goodbye." {
		t.Errorf("spoken lines = %v, want the closing line once", got)
	}
	if got := model.recordedUserText(); len(got) != 0 {
		t.Errorf("cues sent = %v, want none: the model is not asked anything", got)
	}
	if !actions.isArmed() {
		t.Error("the closing target did not arm the ending")
	}
}

func TestSilenceAfterAnOrdinaryTurnStillSendsTheCue(t *testing.T) {
	o, session, model, engine, runtime, actions, log := deadAirHarness(t, farewellBackstopFlow)

	var claims claimWatch
	claims.onBotSaid(1, "What is your account number?")
	claims.onTurnDone(1, false, false)

	o.handleDeadAir(session, runtime, actions, claims.hasFarewellWithoutHangup(), &claims, log)

	if engine.IsTerminal() || actions.isArmed() {
		t.Fatal("an ordinary question ended the call")
	}
	if got := model.recordedUserText(); len(got) != 1 || !strings.Contains(got[0], "still there") {
		t.Errorf("cues sent = %v, want the re-engagement cue once", got)
	}
}

func TestAFarewellFollowedByAnotherTurnOrAToolCallIsNotActedOn(t *testing.T) {
	var claims claimWatch
	claims.onBotSaid(1, "Goodbye.")
	claims.onTurnDone(1, false, false)
	if !claims.hasFarewellWithoutHangup() {
		t.Fatal("the unbacked farewell was not remembered")
	}
	claims.onBotSaid(2, "Actually, one more thing: what is your account number?")
	claims.onTurnDone(2, false, false)
	if claims.hasFarewellWithoutHangup() {
		t.Error("a later ordinary turn did not take the farewell back")
	}

	claims.onBotSaid(3, "Goodbye.")
	claims.onTurnDone(3, false, false)
	claims.onToolCall()
	if claims.hasFarewellWithoutHangup() {
		t.Error("a tool call did not take the farewell back")
	}

	// A goodbye the platform asked for (an armed ending) or one the tool backs
	// is not unbacked in the first place.
	claims.onBotSaid(4, "Goodbye.")
	claims.onTurnDone(4, false, true)
	claims.onBotSaid(5, "Goodbye.")
	claims.onTurnDone(5, true, false)
	if claims.hasFarewellWithoutHangup() {
		t.Error("a backed goodbye counted as unhung")
	}
}

func TestTheFarewellBackstopLeavesAnArmedEndingACampaignAndAFlowWithoutATargetAlone(t *testing.T) {
	t.Run("armed", func(t *testing.T) {
		o, session, model, engine, runtime, actions, log := deadAirHarness(t, farewellBackstopFlow)
		actions.arm(t.Context(), func() {})
		o.handleDeadAir(session, runtime, actions, true, nil, log)
		if engine.IsTerminal() {
			t.Error("the backstop moved a call whose ending is already armed")
		}
		if len(model.recordedUserText()) != 1 {
			t.Error("the cue was not sent")
		}
	})
	t.Run("campaign", func(t *testing.T) {
		o, session, model, engine, runtime, actions, log := deadAirHarness(t, farewellBackstopFlow)
		actions.campaignID = "c-1"
		o.handleDeadAir(session, runtime, actions, true, nil, log)
		if engine.IsTerminal() {
			t.Error("the backstop moved a campaign call")
		}
		if len(model.recordedUserText()) != 1 {
			t.Error("the cue was not sent")
		}
	})
	t.Run("no closing target", func(t *testing.T) {
		o, session, model, engine, runtime, actions, log := deadAirHarness(t, `{
			"id": "no-target", "specVersion": "v2", "initialNode": "welcome",
			"global": {"persona": "You answer the phone."},
			"nodes": {"welcome": {"instruction": "Greet.", "tools": []}}
		}`)
		o.handleDeadAir(session, runtime, actions, true, nil, log)
		if engine.IsTerminal() {
			t.Error("the backstop moved a flow with nowhere to go")
		}
		if len(model.recordedUserText()) != 1 {
			t.Error("the cue was not sent")
		}
	})
}

//
// A caller's decline never ended the call, only the model calling hangup did
// (issue #9): the maxTurnsWithoutTool wall closes the call through the same
// terminal-phase path as any other ending, once the reply that crossed it has
// been heard.
//

// wallFlow is a two-reply wall into a closing phase; the announce is
// substituted in, so a test can drop it.
const wallFlow = `{
	"id": "wall-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"closingTarget": "farewell",
		"maxTurnsWithoutTool": 2
	},
	"nodes": {
		"welcome": {"instruction": "Answer questions.", "tools": []},
		"farewell": {"instruction": "Say goodbye.", ANNOUNCE
			"tools": [], "isTerminal": true}
	}
}`

func wallHarness(t *testing.T, hasAnnounce bool) (*Orchestrator, *Session,
	*fakeModel, *flow.Engine, *flow.Runtime, *callActions, *slog.Logger, *fakeSwitch) {
	t.Helper()
	announce := ""
	if hasAnnounce {
		announce = `"announce": "Thank you for calling, goodbye.",`
	}
	sw := &fakeSwitch{}
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(strings.Replace(wallFlow, "ANNOUNCE", announce, 1)))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)
	model.answerLinesWithATurn(session)
	return o, session, model, engine, runtime, actions, log, sw
}

// replyInTurn is one exchange the wall counts, as drive would see it: the
// caller's words, then turn n finishing without a tool call.
func replyInTurn(runtime *flow.Runtime, wall *turnsWithoutToolWatch, n int) {
	runtime.OnCallerSpoke("no")
	wall.onTurnDone(n, false, runtime)
}

// The wall is crossed when the replies exceed it, and the call moves only once
// the caller has heard the reply that crossed it. The move is any terminal
// arrival's: the ending is armed before the closing line is asked for, the line
// is asked for as one that ends the call, and the call ends when the line has
// been heard.
func TestTheTurnsWithoutToolWallClosesOnceTheCrossingReplyIsHeard(t *testing.T) {
	o, session, model, engine, runtime, actions, log, sw := wallHarness(t, true)
	wall := turnsWithoutToolWatch{toolCallTurn: -1}

	replyInTurn(runtime, &wall, 1)
	replyInTurn(runtime, &wall, 2)
	o.handlePlaybackDone(2, true, &wall, session, runtime, actions, log)
	if engine.IsTerminal() {
		t.Fatal("two replies moved the call; the wall is crossed only when they exceed two")
	}

	replyInTurn(runtime, &wall, 3)
	if engine.IsTerminal() || len(model.spokenLines()) != 0 {
		t.Fatal("the wall moved the call before the reply that crossed it was heard")
	}

	turnBefore := session.currentTurn()
	o.handlePlaybackDone(3, true, &wall, session, runtime, actions, log)
	if !engine.IsTerminal() {
		t.Fatal("the caller heard the crossing reply and the call did not close")
	}
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken lines = %v, want the closing announce once", got)
	}
	if got := model.spokenLinesClosing(); len(got) != 1 || !got[0] {
		t.Errorf("the closing announce was not asked for as a line that ends the call: %v", got)
	}
	if got := model.recordedUserText(); len(got) != 0 {
		t.Errorf("cues sent = %v, want none: the announce is the goodbye", got)
	}
	if !actions.isArmed() || actions.armedInTurn != turnBefore {
		t.Fatalf("armed=%v in turn %d, want armed in turn %d, before the announce's own turn",
			actions.isArmed(), actions.armedInTurn, turnBefore)
	}

	o.handlePlaybackDone(turnBefore+1, true, &wall, session, runtime, actions, log)
	if actions.isArmed() {
		t.Error("the caller heard the announce and the call stayed open")
	}
	if got := sw.variable("aicc_bot_finished"); got != "FLOW_END" {
		t.Errorf("aicc_bot_finished = %q, want FLOW_END", got)
	}
}

// A closing phase with no line of its own gets the goodbye cue, as a dead-air
// move into one does; without it the model has nothing to answer and the call
// ends on the grace cap in silence.
func TestTheTurnsWithoutToolWallAsksForAGoodbyeWhereTheClosingPhaseHasNoLine(t *testing.T) {
	o, session, model, engine, runtime, actions, log, _ := wallHarness(t, false)
	wall := turnsWithoutToolWatch{toolCallTurn: -1}

	for n := 1; n <= 3; n++ {
		replyInTurn(runtime, &wall, n)
	}
	o.handlePlaybackDone(3, true, &wall, session, runtime, actions, log)

	if !engine.IsTerminal() || !actions.isArmed() {
		t.Fatalf("terminal=%v armed=%v, want the call closing", engine.IsTerminal(), actions.isArmed())
	}
	if got := model.spokenLines(); len(got) != 0 {
		t.Errorf("spoken lines = %v, want none", got)
	}
	if got := model.recordedUserText(); len(got) != 1 || !strings.Contains(got[0], "goodbye") {
		t.Errorf("cues sent = %v, want the goodbye cue once", got)
	}
}

// Whatever follows the crossing reply decides again. A tool call puts the
// count back at zero and nothing waits for anybody's playback; a further
// tool-less reply — the caller talked over the crossing one and was answered —
// is the one the caller must hear before the goodbye.
func TestALaterTurnSupersedesTheCrossingReply(t *testing.T) {
	t.Run("a tool call", func(t *testing.T) {
		o, session, _, engine, runtime, actions, log, _ := wallHarness(t, true)
		wall := turnsWithoutToolWatch{toolCallTurn: -1}
		for n := 1; n <= 3; n++ {
			replyInTurn(runtime, &wall, n)
		}
		// The caller asks for something a tool does, in turn 4.
		runtime.OnCallerSpoke("actually, one more thing")
		wall.toolCallTurn = 4
		wall.onTurnDone(4, false, runtime)

		o.handlePlaybackDone(3, true, &wall, session, runtime, actions, log)
		o.handlePlaybackDone(4, true, &wall, session, runtime, actions, log)
		if engine.IsTerminal() {
			t.Fatal("the wall closed a call whose conversation moved on to a tool")
		}
	})

	t.Run("a further reply", func(t *testing.T) {
		o, session, model, engine, runtime, actions, log, _ := wallHarness(t, true)
		wall := turnsWithoutToolWatch{toolCallTurn: -1}
		for n := 1; n <= 3; n++ {
			replyInTurn(runtime, &wall, n)
		}
		// Turn 3 was talked over — its playback never completes — and the
		// caller's words got turn 4.
		replyInTurn(runtime, &wall, 4)

		o.handlePlaybackDone(3, true, &wall, session, runtime, actions, log)
		if engine.IsTerminal() {
			t.Fatal("the superseded reply's playback moved the call")
		}
		o.handlePlaybackDone(4, true, &wall, session, runtime, actions, log)
		if !engine.IsTerminal() || len(model.spokenLines()) != 1 {
			t.Fatal("the reply the caller did hear did not close the call")
		}
	})
}

// drive wires the live events to the wall: final transcripts give the next
// turn a caller to answer, and a completed turn's playback is where the move
// is made.
func TestDriveClosesAToolLessLoopThroughTheWall(t *testing.T) {
	sw := &fakeSwitch{}
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(strings.Replace(wallFlow, "ANNOUNCE",
		`"announce": "Thank you for calling, goodbye.",`, 1)))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	recorder := newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	actions.recorder = recorder
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	go o.drive(t.Context(), session, runtime, actions, recorder, sessionBudget{}, log)

	for range 3 {
		model.events <- provider.Event{Type: provider.EventTypeInputTranscript,
			Text: "no", IsFinal: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples)}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(model.spokenLines()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken lines = %v, want the closing announce once", got)
	}
	if got := model.spokenLinesClosing(); !got[0] {
		t.Error("the closing announce was not asked for as a line that ends the call")
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
	if !strings.Contains(cfg.Instructions, "Greet the caller.") {
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

// global.temperature travels in the session configuration; a flow that sets
// none leaves it nil so the provider's default stands.
func TestTheFlowsTemperatureReachesTheSessionConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	build := func(src string) provider.SessionConfig {
		spec, err := flow.Load([]byte(src))
		if err != nil {
			t.Fatalf("load flow: %v", err)
		}
		engine := flow.NewEngine(spec, "en", nil, log)
		runtime := flow.NewRuntime(engine, &callActions{log: log}, flow.NewBackend(""), nil, log)
		return sessionConfigFor(spec, runtime, "en")
	}
	if got := build(driveFlow).Temperature; got != nil {
		t.Errorf("temperature = %v, want nil for a flow that names none", *got)
	}
	withTemperature := strings.Replace(driveFlow, `"global": {`, `"global": {"temperature": 0.4,`, 1)
	if got := build(withTemperature).Temperature; got == nil || *got != 0.4 {
		t.Errorf("temperature = %v, want 0.4", got)
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
	recorder := newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	actions.recorder = recorder

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.drive(t.Context(), session, runtime, actions, recorder, sessionBudget{}, log)
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
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}

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

		actions.onPlaybackDone(session.currentTurn()+1, true)
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
			{ID: uuid.Must(uuid.NewV7()), Name: "support-en", ExtNumber: "7001", IsEnabled: true},
		}}

		result, err := actions.TransferToAgent(t.Context(),
			flow.TransferRequest{Queue: "support-en", Summary: "wants a person"})
		if err != nil || !result.IsOK {
			t.Fatalf("transfer refused: %+v %v", result, err)
		}
		actions.onPlaybackDone(session.currentTurn()+1, true)

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
		actions.onPlaybackDone(session.currentTurn()+1, true)
		<-session.done

		if got := sw.variable(marker); got != "" {
			t.Errorf("stamped a channel that does not exist: %q", got)
		}
	})
}

// The other deliberate ending: the flow itself concludes, by any route. It is
// containment for the ledger, exactly as if the model had called the hangup
// tool (this path forgot to say so once, and every farewell-ended call reported
// uncontained), and it has to look deliberate to the dialplan too, or every
// flow that ends by design drops its caller into a queue.
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
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	o.afterMove(engine.OnNoInput(), session, runtime, actions, log)
	actions.onPlaybackDone(session.currentTurn()+1, true)
	<-session.done

	if got := sw.variable("aicc_bot_finished"); got != "FLOW_END" {
		t.Errorf("aicc_bot_finished = %q, want FLOW_END", got)
	}
	actions.recorder.mu.Lock()
	endReason := actions.recorder.endReason
	actions.recorder.mu.Unlock()
	if endReason != "HANGUP" {
		t.Errorf("endReason = %q, want HANGUP — a flow-concluded call must count as contained", endReason)
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
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now().Add(-7*time.Second), nil)

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

	actions.onPlaybackDone(session.currentTurn()+1, true)
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

	actions.onPlaybackDone(session.currentTurn()+1, true)
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
	leg     *fakeLeg
	model   *fakeModel
	actions *callActions
	engine  *flow.Engine
	done    chan struct{}
}

func startToolPath(t *testing.T, profile provider.Profile, lang string) *toolPath {
	t.Helper()
	return startToolPathWith(t, profile, lang, closingLineFlow)
}

func startToolPathWith(t *testing.T, profile provider.Profile, lang, flowJSON string) *toolPath {
	t.Helper()
	return startToolPathBackend(t, profile, lang, flowJSON, "")
}

// startToolPathBackend is startToolPathWith for a flow whose tools call a
// business backend at backendURL.
func startToolPathBackend(t *testing.T, profile provider.Profile, lang, flowJSON,
	backendURL string) *toolPath {
	t.Helper()
	return startToolPathFull(t, profile, lang, flowJSON, backendURL, nil, io.Discard)
}

// startToolPathFull is startToolPathBackend with the engine's starting slots
// and a sink for the call's log. configure, if given, adjusts the orchestrator
// before the call is driven.
func startToolPathFull(t *testing.T, profile provider.Profile, lang, flowJSON,
	backendURL string, slots map[string]any, logSink io.Writer,
	configure ...func(*Orchestrator)) *toolPath {
	t.Helper()
	// The session asks the actions whether the ending is armed, as the
	// orchestrator wires it; the actions exist only after the session does.
	var actionsRef atomic.Pointer[callActions]
	session, leg, model := startBridgeWith(t, profile, Config{
		BargeGuard: -1, NoInput: -1,
		IsEndingArmed: func() bool {
			a := actionsRef.Load()
			return a != nil && a.isArmed()
		},
	})
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, &fakeSwitch{})
	o.cfg.Profile = profile
	for _, apply := range configure {
		apply(o)
	}
	log := slog.New(slog.NewTextHandler(logSink, nil))
	spec, err := flow.Load([]byte(flowJSON))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, lang, slots, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	recorder := newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	actions.recorder = recorder
	actionsRef.Store(actions)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(backendURL), nil, log)

	h := &toolPath{session: session, leg: leg, model: model, actions: actions,
		engine: engine, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		o.drive(t.Context(), session, runtime, actions, recorder, sessionBudget{}, log)
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

// callTools has the model call several tools in one response, as fc_1, fc_2
// and so on, and waits for all of them to be answered. The response is left
// open: endResponse ends it.
func (h *toolPath) callTools(t *testing.T, calls ...[2]string) []toolResult {
	t.Helper()
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	for i, call := range calls {
		h.model.events <- provider.Event{
			Type: provider.EventTypeToolCall, ToolCallID: "fc_" + strconv.Itoa(i+1),
			ToolName: call[0], ToolArgs: call[1],
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if results := h.model.recordedToolResults(); len(results) == len(calls) {
			time.Sleep(50 * time.Millisecond)
			return results
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the calls were answered %d times, want %d",
		len(h.model.recordedToolResults()), len(calls))
	return nil
}

// endResponse ends the response the tool calls came in, and gives the drive
// goroutine the moment it needs to follow it up.
func (h *toolPath) endResponse() {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	time.Sleep(50 * time.Millisecond)
}

// playTurn has the model produce one whole turn of speech.
func (h *toolPath) playTurn() {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2),
	}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
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
			h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
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
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
	time.Sleep(100 * time.Millisecond)

	h.actions.onBargeIn(0)
	if h.isCallEnded() || !h.actions.isArmed() {
		t.Fatal("the call ended on speech over a closing line that was never said")
	}
	h.awaitCallEnded(t, 2*time.Second)
}

// The closing line plays behind the rest of the turn that carried the tool
// call, which can go on speaking for seconds after it. The cap started at
// arming cut the line's tail off (issue #24): once the line has been
// generated, the cap covers the audio queued ahead of it, and the call ends
// when the caller has heard the line, not before.
func TestAClosingLineQueuedBehindTheToolCallsTurnIsHeardBeforeTheCap(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 500 * time.Millisecond

	// The turn keeps speaking after the tool call, and its audio is still
	// queued when the line — the tool result's turn — finishes generating.
	h.callTool(t, flow.ToolHangup, `{}`)
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2),
	}
	h.leg.holdFrames(100)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.playTurn()

	time.Sleep(700 * time.Millisecond)
	if h.isCallEnded() || !h.actions.isArmed() {
		t.Fatal("the call ended on the cap counted from arming while the line was still queued")
	}

	h.leg.holdFrames(0)
	h.awaitCallEnded(t, 500*time.Millisecond)
}

// A line that was generated but never finishes playing still ends the call:
// the moved cap covers the queued audio and a margin, and no more.
func TestAClosingLineThatNeverFinishesPlayingStillEndsTheCall(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 100 * time.Millisecond

	h.callTool(t, flow.ToolHangup, `{}`)
	h.leg.holdFrames(5)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.playTurn()

	time.Sleep(lineDrainMargin / 2)
	if h.isCallEnded() {
		t.Fatal("the call ended before the queued line and its margin could play")
	}
	h.awaitCallEnded(t, lineDrainMargin)
}

// On doubao the line is its own turn: the tool result carries the phase's
// instruction, and SpeakText says the line. That path is exact by construction;
// a direction to a model is not.
func TestAToolThatEndsTheCallOnDoubaoSpeaksTheLineAsItsOwnTurn(t *testing.T) {
	for _, testCase := range []struct {
		profile provider.Profile
		lang    string
	}{
		{provider.DoubaoProfile(), "en"},
		{provider.DoubaoProfile(), "zh"},
	} {
		t.Run(testCase.profile.Name+"/"+testCase.lang, func(t *testing.T) {
			lang := testCase.lang
			h := startToolPath(t, testCase.profile, lang)
			line := map[string]string{
				"en": "Thank you for calling, goodbye.", "zh": "感谢您的来电，再见。",
			}[lang]

			answer := h.callTool(t, flow.ToolHangup, `{}`)

			hint := hintOf(t, answer.output)
			if !strings.Contains(hint, "Say goodbye.") {
				t.Errorf("hint = %q, want the farewell phase's instruction", hint)
			}
			if strings.Contains(hint, "farewell") {
				t.Errorf("hint = %q names the node; the model is told no phase's id", hint)
			}
			if strings.Contains(hint, line) {
				t.Errorf("hint = %q carries the line; this client speaks it itself", hint)
			}
			// The line waits for the response the call came in to end.
			if got := h.model.spokenLines(); len(got) != 0 {
				t.Errorf("spoken lines = %v before the response ended, want none", got)
			}

			h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
			time.Sleep(150 * time.Millisecond)
			if got := h.model.spokenLines(); len(got) != 1 || got[0] != line {
				t.Errorf("spoken lines = %v, want [%s]", got, line)
			}
			if got := h.model.recordedCalls(); strings.Join(got, ",") !=
				"UpdateInstructions,SendToolResult,SpeakText" {
				// The phase first: the answer is what asks the model for its
				// turn, and that turn must run under the new phase (#147).
				t.Errorf("calls = %v, want the phase, the answer, then the line", got)
			}
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
		provider.OpenAIProfile(), provider.QwenProfile(),
		provider.DoubaoProfile(),
	} {
		t.Run(profile.Name, func(t *testing.T) {
			h := startToolPath(t, profile, "en")

			answer := h.callTool(t, flow.ToolTakeMessage, `{"message":"call me back"}`)
			h.endResponse()

			hint := hintOf(t, answer.output)
			if !strings.Contains(hint, "Ask whether there is anything else.") {
				t.Errorf("hint = %q, want the new phase's instruction", hint)
			}
			if strings.Contains(hint, "noted") {
				t.Errorf("hint = %q names the node; the model is told no phase's id", hint)
			}
			if got := h.model.spokenLines(); len(got) != 1 || got[0] != "I have taken your message." {
				t.Errorf("spoken lines = %v, want the phase's line once", got)
			}
			// The call goes on from this phase, so the line is not one that
			// ends it — which is what keeps it out of qwen's history.
			if got := h.model.spokenLinesClosing(); len(got) != 1 || got[0] {
				t.Errorf("a non-terminal line was asked for as one that ends the call: %v", got)
			}
			if got := h.model.recordedCalls(); strings.Join(got, ",") !=
				"UpdateInstructions,SendToolResult,SpeakText" {
				// The phase first: the answer is what asks the model for its
				// turn, and that turn must run under the new phase (#147).
				t.Errorf("calls = %v, want the phase, the answer, then the line", got)
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

	actions.onTurnDone(session.currentTurn()+1, true, true)
	actions.onBargeIn(0)
	select {
	case <-fired:
		t.Fatal("a turn stopped before its words existed counted as the closing line")
	case <-time.After(50 * time.Millisecond):
	}

	actions.onTurnDone(session.currentTurn()+2, false, true)
	actions.onBargeIn(0)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("speech after a closing line that ran to completion did not fire")
	}
}

//
// Several tools in one response.
//

// parallelToolsFlow moves on take_message into a phase that allows no tool,
// with a line of its own.
const parallelToolsFlow = `{
	"id": "parallel-tools-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {"persona": "You answer the phone."},
	"nodes": {
		"welcome": {
			"instruction": "Greet the caller.",
			"tools": ["take_message", "hangup"],
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "take_message", "target": "noted"}
			]
		},
		"noted": {
			"instruction": "Ask whether there is anything else.",
			"announce": {"en": "I have taken your message.", "zh": "您的留言已记录。"},
			"tools": []
		}
	}
}`

// A model can ask for two tools in one response. Both were asked for in the
// phase the response was made in, so the first one's move must not get the
// second refused as out of phase (issue #12). And the new phase's line waits
// until the response is over and both are answered: said at the move, it would
// pre-empt a response that still owed an answer.
func TestTheSecondToolOfOneResponseIsNotRefusedByTheFirstsMove(t *testing.T) {
	for _, profile := range []provider.Profile{
		provider.OpenAIProfile(), provider.QwenProfile(), provider.DoubaoProfile(),
	} {
		t.Run(profile.Name, func(t *testing.T) {
			h := startToolPathWith(t, profile, "en", parallelToolsFlow)

			answers := h.callTools(t,
				[2]string{flow.ToolTakeMessage, `{"message":"call me back"}`},
				[2]string{flow.ToolHangup, `{}`})

			for _, answer := range answers {
				if !strings.Contains(answer.output, `"ok":"1"`) {
					t.Errorf("%s answered %s, want ok=1", answer.id, answer.output)
				}
			}
			if h.engine.NodeID() != "noted" {
				t.Errorf("node = %q, want noted", h.engine.NodeID())
			}
			if got := h.model.spokenLines(); len(got) != 0 {
				t.Errorf("spoken lines = %v while the response was still open, want none", got)
			}

			h.endResponse()
			if got := h.model.spokenLines(); len(got) != 1 || got[0] != "I have taken your message." {
				t.Errorf("spoken lines = %v, want the phase's line once", got)
			}
			if got := h.model.recordedCalls(); strings.Join(got, ",") !=
				"UpdateInstructions,SendToolResult,SendToolResult,SpeakText" {
				t.Errorf("calls = %v, want both answers ahead of the line", got)
			}
		})
	}
}

// When two tools of one response each move the call, the last move wins, and
// its phase's line is the one said: the goodbye, in the result on a Realtime
// profile and as a line of its own elsewhere, never the earlier phase's line.
func TestTheLastMoveOfOneResponseWins(t *testing.T) {
	const goodbye = "Thank you for calling, goodbye."
	for _, testCase := range []struct {
		profile       provider.Profile
		isLineInReply bool
	}{
		{provider.OpenAIProfile(), true},
		{provider.QwenProfile(), true},
		{provider.DoubaoProfile(), false},
	} {
		t.Run(testCase.profile.Name, func(t *testing.T) {
			h := startToolPath(t, testCase.profile, "en")

			answers := h.callTools(t,
				[2]string{flow.ToolTakeMessage, `{"message":"call me back"}`},
				[2]string{flow.ToolHangup, `{}`})
			h.endResponse()

			if h.engine.NodeID() != "farewell" {
				t.Fatalf("node = %q, want farewell", h.engine.NodeID())
			}
			if !h.actions.isArmed() {
				t.Error("the call's ending was not armed")
			}
			var want []string
			if testCase.isLineInReply {
				if got := hintOf(t, answers[1].output); got != provider.SayExactly(goodbye, "en") {
					t.Errorf("the second answer's hint = %q, want the goodbye", got)
				}
			} else {
				want = []string{goodbye}
			}
			if got := h.model.spokenLines(); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("spoken lines = %v, want %v", got, want)
			}
		})
	}
}

//
// Keypresses once the ending is armed.
//

// pressKey presses a key on the caller's phone and gives the digit pump the
// moment it needs to act on it.
func (h *toolPath) pressKey(t *testing.T, digit string) {
	t.Helper()
	h.leg.digits <- digit
	deadline := time.Now().Add(2 * time.Second)
	for len(h.leg.digits) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
}

// What a call showed: the caller pressed 0, the transfer was armed, and five
// more presses each cut the closing line off and got a turn of their own, so
// the line was said in fragments, said again, and the transfer waited for the
// last full one. Once the ending is armed a key is recorded and nothing else:
// the line plays out, and the action runs when the caller has heard it.
func TestAKeypressAfterTheEndingIsArmedLeavesTheClosingLineAlone(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 5 * time.Second
	fired := make(chan struct{})

	h.callTool(t, flow.ToolHangup, `{}`)
	if !h.actions.isArmed() {
		t.Fatal("the hangup did not arm the ending")
	}
	h.actions.mu.Lock()
	armedAction := h.actions.armed
	h.actions.armed = func() { armedAction(); close(fired) }
	h.actions.mu.Unlock()
	cues := len(h.model.recordedUserText())
	// The hangup itself muted the turn that made it (#441): one flush, before
	// any key.
	clearsBefore := h.leg.clears()

	// Before the closing line has spoken: the turn the tool result asked for
	// is being made and has no audio yet.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	time.Sleep(20 * time.Millisecond)
	h.pressKey(t, "0")

	// And while it plays.
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2*4),
	}
	h.leg.holdFrames(100)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	time.Sleep(50 * time.Millisecond)
	h.pressKey(t, "0")
	h.pressKey(t, "0")

	if got := h.model.recordedInterrupts(); len(got) != 0 {
		t.Errorf("a keypress interrupted the closing line: %+v", got)
	}
	if got := h.model.recordedUserText(); len(got) != cues {
		t.Errorf("a keypress asked the model for a turn: %v", got[cues:])
	}
	if h.leg.clears() != clearsBefore {
		t.Error("a keypress flushed the closing line")
	}
	select {
	case <-fired:
		t.Fatal("a keypress ran the armed action before the line was heard")
	default:
	}

	h.leg.holdFrames(0)
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the armed action did not run once the closing line was heard")
	}
}

// Speech is another matter: a caller who talks after the closing line has said
// what they had to say, and the armed action runs now.
func TestSpeechAfterTheClosingLineStillRunsTheArmedAction(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 5 * time.Second

	h.callTool(t, flow.ToolHangup, `{}`)
	// 40 frames is 800 ms of the line still to play: inside closingTailAck.
	h.leg.holdFrames(40)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.playTurn()
	time.Sleep(100 * time.Millisecond)
	if h.isCallEnded() {
		t.Fatal("the call ended before the caller spoke")
	}

	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	h.awaitCallEnded(t, 2*time.Second)
}

// Speech over the last second of the closing line is the caller answering the
// goodbye, and ends the call at once, as above, with the tail measured before
// the barge-in flush empties the queue.
func TestSpeechOverTheLastSecondOfTheClosingLineEndsTheCallAtOnce(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 5 * time.Second

	h.callTool(t, flow.ToolHangup, `{}`)
	h.leg.holdFrames(50) // 1 s still to play
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.playTurn()
	time.Sleep(100 * time.Millisecond)

	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	h.awaitCallEnded(t, 2*time.Second)
}

// The call that failed live (#147): the caller asks a new question 2.3 s into
// a nine-second goodbye. That is an interruption, not an acknowledgement. The
// line is cut off as any barge-in does, the ending stays armed, the call stays
// up, and the playback of the answer to the question is what ends it.
func TestSpeechEarlyInTheClosingLineIsAnsweredBeforeTheCallEnds(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 5 * time.Second

	h.callTool(t, flow.ToolHangup, `{}`)
	h.leg.holdFrames(350) // 7 s still to play
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.playTurn()
	time.Sleep(100 * time.Millisecond)

	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	time.Sleep(200 * time.Millisecond)
	if h.isCallEnded() {
		t.Fatal("the call ended on a question asked over the start of the closing line")
	}
	if !h.actions.isArmed() {
		t.Fatal("the ending was dropped; it must stay armed until the answer is heard")
	}
	if got := h.model.recordedInterrupts(); len(got) != 1 {
		t.Fatalf("interrupts = %+v, want the barge-in's flush and truncate to have happened", got)
	}

	// The model answers the question; the caller hears it; the call ends.
	h.leg.holdFrames(0)
	h.playTurn()
	h.awaitCallEnded(t, 2*time.Second)
}

// The answer to an early interruption is itself a closing line: a caller who
// answers it with "ok, bye" over its last second ends the call, and one who
// cuts in on its start is answered again.
func TestTheSameRuleHoldsOverTheAnswerToAnInterruption(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 5 * time.Second

	h.callTool(t, flow.ToolHangup, `{}`)
	h.leg.holdFrames(350)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.playTurn()
	time.Sleep(100 * time.Millisecond)
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	time.Sleep(100 * time.Millisecond)

	// The answer, long; the caller cuts in again early.
	h.leg.holdFrames(300)
	h.playTurn()
	time.Sleep(100 * time.Millisecond)
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	time.Sleep(200 * time.Millisecond)
	if h.isCallEnded() || !h.actions.isArmed() {
		t.Fatal("the second early interruption ended the call")
	}

	// The next answer is short, and the caller acknowledges its last second.
	h.leg.holdFrames(40)
	h.playTurn()
	time.Sleep(100 * time.Millisecond)
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	h.awaitCallEnded(t, 2*time.Second)
}

// The cap is the floor under all of it: when the answer to the interruption
// never reports that it was heard, the call still ends. The cap starts over at
// the interruption (so the answer has time to be made), and the answer's
// TURN_DONE moves it to cover the audio queued.
func TestAnInterruptedClosingLineStillEndsAtTheCapWhenTheAnswerNeverFinishesPlaying(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")
	h.actions.graceCap = 300 * time.Millisecond

	h.callTool(t, flow.ToolHangup, `{}`)
	h.leg.holdFrames(350)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.playTurn()
	time.Sleep(100 * time.Millisecond)
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	time.Sleep(100 * time.Millisecond)
	if h.isCallEnded() {
		t.Fatal("the call ended on the interruption")
	}

	// The answer generates; its playback is stuck, so only the cap can end it.
	h.leg.holdFrames(5)
	h.playTurn()
	time.Sleep(lineDrainMargin / 2)
	if h.isCallEnded() {
		t.Fatal("the call ended before the answer and its margin could play")
	}
	h.awaitCallEnded(t, lineDrainMargin+time.Second)
}

// The unit rule: only the tail still to play decides.
func TestOnlyTheUnplayedTailDecidesWhetherSpeechIsAnAcknowledgement(t *testing.T) {
	actions, session, _ := testActions(t, &fakeSwitch{})
	fired := make(chan struct{})
	actions.arm(t.Context(), func() { close(fired) })
	actions.onTurnDone(session.currentTurn()+1, false, true)

	actions.onBargeIn(closingTailAck + time.Millisecond)
	select {
	case <-fired:
		t.Fatal("speech over a long tail ran the armed action")
	case <-time.After(50 * time.Millisecond):
	}
	if actions.isLineSpoken {
		t.Fatal("the interrupted line still counts as said")
	}
	// Speech again before an answer exists changes nothing.
	actions.onBargeIn(0)
	select {
	case <-fired:
		t.Fatal("speech before any answer ran the armed action")
	case <-time.After(50 * time.Millisecond):
	}

	actions.onTurnDone(session.currentTurn()+2, false, true)
	actions.onBargeIn(closingTailAck)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("speech over the last closingTailAck did not run the armed action")
	}
}

// Before the ending is armed a keypress takes the floor and reaches the model,
// as it always did.
func TestAKeypressBeforeTheEndingIsArmedStillInterrupts(t *testing.T) {
	h := startToolPath(t, provider.QwenProfile(), "en")

	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2*4),
	}
	h.leg.holdFrames(100)
	time.Sleep(50 * time.Millisecond)
	h.pressKey(t, "2")

	got := h.model.recordedInterrupts()
	if len(got) != 1 || got[0].reason != provider.InterruptReasonDTMF {
		t.Errorf("interrupts = %+v, want the keypress to have cut in", got)
	}
	if len(h.model.recordedUserText()) != 1 {
		t.Errorf("user text = %v, want the keypress cue", h.model.recordedUserText())
	}
}

// The human path writes the row for a call an agent placed, and reads the
// bot's part of it off the agent's channel: the bot hanging up has to leave
// its tally there first.
func TestABotThatEndsAnAgentsCallLeavesItsShareOnTheirChannel(t *testing.T) {
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)
	actions.facts = testFacts()
	actions.facts.callType = callTypeOutbound
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now().Add(-9*time.Second), nil)
	actions.recorder.markUnbackedClaim("TRANSFER")

	actions.markFinished("HANGUP")

	if got := sw.variable("aicc_bot_finished"); got != "HANGUP" {
		t.Errorf("aicc_bot_finished = %q, want HANGUP", got)
	}
	if got, err := strconv.Atoi(sw.variable("aicc_bot_sec")); err != nil || got < 9 {
		t.Errorf("aicc_bot_sec = %q, want the seconds the agent was with the bot", sw.variable("aicc_bot_sec"))
	}
	if got := sw.variable("aicc_bot_unbacked_claims"); got != "TRANSFER" {
		t.Errorf("aicc_bot_unbacked_claims = %q, want TRANSFER", got)
	}
}

// A caller from outside has no one to hand the bot's tally to when the bot
// ends the call: it stamps aicc_bot_sec only at a handover, and nowhere else.
func TestABotThatEndsAnInboundCallStampsNoTally(t *testing.T) {
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)
	actions.facts = testFacts()
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now().Add(-9*time.Second), nil)

	actions.markFinished("HANGUP")

	if got := sw.variable("aicc_bot_sec"); got != "" {
		t.Errorf("aicc_bot_sec = %q on an inbound call the bot ended, want none", got)
	}
}

// stampBotShare is what an agent's call carries from the start, so the flow
// survives the agent hanging up first.
func TestTheBotShareOfAnAgentsCallNamesTheFlow(t *testing.T) {
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)
	facts := testFacts()
	actions.stampBotShare(facts)
	if sw.variable("aicc_flow_slug") != "novanet_support" || sw.variable("aicc_did") != "95012" ||
		sw.variable("aicc_flow_id") != facts.flowID.String() {
		t.Errorf("share stamped = %v", sw.variables)
	}
	if got := sw.variable("aicc_bot_sec"); got != "" {
		t.Errorf("aicc_bot_sec = %q: stamping the share must not read as a handover", got)
	}
}

// The flow goes onto the caller's channel as the conversation begins, on every
// call the bot answers, not only at a hand-over: the human path also writes
// the row of a call whose bot session died with the process, and that row named
// no flow though the bot had run one (#313). An agent's call gets the whole
// share, as before; none of it may read as a hand-over.
func TestTheFlowIsOnTheCallersChannelFromTheStart(t *testing.T) {
	for _, tc := range []struct {
		name             string
		callType         callType
		placedToCustomer bool
		wantWholeShare   bool
	}{
		{"an inbound caller", callTypeInbound, false, false},
		{"an AI outbound customer", callTypeOutbound, true, false},
		{"an agent's call to a bot number", callTypeOutbound, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := &fakeSwitch{}
			facts := testFacts()
			facts.callType, facts.isPlacedToCustomer = tc.callType, tc.placedToCustomer
			actions := &callActions{
				orchestrator:  testOrchestrator(t, sw),
				log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
				callerChannel: "caller-channel-1",
				facts:         facts,
			}

			actions.stampAtStart()

			if sw.variable("aicc_flow_id") != facts.flowID.String() || sw.variable("aicc_flow_slug") != "novanet_support" {
				t.Errorf("stamped = %v, want the flow's id and slug", sw.variables)
			}
			// The DID and the language are the dialplan's to set on a caller's
			// channel, and the originate's on a customer's.
			if got, want := sw.variable("aicc_did") != "", tc.wantWholeShare; got != want {
				t.Errorf("aicc_did stamped = %v, want %v (stamped = %v)", got, want, sw.variables)
			}
			if got := sw.variable("aicc_bot_sec"); got != "" {
				t.Errorf("aicc_bot_sec = %q: a stamp at the start must not read as a handover", got)
			}
		})
	}

	t.Run("a call with no caller channel", func(t *testing.T) {
		sw := &fakeSwitch{}
		actions := &callActions{
			orchestrator: testOrchestrator(t, sw),
			log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			facts:        testFacts(),
		}
		actions.stampAtStart()
		if len(sw.variables) != 0 {
			t.Errorf("stamped %v with no channel to stamp", sw.variables)
		}
	})
}

//
// A tool result that changes what the phase's instruction says.
//

// lookupInPlaceFlow has one phase whose instruction quotes slots its own tool
// fills, and no rule that leaves the phase (issue #48).
const lookupInPlaceFlow = `{
	"id": "lookup-in-place-test",
	"specVersion": "v2",
	"initialNode": "report",
	"global": {"persona": "You answer the phone."},
	"tools": {
		"lookup_order": {
			"description": "Look an order up.",
			"parameters": {"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"]},
			"http": {
				"path": "/order",
				"body": {"id": "{args.id}"},
				"successWhen": {"path": "retCode", "equals": "000000"},
				"result": {"device": "data.device", "note": "data.note"}
			}
		}
	},
	"nodes": {
		"report": {
			"instruction": "The order is a {slots.lookup_order.device}.",
			"tools": ["lookup_order"]
		}
	}
}`

// callToolNumber has the model call a tool and waits until the wanted number
// of tool results have been sent back.
func (h *toolPath) callToolNumber(t *testing.T, n int, name, args string) {
	t.Helper()
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_" + strconv.Itoa(n),
		ToolName: name, ToolArgs: args,
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.model.recordedToolResults()) >= n {
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tool call %d (%s) was never answered", n, name)
}

// A lookup that stays in its phase but overwrites a slot the phase's
// instruction quotes re-pins the instructions, ahead of the answer, so the turn
// the answer asks for already runs under the new value. One that changes
// nothing the instruction shows sends no extra update.
func TestAToolResultThatChangesThePhasesInstructionRepinsItBeforeTheAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &body)
		devices := map[any]string{"1": "phone", "2": "tablet", "3": "tablet"}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"retCode": "000000",
			"data":    map[string]any{"device": devices[body["id"]], "note": body["id"]},
		})
	}))
	defer server.Close()

	h := startToolPathBackend(t, provider.OpenAIProfile(), "en", lookupInPlaceFlow, server.URL)

	h.callToolNumber(t, 1, "lookup_order", `{"id":"1"}`)
	if got := h.engine.NodeID(); got != "report" {
		t.Fatalf("node = %q, want the call to stay in report", got)
	}
	if got := strings.Join(h.model.recordedCalls(), ","); got != "UpdateInstructions,SendToolResult" {
		t.Fatalf("calls = %s, want the instructions re-pinned and then the tool answered", got)
	}
	h.model.mu.Lock()
	pinned := append([]string(nil), h.model.instructions...)
	h.model.mu.Unlock()
	if len(pinned) != 1 || !strings.Contains(pinned[0], "The order is a phone.") {
		t.Fatalf("instructions = %q, want one update carrying the looked-up device", pinned)
	}
	if h.actions.isArmed() || len(h.model.spokenLines()) != 0 {
		t.Error("a result that did not change the phase armed an ending or said a line")
	}

	// Another order: the device changes, so the instructions follow.
	h.callToolNumber(t, 2, "lookup_order", `{"id":"2"}`)
	if got := strings.Join(h.model.recordedCalls(), ","); got !=
		"UpdateInstructions,SendToolResult,UpdateInstructions,SendToolResult" {
		t.Fatalf("calls = %s, want a re-pin ahead of the second answer", got)
	}
	h.model.mu.Lock()
	pinned = append([]string(nil), h.model.instructions...)
	h.model.mu.Unlock()
	if len(pinned) != 2 || !strings.Contains(pinned[1], "The order is a tablet.") {
		t.Fatalf("instructions = %q, want the second update to carry the tablet", pinned)
	}

	// A third order with the same device: only a slot nobody quotes moves.
	h.callToolNumber(t, 3, "lookup_order", `{"id":"3"}`)
	if got := strings.Join(h.model.recordedCalls(), ","); got !=
		"UpdateInstructions,SendToolResult,UpdateInstructions,SendToolResult,SendToolResult" {
		t.Fatalf("calls = %s, want the third answer with no instruction update", got)
	}
}

// The deployment's dead-air timeout (AICC_BOT_NO_INPUT_TIMEOUT) reaches each
// call's session, and a non-default value changes when NO_INPUT fires.
func TestTheOrchestratorPassesTheDeadAirTimeoutToTheSession(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	unset := testOrchestrator(t, &fakeSwitch{}).sessionConfig(provider.SessionConfig{}, nil, log)
	if unset.NoInput != 0 {
		t.Errorf("unset NoInput = %s, want 0 (the session's 8 s default)", unset.NoInput)
	}

	o := testOrchestrator(t, &fakeSwitch{})
	o.cfg.NoInput = 150 * time.Millisecond
	cfg := o.sessionConfig(provider.SessionConfig{}, nil, log)
	if cfg.NoInput != 150*time.Millisecond {
		t.Fatalf("session NoInput = %s, want 150ms", cfg.NoInput)
	}

	// With the default (8 s) nothing would fire inside this test; the
	// configured value does, from the config the orchestrator built.
	cfg.BargeGuard = -1
	session, _, model := startBridgeWith(t, provider.OpenAIProfile(), cfg)
	awaitBridgeEvent(t, session, EventTypeReady)
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	awaitBridgeEvent(t, session, EventTypePlaybackDone)
	awaitBridgeEvent(t, session, EventTypeNoInput)
}

// The model answers an interruption in two turns, and the cap has to cover the
// last of them (#147: the second answer was clipped by a cap that only the
// first had moved).
func TestTheCapCoversEveryAnswerToAnInterruptedClosingLine(t *testing.T) {
	actions, session, _ := testActions(t, &fakeSwitch{})
	leg := session.leg.(*fakeLeg)
	actions.graceCap = 200 * time.Millisecond
	fired := make(chan struct{})
	actions.arm(t.Context(), func() { close(fired) })
	armedTurn := session.currentTurn()

	// The closing line finishes generating; the caller cuts in early on it.
	leg.holdFrames(400)
	actions.onTurnDone(armedTurn+1, false, true)
	actions.onBargeIn(closingTailAck + time.Second)
	leg.holdFrames(0)

	// Answer one is short, answer two is long.
	leg.holdFrames(5)
	actions.onTurnDone(armedTurn+2, false, true)
	leg.holdFrames(300) // 6 s queued
	actions.onTurnDone(armedTurn+3, false, true)

	actions.mu.Lock()
	due := actions.capDue
	actions.mu.Unlock()
	if want := time.Now().Add(6*time.Second + lineDrainMargin - time.Second); due.Before(want) {
		t.Fatalf("the cap is due in %v; it does not cover the second answer's playout",
			time.Until(due))
	}
	select {
	case <-fired:
		t.Fatal("the action ran before the second answer was heard")
	case <-time.After(400 * time.Millisecond):
	}
	actions.onPlaybackDone(armedTurn+3, true)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("the second answer's PLAYBACK_DONE did not run the armed action")
	}
}

// An ignored transfer or hangup is a conversation, not a success: the model
// must not promise a transfer that will not happen.
func TestAnIgnoredTransferOverAnArmedHangupIsRefused(t *testing.T) {
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)
	if result, err := actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil || !result.IsOK {
		t.Fatalf("hangup = %+v, %v", result, err)
	}
	result, err := actions.TransferToAgent(t.Context(),
		flow.TransferRequest{Queue: "support", Reason: "CALLER_REQUEST"})
	if err != nil || result.IsOK || result.Error != "CALL_ENDING" || result.Hint == "" {
		t.Fatalf("transfer over an armed hangup = %+v, %v; want a CALL_ENDING refusal", result, err)
	}
	// A repeated hangup is the model's own intent and stays a success.
	if result, err := actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil || !result.IsOK {
		t.Fatalf("repeated hangup = %+v, %v", result, err)
	}
	if got := sw.recordedTransfers(); len(got) != 0 {
		t.Errorf("transfers = %v, want none", got)
	}
}

// armInterruptedHangup arms a hangup, finishes the closing line's turn and has
// the caller cut in early on it. It returns the closing line's turn and a
// channel closed when the hangup's action runs.
func armInterruptedHangup(t *testing.T, graceCap time.Duration) (
	*callActions, *Session, int, chan struct{}) {
	t.Helper()
	actions, session, _ := testActions(t, &fakeSwitch{})
	actions.graceCap = graceCap
	fired := make(chan struct{})
	actions.armAs(t.Context(), false, func() { close(fired) })
	armedTurn := session.currentTurn()
	actions.onTurnDone(armedTurn+1, false, true)
	actions.onBargeIn(closingTailAck + time.Second)
	return actions, session, armedTurn, fired
}

func neverFired(t *testing.T, fired chan struct{}, why string) {
	t.Helper()
	select {
	case <-fired:
		t.Fatal(why)
	case <-time.After(60 * time.Millisecond):
	}
}

// #147: the caller asked a question over the closing line and the model
// answered with a bare hangup. The hangup is refused once, the audio-less turn
// does not end the call, and a turn that plays audio does.
func TestABareHangupAfterAnInterruptionIsRefusedOnceAndItsSilentTurnDoesNotEndTheCall(t *testing.T) {
	actions, _, armedTurn, fired := armInterruptedHangup(t, 5*time.Second)

	result, err := actions.Hangup(t.Context(), flow.HangupRequest{})
	if err != nil || result.IsOK || result.Error != "CALL_ENDING" || result.Hint == "" {
		t.Fatalf("hangup = %+v, %v; want a CALL_ENDING refusal", result, err)
	}
	// The audio-less turn that carried the hangup.
	actions.onTurnDone(armedTurn+2, false, true)
	actions.onPlaybackDone(armedTurn+2, false)
	neverFired(t, fired, "an audio-less turn's playback ran the armed ending")

	// The answer, with audio.
	actions.onTurnDone(armedTurn+3, false, true)
	actions.onPlaybackDone(armedTurn+3, true)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("the answer's playback did not run the armed ending")
	}
}

// The refusal is spent once: a model that calls hangup again gets success, and
// the ending runs on the playback of a turn with audio.
func TestTheSecondHangupAfterTheRefusalSucceeds(t *testing.T) {
	actions, _, armedTurn, fired := armInterruptedHangup(t, 5*time.Second)
	if result, _ := actions.Hangup(t.Context(), flow.HangupRequest{}); result.IsOK {
		t.Fatal("the first bare hangup was not refused")
	}
	result, err := actions.Hangup(t.Context(), flow.HangupRequest{})
	if err != nil || !result.IsOK {
		t.Fatalf("second hangup = %+v, %v; want success", result, err)
	}
	actions.onPlaybackDone(armedTurn+2, false)
	neverFired(t, fired, "an audio-less turn's playback ran the armed ending")
	actions.onPlaybackDone(armedTurn+3, true)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("a turn with audio did not run the armed ending")
	}
}

// A hangup in a turn that has spoken an answer is not bare, so it is not
// refused.
func TestAHangupAfterASpokenAnswerIsNotRefused(t *testing.T) {
	actions, session, armedTurn, _ := armInterruptedHangup(t, 5*time.Second)
	session.mu.Lock()
	session.audioTurns++ // the answer finished generating with audio
	session.mu.Unlock()
	actions.onTurnDone(armedTurn+2, false, true)
	if result, err := actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil || !result.IsOK {
		t.Fatalf("hangup after a spoken answer = %+v, %v; want success", result, err)
	}
}

// Without an interruption nothing changes: a repeated hangup succeeds and the
// audio-less playback of the line's own turn still runs the ending.
func TestARepeatedHangupWithoutAnInterruptionKeepsToday(t *testing.T) {
	actions, session, _ := testActions(t, &fakeSwitch{})
	fired := make(chan struct{})
	actions.armAs(t.Context(), false, func() { close(fired) })
	armedTurn := session.currentTurn()
	if result, err := actions.Hangup(t.Context(), flow.HangupRequest{}); err != nil || !result.IsOK {
		t.Fatalf("repeated hangup = %+v, %v; want success", result, err)
	}
	actions.onTurnDone(armedTurn+1, false, true)
	actions.onPlaybackDone(armedTurn+1, false)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("playback without an interruption did not run the armed ending")
	}
}

// The cap is the floor: after the refusal the model never speaks, and the
// refusal restarted the cap so the call ends on it.
func TestAfterTheRefusalAModelThatNeverSpeaksEndsAtTheCap(t *testing.T) {
	actions, _, armedTurn, fired := armInterruptedHangup(t, 300*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if result, _ := actions.Hangup(t.Context(), flow.HangupRequest{}); result.IsOK {
		t.Fatal("the bare hangup was not refused")
	}
	actions.onPlaybackDone(armedTurn+2, false)
	// 200 ms in, the interruption's own cap would fire at 300 ms; the refusal
	// restarted it, so it is not due yet.
	time.Sleep(150 * time.Millisecond)
	neverFired(t, fired, "the cap was not restarted by the refusal")
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("the cap did not end the call")
	}
}
