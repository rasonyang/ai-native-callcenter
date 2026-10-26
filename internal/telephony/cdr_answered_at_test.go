// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

func atMs(ms int) time.Time     { return base.Add(time.Duration(ms) * time.Millisecond) }
func atMsPtr(ms int) *time.Time { t := atMs(ms); return &t }

// TestClickToDial_AnsweredAtOnlyOnFarSideAnswer tests the complete matrix of 17 shapes
// from issue #330:
// Rule: answeredAt is set only when somebody on the far side of the call answered.
// An agent's phone picking up in front of them (auto-answer) is never that answer.
func TestClickToDial_AnsweredAtOnlyOnFarSideAnswer(t *testing.T) {
	t.Parallel()

	agent1 := uuid.MustParse("00000000-0000-0000-0000-000000001008")
	agent2 := uuid.MustParse("00000000-0000-0000-0000-000000001002")
	flowID := uuid.MustParse("00000000-0000-0000-0000-000000009501")
	queueID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	type expectation struct {
		wantAnsweredAt time.Time
		wantStatus     string
		wantBillSec    int
	}

	tests := []struct {
		num  int
		name string
		snap Snapshot
		exp  expectation
	}{
		{
			num:  1,
			name: "click-to-dial, no trunk leg (gateway down; agent hangs up at 0.8 s before dial-out)",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atMsPtr(800),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atMsPtr(800),
				}},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{}, // absent
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    0,
			},
		},
		{
			num:  2,
			name: "click-to-dial, trunk leg 480 CALL_REJECTED at 4 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(4),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(4),
					},
					{
						Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
						CreatedAt: atMs(600), AnsweredAt: nil, ReleasedAt: atPtr(4), ReleaseCause: "CALL_REJECTED",
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{},
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    0,
			},
		},
		{
			num:  3,
			name: "click-to-dial, trunk USER_BUSY at 2 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(2),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(2),
					},
					{
						Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
						CreatedAt: atMs(600), AnsweredAt: nil, ReleasedAt: atPtr(2), ReleaseCause: causeUserBusy,
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{},
				wantStatus:     store.CDRStatusBusy,
				wantBillSec:    0,
			},
		},
		{
			num:  4,
			name: "click-to-dial, trunk rings out NO_ANSWER at 30 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(30),
					},
					{
						Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
						CreatedAt: atMs(600), AnsweredAt: nil, ReleasedAt: atPtr(30), ReleaseCause: "NO_ANSWER",
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{},
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    0,
			},
		},
		{
			num:  5,
			name: "click-to-dial to a customer, answered at 8 s, talked 22 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(30),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(30),
						Bridges: []BridgeSpan{{StartedAt: at(8), EndedAt: at(30)}},
					},
					{
						Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
						CreatedAt: atMs(600), AnsweredAt: atPtr(8), ReleasedAt: atPtr(30),
						Bridges: []BridgeSpan{{StartedAt: at(8), EndedAt: at(30)}},
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: at(8),
				wantStatus:     store.CDRStatusAnswered,
				wantBillSec:    22,
			},
		},
		{
			num:  6,
			name: "click-to-dial, trunk answered, codec mismatch, no bridge",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(10),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(10),
					},
					{
						Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
						CreatedAt: atMs(600), AnsweredAt: atPtr(2), ReleasedAt: atPtr(10),
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: at(2), // trunk leg's answer (billed)
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    8,
			},
		},
		{
			num:  7,
			name: "click-to-dial 1008 -> 1002, 1002 never rings",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(5),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(5),
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{},
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    0,
			},
		},
		{
			num:  8,
			name: "click-to-dial 1008 -> 1002, rings 6 s, answered, 40 s talk",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(46),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone1008",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(46),
						Bridges: []BridgeSpan{{StartedAt: at(6), EndedAt: at(46)}},
					},
					{
						Role: RoleTarget, Number: "1002", ExtensionNumber: "1002", AgentID: &agent2, ChannelID: "phone1002",
						CreatedAt: atMs(600), AnsweredAt: atPtr(6), ReleasedAt: atPtr(46),
						Bridges: []BridgeSpan{{StartedAt: at(6), EndedAt: at(46)}},
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: at(6), // 1002's answer, not 1008's
				wantStatus:     store.CDRStatusAnswered,
				wantBillSec:    0,
			},
		},
		{
			num:  9,
			name: "keypad dial 1008 -> 1002, answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(20),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone1008",
						CreatedAt: at(0), AnsweredAt: atPtr(6), ReleasedAt: atPtr(20),
						Bridges: []BridgeSpan{{StartedAt: at(6), EndedAt: at(20)}},
					},
					{
						Role: RoleTarget, Number: "1002", ExtensionNumber: "1002", AgentID: &agent2, ChannelID: "phone1002",
						CreatedAt: atMs(200), AnsweredAt: atPtr(6), ReleasedAt: atPtr(20),
						Bridges: []BridgeSpan{{StartedAt: at(6), EndedAt: at(20)}},
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: at(6),
				wantStatus:     store.CDRStatusAnswered,
				wantBillSec:    0,
			},
		},
		{
			num:  10,
			name: "1008 -> 1002, nobody signed in at 1002 (ExtensionNumber only, no AgentID), answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(20),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone1008",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(20),
						Bridges: []BridgeSpan{{StartedAt: at(5), EndedAt: at(20)}},
					},
					{
						Role: RoleTarget, Number: "1002", ExtensionNumber: "1002", AgentID: nil, ChannelID: "phone1002",
						CreatedAt: atMs(200), AnsweredAt: atPtr(5), ReleasedAt: atPtr(20),
						Bridges: []BridgeSpan{{StartedAt: at(5), EndedAt: at(20)}},
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: at(5),
				wantStatus:     store.CDRStatusAnswered,
				wantBillSec:    0,
			},
		},
		{
			num:  11,
			name: "click-to-dial 1008 -> 95001, bot answers at 1.5 s, bot hangs up at 40 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(40),
				Bot: BotShare{DID: "95001", FlowID: &flowID, FlowSlug: "novanet_support"},
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone1008",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(40),
					},
					{
						Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
						CreatedAt: atMs(500), AnsweredAt: atMsPtr(1500), ReleasedAt: atPtr(40),
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: atMs(1500), // bot leg's answer
				wantStatus:     store.CDRStatusAnswered,
				wantBillSec:    0,
			},
		},
		{
			num:  12,
			name: "click-to-dial 1008 -> 95001, no bot slot (bot leg never answered)",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(5),
				Bot: BotShare{DID: "95001", FlowID: &flowID, FlowSlug: "novanet_support"},
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone1008",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(5),
					},
					{
						Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
						CreatedAt: atMs(500), AnsweredAt: nil, ReleasedAt: atPtr(5),
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{},
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    0,
			},
		},
		{
			num:  13,
			name: "signed-out 1008 (no AgentID) click-to-dial, trunk leg 480",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(4),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: nil, ChannelID: "phone",
						CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(4),
					},
					{
						Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
						CreatedAt: atMs(600), AnsweredAt: nil, ReleasedAt: atPtr(4), ReleaseCause: "CALL_REJECTED",
					},
				},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{},
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    0,
			},
		},
		{
			num:  14,
			name: "INBOUND, bot answers, abandoned in queue after 25 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInbound,
				CreatedAt: at(0), EndedAt: atPtr(26),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "13800138000", ChannelID: "caller",
						CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(26),
					},
					{
						Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
						CreatedAt: at(1), AnsweredAt: atMsPtr(1500), ReleasedAt: atPtr(10),
					},
				},
				QueueStays: []QueueFacts{{
					Name: "support",
					JoinedAt: at(10), LeftAt: at(26), Cause: "ABANDONED",
				}},
			},
			exp: expectation{
				wantAnsweredAt: at(1), // caller leg (unchanged)
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    25,
			},
		},
		{
			num:  15,
			name: "AI outbound, customer answers, BOT_BRIDGE_UNCONFIRMED after 30 s",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(30),
				Bot: BotShare{DID: "95099", FlowID: &flowID, FlowSlug: "lead_gen"},
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(30),
					ReleaseCause: "BOT_BRIDGE_UNCONFIRMED",
				}},
			},
			exp: expectation{
				wantAnsweredAt: at(0), // customer leg (unchanged)
				wantStatus:     store.CDRStatusAnswered,
				wantBillSec:    30,
			},
		},
		{
			num:  16,
			name: "INTERNAL on a dialplan-spawned leg (originator has no ExtensionNumber)",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(10),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ChannelID: "spawned",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(10),
				}},
			},
			exp: expectation{
				wantAnsweredAt: at(0), // originator (unchanged)
				wantStatus:     store.CDRStatusAnswered,
				wantBillSec:    0,
			},
		},
		{
			num:  17,
			name: "handset dial into a queue extension, queue answers, nobody takes it",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(20),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(20),
				}},
				QueueStays: []QueueFacts{{
					Name: "internal_queue",
					JoinedAt: atMs(500), LeftAt: at(20), Cause: "ABANDONED",
				}},
			},
			exp: expectation{
				wantAnsweredAt: time.Time{}, // absent: nobody in the queue took it
				wantStatus:     store.CDRStatusNoAnswer,
				wantBillSec:    0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdr := newAssembler(&memoryLedger{}, staticQueues{"support": queueID, "internal_queue": queueID}).assemble(t.Context(), tc.snap)

			if !cdr.AnsweredAt.Equal(tc.exp.wantAnsweredAt) {
				t.Errorf("#%d answeredAt = %v, want %v", tc.num, cdr.AnsweredAt, tc.exp.wantAnsweredAt)
			}
			if cdr.Status != tc.exp.wantStatus {
				t.Errorf("#%d status = %s, want %s", tc.num, cdr.Status, tc.exp.wantStatus)
			}
			if cdr.BillSec != tc.exp.wantBillSec {
				t.Errorf("#%d billSec = %d, want %d", tc.num, cdr.BillSec, tc.exp.wantBillSec)
			}

			// Invariant check: on an agent-placed call with no billed leg,
			// answeredAt is set <=> a leg other than the originator answered.
			originator, _ := split(tc.snap)
			if isAgentPlaced(originator) {
				dialled := dialledLegs(tc.snap)
				billed := billedLeg(tc.snap, originator, dialled, true)
				if billed == nil || billed.AnsweredAt == nil {
					hasOtherAnswer := false
					for _, p := range tc.snap.Parties {
						if p.Role != RoleOriginator && p.AnsweredAt != nil {
							hasOtherAnswer = true
							break
						}
					}
					isSet := !cdr.AnsweredAt.IsZero()
					if isSet != hasOtherAnswer {
						t.Errorf("#%d invariant violated: isSet=%v but hasOtherAnswer=%v", tc.num, isSet, hasOtherAnswer)
					}
				}
			}
		})
	}
}

// TestClickToDial_CallFinishedIntegration verifies that CallFinished persists
// the row with answeredAt properly set (or omitted/NULL) for shapes 1, 8 and 11.
func TestClickToDial_CallFinishedIntegration(t *testing.T) {
	t.Parallel()

	agent1 := uuid.MustParse("00000000-0000-0000-0000-000000001008")
	agent2 := uuid.MustParse("00000000-0000-0000-0000-000000001002")
	flowID := uuid.MustParse("00000000-0000-0000-0000-000000009501")

	// Shape 1: click-to-dial, no trunk leg -> answeredAt must be zero (omitted)
	snap1 := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atMsPtr(800),
		Parties: []PartySnapshot{{
			Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone",
			CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atMsPtr(800),
		}},
	}
	cdrs1 := finishCall(t, snap1)
	if len(cdrs1) != 1 {
		t.Fatalf("shape 1: wrote %d cdrs, want 1", len(cdrs1))
	}
	if !cdrs1[0].AnsweredAt.IsZero() {
		t.Errorf("shape 1: answeredAt = %v, want zero", cdrs1[0].AnsweredAt)
	}
	if cdrs1[0].Status != store.CDRStatusNoAnswer {
		t.Errorf("shape 1: status = %s, want NO_ANSWER", cdrs1[0].Status)
	}

	// Shape 8: click-to-dial 1008 -> 1002, 1002 answers at 6 s -> answeredAt = at(6)
	snap8 := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInternal,
		CreatedAt: at(0), EndedAt: atPtr(46),
		Parties: []PartySnapshot{
			{
				Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone1008",
				CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(46),
				Bridges: []BridgeSpan{{StartedAt: at(6), EndedAt: at(46)}},
			},
			{
				Role: RoleTarget, Number: "1002", ExtensionNumber: "1002", AgentID: &agent2, ChannelID: "phone1002",
				CreatedAt: atMs(600), AnsweredAt: atPtr(6), ReleasedAt: atPtr(46),
				Bridges: []BridgeSpan{{StartedAt: at(6), EndedAt: at(46)}},
			},
		},
	}
	cdrs8 := finishCall(t, snap8)
	if len(cdrs8) != 1 {
		t.Fatalf("shape 8: wrote %d cdrs, want 1", len(cdrs8))
	}
	if !cdrs8[0].AnsweredAt.Equal(at(6)) {
		t.Errorf("shape 8: answeredAt = %v, want %v", cdrs8[0].AnsweredAt, at(6))
	}

	// Shape 11: click-to-dial 1008 -> 95001, bot answers at 1.5 s -> answeredAt = atMs(1500)
	snap11 := Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(40),
		Bot: BotShare{DID: "95001", FlowID: &flowID, FlowSlug: "novanet_support"},
		Parties: []PartySnapshot{
			{
				Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent1, ChannelID: "phone1008",
				CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(40),
			},
			{
				Role: RoleTarget, Number: "95001", IsBotLeg: true, ChannelID: "bot",
				CreatedAt: atMs(500), AnsweredAt: atMsPtr(1500), ReleasedAt: atPtr(40),
			},
		},
	}
	cdrs11 := finishCall(t, snap11)
	if len(cdrs11) != 1 {
		t.Fatalf("shape 11: wrote %d cdrs, want 1", len(cdrs11))
	}
	if !cdrs11[0].AnsweredAt.Equal(atMs(1500)) {
		t.Errorf("shape 11: answeredAt = %v, want %v", cdrs11[0].AnsweredAt, atMs(1500))
	}
}
