// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// When an agent's call reached nobody, its one DIALING hop was labelled with
// the originator's number, which on a call one of our phones placed is the
// agent's own extension: an agent who dialled a wrong number read as having
// dialled themselves, while the number they did dial sat in toNumber on the
// same row. DIALING is the number being rung (the contract's LegKind), so its
// label is toNumber on every shape; the placer stays in fromNumber (#331).
// TestAnUnansweredCallIsStillDialingAndAReachedBotIsStillTheJourney pins the
// gateway-down, internal and bot shapes; this one pins the rest of the issue's
// matrix, including numbers as messy as the dialplan receives them, and holds
// every row to the invariant that the label and toNumber are one fact.
func TestTheDialingHopOfAnAgentsUnreachedCallNamesTheNumberDialled(t *testing.T) {
	t.Parallel()
	agent := uuid.New()

	// A click-to-dial that stopped at the originator: the phone auto-answered
	// 0.3-1.2 s in, and the call ended before any other leg existed.
	dialled := func(agentID *uuid.UUID, number string, releasedMs int, cause string) Snapshot {
		return Snapshot{
			CallID: uuid.New(), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atMsPtr(releasedMs),
			Parties: []PartySnapshot{{
				Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: agentID,
				OtherNumber: number, ChannelID: "phone",
				CreatedAt: at(0), AnsweredAt: atMsPtr(600), ReleasedAt: atMsPtr(releasedMs), ReleaseCause: cause,
			}},
		}
	}

	for _, tc := range []struct {
		name string
		snap Snapshot
		want string // the number as the row carries it, byte for byte
	}{
		{"a number no dialplan route matches", dialled(&agent, "075512", 1100, "NO_ROUTE_DESTINATION"), "075512"},
		{"a keypad dial the agent abandons at 3 s before a trunk leg exists", dialled(&agent, "18688886660", 3000, "ORIGINATOR_CANCEL"), "18688886660"},
		{"an agent who never signed in, the same shape", dialled(nil, "075512", 1100, "NO_ROUTE_DESTINATION"), "075512"},
		{"a number with a plus, spaces and dashes", dialled(&agent, "+86 186-8888-6660", 2000, "NO_ANSWER"), "+86 186-8888-6660"},
		{"a number with the 00 international prefix", dialled(&agent, "008618688886660", 2000, "NO_ANSWER"), "008618688886660"},
		{"a feature code", dialled(&agent, "*72", 2000, "NO_ROUTE_DESTINATION"), "*72"},
		{"a three-digit emergency number", dialled(&agent, "110", 2000, "NO_ROUTE_DESTINATION"), "110"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), tc.snap)

			if want := []string{"DIALING:" + tc.want + ":0"}; !slices.Equal(journey(cdr.Legs), want) {
				t.Errorf("legs = %v, want %v", journey(cdr.Legs), want)
			}
			if cdr.ToNumber != tc.want {
				t.Errorf("toNumber = %q, want %q", cdr.ToNumber, tc.want)
			}
			if cdr.FromNumber != "1008" {
				t.Errorf("fromNumber = %q, want the placer, 1008", cdr.FromNumber)
			}
			if cdr.Status != store.CDRStatusNoAnswer {
				t.Errorf("status = %s, want NO_ANSWER", cdr.Status)
			}
			// The invariant: the label and toNumber are one fact.
			if len(cdr.Legs) != 1 || cdr.Legs[0].Kind != "DIALING" || cdr.Legs[0].Label != cdr.ToNumber {
				t.Errorf("legs %+v: the DIALING label must equal toNumber %q", cdr.Legs, cdr.ToNumber)
			}
		})
	}
}

// A call whose destination was never stamped has no toNumber to name, and the
// label is required, so the hop falls back to the originator's number instead
// of an empty string (#331).
func TestADialingHopWithNoDestinationFallsBackToTheOriginator(t *testing.T) {
	t.Parallel()
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeOutbound,
		CreatedAt: at(0), EndedAt: atPtr(6),
		Parties: []PartySnapshot{{
			Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", ChannelID: "phone",
			CreatedAt: at(0), AnsweredAt: atMsPtr(700), ReleasedAt: atPtr(6), ReleaseCause: "NO_ANSWER",
		}},
	})
	if cdr.ToNumber != "" {
		t.Fatalf("toNumber = %q, want empty: the fixture is meant to carry no destination", cdr.ToNumber)
	}
	if want := []string{"DIALING:1008:0"}; !slices.Equal(journey(cdr.Legs), want) {
		t.Errorf("legs = %v, want %v", journey(cdr.Legs), want)
	}
}

// The rule is not only an agent's: an inbound caller who hung up before the
// switch answered was ringing the number they dialled, so the hop names that
// number and the caller stays in fromNumber. It used to name the caller, the
// same contradiction from the other side (#331).
func TestTheDialingHopOfAnUnansweredInboundCallNamesTheNumberTheCallerDialled(t *testing.T) {
	t.Parallel()
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), Snapshot{
		CallID: uuid.New(), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atMsPtr(1800),
		Parties: []PartySnapshot{{
			Role: RoleOriginator, Number: "13800138000", OtherNumber: "95001", ChannelID: "caller",
			CreatedAt: at(0), ReleasedAt: atMsPtr(1800), ReleaseCause: "ORIGINATOR_CANCEL",
		}},
	})
	if want := []string{"DIALING:95001:0"}; !slices.Equal(journey(cdr.Legs), want) {
		t.Errorf("legs = %v, want %v", journey(cdr.Legs), want)
	}
	if cdr.ToNumber != "95001" || cdr.FromNumber != "13800138000" {
		t.Errorf("toNumber = %q, fromNumber = %q, want 95001 and the caller", cdr.ToNumber, cdr.FromNumber)
	}
	if cdr.Status != store.CDRStatusNoAnswer || !cdr.AnsweredAt.IsZero() {
		t.Errorf("status = %s, answeredAt = %v, want NO_ANSWER and absent", cdr.Status, cdr.AnsweredAt)
	}
}

// The stored row is what the call detail screen and the API read. Row 1 (no
// route, no bot) and row 5 (a bot number whose leg never answered) go through
// CallFinished, on its own goroutine, and the ledger must hold the dialled
// number as the hop's label (#331).
func TestTheStoredJourneyOfAnUnreachedAgentCallNamesTheNumberDialled(t *testing.T) {
	t.Parallel()
	flow := uuid.New()
	agent := uuid.New()

	for _, tc := range []struct {
		name string
		snap Snapshot
		want string
	}{
		{
			name: "no route",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(2),
				Parties: []PartySnapshot{{
					Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent,
					OtherNumber: "075512", ChannelID: "phone",
					CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(2), ReleaseCause: "NO_ROUTE_DESTINATION",
				}},
			},
			want: "075512",
		},
		{
			name: "a bot number whose leg never answered",
			snap: Snapshot{
				CallID: uuid.New(), CallType: events.CallTypeOutbound, CreatedAt: at(0), EndedAt: atPtr(5),
				Bot: BotShare{DID: "95001", FlowID: &flow, FlowSlug: "novanet_support"},
				Parties: []PartySnapshot{
					{
						Role: RoleOriginator, Number: "1008", ExtensionNumber: "1008", AgentID: &agent,
						ChannelID: "phone", CreatedAt: at(0), AnsweredAt: atMsPtr(500), ReleasedAt: atPtr(5),
					},
					{
						Role: RoleTarget, Number: "95001", ChannelID: "bot", IsBotLeg: true,
						CreatedAt: at(1), ReleasedAt: atPtr(5), ReleaseCause: "ORIGINATOR_CANCEL",
					},
				},
			},
			want: "95001",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := finishCall(t, tc.snap)
			if len(rows) != 1 {
				t.Fatalf("wrote %d rows, want 1", len(rows))
			}
			if want := []string{"DIALING:" + tc.want + ":0"}; !slices.Equal(journey(rows[0].Legs), want) {
				t.Errorf("stored legs = %v, want %v", journey(rows[0].Legs), want)
			}
		})
	}
}
