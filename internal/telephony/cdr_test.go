// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
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
	t.Parallel()
	contained := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "13800138000", AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
			{Role: RoleTarget, Number: "95012", IsBotLeg: true, AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
		},
	}
	// The live shape of a contained call: the dialplan exports the DID to the
	// leg it dials towards the bot, so a share is present without a handover
	// ever having happened. Reading "non-empty" as "handed over" made both
	// paths race for this row, settled silently by whichever insert lost the
	// primary key (C21).
	contained.Bot = BotShare{DID: "95012"}

	// A real handover: the bot stamped the caller's channel on its way out.
	transferred := contained
	transferred.CallID = uuid.New()
	transferred.Bot = BotShare{Sec: 20, DID: "95012", IsStamped: true}

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

// A transfer decided in the first second stamps a duration of zero, and that
// is still a handover. Nothing about the share's contents can be the test —
// only that the bot wrote one.
func TestAnImmediateHandoverIsStillAHandover(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Bot: BotShare{Sec: 0, DID: "95012", IsStamped: true},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "13800138000", AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
			{Role: RoleTarget, Number: "95012", IsBotLeg: true, AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
		},
	}
	ledger := &memoryLedger{}
	newAssembler(ledger, staticQueues{}).CallFinished(snap)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		n := len(ledger.cdrs)
		ledger.mu.Unlock()
		if n >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("a call handed to a person in its first second reached no ledger at all")
}

// at builds timestamps relative to one base so durations are legible.
var base = time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)

func at(sec int) time.Time          { return base.Add(time.Duration(sec) * time.Second) }
func atPtr(sec int) *time.Time      { t := at(sec); return &t }
func idPtr(id uuid.UUID) *uuid.UUID { return &id }

// The full journey: bot, queue, agent — every duration lands in its own column.
// The durations are measurements of a call, not a partition of it. They
// overlap, and this fixture is where that has always been visible: bot 30 +
// wait 20 + ring 5 + talk 50 is 105 on a call that lasted 100.
//
// Ringing happens *inside* the queue's window — mod_callcenter dials agents
// while the member waits — so ring is a sub-interval of wait rather than a
// segment after it. The test used to be called "SplitsTheDurations", which
// claimed the opposite of what its own numbers say, and a query built on that
// claim (total − bot − wait − ring − talk > 20, hunting for legs torn down
// early) returned four perfectly ordinary calls (C57).
//
// So the assertion below is deliberate: the four exceed the whole. Anyone
// making them add up has changed what these fields mean and should say so
// here first.
func TestTheDurationsMeasureTheCallRatherThanPartitionIt(t *testing.T) {
	t.Parallel()
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

	// The overlap, stated. Ring is inside wait, so the four cannot be summed
	// and nothing downstream may assume they can.
	if sum := cdr.BotSec + cdr.QueueWaitSec + cdr.RingSec + cdr.TalkSec; sum != 105 {
		t.Errorf("the four durations sum to %d, want 105 on a 100-second call — "+
			"they measure overlapping stretches and were never a partition", sum)
	}
	if cdr.RingSec > cdr.QueueWaitSec {
		t.Errorf("ring %ds is longer than the wait %ds it happened inside",
			cdr.RingSec, cdr.QueueWaitSec)
	}
	// What is true of them: the phases the caller passed through in order do
	// fit, because those are sequential. Ring is the one that is not a phase.
	if seq := cdr.BotSec + cdr.QueueWaitSec + cdr.TalkSec; seq > cdr.TotalSec {
		t.Errorf("bot+wait+talk = %d exceeds the call's %d seconds; those three "+
			"are consecutive stretches and must fit", seq, cdr.TotalSec)
	}
}

// A call that never sought a person has no missed reason, and the silence is
// the answer.
//
// The vocabulary describes one journey — an inbound caller who wanted a person
// — and an outbound call, a call between two extensions, or an inbound call to
// a number nobody serves was never on it. Why those ended is the hangup cause,
// which says it exactly; a second, vaguer answer beside a precise one is not
// an improvement, and naming a caller who abandoned a direct extension would
// collide with SHORT_ABANDONED and ABANDONED_WAITING, which already mean the
// caller left.
func TestACallThatNeverSoughtAPersonHasNoMissedReason(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		callType events.CallType
		cause    string
	}{
		{"an outbound call to a busy number", events.CallTypeOutbound, "USER_BUSY"},
		{"an outbound call nobody picked up", events.CallTypeOutbound, "NO_ANSWER"},
		{"an internal call that ended unanswered", events.CallTypeInternal, "NORMAL_CLEARING"},
		{"an inbound call to a number nobody serves", events.CallTypeInbound, "UNALLOCATED_NUMBER"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), Snapshot{
				CallID:    uuid.New(),
				CallType:  tt.callType,
				CreatedAt: at(0),
				EndedAt:   atPtr(20),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "13800138000", ChannelID: "chan-a",
					CreatedAt: at(0), ReleasedAt: atPtr(20), ReleaseCause: tt.cause,
				}},
			})
			want := store.CDRStatusNoAnswer
			if tt.cause == "USER_BUSY" {
				want = store.CDRStatusBusy
			}
			if cdr.Status != want {
				t.Fatalf("status = %s, want %s", cdr.Status, want)
			}
			if cdr.MissedReason != "" {
				t.Errorf("missedReason = %q, want none — this caller never sought a "+
					"person, and %s already says why the call ended",
					cdr.MissedReason, tt.cause)
			}
		})
	}
}

// Missed-reason precedence, caller phase first, from recorded facts only.
func TestMissedReasons(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()

	tests := []struct {
		name string
		snap Snapshot
		want string
	}{
		{
			// No BridgedAt: a caller who hangs up mid-ring never gets as far
			// as a bridge, which is why requiring one made this reason
			// unreachable for the case it names and filed every such caller
			// under AGENTS_DID_NOT_ANSWER instead.
			name: "abandoned while the agent's phone rang",
			snap: Snapshot{
				Queue: QueueFacts{JoinedAt: at(0), LeftAt: at(15), Cause: "Cancel"},
				Parties: []PartySnapshot{
					{Role: RoleOriginator, ReleasedAt: atPtr(15)},
					{Role: RoleTarget, AgentID: idPtr(agentID), CreatedAt: at(10), ReleasedAt: atPtr(15)},
				},
			},
			want: "ABANDONED_RINGING",
		},
		{
			// The same shape but the caller stayed: the queue took them off
			// the agent who would not pick up. That is the agents' failure,
			// and it must not read as the caller's choice.
			name: "the agent let it ring out while the caller waited",
			snap: Snapshot{
				Queue: QueueFacts{JoinedAt: at(0), LeftAt: at(300), Cause: "Timeout"},
				Parties: []PartySnapshot{
					{Role: RoleOriginator, ReleasedAt: atPtr(300)},
					{Role: RoleTarget, AgentID: idPtr(agentID), CreatedAt: at(10), ReleasedAt: atPtr(25)},
				},
			},
			want: "NO_AVAILABLE_AGENT",
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
			// The queue never said why they left — the departure event is one
			// this campaign has watched go missing (C36). Phones rang and
			// nobody picked up, and that much is recorded, so the row says so
			// rather than inventing a choice the caller may not have made.
			name: "the queue never said why they left, but phones rang",
			snap: Snapshot{
				Queue: QueueFacts{JoinedAt: at(0), LeftAt: at(120)},
				Parties: []PartySnapshot{
					{Role: RoleOriginator, ReleasedAt: atPtr(120)},
					{Role: RoleTarget, AgentID: idPtr(agentID), CreatedAt: at(10), ReleasedAt: atPtr(25)},
				},
			},
			want: "AGENTS_DID_NOT_ANSWER",
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
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// The same call, placed by a system for an agent who never signed into this
// application: the phone is on the desk and registered, presence has never
// heard of them.
//
// The ledger's question is whether an agent's phone placed the call, and it
// was reading whether the originator is an agent it can name. Those coincided
// until third-party dialling existed. When they came apart the row lost its
// talk time and its leg record — 86 answered seconds written as talkSec 0,
// with a DIALING leg at the agent's own extension in place of the trunk it
// actually reached (measured live 2026-08-26). Every such call would report as
// no conversation at all.
//
// Attribution is a separate question and stays unanswered here, which is the
// decision: agentIds is empty because presence genuinely does not know who was
// on this phone. What must not be empty is what the call itself did.
func TestACallPlacedForAnAgentWhoNeverSignedInIsStillTheirPhonesCall(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(30),
		Parties: []PartySnapshot{
			// No AgentID: nobody is signed in at 1008. ExtensionNumber is what
			// the leg itself carries, and it is true regardless.
			{Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", ChannelID: "chan-agent",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(30),
				Bridges: []BridgeSpan{{OtherChannelID: "chan-out", StartedAt: at(8), EndedAt: at(30)}}},
			{Role: RoleTarget, Number: "18688886669", ChannelID: "chan-out",
				CreatedAt: at(2), AnsweredAt: atPtr(8), ReleasedAt: atPtr(30),
				Bridges: []BridgeSpan{{OtherChannelID: "chan-agent", StartedAt: at(8), EndedAt: at(30)}}},
		},
	}

	got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
	if got.Status != store.CDRStatusAnswered {
		t.Errorf("status = %q, want ANSWERED", got.Status)
	}
	if got.TalkSec != 22 {
		t.Errorf("talkSec = %d, want 22 — the conversation happened whether or not "+
			"anyone was signed in at the phone", got.TalkSec)
	}
	if got.RingSec != 6 {
		t.Errorf("ringSec = %d, want 6 (dialled at 2, answered at 8)", got.RingSec)
	}
	if len(got.Legs) != 1 || got.Legs[0].Kind != "TRUNK" || got.Legs[0].Label != "18688886669" {
		t.Errorf("legs = %+v, want one TRUNK leg for the number dialled — a DIALING leg "+
			"at the agent's own extension is the shape this had when the call was "+
			"read as nobody's", got.Legs)
	}
	// Not knowing who was on the phone is the v1 answer and must not become a
	// panic: the branch this now enters used to dereference the agent id it
	// was guaranteed by the very condition that let it in.
	if len(got.AgentIDs) != 0 {
		t.Errorf("agentIds = %v, want none — presence cannot say who was at that phone",
			got.AgentIDs)
	}
	if got.PrimaryAgentID != nil {
		t.Errorf("primaryAgentId = %v, want none", got.PrimaryAgentID)
	}
}

// A call the agent placed is still their call, and whether it was answered is
// decided on the leg dialled out: the agent's own leg auto-answers in front of
// them, and reading that as an answer recorded every unanswered dial-out as a
// conversation, with no agent, no ring and no talk time (found live).
func TestAssembleAttributesCallsTheAgentPlaced(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()

	answeredOut := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(30),
		Parties: []PartySnapshot{
			// The agent's own leg auto-answers in front of them at 0; what
			// says the person they called picked up is the bridge at 8.
			{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ExtensionNumber: "1008", ChannelID: "chan-agent",
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
		{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ExtensionNumber: "1008", AnsweredAt: atPtr(0), ReleasedAt: atPtr(12)},
		{Role: RoleTarget, Number: "18688886669", CreatedAt: at(2), ReleasedAt: atPtr(12)},
	}
	missed := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), unanswered)
	if got := missed; got.Status != store.CDRStatusNoAnswer {
		t.Errorf("nobody picked up, status = %q, want NO_ANSWER", got.Status)
	}
}

// journey renders legs as KIND:label:seconds, or KIND:label:note for a leg
// that carries one, so a test can state a whole journey in one line.
func journey(legs []store.Leg) []string {
	var out []string
	for _, leg := range legs {
		if leg.Note != "" {
			out = append(out, fmt.Sprintf("%s:%s:%s", leg.Kind, leg.Label, leg.Note))
			continue
		}
		out = append(out, fmt.Sprintf("%s:%s:%d", leg.Kind, leg.Label, leg.DurationSec))
	}
	return out
}

// An agent who places a call and hands it on is a hop of its journey, first,
// with their own stretch of the conversation. Modelled on the live call of
// 2026-10-19 (01a15229): wei (1008) click-dialled 18688886666, talked for
// about 20 seconds and transferred the customer to 1020, who talked for 22.
// The row read [AGENT 1020 22s, TRUNK 43s] — wei absent from the journey,
// and the colleague listed before the customer wei had dialled (#267).
//
// The shape is the switch's: the transfer moves the customer's leg (the one
// with no agent on it), so wei's own leg is released at the transfer and a
// new leg is dialled to 1020 and bridged to the customer's.
func TestAnAgentWhoPlacedACallAndHandedItOnIsInItsJourney(t *testing.T) {
	t.Parallel()
	wei, test1, ben := uuid.New(), uuid.New(), uuid.New()

	// wei's phone auto-answers the click at 1, the customer answers at 6 and
	// the two talk until wei transfers at 26.
	placer := PartySnapshot{Role: RoleOriginator, Number: "1008", AgentID: &wei, ExtensionNumber: "1008",
		ChannelID: "wei", CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(26),
		ReleaseCause: "NORMAL_CLEARING",
		Bridges:      []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(6), EndedAt: at(26)}}}
	customer := func(end int, after ...BridgeSpan) PartySnapshot {
		return PartySnapshot{Role: RoleTarget, Number: "18688886666", ChannelID: "customer",
			CreatedAt: at(1), AnsweredAt: atPtr(6), ReleasedAt: atPtr(end),
			Bridges: append([]BridgeSpan{{OtherChannelID: "wei", StartedAt: at(6), EndedAt: at(26)}}, after...)}
	}

	for _, tc := range []struct {
		name  string
		snap  Snapshot
		want  []string
		check func(t *testing.T, cdr store.CDR)
	}{
		{
			name: "to a colleague's extension",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(49),
				Parties: []PartySnapshot{placer,
					customer(49, BridgeSpan{OtherChannelID: "test1", StartedAt: at(27), EndedAt: at(49)}),
					{Role: RoleTarget, Number: "1020", AgentID: &test1, ExtensionNumber: "1020", ChannelID: "test1",
						CreatedAt: at(26), AnsweredAt: atPtr(27), ReleasedAt: atPtr(49),
						Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(27), EndedAt: at(49)}}},
				},
			},
			want: []string{"AGENT:1008:20", "TRUNK:18688886666:43", "AGENT:1020:22"},
			check: func(t *testing.T, cdr store.CDR) {
				// The two agents' stretches are the customer's conversation,
				// short only of the second 1020 took to pick up.
				if sum, trunk := cdr.Legs[0].DurationSec+cdr.Legs[2].DurationSec, cdr.Legs[1].DurationSec; trunk-sum != 1 {
					t.Errorf("wei %ds + 1020 %ds = %d against the customer's %ds; want one second short "+
						"(1020 ringing)", cdr.Legs[0].DurationSec, cdr.Legs[2].DurationSec, sum, trunk)
				}
				if cdr.TalkSec != 42 {
					t.Errorf("talkSec = %d, want 42 as on the live row", cdr.TalkSec)
				}
			},
		},
		{
			name: "to a queue, answered by another agent",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(70),
				Queue: QueueFacts{Name: "support-en", JoinedAt: at(26), BridgedAt: at(40), LeftAt: at(70)},
				Parties: []PartySnapshot{placer,
					customer(70, BridgeSpan{OtherChannelID: "ben", StartedAt: at(40), EndedAt: at(70)}),
					{Role: RoleTarget, Number: "1002", AgentID: &ben, ExtensionNumber: "1002", ChannelID: "ben",
						CreatedAt: at(35), AnsweredAt: atPtr(40), ReleasedAt: atPtr(70),
						Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(40), EndedAt: at(70)}}},
				},
			},
			want: []string{"AGENT:1008:20", "TRUNK:18688886666:64", "QUEUE:support-en:14", "AGENT:1002:30"},
		},
		{
			// Two TRUNK legs, which is all the row used to say: who dialled the
			// first one was nowhere in it.
			name: "to an external number",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(60),
				Parties: []PartySnapshot{placer,
					customer(60, BridgeSpan{OtherChannelID: "onward", StartedAt: at(33), EndedAt: at(60)}),
					{Role: RoleTarget, Number: "13900139000", ChannelID: "onward",
						CreatedAt: at(26), AnsweredAt: atPtr(33), ReleasedAt: atPtr(60),
						Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(33), EndedAt: at(60)}}},
				},
			},
			want: []string{"AGENT:1008:20", "TRUNK:18688886666:54", "TRUNK:13900139000:27"},
		},
		{
			// 1020 rang for twelve seconds and the customer gave up on it.
			name: "to a colleague who never answered",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(38),
				Parties: []PartySnapshot{placer,
					customer(38),
					{Role: RoleTarget, Number: "1020", AgentID: &test1, ExtensionNumber: "1020", ChannelID: "test1",
						CreatedAt: at(26), ReleasedAt: atPtr(38), ReleaseCause: "ORIGINATOR_CANCEL"},
				},
			},
			want: []string{"AGENT:1008:20", "TRUNK:18688886666:32", "AGENT:1020:did not answer"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.snap.CallID = uuid.New()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tc.snap)
			if got := journey(cdr.Legs); !slices.Equal(got, tc.want) {
				t.Errorf("legs = %v, want %v", got, tc.want)
			}
			if cdr.FromNumber != "1008" {
				t.Errorf("fromNumber = %q, want the placer's 1008", cdr.FromNumber)
			}
			if tc.check != nil {
				tc.check(t, cdr)
			}
		})
	}
}

// The agent's own leg ending a moment before the customer's is how every call
// they hang up themselves ends — the call is over when its last leg is — and
// it is not a handover. The journey stays the one leg they dialled.
func TestAnAgentWhoHangsUpFirstHandedNothingOn(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()
	ms := func(sec int, milli int) *time.Time {
		t := at(sec).Add(time.Duration(milli) * time.Millisecond)
		return &t
	}
	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: ms(30, 40),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ExtensionNumber: "1008", ChannelID: "agent",
				CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(30),
				Bridges: []BridgeSpan{{OtherChannelID: "out", StartedAt: at(8), EndedAt: at(30)}}},
			{Role: RoleTarget, Number: "18688886669", ChannelID: "out",
				CreatedAt: at(2), AnsweredAt: atPtr(8), ReleasedAt: ms(30, 40),
				Bridges: []BridgeSpan{{OtherChannelID: "agent", StartedAt: at(8), EndedAt: at(30)}}},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
	if got, want := journey(cdr.Legs), []string{"TRUNK:18688886669:22"}; !slices.Equal(got, want) {
		t.Errorf("legs = %v, want %v", got, want)
	}
}

// ms is a moment sec seconds and milli milliseconds into the call: a leg the
// switch tears down because its partner hung up releases a few milliseconds
// after it, and which one went first is the whole question.
func ms(sec, milli int) *time.Time {
	t := at(sec).Add(time.Duration(milli) * time.Millisecond)
	return &t
}

// The hangup cause of a call its placer handed on is how the call ended, not
// how the placer's leg did (#282). The shape is the live call of 2026-10-19
// (01a15229): wei (1008) click-dials 18688886666, talks, and transfers at 26.
// The transfer releases wei's leg with NORMAL_CLEARING whatever happens next,
// and that was the row's cause every time.
//
// The causes differ between legs where the switch can make them differ, so
// each case can tell which leg the answer came from: a carrier commonly
// reports a mobile hanging up as NORMAL_UNSPECIFIED, and the leg torn down
// behind it carries the switch's own NORMAL_CLEARING.
func TestTheCauseOfAHandedOnCallIsTheLegThatEndedIt(t *testing.T) {
	t.Parallel()
	wei, test1, ben := uuid.New(), uuid.New(), uuid.New()

	placer := PartySnapshot{Role: RoleOriginator, Number: "1008", AgentID: &wei, ExtensionNumber: "1008",
		ChannelID: "wei", CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(26),
		ReleaseCause: "NORMAL_CLEARING",
		Bridges:      []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(6), EndedAt: at(26)}}}
	customer := func(end *time.Time, cause string, after ...BridgeSpan) PartySnapshot {
		return PartySnapshot{Role: RoleTarget, Number: "18688886666", ChannelID: "customer",
			CreatedAt: at(1), AnsweredAt: atPtr(6), ReleasedAt: end, ReleaseCause: cause,
			Bridges: append([]BridgeSpan{{OtherChannelID: "wei", StartedAt: at(6), EndedAt: at(26)}}, after...)}
	}
	colleague := func(answered bool, end *time.Time, cause string) PartySnapshot {
		p := PartySnapshot{Role: RoleTarget, Number: "1020", AgentID: &test1, ExtensionNumber: "1020",
			ChannelID: "test1", CreatedAt: at(26), ReleasedAt: end, ReleaseCause: cause}
		if answered {
			p.AnsweredAt = atPtr(27)
			p.Bridges = []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(27), EndedAt: *end}}
		}
		return p
	}

	for _, tc := range []struct {
		name string
		snap Snapshot
		want string
	}{
		{
			name: "to a colleague who answered, the customer hangs up",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(49, 30),
				Parties: []PartySnapshot{placer,
					customer(atPtr(49), "NORMAL_UNSPECIFIED", BridgeSpan{OtherChannelID: "test1", StartedAt: at(27), EndedAt: at(49)}),
					colleague(true, ms(49, 30), "NORMAL_CLEARING"),
				},
			},
			want: "NORMAL_UNSPECIFIED",
		},
		{
			// The customer gave up while 1020 rang; the switch cancelled the
			// ring behind them.
			name: "to a colleague, the customer hangs up while it rings",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(38, 20),
				Parties: []PartySnapshot{placer,
					customer(atPtr(38), "NORMAL_UNSPECIFIED"),
					colleague(false, ms(38, 20), "ORIGINATOR_CANCEL"),
				},
			},
			want: "NORMAL_UNSPECIFIED",
		},
		{
			// The onward number refused and the customer, left with nobody,
			// was hung up behind it.
			name: "to an outside number that is busy",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(28, 45),
				Parties: []PartySnapshot{placer,
					customer(ms(28, 45), "NORMAL_CLEARING"),
					{Role: RoleTarget, Number: "13900139000", ChannelID: "onward",
						CreatedAt: at(26), ReleasedAt: atPtr(28), ReleaseCause: "USER_BUSY"},
				},
			},
			want: "USER_BUSY",
		},
		{
			name: "the colleague hangs up",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(49, 30),
				Parties: []PartySnapshot{placer,
					customer(ms(49, 30), "NORMAL_CLEARING", BridgeSpan{OtherChannelID: "test1", StartedAt: at(27), EndedAt: at(49)}),
					colleague(true, atPtr(49), "NORMAL_CLEARING"),
				},
			},
			want: "NORMAL_CLEARING",
		},
		{
			// The colleague's browser phone lost its media; that ended the call,
			// and the customer's leg torn down behind it says nothing about why.
			name: "the colleague's phone drops",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(49, 30),
				Parties: []PartySnapshot{placer,
					customer(ms(49, 30), "NORMAL_CLEARING", BridgeSpan{OtherChannelID: "test1", StartedAt: at(27), EndedAt: at(49)}),
					colleague(true, atPtr(49), "MEDIA_TIMEOUT"),
				},
			},
			want: "MEDIA_TIMEOUT",
		},
		{
			// Live 01a161a2: wei transferred straight to 1020, which declined
			// it. The rejection ended the transfer, and the customer with it.
			name: "to a colleague's extension that rejects the transfer",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(28, 45),
				Parties: []PartySnapshot{placer,
					customer(ms(28, 45), "NORMAL_CLEARING"),
					colleague(false, atPtr(28), "CALL_REJECTED"),
				},
			},
			want: "CALL_REJECTED",
		},
		{
			// Handed to a queue: ben's phone was offered the call and rang
			// out, and the customer gave up 400 ms later. An unanswered offer
			// leaves the customer waiting in the queue; it releasing just
			// before them is a coincidence, not what ended the call.
			name: "to a queue, an offer rings out just before the customer gives up",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(50, 400),
				QueueStays: []QueueFacts{{Name: "support-en", JoinedAt: at(26), LeftAt: at(50), Cause: "Cancel"}},
				Parties: []PartySnapshot{placer,
					customer(ms(50, 400), "NORMAL_UNSPECIFIED"),
					{Role: RoleTarget, Number: "1002", AgentID: &ben, ExtensionNumber: "1002", ChannelID: "ben",
						CreatedAt: at(30), ReleasedAt: atPtr(50), ReleaseCause: "NO_ANSWER"},
				},
			},
			want: "NORMAL_UNSPECIFIED",
		},
		{
			// A ring-all queue: 1020 answered, so the switch withdrew ben's
			// offer (LOSE_RACE); the customer hung up on 1020 at once, and
			// 1020's leg followed them down. Losing the race ended nothing.
			name: "to a ring-all queue, the losing offer is not the cause",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: ms(40, 430),
				QueueStays: []QueueFacts{{Name: "support-en", JoinedAt: at(26), BridgedAt: at(40), LeftAt: at(40), Cause: "Terminated"}},
				Parties: []PartySnapshot{placer,
					customer(ms(40, 400), "NORMAL_UNSPECIFIED", BridgeSpan{OtherChannelID: "test1", StartedAt: at(40), EndedAt: *ms(40, 400)}),
					{Role: RoleTarget, Number: "1020", AgentID: &test1, ExtensionNumber: "1020", ChannelID: "test1",
						CreatedAt: at(30), AnsweredAt: atPtr(40), ReleasedAt: ms(40, 430), ReleaseCause: "NORMAL_CLEARING",
						Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(40), EndedAt: *ms(40, 430)}}},
					{Role: RoleTarget, Number: "1002", AgentID: &ben, ExtensionNumber: "1002", ChannelID: "ben",
						CreatedAt: at(30), ReleasedAt: ms(40, 10), ReleaseCause: "LOSE_RACE"},
				},
			},
			want: "NORMAL_UNSPECIFIED",
		},
		{
			// Handed to a queue: 1020 was offered the call and did not pick
			// up, and the customer waited another twenty seconds before giving
			// up. The missed offer is not what ended the call.
			name: "to a queue, the customer gives up long after a missed offer",
			snap: Snapshot{
				CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(70),
				Queue: QueueFacts{Name: "support-en", JoinedAt: at(26), LeftAt: at(70), Cause: "Cancel"},
				Parties: []PartySnapshot{placer,
					customer(atPtr(70), "NORMAL_UNSPECIFIED"),
					{Role: RoleTarget, Number: "1002", AgentID: &ben, ExtensionNumber: "1002", ChannelID: "ben",
						CreatedAt: at(30), ReleasedAt: atPtr(50), ReleaseCause: "NO_ANSWER"},
				},
			},
			want: "NORMAL_UNSPECIFIED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.snap.CallID = uuid.New()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tc.snap)
			if cdr.HangupCause != tc.want {
				t.Errorf("hangupCause = %q, want %q", cdr.HangupCause, tc.want)
			}
			// The customer talked to wei, so the call was answered however it
			// ended; the cause must not move the status.
			if cdr.Status != store.CDRStatusAnswered {
				t.Errorf("status = %q, want ANSWERED", cdr.Status)
			}
		})
	}

	// A call wei stayed on to the end keeps wei's cause, even when the
	// customer's leg is the last to go by a few milliseconds.
	t.Run("no transfer, the agent hangs up first", func(t *testing.T) {
		t.Parallel()
		snap := Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: ms(30, 40),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "1008", AgentID: &wei, ExtensionNumber: "1008", ChannelID: "wei",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(30), ReleaseCause: "NORMAL_CLEARING",
					Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(8), EndedAt: at(30)}}},
				{Role: RoleTarget, Number: "18688886669", ChannelID: "customer",
					CreatedAt: at(2), AnsweredAt: atPtr(8), ReleasedAt: ms(30, 40), ReleaseCause: "NORMAL_UNSPECIFIED",
					Bridges: []BridgeSpan{{OtherChannelID: "wei", StartedAt: at(8), EndedAt: at(30)}}},
			},
		}
		cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
		if cdr.HangupCause != "NORMAL_CLEARING" {
			t.Errorf("hangupCause = %q, want the agent's own NORMAL_CLEARING", cdr.HangupCause)
		}
	})
}

// A leg at one of our own phones is an agent's leg in the journey whether or
// not anybody is signed in there (#284). It used to read TRUNK whenever no
// agent was, so wei's transfer to an unattended 1020 drew 1020 as an outside
// number. TRUNK stays for what went out through a carrier, including a number
// that only looks like an extension: the leg's own ExtensionNumber is the
// witness, never the digits.
func TestALegAtOneOfOurPhonesIsAnAgentsLeg(t *testing.T) {
	t.Parallel()
	wei, test1 := uuid.New(), uuid.New()

	placer := PartySnapshot{Role: RoleOriginator, Number: "1008", AgentID: &wei, ExtensionNumber: "1008",
		ChannelID: "wei", CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(26),
		ReleaseCause: "NORMAL_CLEARING",
		Bridges:      []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(6), EndedAt: at(26)}}}
	customer := PartySnapshot{Role: RoleTarget, Number: "18688886666", ChannelID: "customer",
		CreatedAt: at(1), AnsweredAt: atPtr(6), ReleasedAt: atPtr(56), ReleaseCause: "NORMAL_CLEARING",
		Bridges: []BridgeSpan{{OtherChannelID: "wei", StartedAt: at(6), EndedAt: at(26)}}}
	// Rings out for thirty seconds after the transfer; the customer gives up.
	target := func(p PartySnapshot) PartySnapshot {
		p.Role, p.ChannelID = RoleTarget, "target"
		p.CreatedAt, p.ReleasedAt, p.ReleaseCause = at(26), ms(56, 20), "ORIGINATOR_CANCEL"
		return p
	}

	for _, tc := range []struct {
		name   string
		target PartySnapshot
		want   string
	}{
		{
			name:   "an extension with nobody signed in",
			target: target(PartySnapshot{Number: "1020", ExtensionNumber: "1020"}),
			want:   "AGENT:1020:did not answer",
		},
		{
			name:   "an extension with an agent signed in",
			target: target(PartySnapshot{Number: "1020", AgentID: &test1, ExtensionNumber: "1020"}),
			want:   "AGENT:1020:did not answer",
		},
		{
			name:   "an outside number",
			target: target(PartySnapshot{Number: "13900139000"}),
			want:   "TRUNK:13900139000:did not answer",
		},
		{
			// A short carrier number with an extension's digits: nothing on
			// the leg says it is one of our phones.
			name:   "an outside number that resembles an extension",
			target: target(PartySnapshot{Number: "1020"}),
			want:   "TRUNK:1020:did not answer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: ms(56, 20),
				Parties: []PartySnapshot{placer, customer, tc.target},
			}
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
			want := []string{"AGENT:1008:20", "TRUNK:18688886666:50", tc.want}
			if got := journey(cdr.Legs); !slices.Equal(got, want) {
				t.Errorf("legs = %v, want %v", got, want)
			}
		})
	}
}

// An inbound call passed between agents names the same legs it always did,
// now in the order they joined. The two orders agree on the usual journey
// (queue, then the agents it reached) and part where an agent who took the
// caller directly passed them to a queue: by kind the queue came first,
// although the agent had been talking to the caller for half a minute
// before it existed.
func TestAnInboundJourneyIsInTheOrderItHappened(t *testing.T) {
	t.Parallel()
	wei, ben := uuid.New(), uuid.New()

	for _, tc := range []struct {
		name string
		snap Snapshot
		want []string
	}{
		{
			name: "queue, then one agent to another",
			snap: Snapshot{
				CallType: events.CallTypeInbound, CreatedAt: at(0), EndedAt: atPtr(200),
				Queue: QueueFacts{Name: "support-en", JoinedAt: at(2), BridgedAt: at(100)},
				Parties: []PartySnapshot{
					{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
						AnsweredAt: atPtr(0), ReleasedAt: atPtr(200)},
					{Role: RoleTarget, Number: "1008", AgentID: &wei, ChannelID: "wei",
						CreatedAt: at(95), AnsweredAt: atPtr(100), ReleasedAt: atPtr(158),
						Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(100), EndedAt: at(158)}}},
					{Role: RoleTarget, Number: "1007", AgentID: &ben, ChannelID: "ben",
						CreatedAt: at(158), AnsweredAt: atPtr(165), ReleasedAt: atPtr(200),
						Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(165), EndedAt: at(200)}}},
				},
			},
			want: []string{"QUEUE:support-en:98", "AGENT:1008:58", "AGENT:1007:35"},
		},
		{
			name: "a direct extension that passed the caller to a queue",
			snap: Snapshot{
				CallType: events.CallTypeInbound, CreatedAt: at(0), EndedAt: atPtr(100),
				Queue: QueueFacts{Name: "support-en", JoinedAt: at(40), BridgedAt: at(52)},
				Parties: []PartySnapshot{
					{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
						AnsweredAt: atPtr(0), ReleasedAt: atPtr(100)},
					{Role: RoleTarget, Number: "1001", AgentID: &wei, ChannelID: "wei",
						CreatedAt: at(1), AnsweredAt: atPtr(5), ReleasedAt: atPtr(40),
						Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(5), EndedAt: at(40)}}},
					{Role: RoleTarget, Number: "1002", AgentID: &ben, ChannelID: "ben",
						CreatedAt: at(48), AnsweredAt: atPtr(52), ReleasedAt: atPtr(100),
						Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(52), EndedAt: at(100)}}},
				},
			},
			want: []string{"AGENT:1001:35", "QUEUE:support-en:12", "AGENT:1002:48"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.snap.CallID = uuid.New()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tc.snap)
			if got := journey(cdr.Legs); !slices.Equal(got, tc.want) {
				t.Errorf("legs = %v, want %v", got, tc.want)
			}
		})
	}
}

// The live RONA shape (2026-08-21, call 01a02299): the bot answered and handed
// the caller to a queue, the switch dialled one agent twenty-three times over
// 159 seconds, nobody picked up, and the caller gave up. Reading the bot's own
// answer as the call's made this ANSWERED with no missed reason — and since
// every inbound call here meets the bot first, that hid queue abandonment
// across the board.
func TestACallNobodyAnsweredIsNotAnsweredByTheBotHavingSpoken(t *testing.T) {
	t.Parallel()
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

// A transferred call's BOT leg names the flow, exactly as the bot names it on
// the row it writes for a call it kept. One flow, one label, whichever path
// wrote the row — the walkthrough found the same flow reading "mobile_support"
// on one call and "" on the next.
func TestTheBotLegOfATransferredCallNamesTheFlow(t *testing.T) {
	t.Parallel()
	flow := uuid.New()
	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(60),
		Bot: BotShare{Sec: 20, FlowID: &flow, FlowSlug: "mobile_support", DID: "95012", IsStamped: true},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", AnsweredAt: atPtr(0), ReleasedAt: atPtr(60)},
		},
	}

	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)

	var bot *store.Leg
	for i := range cdr.Legs {
		if cdr.Legs[i].Kind == "BOT" {
			bot = &cdr.Legs[i]
		}
	}
	if bot == nil {
		t.Fatalf("no BOT leg in %+v", cdr.Legs)
	}
	if bot.Label != "mobile_support" {
		t.Errorf("BOT leg label = %q, want mobile_support", bot.Label)
	}
}

// The claims the bot made with no tool call behind it travel with the caller:
// stamped on their channel at the handover, read back here, and on the row
// the human path writes. A word the ledger does not know is dropped rather
// than carried, because the column's CHECK would refuse the whole row for it.
func TestATransferredCallKeepsTheBotsUnbackedClaims(t *testing.T) {
	t.Parallel()
	vars := map[string]string{
		"aicc_bot_sec":             "20",
		"aicc_bot_unbacked_claims": "TRANSFER,PROMISE, LOOKUP,TRANSFER",
	}
	share := botShareFrom(func(name string) string { return vars[name] })
	if want := []string{"TRANSFER", "LOOKUP"}; !slices.Equal(share.UnbackedClaims, want) {
		t.Fatalf("read %v from the channel, want %v", share.UnbackedClaims, want)
	}

	// The bot leg hangs up first and knows only the DID; the caller's leg
	// brings the rest.
	merged := BotShare{DID: "95001"}
	merged.Merge(share)
	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(60),
		Bot: merged,
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", AnsweredAt: atPtr(0), ReleasedAt: atPtr(60)},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
	if want := []string{"TRANSFER", "LOOKUP"}; !slices.Equal(cdr.UnbackedClaims, want) {
		t.Errorf("unbackedClaims = %v, want %v", cdr.UnbackedClaims, want)
	}

	if none := botShareFrom(func(string) string { return "" }); none.UnbackedClaims != nil {
		t.Errorf("a channel with no stamp read as %v", none.UnbackedClaims)
	}
}

// A call passed from one agent to another is one conversation carried by two
// people, and both stretches are work. Modelled on the live transfer of
// 2026-08-21 (call 01a021f5): wei held it for 58 seconds, ben for 35, and the
// ledger booked 58 because it read only the first leg that answered.
func TestTalkTimeCountsEveryAgentTheCallReached(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// The switch's own count of the billable seconds rides onto the row beside
// ours, so a charge can be checked instead of only asserted.
func TestBillableTimeIsCheckedAgainstTheSwitch(t *testing.T) {
	t.Parallel()
	base := func(switchBilled int) Snapshot {
		return Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeInbound,
			CreatedAt: at(0), EndedAt: atPtr(103),
			Bot: BotShare{Sec: 9, DID: "95001"},
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
					AnsweredAt: atPtr(0), ReleasedAt: atPtr(103), BilledSec: switchBilled},
			},
		}
	}

	// One second apart is arithmetic: we truncate where the switch rounds.
	agreed := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), base(104))
	if agreed.BillSec != 103 {
		t.Errorf("billSec = %d, want 103", agreed.BillSec)
	}
	if got := agreed.Tech["switchBillSec"]; got != 104 {
		t.Errorf("tech.switchBillSec = %v, want 104 — the second source has to reach the row", got)
	}

	// A leg the switch never billed leaves the field off rather than claiming 0.
	none := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), base(0))
	if _, present := none.Tech["switchBillSec"]; present {
		t.Error("tech carries a switch figure the switch never gave")
	}
}

// The live regression of 2026-08-21: an agent dialled another extension and
// the row came back billed for 23 seconds of a 21-second call. The agent's own
// leg auto-answers in front of them two seconds before the call exists as far
// as the ledger is concerned, and anchoring the money there produced a bill
// longer than the thing it was for.
func TestBillingIgnoresTheAgentsOwnAutoAnsweredLeg(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()

	// An agent placing an outbound call: their own leg answers at 0, the
	// person they called answers at 6.
	outbound := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ExtensionNumber: "1008", ChannelID: "agent",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(40),
				Bridges: []BridgeSpan{{OtherChannelID: "out", StartedAt: at(6), EndedAt: at(40)}}},
			{Role: RoleTarget, Number: "18688886669", ChannelID: "out",
				CreatedAt: at(2), AnsweredAt: atPtr(6), ReleasedAt: atPtr(40),
				Bridges: []BridgeSpan{{OtherChannelID: "agent", StartedAt: at(6), EndedAt: at(40)}}},
		},
	}
	got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), outbound)
	if got.BillSec != 34 {
		t.Errorf("billSec = %d, want 34 — the carrier charges from when the person answered at 6, "+
			"not from the agent's own phone picking up at 0", got.BillSec)
	}
	if got.BillSec > got.TotalSec {
		t.Errorf("billSec %d exceeds totalSec %d, which cannot be true of any call",
			got.BillSec, got.TotalSec)
	}

	// Two extensions: nobody charges for it, but it was still answered.
	internal := outbound
	internal.CallID = uuid.New()
	internal.CallType = events.CallTypeInternal
	got = newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), internal)
	if got.BillSec != 0 {
		t.Errorf("billSec = %d on a call between two extensions, want 0 — nobody bills for it",
			got.BillSec)
	}
	if got.AnsweredAt.IsZero() {
		t.Error("an internal call that was answered records no answer time")
	}

	// Inbound is unchanged: the caller's own leg is the one facing the carrier.
	inbound := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", ChannelID: "caller",
				AnsweredAt: atPtr(1), ReleasedAt: atPtr(40)},
		},
	}
	got = newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), inbound)
	if got.BillSec != 39 {
		t.Errorf("billSec = %d, want 39 — inbound still bills from the switch answering", got.BillSec)
	}
}

// A DID on a call this platform placed is the number the call went out *from*,
// not a number anybody dialled. Written as though every call carrying a DID
// were inbound, the row the human path writes came out reversed — from the
// customer, to our own DID — and no AI outbound call could be found by the
// number it actually rang. The bot's own writer had the identical bug and was
// fixed first (C43), which is why only a call that reached an agent still
// showed it: after a transfer this assembler owns the row, not the bot's.
// Live, 2026-08-23: 95002 → 18688886669, transferred, landed as
// from=18688886669 to=95002.
func TestAssembleGivesAnOutboundCallOnADIDItsRealDirection(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()
	placed := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(30),
		Bot: BotShare{Sec: 12, DID: "95002"},
		Parties: []PartySnapshot{
			// The leg the registry calls the originator is the customer's:
			// this platform dialled it, so it exists before anything else.
			{Role: RoleOriginator, Number: "18688886669", ChannelID: "chan-customer",
				AnsweredAt: atPtr(4), ReleasedAt: atPtr(30)},
			{Role: RoleTarget, Number: "1008", AgentID: &agentID, ChannelID: "chan-agent",
				CreatedAt: at(16), AnsweredAt: atPtr(20), ReleasedAt: atPtr(30)},
		},
	}
	got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), placed)
	if got.FromNumber != "95002" {
		t.Errorf("fromNumber = %q, want the DID 95002 — the number this platform "+
			"called from", got.FromNumber)
	}
	if got.ToNumber != "18688886669" {
		t.Errorf("toNumber = %q, want the number that was rung", got.ToNumber)
	}

	// The branch that must not move with it: on a call that came in, the DID
	// is what the caller dialled and stays the destination.
	arrived := placed
	arrived.CallID = uuid.New()
	arrived.CallType = events.CallTypeInbound
	got = newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), arrived)
	if got.FromNumber != "18688886669" || got.ToNumber != "95002" {
		t.Errorf("an inbound call landed as %q → %q, want 18688886669 → 95002",
			got.FromNumber, got.ToNumber)
	}
}

// An agent's click-to-dial to a DID is OUTBOUND and carries the DID from the
// bot leg, like a call this platform placed to a customer — but the originator
// is the agent and the DID is what they dialled. Once the bot handed the call
// to a queue, this assembler owned the row and swapped its ends as though the
// platform had placed it. Live, v0.3.0 fresh install: 1000 dialled 95001, the
// bot transferred to support-en, and the row read 95001 → 1000 (#80).
func TestAssembleKeepsAnAgentsCallToABotNumberInItsDirection(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()
	signedIn := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(20),
		Bot: BotShare{Sec: 15, DID: "95001", Queue: "support-en", IsStamped: true},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "1000", AgentID: &agentID, ExtensionNumber: "1000",
				ChannelID: "chan-agent", AnsweredAt: atPtr(0), ReleasedAt: atPtr(20)},
		},
	}
	got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), signedIn)
	if got.FromNumber != "1000" || got.ToNumber != "95001" {
		t.Errorf("an agent's call to a bot number landed as %q → %q, want 1000 → 95001",
			got.FromNumber, got.ToNumber)
	}

	// The extension is what says an agent's phone placed it, signed in or not.
	signedOut := signedIn
	signedOut.CallID = uuid.New()
	signedOut.Parties = []PartySnapshot{
		{Role: RoleOriginator, Number: "1000", ExtensionNumber: "1000",
			ChannelID: "chan-agent", AnsweredAt: atPtr(0), ReleasedAt: atPtr(20)},
	}
	got = newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), signedOut)
	if got.FromNumber != "1000" || got.ToNumber != "95001" {
		t.Errorf("a signed-out extension's call to a bot number landed as %q → %q, "+
			"want 1000 → 95001", got.FromNumber, got.ToNumber)
	}
}

// Which leg faces whoever charges for the call depends on who placed it. An
// agent dialling out sits on the originator, so the carrier's leg is the one
// dialled — and the agent's own auto-answering phone must never be read as the
// call becoming billable. On a call this platform placed, the leg towards the
// carrier *is* the originator, because we created it; looking past it for a
// dialled trunk that does not exist billed every AI outbound call at zero.
// Live, 2026-08-23: bill_sec 0 against the switch's own 21 on that same leg.
func TestAssembleBillsTheLegFacingTheCarrierOnEitherKindOfOutboundCall(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()
	placed := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(30),
		Bot: BotShare{Sec: 12, DID: "95002"},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "18688886669", ChannelID: "chan-customer",
				AnsweredAt: atPtr(4), ReleasedAt: atPtr(30), BilledSec: 26},
			{Role: RoleTarget, Number: "1008", AgentID: &agentID, ChannelID: "chan-agent",
				CreatedAt: at(16), AnsweredAt: atPtr(20), ReleasedAt: atPtr(30)},
		},
	}
	got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), placed)
	if got.BillSec != 26 {
		t.Errorf("billSec = %d, want 26 — the customer answered at 4 and the call "+
			"ran to 30, and the carrier charges for all of it", got.BillSec)
	}
	if !got.AnsweredAt.Equal(at(4)) {
		t.Errorf("answeredAt = %v, want the moment the customer picked up", got.AnsweredAt)
	}

	// The branch that must not move with it: an agent dialled out and nobody
	// picked up. Their own phone auto-answered in front of them, and that is
	// not a billable call.
	unanswered := Snapshot{
		CallID:    uuid.New(),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(12),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ExtensionNumber: "1008",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(12)},
			{Role: RoleTarget, Number: "18688886669", CreatedAt: at(2), ReleasedAt: atPtr(12)},
		},
	}
	if got = newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), unanswered); got.BillSec != 0 {
		t.Errorf("billSec = %d on a dial-out nobody answered, want 0", got.BillSec)
	}
}

// warnings captures what the assembler said, so an alarm can be tested for
// silence as well as for sounding.
func assemblerLogging(buf *bytes.Buffer) *CDRAssembler {
	return NewCDRAssembler(&memoryLedger{}, staticQueues{}, nil,
		slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

// The drift warning compared our figure for one leg against the switch's for
// another, so it fired on every unanswered click-to-dial — a thing that
// happens all day — with the ledger right every time (C54).
//
// An alarm that also rings when nothing is wrong is worse than no alarm, and
// this one had already cost what the pattern threatens: C49 announced itself
// in this exact line for two days and went unread, because the same line was
// firing on calls that were fine. So the fix is to compare like with like, not
// to quieten it.
func TestTheBillingAlarmRingsForTheLegItActuallyBilled(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()

	t.Run("an unanswered dial-out is silent", func(t *testing.T) {
		t.Parallel()
		// The agent's own leg auto-answers and the switch bills it for the
		// whole time the far end rang; we bill the leg facing the carrier,
		// which never answered, so nothing.
		var buf bytes.Buffer
		cdr := assemblerLogging(&buf).assemble(t.Context(), Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(30),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "1008", AgentID: idPtr(agentID), ExtensionNumber: "1008", ChannelID: "agent",
					AnsweredAt: atPtr(1), ReleasedAt: atPtr(30), BilledSec: 29},
				{Role: RoleTarget, Number: "18688886669", ChannelID: "trunk",
					CreatedAt: at(2), ReleasedAt: atPtr(30)},
			},
		})
		if cdr.BillSec != 0 {
			t.Errorf("billSec = %d, want 0 — nobody answered", cdr.BillSec)
		}
		if strings.Contains(buf.String(), "disagree on billable time") {
			t.Errorf("the alarm rang on a correct row: %s", buf.String())
		}
	})

	t.Run("two extensions talking have no billing claim to check", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cdr := assemblerLogging(&buf).assemble(t.Context(), Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeInternal,
			CreatedAt: at(0), EndedAt: atPtr(30),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "1008", AgentID: idPtr(agentID), ExtensionNumber: "1008", ChannelID: "agent",
					AnsweredAt: atPtr(1), ReleasedAt: atPtr(30), BilledSec: 29},
			},
		})
		if _, present := cdr.Tech["switchBillSec"]; present {
			t.Error("an internal call carries a billing comparison nobody bills for")
		}
		if strings.Contains(buf.String(), "disagree on billable time") {
			t.Errorf("the alarm rang on a call nobody bills: %s", buf.String())
		}
	})

	t.Run("the leg C49 was about is still watched", func(t *testing.T) {
		t.Parallel()
		// An AI outbound this platform placed: the billed leg *is* the
		// originator, so the alarm still covers the leg C49 was about. C49's
		// own numbers cannot be replayed — it is fixed and the two now agree —
		// so the disagreement is made on that same leg: ours is the 21 seconds
		// since it answered, and the switch is made to say 40.
		var buf bytes.Buffer
		cdr := assemblerLogging(&buf).assemble(t.Context(), Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(21),
			Bot: BotShare{Sec: 21, DID: "95002"},
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "18688886669", ChannelID: "customer",
					AnsweredAt: atPtr(0), ReleasedAt: atPtr(21), BilledSec: 40},
			},
		})
		if cdr.BillSec != 21 {
			t.Fatalf("billSec = %d, want 21 — the leg we bill is the one we created", cdr.BillSec)
		}
		if !strings.Contains(buf.String(), "disagree on billable time") {
			t.Errorf("the alarm that should have caught C49 no longer rings: %s", buf.String())
		}
	})
}

// Both halves of one defect: a call that never connected still has to say who
// it was between. A rejected caller had dialled something and the row did not
// say what (C31); an AI outbound nobody answered had been dialled to somebody
// and the row did not say who (C53).
func TestACallThatNeverConnectedStillRecordsWhoItWasBetween(t *testing.T) {
	t.Parallel()
	t.Run("a rejected caller's row says what they dialled", func(t *testing.T) {
		t.Parallel()
		// 95009 is served by nobody, so aicc_inbound rejects before a second
		// leg exists. The only leg there is knows its own destination.
		cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeInbound,
			CreatedAt: at(0), EndedAt: atPtr(0),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "18688886669", OtherNumber: "95009",
					ChannelID: "caller", ReleasedAt: atPtr(0),
					ReleaseCause: "UNALLOCATED_NUMBER"},
			},
		})
		if cdr.ToNumber != "95009" {
			t.Errorf("toNumber = %q, want 95009 — "+
				"a disconnected number rung all day and somebody scanning numbers "+
				"look the same in a ledger that does not say which was dialled",
				cdr.ToNumber)
		}
		if cdr.FromNumber != "18688886669" {
			t.Errorf("fromNumber = %q, want the caller", cdr.FromNumber)
		}
	})

	t.Run("an unanswered AI outbound row says who was called", func(t *testing.T) {
		t.Parallel()
		// The DID rides the customer's leg from creation, so the row takes the
		// direction a platform-placed call has even though nothing answered.
		cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(0),
			Bot: BotShare{DID: "95002"},
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "18688886669", ChannelID: "customer",
					ReleasedAt: atPtr(0), ReleaseCause: "NO_USER_RESPONSE"},
			},
		})
		if cdr.ToNumber != "18688886669" {
			t.Errorf("toNumber = %q, want the customer — an outbound campaign ringing "+
				"out and one that never ran must not look the same", cdr.ToNumber)
		}
		if cdr.FromNumber != "95002" {
			t.Errorf("fromNumber = %q, want the DID it went out from", cdr.FromNumber)
		}
		if cdr.Status != store.CDRStatusNoAnswer {
			t.Errorf("status = %q, want NO_ANSWER", cdr.Status)
		}
	})
}

// An agent who dials a platform number and has the bot hang up on them has
// made a call of their own. The bot's row for it had the bot as its only leg,
// no agent, a containment it did not earn and a bill for the whole call
// (#76, #104); the human path writes it now, with the bot's share taken from
// the agent's channel.
func TestAnAgentsCallThatTheBotEndedIsTheAgentsCall(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()
	for _, tc := range []struct {
		name     string
		callType events.CallType
		agent    *uuid.UUID
	}{
		{"signed in, dialled out", events.CallTypeOutbound, &agentID},
		{"signed out, dialled out", events.CallTypeOutbound, nil},
		{"signed in, internal", events.CallTypeInternal, &agentID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := Snapshot{
				CallID:    uuid.New(),
				CallType:  tc.callType,
				CreatedAt: at(0), EndedAt: atPtr(20),
				// No handover: the bot ended the call, and stamped its tally
				// on the agent's channel on the way (aicc_bot_sec).
				Bot: BotShare{Sec: 18, DID: "95001", FlowSlug: "novanet_support", IsStamped: true},
				Parties: []PartySnapshot{
					{Role: RoleOriginator, Number: "1000", AgentID: tc.agent, ExtensionNumber: "1000",
						ChannelID: "chan-agent", AnsweredAt: atPtr(0), ReleasedAt: atPtr(20)},
					{Role: RoleTarget, Number: "95001", IsBotLeg: true, CreatedAt: at(1),
						AnsweredAt: atPtr(2), ReleasedAt: atPtr(20),
						Bridges: []BridgeSpan{{OtherChannelID: "chan-agent", StartedAt: at(2), EndedAt: at(20)}}},
				},
			}
			ledger := &memoryLedger{}
			newAssembler(ledger, staticQueues{}).CallFinished(snap)
			awaitCDRs(t, ledger, 1)

			got := ledger.cdrs[0]
			if got.Status != store.CDRStatusAnswered || got.MissedReason != "" {
				t.Errorf("status=%q missedReason=%q, want ANSWERED and no reason: the bot answered",
					got.Status, got.MissedReason)
			}
			if got.FromNumber != "1000" || got.ToNumber != "95001" {
				t.Errorf("%q → %q, want 1000 → 95001", got.FromNumber, got.ToNumber)
			}
			if tc.agent != nil {
				if got.PrimaryAgentID == nil || *got.PrimaryAgentID != agentID ||
					len(got.AgentIDs) != 1 || got.AgentIDs[0] != agentID {
					t.Errorf("primary=%v agentIds=%v, want the agent who dialled",
						got.PrimaryAgentID, got.AgentIDs)
				}
			} else if got.PrimaryAgentID != nil || len(got.AgentIDs) != 0 {
				t.Errorf("primary=%v agentIds=%v, want none: nobody was signed in",
					got.PrimaryAgentID, got.AgentIDs)
			}
			if got.TalkSec != 0 {
				t.Errorf("talkSec = %d, want 0: no person was reached", got.TalkSec)
			}
			if got.IsContained {
				t.Error("the bot answering an agent was counted as containing a customer")
			}
			if got.BillSec != 0 {
				t.Errorf("billSec = %d, want 0: no carrier leg answered", got.BillSec)
			}
			if got.BotSec != 18 {
				t.Errorf("botSec = %d, want the bot's 18", got.BotSec)
			}
			var kinds []string
			for _, leg := range got.Legs {
				kinds = append(kinds, leg.Kind+":"+leg.Label)
			}
			if !slices.Equal(kinds, []string{"AGENT:1000", "BOT:novanet_support"}) {
				t.Errorf("legs = %v, want the agent and the bot", kinds)
			}
		})
	}
}

// An agent's call that the bot never stamped anything on (the agent hung up
// first) is still the agent's call.
func TestAnAgentWhoHangsUpOnTheBotStillOwnsTheCall(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(10),
		Bot: BotShare{DID: "95001"},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "1000", ExtensionNumber: "1000",
				AnsweredAt: atPtr(0), ReleasedAt: atPtr(10)},
			{Role: RoleTarget, Number: "95001", IsBotLeg: true, CreatedAt: at(1),
				AnsweredAt: atPtr(2), ReleasedAt: atPtr(10),
				Bridges: []BridgeSpan{{StartedAt: at(2), EndedAt: at(10)}}},
		},
	}
	ledger := &memoryLedger{}
	var buf bytes.Buffer
	NewCDRAssembler(ledger, staticQueues{}, nil,
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))).CallFinished(snap)
	awaitCDRs(t, ledger, 1)
	if got := ledger.cdrs[0]; got.Status != store.CDRStatusAnswered || got.BotSec != 8 {
		t.Errorf("status=%q botSec=%d, want ANSWERED with the bot's 8 from its bridge",
			got.Status, got.BotSec)
	}
	// The bot had no moment to stamp its time, so there is nothing to check
	// the bridge against (seen live on the first such call).
	if strings.Contains(buf.String(), "stamped duration disagrees") {
		t.Errorf("warned about a duration nobody stamped: %s", buf.String())
	}
}

// A caller from outside whom the bot kept is still the bot's to write.
func TestACallerTheBotKeptIsStillLeftToTheBot(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Bot: BotShare{DID: "95001"},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "13800138000", AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
			{Role: RoleTarget, Number: "95001", IsBotLeg: true, AnsweredAt: atPtr(0), ReleasedAt: atPtr(40)},
		},
	}
	ledger := &memoryLedger{}
	newAssembler(ledger, staticQueues{}).CallFinished(snap)
	time.Sleep(100 * time.Millisecond)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.cdrs) != 0 {
		t.Errorf("wrote %d cdrs for a contained inbound call, want none", len(ledger.cdrs))
	}
}

// awaitCDRs waits for the assembler's goroutine to write n rows, then gives a
// wrong extra write time to land before the caller judges.
func awaitCDRs(t *testing.T, ledger *memoryLedger, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		got := len(ledger.cdrs)
		ledger.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.cdrs) != n {
		t.Fatalf("wrote %d cdrs, want %d", len(ledger.cdrs), n)
	}
}

// finishedStorage tells the test that CallFinished's goroutine has decided:
// recording ingestion runs after the row is written or declined, so once
// Ingest is reached the ledger holds every row this call will get. No file
// is found, so nothing else happens.
type finishedStorage struct{ reached chan struct{} }

func (finishedStorage) Backend() string { return "FS" }
func (finishedStorage) Bucket() string  { return "" }
func (s finishedStorage) Ingest(context.Context, string) (int64, error) {
	close(s.reached)
	return 0, errors.New("no file")
}

// finishCall hands the snapshot to CallFinished and returns the rows this path
// wrote for it, once its goroutine has finished deciding. No sleep: the
// storage's Ingest is the goroutine's own last step before returning.
func finishCall(t *testing.T, snap Snapshot) []store.CDR {
	t.Helper()
	ledger := &memoryLedger{}
	storage := finishedStorage{reached: make(chan struct{})}
	a := NewCDRAssembler(ledger, staticQueues{"support": uuid.New()}, storage,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.flushWait = 0
	a.CallFinished(snap)
	select {
	case <-storage.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("CallFinished never finished deciding")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return slices.Clone(ledger.cdrs)
}

// A bot leg the switch dialled and the bot never answered is not a bot
// session, and only a session writes the bot's row (internal/aicall: the
// recorder's finish is deferred inside a conversation the ACK starts). Left to
// the bot because a bot leg existed, each of these calls reached the ledger
// from neither side (#313).
func TestABotThatNeverAnsweredLeavesTheRowToThisPath(t *testing.T) {
	t.Parallel()
	flow := uuid.New()
	agentID := uuid.New()

	// An AI outbound call: the customer's leg is the originator, answered on
	// the trunk, with the DID's flow stamped on it when it was placed.
	outbound := func(endedAt int, cause string, bot PartySnapshot) Snapshot {
		return Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(endedAt),
			Bot: BotShare{DID: "95099", FlowID: &flow},
			Parties: []PartySnapshot{{
				Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
				CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(endedAt),
				ReleaseCause: cause,
			}, bot},
		}
	}
	// An inbound call: the dialplan answers the carrier's caller before it
	// bridges to the bot, and exports the DID to the bot's leg.
	refusedInbound := func(endedAt int, cause string, more ...PartySnapshot) Snapshot {
		return Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeInbound,
			CreatedAt: at(0), EndedAt: atPtr(endedAt),
			Bot: BotShare{DID: "95001"},
			Parties: append([]PartySnapshot{{
				Role: RoleOriginator, Number: "13800138000", ChannelID: "caller",
				CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(endedAt),
				ReleaseCause: cause,
			}, {
				// 486 Busy Here at capacity; a peer the list drops gets no
				// answer at all and times out, which is the same leg here.
				Role: RoleTarget, Number: "95001", ChannelID: "bot", IsBotLeg: true,
				CreatedAt: at(0), ReleasedAt: atPtr(0), ReleaseCause: "USER_BUSY",
			}}, more...),
		}
	}
	withQueue := func(s Snapshot) Snapshot {
		s.Queue = QueueFacts{Name: "support", JoinedAt: at(1), BridgedAt: at(6)}
		return s
	}

	for _, tc := range []struct {
		name  string
		snap  Snapshot
		legs  []string
		agent *uuid.UUID
	}{
		{
			// (a) The uuid_transfer dialled the bot's leg and the customer
			// hung up before the bot's 200 OK; the switch CANCELled it.
			name: "the customer hung up before the bot answered",
			snap: outbound(12, "NORMAL_CLEARING", PartySnapshot{
				Role: RoleTarget, Number: "95099", ChannelID: "bot", IsBotLeg: true,
				CreatedAt: at(11), ReleasedAt: atPtr(12), ReleaseCause: "ORIGINATOR_CANCEL",
			}),
			legs: []string{"TRUNK:18688886666:12"},
		},
		{
			// (b) An ESL stall held the bridge past its window; the platform
			// withdrew the reservation and is ending the customer's leg, and
			// the transfer the switch runs late is refused 480 by the bot.
			name: "a late bridge the bot refused once its reservation was withdrawn",
			snap: outbound(30, "BOT_BRIDGE_UNCONFIRMED", PartySnapshot{
				Role: RoleTarget, Number: "95099", ChannelID: "bot", IsBotLeg: true,
				CreatedAt: at(30), ReleasedAt: atPtr(30), ReleaseCause: "NO_USER_RESPONSE",
			}),
			legs: []string{"TRUNK:18688886666:30"},
		},
		{
			// The bot refused an inbound call and the number has no fallback
			// queue: the caller is told the call cannot be completed.
			name: "an inbound call the bot refused, with no fallback queue",
			snap: refusedInbound(4, "NORMAL_TEMPORARY_FAILURE"),
			legs: []string{"TRUNK:13800138000:4"},
		},
		{
			// The same refusal on a number with a fallback queue: the caller
			// reached a person, and that call was missing from the ledger.
			name: "an inbound call the bot refused, answered from the fallback queue",
			snap: withQueue(refusedInbound(60, "NORMAL_CLEARING", PartySnapshot{
				Role: RoleTarget, Number: "1007", AgentID: &agentID, ChannelID: "agent",
				CreatedAt: at(2), AnsweredAt: atPtr(6), ReleasedAt: atPtr(60),
				Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(6), EndedAt: at(60)}},
			})),
			legs:  []string{"QUEUE:support:5", "AGENT:1007:54"},
			agent: &agentID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := finishCall(t, tc.snap)
			if len(rows) != 1 {
				t.Fatalf("wrote %d rows, want exactly one: no bot session exists to write it", len(rows))
			}
			got := rows[0]
			if got.CallID != tc.snap.CallID {
				t.Errorf("wrote call %s, want %s", got.CallID, tc.snap.CallID)
			}
			if got.Status != store.CDRStatusAnswered {
				t.Errorf("status = %s, want ANSWERED: the caller was answered", got.Status)
			}
			if j := journey(got.Legs); !slices.Equal(j, tc.legs) {
				t.Errorf("legs = %v, want %v", j, tc.legs)
			}
			if got.BotSec != 0 || got.IsContained {
				t.Errorf("botSec=%d contained=%v, want 0 and false: the bot never answered",
					got.BotSec, got.IsContained)
			}
			if tc.agent != nil && (got.PrimaryAgentID == nil || *got.PrimaryAgentID != *tc.agent) {
				t.Errorf("primary agent = %v, want the agent who answered", got.PrimaryAgentID)
			}
		})
	}
}

// The controls: a bot that answered has a session, and that session writes the
// row unless it handed the call to a person. Two writers for one call race for
// its primary key, and the one that ends later wins (store.InsertCDR).
func TestABotThatAnsweredStillDecidesWhoWritesTheRow(t *testing.T) {
	t.Parallel()
	flow := uuid.New()
	answeredBot := func(number string) PartySnapshot {
		return PartySnapshot{
			Role: RoleTarget, Number: number, ChannelID: "bot", IsBotLeg: true,
			CreatedAt: at(1), AnsweredAt: atPtr(1), ReleasedAt: atPtr(40),
			Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(1), EndedAt: at(40)}},
		}
	}
	keptOutbound := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Bot: BotShare{DID: "95099", FlowID: &flow, FlowSlug: "lead_qualification"},
		Parties: []PartySnapshot{{
			Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
			CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(40),
			ReleaseCause: "NORMAL_CLEARING",
		}, answeredBot("95099")},
	}
	keptInbound := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Bot: BotShare{DID: "95001", FlowID: &flow, FlowSlug: "novanet_support"},
		Parties: []PartySnapshot{{
			Role: RoleOriginator, Number: "13800138000", ChannelID: "customer",
			CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(40),
			ReleaseCause: "NORMAL_CLEARING",
		}, answeredBot("95001")},
	}
	handedOver := keptOutbound
	handedOver.CallID = uuid.New()
	handedOver.Bot = BotShare{Sec: 39, DID: "95099", FlowID: &flow, FlowSlug: "lead_qualification",
		IsStamped: true}

	for _, tc := range []struct {
		name string
		snap Snapshot
		rows int
	}{
		{"an AI outbound call the bot kept is the bot's", keptOutbound, 0},
		{"an inbound call the bot kept is the bot's", keptInbound, 0},
		{"a call the bot handed over is this path's", handedOver, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := finishCall(t, tc.snap)
			if len(rows) != tc.rows {
				t.Fatalf("wrote %d rows, want %d", len(rows), tc.rows)
			}
			if tc.rows == 1 {
				if j := journey(rows[0].Legs); len(j) == 0 || !strings.HasPrefix(j[0], "BOT:") {
					t.Errorf("legs = %v, want the bot first: it answered", j)
				}
			}
		})
	}
}

// A callee who is busy or declines releases the leg we dialled with USER_BUSY,
// and the ledger used to file that with the calls nobody picked up (#180).
// Only that one cause counts, only for an unanswered outbound call, and only
// on the leg that went out towards the callee: an agent's own leg
// auto-answers and says nothing about the far end.
func TestAnUnansweredOutboundCallToABusyCalleeIsBusy(t *testing.T) {
	t.Parallel()
	agentID := uuid.New()

	// platform: the customer's leg is the originator, because we created it.
	platform := func(cause string, bot bool) Snapshot {
		s := Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(9),
			Bot: BotShare{DID: "95099"},
			Parties: []PartySnapshot{{
				Role: RoleOriginator, Number: "18688886669", ChannelID: "customer",
				CreatedAt: at(0), ReleasedAt: atPtr(9), ReleaseCause: cause,
			}},
		}
		if bot {
			// The bot leg was set up but the callee never picked up.
			s.Parties = append(s.Parties, PartySnapshot{
				Role: RoleTarget, IsBotLeg: true, ChannelID: "bot", CreatedAt: at(0), ReleasedAt: atPtr(9),
			})
		}
		return s
	}
	// agent: the agent's leg auto-answered; the far end is a dialled leg.
	agent := func(agentCause, farCause string, farAnswered bool) Snapshot {
		far := PartySnapshot{
			Role: RoleTarget, Number: "18688886669", ChannelID: "far",
			CreatedAt: at(2), ReleasedAt: atPtr(12), ReleaseCause: farCause,
		}
		if farAnswered {
			far.AnsweredAt = atPtr(8)
		}
		return Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(12),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ExtensionNumber: "1008",
					ChannelID: "agent", AnsweredAt: atPtr(0), ReleasedAt: atPtr(12), ReleaseCause: agentCause},
				far,
			},
		}
	}
	inbound := func(cause string) Snapshot {
		return Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeInbound,
			Queue:     QueueFacts{JoinedAt: at(0), LeftAt: at(30), Cause: "Timeout"},
			CreatedAt: at(0), EndedAt: atPtr(30),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "13800138000", ChannelID: "caller",
					CreatedAt: at(0), ReleasedAt: atPtr(30), ReleaseCause: cause},
				{Role: RoleTarget, AgentID: &agentID, ChannelID: "agent",
					CreatedAt: at(10), ReleasedAt: atPtr(25), ReleaseCause: cause},
			},
		}
	}

	tests := []struct {
		name string
		snap Snapshot
		want string
	}{
		{"platform call, callee busy", platform("USER_BUSY", false), store.CDRStatusBusy},
		{"AI outbound, callee busy, bot never answered", platform("USER_BUSY", true), store.CDRStatusBusy},
		{"platform call, rang out", platform("NO_ANSWER", false), store.CDRStatusNoAnswer},
		{"platform call, no user response", platform("NO_USER_RESPONSE", false), store.CDRStatusNoAnswer},
		{"platform call, declined with CALL_REJECTED", platform("CALL_REJECTED", false), store.CDRStatusNoAnswer},
		{"platform call, number does not exist", platform("UNALLOCATED_NUMBER", false), store.CDRStatusNoAnswer},
		{"platform call, temporary failure", platform("NORMAL_TEMPORARY_FAILURE", false), store.CDRStatusNoAnswer},
		{"platform call, no cause recorded", platform("", false), store.CDRStatusNoAnswer},
		{"platform call, cause in another case", platform("user_busy", false), store.CDRStatusNoAnswer},

		{"agent call, far end busy, own leg cleared normally", agent("NORMAL_CLEARING", "USER_BUSY", false), store.CDRStatusBusy},
		{"agent call, far end busy, own leg also USER_BUSY", agent("USER_BUSY", "USER_BUSY", false), store.CDRStatusBusy},
		{"agent call, far end rang out", agent("NORMAL_CLEARING", "NO_ANSWER", false), store.CDRStatusNoAnswer},
		{"agent call, only the agent's own leg says USER_BUSY", agent("USER_BUSY", "NO_ANSWER", false), store.CDRStatusNoAnswer},
		{"agent call, far end declined with CALL_REJECTED", agent("NORMAL_CLEARING", "CALL_REJECTED", false), store.CDRStatusNoAnswer},

		{"inbound call, every leg USER_BUSY", inbound("USER_BUSY"), store.CDRStatusNoAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tt.snap)
			if got.Status != tt.want {
				t.Errorf("status = %s, want %s", got.Status, tt.want)
			}
		})
	}

	t.Run("an answered call stays answered however a later leg released", func(t *testing.T) {
		t.Parallel()
		s := agent("NORMAL_CLEARING", "USER_BUSY", true)
		s.Parties[1].Bridges = []BridgeSpan{{OtherChannelID: "agent", StartedAt: at(8), EndedAt: at(12)}}
		s.Parties[0].Bridges = []BridgeSpan{{OtherChannelID: "far", StartedAt: at(8), EndedAt: at(12)}}
		// A second dialled leg that was busy, after the first one talked.
		s.Parties = append(s.Parties, PartySnapshot{
			Role: RoleTarget, Number: "18688886670", ChannelID: "far2",
			CreatedAt: at(10), ReleasedAt: atPtr(11), ReleaseCause: "USER_BUSY",
		})
		got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), s)
		if got.Status != store.CDRStatusAnswered {
			t.Errorf("status = %s, want ANSWERED", got.Status)
		}
	})

	t.Run("a busy row keeps what the unanswered branch would have given it", func(t *testing.T) {
		t.Parallel()
		asm := func(s Snapshot) store.CDR {
			return newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), s)
		}
		busy, noAnswer := asm(agent("NORMAL_CLEARING", "USER_BUSY", false)), asm(agent("NORMAL_CLEARING", "NO_ANSWER", false))
		if busy.Status != store.CDRStatusBusy || noAnswer.Status != store.CDRStatusNoAnswer {
			t.Fatalf("statuses = %s / %s", busy.Status, noAnswer.Status)
		}
		if busy.MissedReason != noAnswer.MissedReason || busy.RingSec != noAnswer.RingSec ||
			busy.TalkSec != noAnswer.TalkSec || busy.BillSec != noAnswer.BillSec {
			t.Errorf("busy row {missed %q ring %d talk %d bill %d} differs from no-answer {%q %d %d %d}",
				busy.MissedReason, busy.RingSec, busy.TalkSec, busy.BillSec,
				noAnswer.MissedReason, noAnswer.RingSec, noAnswer.TalkSec, noAnswer.BillSec)
		}

		// A queue-served platform call exercises the ring and missed-reason
		// fields with real values.
		withAgent := func(cause string) Snapshot {
			s := platform(cause, false)
			s.Queue = QueueFacts{JoinedAt: at(0), LeftAt: at(9), Cause: "Cancel"}
			s.Parties = append(s.Parties, PartySnapshot{
				Role: RoleTarget, AgentID: &agentID, ChannelID: "a", CreatedAt: at(3), ReleasedAt: atPtr(9),
			})
			return s
		}
		b, n := asm(withAgent("USER_BUSY")), asm(withAgent("NO_ANSWER"))
		if b.Status != store.CDRStatusBusy || n.Status != store.CDRStatusNoAnswer {
			t.Fatalf("statuses = %s / %s", b.Status, n.Status)
		}
		if b.MissedReason != n.MissedReason || b.RingSec != n.RingSec {
			t.Errorf("busy {missed %q ring %d} differs from no-answer {%q %d}",
				b.MissedReason, b.RingSec, n.MissedReason, n.RingSec)
		}
	})
}

// An AI outbound call the customer answered and nothing on our side ever
// joined used to get the journey of a call nobody picked up: DIALING at the
// customer's number, 0 s. Beside its own answered_at and bill_sec that is a row
// contradicting itself — the customer answered, the carrier billed, and the
// journey says the phone was still ringing.
//
// Live 2026-10-22 (#313): two calls whose bridge to the bot never ran, billed
// 30 and 35 seconds, both read [DIALING 18688886666 0s]. How each of them
// ended is a case below; the shape of the journey is the same for all, because
// the customer was answered on the trunk whatever failed after that.
func TestAnAnsweredCallThatReachedNobodyIsTheTrunkItWasAnsweredOn(t *testing.T) {
	t.Parallel()
	flow := uuid.New()

	// The originator of a call this platform placed is the customer's own leg,
	// created towards the trunk. The flow is stamped on it when it is placed,
	// so the row knows what the call was for even when the bot never joined.
	unreached := func(cause string, answeredAt, endedAt int, withFlow bool) Snapshot {
		s := Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(endedAt),
			Bot: BotShare{DID: "95099"},
			Parties: []PartySnapshot{{
				Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
				CreatedAt: at(0), AnsweredAt: atPtr(answeredAt), ReleasedAt: atPtr(endedAt),
				ReleaseCause: cause,
			}},
		}
		if withFlow {
			// As the outbound originator stamps it: the DID's flow, before
			// any bot has had a chance to name itself.
			s.Bot.FlowID = &flow
		}
		return s
	}

	for _, tc := range []struct {
		name string
		snap Snapshot
		want []string
		// billSec is the answered window the trunk leg must match.
		billSec int
	}{
		{
			// The bridge command was never confirmed and never ran: the
			// platform hung up the answered leg thirty seconds later.
			name:    "the bridge to the bot never ran",
			snap:    unreached("BOT_BRIDGE_UNCONFIRMED", 0, 30, true),
			want:    []string{"TRUNK:18688886666:30"},
			billSec: 30,
		},
		{
			// No bot slot came free while the customer waited; the leg went the
			// moment the hold ended, so a second of it was billed.
			name:    "no bot slot, hung up at once",
			snap:    unreached("OVERFLOW", 0, 1, true),
			want:    []string{"TRUNK:18688886666:1"},
			billSec: 1,
		},
		{
			// Recovery ended a leg nobody was watching, 8 seconds in.
			name:    "recovery ended a parked leg",
			snap:    unreached("RECOVERY_ON_TIMER_EXPIRE", 0, 8, true),
			want:    []string{"TRUNK:18688886666:8"},
			billSec: 8,
		},
		{
			// The customer hung up while the bridge was being made, before the
			// transfer had dialled the bot: they were on the trunk for the
			// whole half minute.
			name:    "the customer hung up during the bridge",
			snap:    unreached("NORMAL_CLEARING", 0, 30, true),
			want:    []string{"TRUNK:18688886666:30"},
			billSec: 30,
		},
		{
			// One step later: the uuid_transfer had dialled the bot's leg, and
			// the customer hung up before the bot's 200 OK. A bot leg nobody
			// answered is not the bot having been there, so there is no BOT
			// hop of 0 s and the trunk is not lost behind one.
			name: "the customer hung up before the bot's leg answered",
			snap: func() Snapshot {
				s := unreached("NORMAL_CLEARING", 0, 12, true)
				s.Parties = append(s.Parties, PartySnapshot{
					Role: RoleTarget, Number: "95099", ChannelID: "bot", IsBotLeg: true,
					CreatedAt: at(11), ReleasedAt: atPtr(12), ReleaseCause: "ORIGINATOR_CANCEL",
				})
				return s
			}(),
			want:    []string{"TRUNK:18688886666:12"},
			billSec: 12,
		},
		{
			// Answered and gone in under a second: the duration truncates to 0,
			// as bill_sec does, and the leg is still the trunk.
			name: "the customer hung up 0.3 s after answering",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: ms(0, 300),
				Bot: BotShare{DID: "95099", FlowID: &flow},
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: ms(0, 300),
					ReleaseCause: "NORMAL_CLEARING",
				}},
			},
			want:    []string{"TRUNK:18688886666:0"},
			billSec: 0,
		},
		{
			// A leg placed before the flow was stamped on it (an older build,
			// or one adopted across an upgrade): the journey is the same, and
			// only flow_id is missing.
			name:    "a call whose flow was never stamped",
			snap:    unreached("BOT_BRIDGE_UNCONFIRMED", 0, 30, false),
			want:    []string{"TRUNK:18688886666:30"},
			billSec: 30,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tc.snap)
			if got := journey(cdr.Legs); !slices.Equal(got, tc.want) {
				t.Errorf("legs = %v, want %v", got, tc.want)
			}
			if cdr.Status != store.CDRStatusAnswered {
				t.Errorf("status = %s, want ANSWERED: the customer answered", cdr.Status)
			}
			if cdr.BillSec != tc.billSec {
				t.Errorf("billSec = %d, want %d", cdr.BillSec, tc.billSec)
			}
			if tc.snap.Bot.FlowID != nil && cdr.FlowID == nil {
				t.Error("flowId is null on a call placed for a flow")
			}
			var spanned int
			for _, leg := range cdr.Legs {
				if leg.DurationSec < 0 {
					t.Errorf("leg %+v has a negative duration", leg)
				}
				if leg.Kind == "TRUNK" {
					spanned += leg.DurationSec
				}
			}
			// The trunk leg is the answered window bill_sec measures, not a
			// second measurement that could disagree with it.
			if spanned != cdr.BillSec {
				t.Errorf("the trunk leg spans %ds against billSec %ds", spanned, cdr.BillSec)
			}
		})
	}
}

// The controls: a call nobody answered is still DIALING, and a call the bot did
// join keeps the journey it always had — the fallback above must not add a
// trunk hop to every AI call.
func TestAnUnansweredCallIsStillDialingAndAReachedBotIsStillTheJourney(t *testing.T) {
	t.Parallel()
	flow := uuid.New()
	unanswered := func(cause string) Snapshot {
		return Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(9),
			Bot: BotShare{DID: "95099", FlowID: &flow, FlowSlug: "lead_qualification"},
			Parties: []PartySnapshot{{
				Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
				CreatedAt: at(0), ReleasedAt: atPtr(9), ReleaseCause: cause,
			}},
		}
	}

	for _, tc := range []struct {
		name   string
		cause  string
		status string
	}{
		{"rang out", "NO_ANSWER", store.CDRStatusNoAnswer},
		{"busy", "USER_BUSY", store.CDRStatusBusy},
		{"rejected by the trunk", "CALL_REJECTED", store.CDRStatusNoAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), unanswered(tc.cause))
			if got, want := journey(cdr.Legs), []string{"DIALING:18688886666:0"}; !slices.Equal(got, want) {
				t.Errorf("legs = %v, want %v", got, want)
			}
			if cdr.Status != tc.status {
				t.Errorf("status = %s, want %s", cdr.Status, tc.status)
			}
			if cdr.BillSec != 0 {
				t.Errorf("billSec = %d, want 0 on a call nobody answered", cdr.BillSec)
			}
		})
	}

	// Calls on our own side of the trunk. Our phone's own answer is not the
	// call being answered: click-to-dial rings the agent's phone first and it
	// auto-answers in front of them, so a call that reached nobody beyond it
	// is still DIALING, whatever its originator's answeredAt says. A leg that
	// never left the building is never a TRUNK, answered or not.
	phone := PartySnapshot{
		Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", ChannelID: "phone",
		CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
	}
	for _, tc := range []struct {
		name   string
		snap   Snapshot
		want   []string
		status string
	}{
		{
			// The gateway was down, or the agent hung up before the switch
			// dialled out: the trunk leg never existed.
			name: "a click-to-dial whose trunk leg never came up",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", ChannelID: "phone",
					OtherNumber: "18688886660",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
				}},
			},
			want:   []string{"DIALING:18688886660:0"},
			status: store.CDRStatusNoAnswer,
		},
		{
			// The same between two extensions: the far phone was never rung.
			name: "a call between our own phones that reached nobody",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", ChannelID: "phone",
					OtherNumber: "1002",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
				}},
			},
			want:   []string{"DIALING:1002:0"},
			status: store.CDRStatusNoAnswer,
		},
		{
			// An agent dialled a bot's DID and the bot never answered (no slot,
			// or the agent gave up first). The bot's leg was dialled, but the
			// bot was not there: no BOT hop of 0 s, and not answered — the
			// agent's own phone picking up is all that happened.
			name: "an agent's call to a DID the bot never answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Bot: BotShare{DID: "95001", FlowID: &flow, FlowSlug: "novanet_support"},
				Parties: []PartySnapshot{phone, {
					Role: RoleTarget, Number: "95001", ChannelID: "bot", IsBotLeg: true,
					CreatedAt: at(1), ReleasedAt: atPtr(6), ReleaseCause: "ORIGINATOR_CANCEL",
				}},
			},
			want:   []string{"DIALING:95001:0"},
			status: store.CDRStatusNoAnswer,
		},
		{
			// A call whose dialled destination was not stamped falls back to the
			// originator's number rather than producing an empty label.
			name: "an unreached call with empty dialled number falls back to originator",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{phone},
			},
			want:   []string{"DIALING:1008:0"},
			status: store.CDRStatusNoAnswer,
		},
		{
			// An internal call whose only leg is one the dialplan spawned (no
			// extension of ours on it) and which reached nobody: answered, by
			// the switch, but nothing faces a carrier on an internal call, and
			// a TRUNK here would be a carrier leg there was none of.
			name: "an internal call on a leg the dialplan spawned",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "7001", ChannelID: "spawned",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
				}},
			},
			want:   []string{"AGENT:7001:6"},
			status: store.CDRStatusAnswered,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tc.snap)
			if got := journey(cdr.Legs); !slices.Equal(got, tc.want) {
				t.Errorf("legs = %v, want %v", got, tc.want)
			}
			if cdr.Status != tc.status {
				t.Errorf("status = %s, want %s", cdr.Status, tc.status)
			}
			if cdr.BillSec != 0 {
				t.Errorf("billSec = %d, want 0: no carrier leg answered", cdr.BillSec)
			}
		})
	}

	t.Run("the bot answered and kept the call", func(t *testing.T) {
		t.Parallel()
		snap := Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(45),
			Bot: BotShare{Sec: 45, FlowID: &flow, FlowSlug: "lead_qualification", DID: "95099"},
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(45)},
				{Role: RoleTarget, Number: "95099", ChannelID: "bot", IsBotLeg: true,
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(45)},
			},
		}
		cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
		if got, want := journey(cdr.Legs), []string{"BOT:lead_qualification:45"}; !slices.Equal(got, want) {
			t.Errorf("legs = %v, want %v", got, want)
		}
		if cdr.Status != store.CDRStatusAnswered {
			t.Errorf("status = %s, want ANSWERED", cdr.Status)
		}
	})

	t.Run("the bot answered and handed the call to a queue and an agent", func(t *testing.T) {
		t.Parallel()
		agentID := uuid.New()
		snap := Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(200),
			Bot:   BotShare{Sec: 30, FlowID: &flow, FlowSlug: "lead_qualification", DID: "95099", IsStamped: true},
			Queue: QueueFacts{Name: "support-en", JoinedAt: at(32), BridgedAt: at(60)},
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(200)},
				{Role: RoleTarget, Number: "95099", ChannelID: "bot", IsBotLeg: true,
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(32)},
				{Role: RoleTarget, Number: "1008", AgentID: &agentID, ChannelID: "agent",
					CreatedAt: at(58), AnsweredAt: atPtr(60), ReleasedAt: atPtr(200),
					Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(60), EndedAt: at(200)}}},
			},
		}
		cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
		want := []string{"BOT:lead_qualification:30", "QUEUE:support-en:28", "AGENT:1008:140"}
		if got := journey(cdr.Legs); !slices.Equal(got, want) {
			t.Errorf("legs = %v, want %v", got, want)
		}
		if cdr.Status != store.CDRStatusAnswered {
			t.Errorf("status = %s, want ANSWERED", cdr.Status)
		}
	})
}
