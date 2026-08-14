// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type memoryLedger struct {
	mu          sync.Mutex
	cdrs        []store.CDR
	queueEvents []string
}

func (m *memoryLedger) InsertCDR(_ context.Context, cdr store.CDR) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cdrs = append(m.cdrs, cdr)
	return nil
}

func (m *memoryLedger) InsertQueueEvent(_ context.Context, _ time.Time,
	_ *uuid.UUID, _ uuid.UUID, event string, _ *uuid.UUID, waitMs int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueEvents = append(m.queueEvents, event)
	_ = waitMs
	return nil
}

type staticQueues map[string]uuid.UUID

func (q staticQueues) QueueIDByName(_ context.Context, name string) (uuid.UUID, bool) {
	id, ok := q[name]
	return id, ok
}

func newAssembler(ledger *memoryLedger, queues staticQueues) *CDRAssembler {
	return NewCDRAssembler(ledger, queues, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// at builds timestamps relative to one base so durations are legible.
var base = time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)

func at(sec int) time.Time          { return base.Add(time.Duration(sec) * time.Second) }
func atPtr(sec int) *time.Time      { t := at(sec); return &t }
func idPtr(id uuid.UUID) *uuid.UUID { return &id }

// The full journey: bot, queue, agent — every duration lands in its own column.
func TestAssembleAnsweredCallSplitsTheDurations(t *testing.T) {
	agentID := uuid.New()
	queueID := uuid.New()
	flowID := uuid.New()

	snap := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeInbound,
		Language:  "zh",
		CreatedAt: at(0),
		EndedAt:   atPtr(100),
		Bot:       BotShare{Sec: 30, FlowID: &flowID, DID: "95012", Queue: "support-zh", Summary: "wants a refund"},
		Queue: QueueFacts{
			Name: "support-zh", JoinedAt: at(30), BridgedAt: at(50),
		},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "13800138000", ChannelID: "chan-a",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(100), ReleaseCause: "NORMAL_CLEARING"},
			{Role: RoleTarget, Number: "1007", AgentID: idPtr(agentID),
				CreatedAt: at(45), AnsweredAt: atPtr(50), ReleasedAt: atPtr(100)},
		},
	}

	cdr := newAssembler(&memoryLedger{}, staticQueues{"support-zh": queueID}).
		assemble(t.Context(), snap)

	if cdr.Status != store.CDRStatusAnswered {
		t.Fatalf("status = %s", cdr.Status)
	}
	if cdr.BotSec != 30 || cdr.QueueWaitSec != 20 || cdr.RingSec != 5 || cdr.TalkSec != 50 || cdr.TotalSec != 100 {
		t.Errorf("durations bot=%d wait=%d ring=%d talk=%d total=%d",
			cdr.BotSec, cdr.QueueWaitSec, cdr.RingSec, cdr.TalkSec, cdr.TotalSec)
	}
	if cdr.QueueID == nil || *cdr.QueueID != queueID {
		t.Error("the queue name was not resolved to its id")
	}
	if cdr.PrimaryAgentID == nil || *cdr.PrimaryAgentID != agentID {
		t.Error("the answering agent is not the primary")
	}
	if cdr.HangupCause != "NORMAL_CLEARING" || cdr.FromNumber != "13800138000" {
		t.Errorf("cause=%s from=%s", cdr.HangupCause, cdr.FromNumber)
	}
	if cdr.UserData["botSummary"] != "wants a refund" {
		t.Error("the bot summary did not reach userData")
	}
	if len(cdr.Legs) != 3 || cdr.Legs[0].Kind != "BOT" || cdr.Legs[1].Kind != "QUEUE" || cdr.Legs[2].Kind != "AGENT" {
		t.Errorf("legs = %+v", cdr.Legs)
	}
	if cdr.MissedReason != "" {
		t.Errorf("an answered call carries missed reason %q", cdr.MissedReason)
	}
}

// Missed-reason precedence, caller phase first, from recorded facts only.
func TestMissedReasons(t *testing.T) {
	agentID := uuid.New()

	tests := []struct {
		name string
		snap Snapshot
		want string
	}{
		{
			name: "abandoned while the agent's phone rang",
			snap: Snapshot{
				Queue: QueueFacts{JoinedAt: at(0), BridgedAt: at(10), LeftAt: at(15), Cause: "Cancel"},
				Parties: []PartySnapshot{
					{Role: RoleOriginator, ReleasedAt: atPtr(15)},
					{Role: RoleTarget, AgentID: idPtr(agentID), CreatedAt: at(10), ReleasedAt: atPtr(15)},
				},
			},
			want: "ABANDONED_RINGING",
		},
		{
			name: "gave up almost immediately",
			snap: Snapshot{
				Queue:   QueueFacts{JoinedAt: at(0), LeftAt: at(3), Cause: "Cancel"},
				Parties: []PartySnapshot{{Role: RoleOriginator, ReleasedAt: atPtr(3)}},
			},
			want: "SHORT_ABANDONED",
		},
		{
			name: "waited properly then gave up",
			snap: Snapshot{
				Queue:   QueueFacts{JoinedAt: at(0), LeftAt: at(90), Cause: "Cancel", CancelReason: "BREAK_OUT"},
				Parties: []PartySnapshot{{Role: RoleOriginator, ReleasedAt: atPtr(90)}},
			},
			want: "ABANDONED_WAITING",
		},
		{
			name: "the queue gave up on the caller",
			snap: Snapshot{
				Queue:   QueueFacts{JoinedAt: at(0), LeftAt: at(300), Cause: "Timeout"},
				Parties: []PartySnapshot{{Role: RoleOriginator, ReleasedAt: atPtr(300)}},
			},
			want: "NO_AVAILABLE_AGENT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.snap.CallID = uuid.New()
			tt.snap.CallType = events.CallTypeInbound
			tt.snap.CreatedAt = at(0)
			tt.snap.EndedAt = atPtr(300)

			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tt.snap)
			if cdr.Status != store.CDRStatusNoAnswer {
				t.Fatalf("status = %s", cdr.Status)
			}
			if cdr.MissedReason != tt.want {
				t.Errorf("missedReason = %q, want %q", cdr.MissedReason, tt.want)
			}
		})
	}
}

// A pure bot call arriving through the human path (transferred and hung up in
// queue never happened here): bot share present, no agent — still ANSWERED.
func TestBotOnlyCallIsAnswered(t *testing.T) {
	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Bot: BotShare{Sec: 40, DID: "95012"},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "1007", AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
	if cdr.Status != store.CDRStatusAnswered || cdr.BotSec != 40 {
		t.Errorf("status=%s botSec=%d", cdr.Status, cdr.BotSec)
	}
	if len(cdr.Legs) == 0 || cdr.Legs[0].Kind != "BOT" {
		t.Errorf("legs = %+v", cdr.Legs)
	}
}

// Queue movements land as ledger rows with the wait computed from the queue's
// own clock.
func TestQueueEventsAreRecorded(t *testing.T) {
	ledger := &memoryLedger{}
	queueID := uuid.New()
	assembler := newAssembler(ledger, staticQueues{"support-en": queueID})

	callID := uuid.New()
	events := []SwitchEvent{
		{Kind: KindQueueMemberJoined, Queue: "support-en", OccurredAt: at(0), JoinedAt: at(0)},
		{Kind: KindQueueAgentOffered, Queue: "support-en", OccurredAt: at(5)},
		{Kind: KindQueueBridgeStart, Queue: "support-en", OccurredAt: at(8), JoinedAt: at(0)},
		{Kind: KindQueueMemberLeft, Queue: "support-en", OccurredAt: at(60),
			JoinedAt: at(0), LeftAt: at(60), Cause: "Terminated"},
	}
	for _, ev := range events {
		assembler.QueueEvent(t.Context(), ev, &callID, nil)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		n := len(ledger.queueEvents)
		ledger.mu.Unlock()
		if n == 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Each write runs on its own goroutine, so arrival order is not promised;
	// the ledger orders by occurred_at when it matters.
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	seen := map[string]int{}
	for _, name := range ledger.queueEvents {
		seen[name]++
	}
	for _, name := range []string{store.QueueEventJoined, store.QueueEventOffered,
		store.QueueEventBridged, store.QueueEventLeft} {
		if seen[name] != 1 {
			t.Errorf("event %s recorded %d times: %v", name, seen[name], ledger.queueEvents)
		}
	}
}

// An abandonment is its own event name, derived from the recorded cause.
func TestAnAbandonedMemberIsRecordedAsAbandoned(t *testing.T) {
	ledger := &memoryLedger{}
	assembler := newAssembler(ledger, staticQueues{"support-en": uuid.New()})

	assembler.QueueEvent(t.Context(), SwitchEvent{
		Kind: KindQueueMemberLeft, Queue: "support-en",
		OccurredAt: at(20), JoinedAt: at(0), LeftAt: at(20), Cause: "Cancel",
	}, nil, nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		n := len(ledger.queueEvents)
		ledger.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.queueEvents) != 1 || ledger.queueEvents[0] != store.QueueEventAbandoned {
		t.Errorf("events = %v, want one ABANDONED", ledger.queueEvents)
	}
}
