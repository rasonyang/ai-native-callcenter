// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

//
// Unbacked claims through the whole drive loop (#44): from the model's words to
// the switch's channel and the ledger row, over the fakes.
//

// orderedSwitch records what reached the switch in the order it arrived, so a
// test can say a stamp went out before the caller was moved.
type orderedSwitch struct {
	mu  sync.Mutex
	ops []string
}

func (s *orderedSwitch) record(op string) {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	s.mu.Unlock()
}

func (s *orderedSwitch) TransferToExtension(channelID, extension, _ string) error {
	s.record("transfer " + channelID + "→" + extension)
	return nil
}

func (s *orderedSwitch) SetVariable(channelID, name, value string) error {
	s.record("set " + channelID + " " + name + "=" + value)
	return nil
}

func (s *orderedSwitch) EndCallerWithTheirBridge(channelID string) error {
	s.record("handOn " + channelID)
	return nil
}

func (s *orderedSwitch) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ops)
}

func (s *orderedSwitch) indexOf(prefix string) int {
	return slices.IndexFunc(s.recorded(), func(op string) bool {
		return strings.HasPrefix(op, prefix)
	})
}

// syncBuffer is a log sink the drive goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) count(needle string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), needle)
}

// claimsFlow lets the model transfer, hang up and take a message from its
// first phase, which is all these scenarios need.
const claimsFlow = `{
	"id": "claims-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["transfer_to_agent", "hangup", "take_message"]
	},
	"nodes": {
		"welcome": {"instruction": "Help the caller.", "tools": []}
	}
}`

const unbackedClaimLog = "the bot claimed an action no tool call backs"

type claimsHarness struct {
	o        *Orchestrator
	session  *Session
	leg      *fakeLeg
	model    *fakeModel
	runtime  *flow.Runtime
	actions  *callActions
	recorder *callRecorder
	sw       *orderedSwitch
	logs     *syncBuffer
	queueID  uuid.UUID
	done     chan struct{}
}

// newClaimsHarness starts drive on a call whose caller channel is chan-9 and
// whose only open queue is "support" on 7001.
func newClaimsHarness(t *testing.T) *claimsHarness {
	t.Helper()
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	h := &claimsHarness{session: session, leg: leg, model: model,
		sw: &orderedSwitch{}, logs: &syncBuffer{}, queueID: uuid.New(),
		done: make(chan struct{})}
	log := slog.New(slog.NewTextHandler(h.logs, nil))

	o, err := NewOrchestrator(OrchestratorConfig{
		Catalog: &fakeCatalog{queues: []catalog.Queue{
			{ID: h.queueID, Name: "support", ExtNumber: "7001", IsEnabled: true},
		}},
		Flows:   fakeFlows{},
		Switch:  h.sw,
		Profile: provider.OpenAIProfile(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	h.o = o

	spec, err := flow.Load([]byte(claimsFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "zh", nil, log)
	h.recorder = newCallRecorder(uuid.New(), time.Now(), nil)
	h.actions = &callActions{orchestrator: o, session: session, log: log,
		callerChannel: "chan-9", recorder: h.recorder, facts: testFacts()}
	h.runtime = flow.NewRuntime(engine, h.actions, flow.NewBackend(""), nil, log)

	go func() {
		defer close(h.done)
		o.drive(t.Context(), session, h.runtime, h.actions, h.recorder, sessionBudget{}, log)
		// What handle does once drive returns: whatever was waiting for a
		// closing line will never hear it.
		h.actions.disarm()
	}()
	return h
}

// turn plays one model turn to completion: what it says ("" is nothing) and
// the tool it calls after the words ("" is none), as a Realtime response
// delivers them — the transcript of its speech lands before the call does.
func (h *claimsHarness) turn(text, tool, args string) {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	if text != "" {
		h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
			Text: text, IsFinal: true}
	}
	if tool != "" {
		h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
			ToolCallID: "fc_" + tool, ToolName: tool, ToolArgs: args}
	}
	if text != "" {
		h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples)}
	}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
}

func (h *claimsHarness) callerSays(text string) {
	h.model.events <- provider.Event{Type: provider.EventTypeInputTranscript,
		Text: text, IsFinal: true}
}

// awaitToolResults waits until n tool calls have been answered.
func (h *claimsHarness) awaitToolResults(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.model.recordedToolResults()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%d tool calls answered, want %d", len(h.model.recordedToolResults()), n)
}

// awaitEnd waits for drive to return and writes the row the bot owns.
func (h *claimsHarness) awaitEnd(t *testing.T) store.CDR {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("drive did not return after the call ended")
	}
	ledger := newFakeLedger()
	h.recorder.finish(ledger, testFacts(), discard())
	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d cdrs, want 1", len(ledger.cdrs))
	}
	return ledger.cdrs[0]
}

// A transfer the bot announced a turn before it made one: the announcement is
// one unbacked claim, the lines backed by the real transfer add nothing, and
// the claim travels on the caller's channel ahead of the caller, so the row
// the human path writes carries it too.
func TestDriveRecordsAClaimedTransferAndCarriesItToTheHandover(t *testing.T) {
	h := newClaimsHarness(t)

	h.turn("好的，我马上为您转接人工客服。", "", "")
	h.callerSays("好的")
	h.turn("", flow.ToolTransferToAgent,
		`{"queue":"support","reason":"AGENT","summary":"asked for a person"}`)
	h.awaitToolResults(t, 1)
	h.turn("正在为您转接，请稍候。", "", "")

	cdr := h.awaitEnd(t)

	if got := h.logs.count(unbackedClaimLog); got != 1 {
		t.Errorf("logged %d unbacked claims, want 1: the announcement only", got)
	}
	if !slices.Equal(cdr.UnbackedClaims, []string{store.UnbackedClaimTransfer}) {
		t.Errorf("unbackedClaims = %v, want [TRANSFER]", cdr.UnbackedClaims)
	}
	if cdr.QueueID == nil || *cdr.QueueID != h.queueID {
		t.Errorf("queueId = %v, want the queue the caller was handed to", cdr.QueueID)
	}
	if cdr.IsContained {
		t.Error("a transferred call was marked contained")
	}

	stamp := h.sw.indexOf("set chan-9 aicc_bot_unbacked_claims=TRANSFER")
	moved := h.sw.indexOf("transfer chan-9→7001")
	if stamp < 0 || moved < 0 {
		t.Fatalf("switch saw %v, want the claims stamped and the caller moved", h.sw.recorded())
	}
	if stamp > moved {
		t.Errorf("the claims were stamped after the caller moved: %v", h.sw.recorded())
	}
}

// A lookup promised and never made, on a call the caller then leaves: the row
// the bot writes says so, and names no queue.
func TestDriveRecordsAnUnbackedLookupOnACallTheBotKeeps(t *testing.T) {
	h := newClaimsHarness(t)

	h.turn("好的，维修单号是 RMA1002，我帮您查询一下。", "", "")
	time.Sleep(50 * time.Millisecond) // let drive judge the turn before the leg goes
	h.leg.Stop()

	cdr := h.awaitEnd(t)

	if !slices.Equal(cdr.UnbackedClaims, []string{store.UnbackedClaimLookup}) {
		t.Errorf("unbackedClaims = %v, want [LOOKUP]", cdr.UnbackedClaims)
	}
	if cdr.QueueID != nil {
		t.Errorf("queueId = %v on a call nobody transferred", cdr.QueueID)
	}
	if cdr.IsContained {
		t.Error("a caller who left was marked contained")
	}
	if h.sw.indexOf("set chan-9 aicc_bot_unbacked_claims") >= 0 {
		t.Errorf("claims were stamped on a call never handed over: %v", h.sw.recorded())
	}
}

// Claims a tool does back are never flagged: words that arrive before the
// tool call of their own turn, the reply to a tool result, and a goodbye
// while the hangup is armed and the reply to it was cut short.
func TestDriveDoesNotFlagClaimsAToolBacks(t *testing.T) {
	h := newClaimsHarness(t)

	// The words first, the call after, in one response.
	h.turn("Let me check that for you, and I'll pass on your message.",
		flow.ToolTakeMessage, `{"message":"call me back"}`)
	h.awaitToolResults(t, 1)
	// The reply to the result.
	h.turn("I've passed on your message.", "", "")
	h.callerSays("thanks, bye")
	// Goodbye in the same turn as the hangup.
	h.turn("Goodbye!", flow.ToolHangup, `{"isFarewellSpoken":true}`)
	h.awaitToolResults(t, 2)
	// The reply to the hangup is cut short, so the next turn is not a reply
	// to anything — only the armed hangup backs its goodbye.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeInterrupted, Status: "cancelled"}
	h.turn("Thank you for calling, goodbye.", "", "")

	cdr := h.awaitEnd(t)

	if got := h.logs.count(unbackedClaimLog); got != 0 {
		t.Errorf("logged %d unbacked claims on a call whose every claim a tool backed", got)
	}
	if len(cdr.UnbackedClaims) != 0 {
		t.Errorf("unbackedClaims = %v, want none", cdr.UnbackedClaims)
	}
	if !cdr.IsContained {
		t.Error("a call the bot closed with hangup was not contained")
	}
}

// A transfer the tool accepted that never reached the switch names no queue:
// the caller left during the closing line, and a queue on the row counted the
// call as a queue call answered inside the SLA.
func TestDriveNamesNoQueueForATransferThatNeverRan(t *testing.T) {
	h := newClaimsHarness(t)

	h.turn("", flow.ToolTransferToAgent,
		`{"queue":"support","reason":"AGENT","summary":"asked for a person"}`)
	h.awaitToolResults(t, 1)
	h.leg.Stop()

	cdr := h.awaitEnd(t)

	if cdr.QueueID != nil {
		t.Errorf("queueId = %v for a transfer that never ran", cdr.QueueID)
	}
	if cdr.IsContained {
		t.Error("a call the bot decided to transfer was marked contained")
	}
	// The armed transfer was retired with the call; nothing left behind moves
	// the caller afterwards.
	time.Sleep(50 * time.Millisecond)
	if h.sw.indexOf("transfer ") >= 0 {
		t.Errorf("the caller was moved after the call ended: %v", h.sw.recorded())
	}
	if len(cdr.UnbackedClaims) != 0 {
		t.Errorf("unbackedClaims = %v, want none", cdr.UnbackedClaims)
	}
}
