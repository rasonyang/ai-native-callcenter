// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

type fakeLedger struct {
	mu        sync.Mutex
	cdrs      []store.CDR
	callbacks []store.Callback
}

func newFakeLedger() *fakeLedger { return &fakeLedger{} }

func (f *fakeLedger) InsertCDR(_ context.Context, cdr store.CDR) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cdrs = append(f.cdrs, cdr)
	return nil
}

func (f *fakeLedger) InsertCallback(_ context.Context, callID, queueID *uuid.UUID, phone, message string) (store.Callback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cb := store.Callback{ID: uuid.New(), CallID: callID, QueueID: queueID,
		PhoneNumber: phone, Message: message, Status: store.CallbackStatusOpen}
	f.callbacks = append(f.callbacks, cb)
	return cb, nil
}

func testFacts() *callFacts {
	flowID := uuid.New()
	return &callFacts{
		callType: callTypeInbound, language: "zh", fromNumber: "13800138000",
		did: "95012", flowID: &flowID, flowSlug: "novanet_support",
		isRecordingEnabled: true,
	}
}

// fakeTranscripts captures what the transcript actor writes, which is where
// the transcript lives now that the recorder posts rather than accumulates.
type fakeTranscripts struct {
	mu    sync.Mutex
	lines map[uuid.UUID][]store.TranscriptLine
}

func newFakeTranscripts() *fakeTranscripts {
	return &fakeTranscripts{lines: map[uuid.UUID][]store.TranscriptLine{}}
}

func (f *fakeTranscripts) InsertTranscriptLine(_ context.Context, callID uuid.UUID, line store.TranscriptLine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines[callID] = append(f.lines[callID], line)
	return nil
}

func (f *fakeTranscripts) get(callID uuid.UUID) []store.TranscriptLine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.TranscriptLine(nil), f.lines[callID]...)
}

// recorderWithTranscript builds a recorder over a real actor, so tests exercise
// the ordering the actor owns rather than a stand-in for it.
func recorderWithTranscript(t *testing.T, callID uuid.UUID, startedAt time.Time) (*callRecorder, *fakeTranscripts, func()) {
	t.Helper()
	lines := newFakeTranscripts()
	reg := transcript.NewRegistry(lines, nil, discard())
	actor := reg.For(callID, "INBOUND", startedAt)
	var once sync.Once
	flush := func() { once.Do(func() { reg.Close(callID) }) }
	t.Cleanup(flush)
	return newCallRecorder(callID, startedAt, actor), lines, flush
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A call the bot finished itself is contained, and the ledger says so.
func TestAHangupCallWritesAContainedCDRAndTheTranscript(t *testing.T) {
	ledger := newFakeLedger()
	callID := uuid.New()
	recorder, transcripts, flushTranscript := recorderWithTranscript(t, callID, time.Now().Add(-30*time.Second))

	recorder.say(store.SpeakerBot, "感谢致电 NovaNet")
	recorder.say(store.SpeakerCustomer, "帮我查个问题")
	recorder.toolCall("hangup", "{}")
	recorder.toolResult("hangup", `{"ok":"1"}`, "")
	recorder.markHangup()

	recorder.finish(ledger, testFacts(), discard())

	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d cdrs, want 1", len(ledger.cdrs))
	}
	cdr := ledger.cdrs[0]
	if cdr.CallID != callID || cdr.CallType != "INBOUND" || cdr.Language != "zh" {
		t.Errorf("cdr identity = %+v", cdr)
	}
	if !cdr.IsContained {
		t.Error("a call the bot closed properly was not marked contained")
	}
	if cdr.Status != store.CDRStatusAnswered || cdr.HangupCause != "NORMAL_CLEARING" {
		t.Errorf("status=%s cause=%s", cdr.Status, cdr.HangupCause)
	}
	if cdr.BotSec < 29 || cdr.TotalSec < 29 {
		t.Errorf("durations botSec=%d totalSec=%d, want ~30", cdr.BotSec, cdr.TotalSec)
	}
	if len(cdr.Legs) != 1 || cdr.Legs[0].Kind != "BOT" || cdr.Legs[0].Label != "novanet_support" {
		t.Errorf("legs = %+v", cdr.Legs)
	}

	flushTranscript()
	entries := transcripts.get(callID)
	if len(entries) != 4 {
		t.Fatalf("transcript has %d entries, want 4", len(entries))
	}
	// Order is the conversation's own, and seq is dense from 1.
	for i, e := range entries {
		if e.Seq != i+1 {
			t.Errorf("entry %d has seq %d", i, e.Seq)
		}
	}
	if entries[0].Speaker != store.SpeakerBot || entries[0].Kind != store.TranscriptKindText {
		t.Errorf("first entry = %+v", entries[0])
	}
	if entries[2].Kind != store.TranscriptKindToolCall {
		t.Errorf("third entry kind = %s", entries[2].Kind)
	}
}

// A transferred call is not finished: the human path owns the one CDR, and the
// bot writes only what it alone saw — the transcript.
func TestATransferredCallStillWritesTheRowItKnows(t *testing.T) {
	ledger := newFakeLedger()
	callID := uuid.New()
	queueID := uuid.New()
	recorder, transcripts, flushTranscript := recorderWithTranscript(t, callID, time.Now())

	recorder.say(store.SpeakerCustomer, "转人工")
	recorder.markTransferred("", "")
	recorder.markHandedOver(queueID)

	recorder.finish(ledger, testFacts(), discard())

	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d cdrs for a call it handed away, want the one it knows about",
			len(ledger.cdrs))
	}
	cdr := ledger.cdrs[0]
	if cdr.QueueID == nil || *cdr.QueueID != queueID {
		t.Errorf("queueId = %v, want the queue the caller was handed to", cdr.QueueID)
	}
	if cdr.Status != store.CDRStatusAnswered {
		t.Errorf("status = %s, want ANSWERED — the bot did answer this call", cdr.Status)
	}
	if cdr.IsContained {
		t.Error("a call handed to a person was marked contained")
	}
	flushTranscript()
	if len(transcripts.get(callID)) != 1 {
		t.Error("the transcript was lost with the transfer")
	}
}

// A transfer the tool accepted but the switch was never told to make — the
// caller hung up during the closing line, or the model hung up over it — is
// not a queue call. Written with the queue, the row counted in the queue's
// figures as answered with no wait (fujiezee on #44; 01a0bc55 and 01a0bd1e
// on the dev stack).
func TestATransferNeverHandedOverNamesNoQueue(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)

	recorder.markTransferred("", "")
	recorder.finish(ledger, testFacts(), discard())

	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d cdrs", len(ledger.cdrs))
	}
	cdr := ledger.cdrs[0]
	if cdr.QueueID != nil {
		t.Errorf("queueId = %v for a caller who never reached the queue", *cdr.QueueID)
	}
	if cdr.IsContained {
		t.Error("a call the bot decided to hand on was marked contained")
	}
}

// A transfer the caller hung up on leaves the bot's row as the only one, so it
// carries the summary and reason the bot produced, under the keys the human
// path uses, beside the business data the call was placed with. Before, the
// row had only the business data and the summary was lost (01a0f651 and
// 01a0f664 on the dev stack).
func TestATransferNeverHandedOverKeepsTheBotsSummary(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	facts := testFacts()
	facts.userData = map[string]any{"orderId": "A-1001"}

	recorder.markTransferred("Caller asked to be transferred.", "Caller requested a transfer")
	recorder.finish(ledger, facts, discard())

	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d cdrs", len(ledger.cdrs))
	}
	got := ledger.cdrs[0].UserData
	want := map[string]any{
		"orderId":    "A-1001",
		"botSummary": "Caller asked to be transferred.",
		"botReason":  "Caller requested a transfer",
	}
	if !maps.Equal(got, want) {
		t.Errorf("userData = %v, want %v", got, want)
	}
	if len(facts.userData) != 1 {
		t.Errorf("the call's own userData was changed: %v", facts.userData)
	}
}

// A call that never decided to transfer writes the business data it was placed
// with and nothing of the bot's.
func TestARowWithoutATransferAddsNoSummary(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)

	recorder.markHangup()
	recorder.finish(ledger, testFacts(), discard())

	if got := ledger.cdrs[0].UserData; got != nil {
		t.Errorf("userData = %v, want none", got)
	}
}

// A claim the bot made with no tool call behind it reaches the row, each kind
// once and in a stable order.
func TestUnbackedClaimsReachTheRow(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)

	recorder.markUnbackedClaim(store.UnbackedClaimTransfer)
	recorder.markUnbackedClaim(store.UnbackedClaimLookup)
	recorder.markUnbackedClaim(store.UnbackedClaimTransfer)
	recorder.markHangup()
	recorder.finish(ledger, testFacts(), discard())

	got := ledger.cdrs[0].UnbackedClaims
	want := []string{store.UnbackedClaimLookup, store.UnbackedClaimTransfer}
	if !slices.Equal(got, want) {
		t.Errorf("unbackedClaims = %v, want %v", got, want)
	}

	clean := newFakeLedger()
	newCallRecorder(uuid.New(), time.Now(), nil).finish(clean, testFacts(), discard())
	if claims := clean.cdrs[0].UnbackedClaims; claims != nil {
		t.Errorf("a call with no claims wrote %v", claims)
	}
}

// A caller who hangs up mid-conversation is answered but not contained.
func TestACallerHangupIsAnsweredButNotContained(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	recorder.say(store.SpeakerBot, "你好")

	recorder.finish(ledger, testFacts(), discard()) // no end reason: caller left

	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d cdrs", len(ledger.cdrs))
	}
	if ledger.cdrs[0].IsContained {
		t.Error("a caller abandonment was marked contained")
	}
	if ledger.cdrs[0].Status != store.CDRStatusAnswered {
		t.Errorf("status = %s", ledger.cdrs[0].Status)
	}
}

// A failed conversation is a FAILED row with its cause.
func TestAFailedCallWritesAFailedCDR(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	recorder.markFailed("MEDIA_OR_PROVIDER_FAILURE")

	recorder.finish(ledger, testFacts(), discard())

	cdr := ledger.cdrs[0]
	if cdr.Status != store.CDRStatusFailed || cdr.HangupCause != "MEDIA_OR_PROVIDER_FAILURE" {
		t.Errorf("status=%s cause=%s", cdr.Status, cdr.HangupCause)
	}
	if cdr.IsContained {
		t.Error("a failure was marked contained")
	}
}

// A nil ledger writes nothing and panics nowhere.
func TestANilLedgerIsANoOp(t *testing.T) {
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	recorder.say(store.SpeakerBot, "hello")
	recorder.finish(nil, testFacts(), discard())
}

// take_message persists an OPEN callback, defaulting the number to the caller,
// and announces the saved row to the event stream.
func TestTakeMessagePersistsAndAnnouncesACallback(t *testing.T) {
	ledger := newFakeLedger()
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)
	actions.orchestrator.cfg.Ledger = ledger
	actions.recorder = newCallRecorder(uuid.New(), time.Now(), nil)
	actions.facts = testFacts()
	var announced []store.Callback
	actions.orchestrator.cfg.AnnounceCallback = func(cb store.Callback) {
		announced = append(announced, cb)
	}

	result, err := actions.TakeMessage(t.Context(), flow.MessageRequest{Message: "请明天回电"})
	if err != nil || !result.IsOK {
		t.Fatalf("take_message failed: %+v %v", result, err)
	}

	if len(ledger.callbacks) != 1 {
		t.Fatalf("saved %d callbacks", len(ledger.callbacks))
	}
	cb := ledger.callbacks[0]
	if cb.PhoneNumber != "13800138000" {
		t.Errorf("phone = %q, want the caller's number as the default", cb.PhoneNumber)
	}
	if cb.Message != "请明天回电" || cb.Status != store.CallbackStatusOpen {
		t.Errorf("callback = %+v", cb)
	}
	if len(announced) != 1 || announced[0].ID != cb.ID {
		t.Errorf("announced %+v, want the saved callback once", announced)
	}
}

// A call the bot answered and nobody else took is billed by the carrier from
// the answer, exactly like one a person took. This path writes the row for
// those calls, and it was writing zero (found in the regression pass,
// 2026-08-21: a 34-second contained call billed as free).
func TestAContainedCallIsStillBilled(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now().Add(-40*time.Second), nil)
	recorder.answeredAt = time.Now().Add(-34 * time.Second)
	recorder.markHangup()
	recorder.finish(ledger, testFacts(), discard())

	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(ledger.cdrs))
	}
	got := ledger.cdrs[0]
	if got.BillSec < 33 || got.BillSec > 35 {
		t.Errorf("billSec = %d, want about 34 — the carrier charged for every second of it",
			got.BillSec)
	}
	if got.BillSec > got.TotalSec {
		t.Errorf("billSec %d exceeds totalSec %d", got.BillSec, got.TotalSec)
	}
}

// The DID is always this side of the call and the ANI is always the far side.
// Which of them is "from" therefore depends on who called whom, and writing
// every row as though the call had come in reversed every outbound one — with
// to_number always equal to the DID, so no AI outbound call could be found by
// the number it actually called (C43, measured live on 2026-08-23).
func TestTheLedgerKnowsWhichEndOfAnOutboundCallIsWhich(t *testing.T) {
	for _, tc := range []struct {
		name             string
		callType         callType
		placedToCustomer bool
		wantFrom, wantTo string
	}{
		{"a call that came in", callTypeInbound, false, "13800138000", "95012"},
		{"a call this platform placed", callTypeOutbound, true, "95012", "13800138000"},
		// An agent's call to a DID is not the bot's row to write at all; the
		// assembler's ends are checked in telephony's cdr tests.
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newFakeLedger()
			recorder := newCallRecorder(uuid.New(), time.Now().Add(-10*time.Second), nil)
			recorder.markHangup()

			facts := testFacts()
			facts.callType = tc.callType
			facts.isPlacedToCustomer = tc.placedToCustomer
			recorder.finish(ledger, facts, discard())

			if len(ledger.cdrs) != 1 {
				t.Fatalf("wrote %d cdrs, want 1", len(ledger.cdrs))
			}
			cdr := ledger.cdrs[0]
			if cdr.FromNumber != tc.wantFrom || cdr.ToNumber != tc.wantTo {
				t.Errorf("from/to = %s/%s, want %s/%s",
					cdr.FromNumber, cdr.ToNumber, tc.wantFrom, tc.wantTo)
			}
			// The DID is the DID either way: it says which of our numbers the
			// call belongs to, not which end of it.
			if cdr.DID != "95012" {
				t.Errorf("did = %s, want 95012", cdr.DID)
			}
			// And on an outbound call the number dialled must be findable —
			// which it is not while to_number simply repeats the DID.
			if tc.placedToCustomer && cdr.ToNumber == cdr.DID {
				t.Error("to_number is the DID again; searching AI outbound calls by " +
					"the number they called finds nothing")
			}
		})
	}
}

// A tool result reaches the transcript without the hint the model was given:
// the hint is instruction text, and the transcript is read by people. The
// phase the call moved to is recorded in its place.
func TestAToolResultIsRecordedWithoutItsHint(t *testing.T) {
	callID := uuid.New()
	recorder, transcripts, flushTranscript := recorderWithTranscript(t, callID, time.Now())

	recorder.toolResult("transfer_to_agent",
		`{"ok":"1","queue":"support","hint":"Current phase:\nAnnounce the transfer."}`, "handoff")
	recorder.toolResult("lookup_account", `{"ok":"0","hint":"Tell the caller."}`, "")

	flushTranscript()
	entries := transcripts.get(callID)
	if len(entries) != 2 {
		t.Fatalf("transcript has %d entries, want 2", len(entries))
	}

	moved := entries[0].Content
	output, _ := moved["output"].(string)
	if strings.Contains(output, "hint") || strings.Contains(output, "Announce the transfer") {
		t.Errorf("the recorded output carries the hint: %s", output)
	}
	if !strings.Contains(output, `"queue":"support"`) {
		t.Errorf("the recorded output lost the tool's own fields: %s", output)
	}
	if moved["movedTo"] != "handoff" {
		t.Errorf("movedTo = %v, want the node the call moved to", moved["movedTo"])
	}

	stayed := entries[1].Content
	if output, _ := stayed["output"].(string); strings.Contains(output, "Tell the caller") {
		t.Errorf("the recorded output carries the hint: %s", output)
	}
	if _, ok := stayed["movedTo"]; ok {
		t.Errorf("a result that moved nowhere records movedTo = %v", stayed["movedTo"])
	}
}

// A call the time limit ended says so, and is never contained: the platform,
// not the conversation, closed it (03-data: is_contained).
func TestACallEndedByTheTimeLimitIsNotContained(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now(), nil)
	recorder.markSessionLimit()
	recorder.markHangup()

	recorder.finish(ledger, testFacts(), discard())

	cdr := ledger.cdrs[0]
	if cdr.Status != store.CDRStatusAnswered || cdr.HangupCause != "SESSION_LIMIT" {
		t.Errorf("status=%s cause=%s, want ANSWERED and SESSION_LIMIT", cdr.Status, cdr.HangupCause)
	}
	if cdr.IsContained {
		t.Error("a call the time limit ended was marked contained")
	}
}

func TestCallTypeFromHeader(t *testing.T) {
	for in, want := range map[string]callType{
		"INBOUND": callTypeInbound, "OUTBOUND": callTypeOutbound,
		"INTERNAL": callTypeInternal, "": callTypeInbound,
		"outbound": callTypeInbound, "GARBAGE": callTypeInbound,
	} {
		if got := callTypeFromHeader(in); got != want {
			t.Errorf("callTypeFromHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

// A call an agent placed to a bot number is the agent's, and the human path
// writes it (#76). The bot writing it too gave a row with the bot as its only
// leg, no agent, and a containment it did not earn.
func TestACallAnAgentPlacedIsNotWrittenByTheBot(t *testing.T) {
	for name, facts := range map[string]*callFacts{
		"click-to-dial":  {callType: callTypeOutbound},
		"internal agent": {callType: callTypeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := newFakeLedger()
			recorder := newCallRecorder(uuid.New(), time.Now().Add(-10*time.Second), nil)
			recorder.markHangup()
			recorder.finish(ledger, facts, discard())
			if len(ledger.cdrs) != 0 {
				t.Fatalf("wrote %d cdrs, want none: the human path owns the row", len(ledger.cdrs))
			}
		})
	}
}

// The calls that are the bot's stay the bot's: a caller from outside, and the
// AI outbound bridge's call to a customer (typed OUTBOUND, but not an agent's).
func TestACallNoAgentPlacedIsStillWrittenByTheBot(t *testing.T) {
	for name, facts := range map[string]*callFacts{
		"inbound":             testFacts(),
		"placed to customer":  {callType: callTypeOutbound, isPlacedToCustomer: true},
		"outbound to the bot": {callType: callTypeOutbound, isPlacedToCustomer: true, did: "95001"},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := newFakeLedger()
			recorder := newCallRecorder(uuid.New(), time.Now().Add(-10*time.Second), nil)
			recorder.markHangup()
			recorder.finish(ledger, facts, discard())
			if len(ledger.cdrs) != 1 {
				t.Fatalf("wrote %d cdrs, want 1", len(ledger.cdrs))
			}
			if facts.callType == callTypeInbound && !ledger.cdrs[0].IsContained {
				t.Error("a contained inbound call lost its containment")
			}
		})
	}
}
