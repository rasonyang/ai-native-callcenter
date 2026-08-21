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

func (m *memoryLedger) InsertRecording(_ context.Context, r store.Recording) (store.Recording, error) {
	return r, nil
}

func (m *memoryLedger) MarkRecorded(context.Context, uuid.UUID) error { return nil }

type staticQueues map[string]uuid.UUID

func (q staticQueues) QueueIDByName(_ context.Context, name string) (uuid.UUID, bool) {
	id, ok := q[name]
	return id, ok
}

func newAssembler(ledger *memoryLedger, queues staticQueues) *CDRAssembler {
	return NewCDRAssembler(ledger, queues, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The bot writes the CDR for calls it finished; this path writes for calls a
// person finished, including bot calls that were handed over (the bot-share
// stamp marks the handover). Writing both sides doubled every contained call
// in the reports.
func TestTheBotOwnsItsFinishedCalls(t *testing.T) {
	contained := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "13800138000", AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
			{Role: RoleTarget, Number: "95012", IsBotLeg: true, AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
		},
	}
	transferred := contained
	transferred.CallID = uuid.New()
	transferred.Bot = BotShare{Sec: 20, DID: "95012"}

	ledger := &memoryLedger{}
	assembler := newAssembler(ledger, staticQueues{})
	assembler.CallFinished(contained)
	assembler.CallFinished(transferred)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		n := len(ledger.cdrs)
		ledger.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let a wrong second write land before judging

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.cdrs) != 1 {
		t.Fatalf("wrote %d cdrs, want only the transferred call's", len(ledger.cdrs))
	}
	if ledger.cdrs[0].CallID != transferred.CallID {
		t.Errorf("wrote the contained call's cdr — that row belongs to the bot")
	}
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
				CreatedAt: at(45), AnsweredAt: atPtr(50), ReleasedAt: atPtr(100),
				Bridges: []BridgeSpan{{OtherChannelID: "chan-a", StartedAt: at(50), EndedAt: at(100)}}},
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

// A call the agent placed is still their call, and whether it was answered is
// decided on the leg dialled out: the agent's own leg auto-answers in front of
// them, and reading that as an answer recorded every unanswered dial-out as a
// conversation, with no agent, no ring and no talk time (found live).
func TestAssembleAttributesCallsTheAgentPlaced(t *testing.T) {
	agentID := uuid.New()

	answeredOut := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(30),
		Parties: []PartySnapshot{
			// The agent's own leg auto-answers in front of them at 0; what
			// says the person they called picked up is the bridge at 8.
			{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ChannelID: "chan-agent",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(30),
				Bridges: []BridgeSpan{{OtherChannelID: "chan-out", StartedAt: at(8), EndedAt: at(30)}}},
			{Role: RoleTarget, Number: "18688886669", ChannelID: "chan-out",
				CreatedAt: at(2), AnsweredAt: atPtr(8), ReleasedAt: atPtr(30),
				Bridges: []BridgeSpan{{OtherChannelID: "chan-agent", StartedAt: at(8), EndedAt: at(30)}}},
		},
	}
	got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), answeredOut)
	if got.Status != store.CDRStatusAnswered {
		t.Errorf("status = %q, want ANSWERED", got.Status)
	}
	if len(got.AgentIDs) != 1 || got.AgentIDs[0] != agentID {
		t.Errorf("agentIds = %v, want the agent who dialled", got.AgentIDs)
	}
	if got.PrimaryAgentID == nil || *got.PrimaryAgentID != agentID {
		t.Error("the call the agent placed has no primary agent")
	}
	if got.RingSec != 6 {
		t.Errorf("ringSec = %d, want 6 (dialled at 2, answered at 8)", got.RingSec)
	}
	if got.TalkSec != 22 {
		t.Errorf("talkSec = %d, want 22 (answered at 8, ended at 30)", got.TalkSec)
	}
	if len(got.Legs) != 1 || got.Legs[0].Kind != "TRUNK" {
		t.Errorf("legs = %+v, want one TRUNK leg for the number dialled", got.Legs)
	}

	unanswered := answeredOut
	unanswered.CallID = uuid.New()
	unanswered.Parties = []PartySnapshot{
		{Role: RoleOriginator, Number: "1008", AgentID: &agentID, AnsweredAt: atPtr(0), ReleasedAt: atPtr(12)},
		{Role: RoleTarget, Number: "18688886669", CreatedAt: at(2), ReleasedAt: atPtr(12)},
	}
	missed := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), unanswered)
	if got := missed; got.Status != store.CDRStatusNoAnswer {
		t.Errorf("nobody picked up, status = %q, want NO_ANSWER", got.Status)
	}
}

// The live RONA shape (2026-08-21, call 01a02299): the bot answered and handed
// the caller to a queue, the switch dialled one agent twenty-three times over
// 159 seconds, nobody picked up, and the caller gave up. Reading the bot's own
// answer as the call's made this ANSWERED with no missed reason — and since
// every inbound call here meets the bot first, that hid queue abandonment
// across the board.
func TestACallNobodyAnsweredIsNotAnsweredByTheBotHavingSpoken(t *testing.T) {
	agentID := uuid.New()

	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(175),
		Bot:   BotShare{Sec: 9, DID: "95001"},
		Queue: QueueFacts{Name: "support-en", JoinedAt: at(16), LeftAt: at(175), Cause: "Cancel"},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", AnsweredAt: atPtr(0), ReleasedAt: atPtr(175)},
		},
	}
	// Twenty-three deliveries towards the same agent, none of them answered.
	for i := range 23 {
		snap.Parties = append(snap.Parties, PartySnapshot{
			Role: RoleTarget, Number: "1008", AgentID: idPtr(agentID),
			CreatedAt: at(16 + i), ReleasedAt: atPtr(17 + i),
		})
	}

	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)

	if cdr.Status != store.CDRStatusNoAnswer {
		t.Errorf("status = %s, want NO_ANSWER — the bot spoke, but nobody the caller was waiting for did",
			cdr.Status)
	}
	if cdr.MissedReason != "ABANDONED_WAITING" {
		t.Errorf("missedReason = %q, want ABANDONED_WAITING", cdr.MissedReason)
	}
	if len(cdr.AgentIDs) != 1 {
		t.Errorf("agentIds has %d entries, want 1 — twenty-three retries are one agent", len(cdr.AgentIDs))
	}
	if cdr.RingSec == 0 {
		t.Error("ringSec = 0 after 159 seconds of ringing")
	}
	if cdr.TalkSec != 0 || cdr.PrimaryAgentID != nil {
		t.Errorf("talkSec=%d primaryAgent=%v, want nothing — nobody talked", cdr.TalkSec, cdr.PrimaryAgentID)
	}
	// The bot's own share is still the bot's, and still on the row.
	if cdr.BotSec != 9 {
		t.Errorf("botSec = %d, want 9 — the bot did speak", cdr.BotSec)
	}
	// The journey and the row's own bot_sec are the same number.
	for _, leg := range cdr.Legs {
		if leg.Kind == "BOT" && leg.DurationSec != cdr.BotSec {
			t.Errorf("the BOT leg reads %ds while bot_sec reads %ds; one row, two answers",
				leg.DurationSec, cdr.BotSec)
		}
	}
}

// A queue that timed the caller out, after the bot had served them, is equally
// not an answered call.
func TestABotServedCallThatTimedOutInQueueIsMissed(t *testing.T) {
	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(300),
		Bot:     BotShare{Sec: 12, DID: "95001"},
		Queue:   QueueFacts{Name: "support-en", JoinedAt: at(12), LeftAt: at(300), Cause: "Timeout"},
		Parties: []PartySnapshot{{Role: RoleOriginator, AnsweredAt: atPtr(0), ReleasedAt: atPtr(300)}},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
	if cdr.Status != store.CDRStatusNoAnswer || cdr.MissedReason != "NO_AVAILABLE_AGENT" {
		t.Errorf("status=%s missedReason=%q, want NO_ANSWER/NO_AVAILABLE_AGENT", cdr.Status, cdr.MissedReason)
	}
}

// A call passed from one agent to another is one conversation carried by two
// people, and both stretches are work. Modelled on the live transfer of
// 2026-08-21 (call 01a021f5): wei held it for 58 seconds, ben for 35, and the
// ledger booked 58 because it read only the first leg that answered.
func TestTalkTimeCountsEveryAgentTheCallReached(t *testing.T) {
	wei, ben := uuid.New(), uuid.New()

	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(200),
		Queue: QueueFacts{Name: "support-en", JoinedAt: at(2), BridgedAt: at(100)},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(200)},
			{Role: RoleTarget, Number: "1008", AgentID: idPtr(wei), ChannelID: "wei",
				CreatedAt: at(95), AnsweredAt: atPtr(100), ReleasedAt: atPtr(158),
				Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(100), EndedAt: at(158)}}},
			{Role: RoleTarget, Number: "1007", AgentID: idPtr(ben), ChannelID: "ben",
				CreatedAt: at(158), AnsweredAt: atPtr(165), ReleasedAt: atPtr(200),
				Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(165), EndedAt: at(200)}}},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)

	if cdr.TalkSec != 58+35 {
		t.Errorf("talkSec = %d, want 93 — wei's 58 and ben's 35 are both work", cdr.TalkSec)
	}
	if cdr.PrimaryAgentID == nil || *cdr.PrimaryAgentID != wei {
		t.Error("the agent who first reached the caller is not the primary")
	}
	if cdr.RingSec != 5 {
		t.Errorf("ringSec = %d, want 5 — wei's leg was dialled at 95 and bridged at 100", cdr.RingSec)
	}
}

// A leg can answer without anyone being reached: an auto-answer phone picks up
// in front of nobody, and a leg whose codec cannot meet the caller's returns a
// clean 200 with no media at all — which is exactly what the click-to-dial
// INCOMPATIBLE_DESTINATION of 2026-08-20 did.
func TestALegThatAnsweredWithoutABridgeReachedNobody(t *testing.T) {
	agentID := uuid.New()

	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Queue: QueueFacts{Name: "support-en", JoinedAt: at(2), LeftAt: at(40), Cause: "Cancel"},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
			// Answered at 10, never bridged, died of a codec mismatch.
			{Role: RoleTarget, Number: "1008", AgentID: idPtr(agentID), ChannelID: "agent",
				CreatedAt: at(8), AnsweredAt: atPtr(10), ReleasedAt: atPtr(12),
				ReleaseCause: "INCOMPATIBLE_DESTINATION"},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)

	if cdr.Status != store.CDRStatusNoAnswer {
		t.Errorf("status = %s, want NO_ANSWER — the phone answered, the caller heard nobody", cdr.Status)
	}
	if cdr.TalkSec != 0 {
		t.Errorf("talkSec = %d, want 0 — there was no two-way media", cdr.TalkSec)
	}
	if cdr.PrimaryAgentID != nil {
		t.Error("a call nobody was reached on has a primary agent")
	}
}

// Owner's ruling (2026-08-21): hold counts as talk. The caller hears music
// instead of a person, but the agent has not left the call.
func TestHoldCountsAsTalk(t *testing.T) {
	agentID := uuid.New()

	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(100),
		Queue: QueueFacts{Name: "support-en", JoinedAt: at(2), BridgedAt: at(10)},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(100)},
			// One unbroken stretch across a hold from 40 to 70: the switch may
			// or may not unbridge on hold, and the accounting does not depend
			// on finding out.
			{Role: RoleTarget, Number: "1008", AgentID: idPtr(agentID), ChannelID: "agent",
				CreatedAt: at(5), AnsweredAt: atPtr(10), ReleasedAt: atPtr(100),
				Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(10), EndedAt: at(100)}}},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)

	if cdr.TalkSec != 90 {
		t.Errorf("talkSec = %d, want 90 — the 30 seconds on hold are still the agent's call", cdr.TalkSec)
	}
}

// The carrier bills from the moment the switch answered, whoever did or did
// not take the call afterwards. Modelled on the RONA call of 2026-08-21: the
// bot spoke, nobody took it, and the carrier billed all 142 seconds.
func TestBillingRunsFromTheSwitchAnsweringNotTheAgent(t *testing.T) {
	agentID := uuid.New()

	missed := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(142),
		Bot:   BotShare{Sec: 12, DID: "95001"},
		Queue: QueueFacts{Name: "support-en", JoinedAt: at(21), LeftAt: at(142), Cause: "Cancel"},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(142)},
			{Role: RoleTarget, Number: "1008", AgentID: idPtr(agentID), ChannelID: "agent",
				CreatedAt: at(21), ReleasedAt: atPtr(142)},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), missed)

	if cdr.Status != store.CDRStatusNoAnswer {
		t.Fatalf("status = %s, want NO_ANSWER", cdr.Status)
	}
	if cdr.BillSec != 142 {
		t.Errorf("billSec = %d, want 142 — the switch answered at 0 and the carrier bills from there",
			cdr.BillSec)
	}
	if cdr.AnsweredAt.IsZero() {
		t.Error("answeredAt is unset on a call the switch answered; billing has no anchor")
	}
	if cdr.TalkSec != 0 {
		t.Errorf("talkSec = %d, want 0 — nobody was reached", cdr.TalkSec)
	}

	// And on a call an agent did take, billing still runs from the caller's
	// own answer, not from the agent's pickup.
	took := missed
	took.CallID = uuid.New()
	took.Parties = []PartySnapshot{
		{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
			AnsweredAt: atPtr(0), ReleasedAt: atPtr(142)},
		{Role: RoleTarget, Number: "1008", AgentID: idPtr(agentID), ChannelID: "agent",
			CreatedAt: at(21), AnsweredAt: atPtr(30), ReleasedAt: atPtr(142),
			Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(30), EndedAt: at(142)}}},
	}
	answered := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), took)
	if answered.BillSec != 142 {
		t.Errorf("billSec = %d, want 142 — the agent arriving at 30 does not move the carrier's clock",
			answered.BillSec)
	}
	if answered.TalkSec != 112 {
		t.Errorf("talkSec = %d, want 112 — the agent's own stretch", answered.TalkSec)
	}
}
