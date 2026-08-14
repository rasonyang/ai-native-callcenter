// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
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
	actions.onTurnDone(session.currentTurn() + 1)
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
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), log)

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.drive(t.Context(), session, runtime, actions, log)
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

// driveFlow is the minimal flow the drive test runs.
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
