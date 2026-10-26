// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// TestDialingHopNamesDialledNumber covers Issue #331:
// When an agent's call reaches nobody, the DIALING hop of its journey shows
// the number the agent dialled (cdr.ToNumber), not the agent's own extension.
// It verifies the invariant: for every unreached DIALING hop, leg.Label == cdr.ToNumber.
func TestDialingHopNamesDialledNumber(t *testing.T) {
	t.Parallel()
	flow := uuid.New()
	agentID := uuid.New()

	for _, tc := range []struct {
		name       string
		snap       Snapshot
		wantLegs   []string
		wantTo     string
		wantFrom   string
		wantStatus string
	}{
		{
			// 1. click-to-dial 1008 -> 075512 (no dialplan route, NO_ROUTE_DESTINATION, no trunk leg)
			name: "click-to-dial no route destination",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(1),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agentID,
					OtherNumber: "075512", ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(1),
					ReleaseCause: "NO_ROUTE_DESTINATION",
				}},
			},
			wantLegs:   []string{"DIALING:075512:0"},
			wantTo:     "075512",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 2. click-to-dial 1008 -> 18688886660, gateway down (no trunk leg)
			name: "click-to-dial gateway down",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agentID,
					OtherNumber: "18688886660", ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
					ReleaseCause: "GATEWAY_DOWN",
				}},
			},
			wantLegs:   []string{"DIALING:18688886660:0"},
			wantTo:     "18688886660",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 3. keypad dial 1008 -> 18688886660 (inbound to switch, DestinationNumber = dialled)
			name: "keypad dial agent hangs up before trunk exists",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(3),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agentID,
					OtherNumber: "18688886660", ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(3),
					ReleaseCause: "ORIGINATOR_CANCEL",
				}},
			},
			wantLegs:   []string{"DIALING:18688886660:0"},
			wantTo:     "18688886660",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 4. INTERNAL click-to-dial 1008 -> 1002, 1002 never rung
			name: "internal call between extensions unreached",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeInternal,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agentID,
					OtherNumber: "1002", ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
					ReleaseCause: "NO_ANSWER",
				}},
			},
			wantLegs:   []string{"DIALING:1002:0"},
			wantTo:     "1002",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 5. click-to-dial 1008 -> 95001, bot leg dialled and never answered (ORIGINATOR_CANCEL)
			name: "click-to-dial to bot DID unreached",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Bot: BotShare{DID: "95001", FlowID: &flow, FlowSlug: "novanet_support"},
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agentID,
						ChannelID: "phone", CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
					},
					{
						Role: RoleTarget, Number: "95001", ChannelID: "bot", IsBotLeg: true,
						CreatedAt: at(1), ReleasedAt: atPtr(6), ReleaseCause: "ORIGINATOR_CANCEL",
					},
				},
			},
			wantLegs:   []string{"DIALING:95001:0"},
			wantTo:     "95001",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 6. signed-out 1008 (no AgentID, ExtensionNumber only), as case 1
			name: "signed-out extension unreached call",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(1),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: nil,
					OtherNumber: "075512", ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(1),
					ReleaseCause: "NO_ROUTE_DESTINATION",
				}},
			},
			wantLegs:   []string{"DIALING:075512:0"},
			wantTo:     "075512",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 7. messy numbers dialled: +86 186-8888-6660, 0086..., *72, 3-digit 110
			name: "messy dialled numbers format preservation",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(2),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agentID,
					OtherNumber: "+86 186-8888-6660", ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(2),
					ReleaseCause: "NO_ANSWER",
				}},
			},
			wantLegs:   []string{"DIALING:+86 186-8888-6660:0"},
			wantTo:     "+86 186-8888-6660",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 8. OtherNumber empty (originate with no destination stamped): fallback to originator
			name: "empty destination fallback to originator number",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", ChannelID: "phone",
					OtherNumber: "", CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
					ReleaseCause: "NO_ANSWER",
				}},
			},
			wantLegs:   []string{"DIALING:1008:0"},
			wantTo:     "",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 9. control: AI outbound, NO_ANSWER / USER_BUSY / 480 from trunk
			name: "control AI outbound unanswered",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(9),
				Bot: BotShare{DID: "95099", FlowID: &flow, FlowSlug: "lead_qualification"},
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "18688886666", ChannelID: "customer",
					CreatedAt: at(0), ReleasedAt: atPtr(9), ReleaseCause: "NO_ANSWER",
				}},
			},
			wantLegs:   []string{"DIALING:18688886666:0"},
			wantTo:     "18688886666",
			wantFrom:   "95099",
			wantStatus: store.CDRStatusNoAnswer,
		},
		{
			// 10. control: click-to-dial whose trunk leg exists and got 480
			name: "control click-to-dial with trunk leg reached",
			snap: Snapshot{
				CallID:   uuid.New(),
				CallType: events.CallTypeOutbound,
				CreatedAt: at(0), EndedAt: atPtr(6),
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", ChannelID: "phone",
						OtherNumber: "18688886660", CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(6),
					},
					{
						Role: RoleTarget, Number: "18688886660", ChannelID: "trunk",
						CreatedAt: at(1), ReleasedAt: atPtr(6), ReleaseCause: "USER_BUSY",
					},
				},
			},
			wantLegs:   []string{"TRUNK:18688886660:did not answer"},
			wantTo:     "18688886660",
			wantFrom:   "1008",
			wantStatus: store.CDRStatusBusy,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assembler := newAssembler(&memoryLedger{}, staticQueues{})
			cdr := assembler.assemble(context.Background(), tc.snap)

			if got := journey(cdr.Legs); !slices.Equal(got, tc.wantLegs) {
				t.Errorf("legs = %v, want %v", got, tc.wantLegs)
			}
			if cdr.ToNumber != tc.wantTo {
				t.Errorf("ToNumber = %q, want %q", cdr.ToNumber, tc.wantTo)
			}
			if cdr.FromNumber != tc.wantFrom {
				t.Errorf("FromNumber = %q, want %q", cdr.FromNumber, tc.wantFrom)
			}
			if cdr.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", cdr.Status, tc.wantStatus)
			}

			// Invariant verification: if the only leg is DIALING and ToNumber is non-empty,
			// leg.Label must strictly equal cdr.ToNumber.
			if len(cdr.Legs) == 1 && cdr.Legs[0].Kind == "DIALING" && cdr.ToNumber != "" {
				if cdr.Legs[0].Label != cdr.ToNumber {
					t.Errorf("DIALING leg label %q does not match ToNumber %q", cdr.Legs[0].Label, cdr.ToNumber)
				}
			}
		})
	}
}

// TestDialingHop_CallFinishedPersistence verifies that an unreached click-to-dial
// call written via CallFinished stores the DIALING leg with the dialled number in the ledger.
func TestDialingHop_CallFinishedPersistence(t *testing.T) {
	t.Parallel()
	ledger := &memoryLedger{}
	assembler := newAssembler(ledger, staticQueues{})

	// Case 1: unreached click-to-dial to external destination
	snap1 := Snapshot{
		CallID:   uuid.New(),
		CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(2),
		Parties: []PartySnapshot{{
			Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008",
			OtherNumber: "075512", ChannelID: "phone",
			CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(2),
			ReleaseCause: "NO_ROUTE_DESTINATION",
		}},
	}
	assembler.CallFinished(snap1)

	// Case 5: unreached click-to-dial to bot DID
	flow := uuid.New()
	snap2 := Snapshot{
		CallID:   uuid.New(),
		CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(5),
		Bot: BotShare{DID: "95001", FlowID: &flow, FlowSlug: "novanet_support"},
		Parties: []PartySnapshot{
			{
				Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008",
				ChannelID: "phone", CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(5),
			},
			{
				Role: RoleTarget, Number: "95001", ChannelID: "bot", IsBotLeg: true,
				CreatedAt: at(1), ReleasedAt: atPtr(5), ReleaseCause: "ORIGINATOR_CANCEL",
			},
		},
	}
	assembler.CallFinished(snap2)

	deadline := time.Now().Add(2 * time.Second)
	for {
		ledger.mu.Lock()
		count := len(ledger.cdrs)
		ledger.mu.Unlock()
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 inserted CDRs within deadline, got %d", count)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ledger.mu.Lock()
	defer ledger.mu.Unlock()

	byID := map[uuid.UUID]store.CDR{}
	for _, c := range ledger.cdrs {
		byID[c.CallID] = c
	}

	cdr1, ok1 := byID[snap1.CallID]
	if !ok1 || len(cdr1.Legs) != 1 || cdr1.Legs[0].Kind != "DIALING" || cdr1.Legs[0].Label != "075512" {
		t.Errorf("cdr1 legs = %+v, want DIALING 075512", cdr1.Legs)
	}

	cdr2, ok2 := byID[snap2.CallID]
	if !ok2 || len(cdr2.Legs) != 1 || cdr2.Legs[0].Kind != "DIALING" || cdr2.Legs[0].Label != "95001" {
		t.Errorf("cdr2 legs = %+v, want DIALING 95001", cdr2.Legs)
	}
}
