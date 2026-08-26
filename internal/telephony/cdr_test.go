// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"bytes"
	"context"
	"io"
	"log/slog"
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
			if cdr.Status != store.CDRStatusNoAnswer {
				t.Fatalf("status = %s, want NO_ANSWER", cdr.Status)
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

// The switch's own count of the billable seconds rides onto the row beside
// ours, so a charge can be checked instead of only asserted.
func TestBillableTimeIsCheckedAgainstTheSwitch(t *testing.T) {
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
	agentID := uuid.New()

	// An agent placing an outbound call: their own leg answers at 0, the
	// person they called answers at 6.
	outbound := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "1008", AgentID: &agentID, ChannelID: "agent",
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

// Which leg faces whoever charges for the call depends on who placed it. An
// agent dialling out sits on the originator, so the carrier's leg is the one
// dialled — and the agent's own auto-answering phone must never be read as the
// call becoming billable. On a call this platform placed, the leg towards the
// carrier *is* the originator, because we created it; looking past it for a
// dialled trunk that does not exist billed every AI outbound call at zero.
// Live, 2026-08-23: bill_sec 0 against the switch's own 21 on that same leg.
func TestAssembleBillsTheLegFacingTheCarrierOnEitherKindOfOutboundCall(t *testing.T) {
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
			{Role: RoleOriginator, Number: "1008", AgentID: &agentID,
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
	agentID := uuid.New()

	t.Run("an unanswered dial-out is silent", func(t *testing.T) {
		// The agent's own leg auto-answers and the switch bills it for the
		// whole time the far end rang; we bill the leg facing the carrier,
		// which never answered, so nothing.
		var buf bytes.Buffer
		cdr := assemblerLogging(&buf).assemble(t.Context(), Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(30),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "1008", AgentID: idPtr(agentID), ChannelID: "agent",
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
		var buf bytes.Buffer
		cdr := assemblerLogging(&buf).assemble(t.Context(), Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeInternal,
			CreatedAt: at(0), EndedAt: atPtr(30),
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "1008", AgentID: idPtr(agentID), ChannelID: "agent",
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
	t.Run("a rejected caller's row says what they dialled", func(t *testing.T) {
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
