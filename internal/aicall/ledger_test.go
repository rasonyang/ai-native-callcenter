// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type fakeLedger struct {
	mu          sync.Mutex
	cdrs        []store.CDR
	transcripts map[uuid.UUID][]store.TranscriptEntry
	callbacks   []store.Callback
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{transcripts: map[uuid.UUID][]store.TranscriptEntry{}}
}

func (f *fakeLedger) InsertCDR(_ context.Context, cdr store.CDR) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cdrs = append(f.cdrs, cdr)
	return nil
}

func (f *fakeLedger) InsertTranscript(_ context.Context, callID uuid.UUID, entries []store.TranscriptEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transcripts[callID] = entries
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

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A call the bot finished itself is contained, and the ledger says so.
func TestAHangupCallWritesAContainedCDRAndTheTranscript(t *testing.T) {
	ledger := newFakeLedger()
	callID := uuid.New()
	recorder := newCallRecorder(callID, time.Now().Add(-30*time.Second))

	recorder.say(store.TranscriptRoleBot, "感谢致电 NovaNet")
	recorder.say(store.TranscriptRoleCaller, "帮我查个问题")
	recorder.toolCall("hangup", "{}")
	recorder.toolResult("hangup", `{"ok":"1"}`)
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

	entries := ledger.transcripts[callID]
	if len(entries) != 4 {
		t.Fatalf("transcript has %d entries, want 4", len(entries))
	}
	// Order is the conversation's own, and seq is dense from 1.
	for i, e := range entries {
		if e.Seq != i+1 {
			t.Errorf("entry %d has seq %d", i, e.Seq)
		}
	}
	if entries[0].Role != store.TranscriptRoleBot || entries[0].Kind != store.TranscriptKindText {
		t.Errorf("first entry = %+v", entries[0])
	}
	if entries[2].Kind != store.TranscriptKindToolCall {
		t.Errorf("third entry kind = %s", entries[2].Kind)
	}
}

// A transferred call is not finished: the human path owns the one CDR, and the
// bot writes only what it alone saw — the transcript.
func TestATransferredCallWritesTheTranscriptButNoCDR(t *testing.T) {
	ledger := newFakeLedger()
	callID := uuid.New()
	recorder := newCallRecorder(callID, time.Now())

	recorder.say(store.TranscriptRoleCaller, "转人工")
	recorder.markTransferred(uuid.New())

	recorder.finish(ledger, testFacts(), discard())

	if len(ledger.cdrs) != 0 {
		t.Fatalf("the bot wrote a CDR for a call it handed away: %+v", ledger.cdrs)
	}
	if len(ledger.transcripts[callID]) != 1 {
		t.Error("the transcript was lost with the transfer")
	}
}

// A caller who hangs up mid-conversation is answered but not contained.
func TestACallerHangupIsAnsweredButNotContained(t *testing.T) {
	ledger := newFakeLedger()
	recorder := newCallRecorder(uuid.New(), time.Now())
	recorder.say(store.TranscriptRoleBot, "你好")

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
	recorder := newCallRecorder(uuid.New(), time.Now())
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
	recorder := newCallRecorder(uuid.New(), time.Now())
	recorder.say(store.TranscriptRoleBot, "hello")
	recorder.finish(nil, testFacts(), discard())
}

// take_message persists an OPEN callback, defaulting the number to the caller.
func TestTakeMessagePersistsACallback(t *testing.T) {
	ledger := newFakeLedger()
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)
	actions.orchestrator.cfg.Ledger = ledger
	actions.recorder = newCallRecorder(uuid.New(), time.Now())
	actions.facts = testFacts()

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
}

// A saved callback is announced to the event stream; a failed save is not.
func TestTakeMessageAnnouncesTheCallback(t *testing.T) {
	ledger := newFakeLedger()
	sw := &fakeSwitch{}
	actions, _, _ := testActions(t, sw)
	actions.orchestrator.cfg.Ledger = ledger
	actions.recorder = newCallRecorder(uuid.New(), time.Now())
	actions.facts = testFacts()

	var announced []store.Callback
	actions.orchestrator.cfg.AnnounceCallback = func(cb store.Callback) {
		announced = append(announced, cb)
	}

	if _, err := actions.TakeMessage(t.Context(), flow.MessageRequest{Message: "回电"}); err != nil {
		t.Fatal(err)
	}
	if len(announced) != 1 {
		t.Fatalf("announced %d callbacks, want 1", len(announced))
	}
	if announced[0].Message != "回电" {
		t.Errorf("announced the wrong row: %+v", announced[0])
	}
}
