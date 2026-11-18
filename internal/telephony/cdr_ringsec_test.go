// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// On a call placed to an outside number, ringSec is the carrier-facing leg's
// ringing: its first 180 or 183 to its answer, or to its release when it did
// not answer, and 0 when it never rang (#576).
func TestARingOnACallPlacedToAnOutsideNumberIsTheCarrierLegsFirstProvisional(t *testing.T) {
	t.Parallel()
	customerLeg := func(mod func(*PartySnapshot)) Snapshot {
		leg := PartySnapshot{Role: RoleOriginator, Number: "13800138000", ChannelID: "customer",
			CreatedAt: at(0), ReleasedAt: atPtr(40), ReleaseCause: "RECOVERY_ON_TIMER_EXPIRE"}
		mod(&leg)
		return Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(40),
			Bot:     BotShare{DID: "95012"},
			Parties: []PartySnapshot{leg},
		}
	}
	assemble := func(s Snapshot) store.CDR {
		return newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), s)
	}

	t.Run("nobody answered: first 180 to release, with the far end's 408", func(t *testing.T) {
		t.Parallel()
		got := assemble(customerLeg(func(p *PartySnapshot) {
			p.RingStartedAt, p.ReleasedAt, p.SIPStatus = atPtr(3), atPtr(26), 408
		}))
		if got.Status != store.CDRStatusNoAnswer {
			t.Fatalf("status = %s, want NO_ANSWER", got.Status)
		}
		if got.RingSec != 23 {
			t.Errorf("ringSec = %d, want 23 (180 at 3, released at 26)", got.RingSec)
		}
		if got.Tech[store.CDRTechSIPStatus] != 408 {
			t.Errorf("tech.sipStatus = %v, want 408", got.Tech[store.CDRTechSIPStatus])
		}
		if want := store.FormatRingStartedAt(at(3)); got.Tech[store.CDRTechRingStartedAt] != want {
			t.Errorf("tech.ringStartedAt = %v, want %s", got.Tech[store.CDRTechRingStartedAt], want)
		}
	})

	t.Run("nobody answered and it never rang: 0, and no ringStartedAt", func(t *testing.T) {
		t.Parallel()
		got := assemble(customerLeg(func(p *PartySnapshot) { p.SIPStatus = 408 }))
		if got.RingSec != 0 {
			t.Errorf("ringSec = %d, want 0 (no 180 or 183)", got.RingSec)
		}
		if _, ok := got.Tech[store.CDRTechRingStartedAt]; ok {
			t.Error("tech.ringStartedAt is present on a leg that never rang")
		}
		if got.Tech[store.CDRTechSIPStatus] != 408 {
			t.Errorf("tech.sipStatus = %v, want 408", got.Tech[store.CDRTechSIPStatus])
		}
	})

	t.Run("our own cancel carries no sipStatus", func(t *testing.T) {
		t.Parallel()
		got := assemble(customerLeg(func(p *PartySnapshot) {
			p.RingStartedAt, p.ReleaseCause = atPtr(2), "NO_ANSWER"
		}))
		if got.RingSec != 38 {
			t.Errorf("ringSec = %d, want 38 (180 at 2, released at 40)", got.RingSec)
		}
		if _, ok := got.Tech[store.CDRTechSIPStatus]; ok {
			t.Errorf("tech.sipStatus = %v, want none", got.Tech[store.CDRTechSIPStatus])
		}
	})

	t.Run("answered by the bot: first 180 to the answer", func(t *testing.T) {
		t.Parallel()
		s := customerLeg(func(p *PartySnapshot) {
			p.RingStartedAt, p.AnsweredAt, p.ReleaseCause = atPtr(3), atPtr(8), "NORMAL_CLEARING"
		})
		s.Bot.Sec = 30
		got := assemble(s)
		if got.Status != store.CDRStatusAnswered || got.RingSec != 5 {
			t.Errorf("status %s, ringSec %d; want ANSWERED, 5", got.Status, got.RingSec)
		}
	})

	t.Run("answered without a provisional response: 0", func(t *testing.T) {
		t.Parallel()
		s := customerLeg(func(p *PartySnapshot) {
			p.AnsweredAt, p.ReleaseCause = atPtr(8), "NORMAL_CLEARING"
		})
		s.Bot.Sec = 30
		if got := assemble(s); got.RingSec != 0 {
			t.Errorf("ringSec = %d, want 0", got.RingSec)
		}
	})

	t.Run("a clock that disagrees never gives a negative ring", func(t *testing.T) {
		t.Parallel()
		got := assemble(customerLeg(func(p *PartySnapshot) {
			p.RingStartedAt, p.ReleasedAt = atPtr(30), atPtr(20)
		}))
		if got.RingSec != 0 {
			t.Errorf("ringSec = %d, want 0", got.RingSec)
		}
	})

	t.Run("taken over by a person: still the customer's ringing", func(t *testing.T) {
		t.Parallel()
		agent := uuid.Must(uuid.NewV7())
		s := customerLeg(func(p *PartySnapshot) {
			p.RingStartedAt, p.AnsweredAt, p.ReleaseCause = atPtr(3), atPtr(8), "NORMAL_CLEARING"
			p.Bridges = []BridgeSpan{{OtherChannelID: "agent", StartedAt: at(30), EndedAt: at(40)}}
		})
		s.Bot.Sec = 20
		s.Parties = append(s.Parties, PartySnapshot{
			Role: RoleTarget, AgentID: &agent, ChannelID: "agent", Number: "1008",
			CreatedAt: at(21), AnsweredAt: atPtr(24), ReleasedAt: atPtr(40),
			Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(30), EndedAt: at(40)}},
		})
		got := assemble(s)
		if got.Status != store.CDRStatusAnswered {
			t.Fatalf("status = %s, want ANSWERED", got.Status)
		}
		// The agent leg alone would say 9 (created at 21, bridged at 30).
		if got.RingSec != 5 {
			t.Errorf("ringSec = %d, want 5 (the customer's 180 at 3, answer at 8)", got.RingSec)
		}
	})
}

func TestARingOnACallAnAgentPlacedIsTheTrunkLegs(t *testing.T) {
	t.Parallel()
	agent := uuid.Must(uuid.NewV7())
	placer := PartySnapshot{Role: RoleOriginator, Number: "1008", AgentID: &agent, ExtensionNumber: "1008",
		ChannelID: "chan-agent", CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(40)}
	assemble := func(parties ...PartySnapshot) store.CDR {
		return newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(40), Parties: append([]PartySnapshot{placer}, parties...),
		})
	}

	t.Run("nobody answered: the leg that rang first", func(t *testing.T) {
		t.Parallel()
		got := assemble(
			PartySnapshot{Role: RoleTarget, Number: "18688886669", ChannelID: "a", CreatedAt: at(2),
				RingStartedAt: atPtr(9), ReleasedAt: atPtr(15), SIPStatus: 486},
			PartySnapshot{Role: RoleTarget, Number: "18688886669", ChannelID: "b", CreatedAt: at(16),
				RingStartedAt: atPtr(4), ReleasedAt: atPtr(30), SIPStatus: 408},
		)
		if got.Status != store.CDRStatusNoAnswer {
			t.Fatalf("status = %s, want NO_ANSWER", got.Status)
		}
		if got.RingSec != 26 {
			t.Errorf("ringSec = %d, want 26 (the leg that rang at 4, released at 30)", got.RingSec)
		}
		if got.Tech[store.CDRTechSIPStatus] != 408 {
			t.Errorf("tech.sipStatus = %v, want 408", got.Tech[store.CDRTechSIPStatus])
		}
	})

	t.Run("answered: the leg that answered", func(t *testing.T) {
		t.Parallel()
		got := assemble(
			PartySnapshot{Role: RoleTarget, Number: "18688886669", ChannelID: "a", CreatedAt: at(2),
				RingStartedAt: atPtr(3), ReleasedAt: atPtr(5)},
			PartySnapshot{Role: RoleTarget, Number: "18688886669", ChannelID: "b", CreatedAt: at(6),
				RingStartedAt: atPtr(7), AnsweredAt: atPtr(12), ReleasedAt: atPtr(40),
				Bridges: []BridgeSpan{{OtherChannelID: "chan-agent", StartedAt: at(12), EndedAt: at(40)}}},
		)
		if got.RingSec != 5 {
			t.Errorf("ringSec = %d, want 5 (180 at 7, answered at 12)", got.RingSec)
		}
	})

	t.Run("an agent who dialled a bot number has no carrier leg: 0", func(t *testing.T) {
		t.Parallel()
		s := Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(40), Bot: BotShare{Sec: 30, DID: "95012", IsSessionStarted: true},
			Parties: []PartySnapshot{placer, {Role: RoleTarget, IsBotLeg: true, ChannelID: "bot",
				CreatedAt: at(1), AnsweredAt: atPtr(2), ReleasedAt: atPtr(40)}},
		}
		got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), s)
		if got.RingSec != 0 {
			t.Errorf("ringSec = %d, want 0", got.RingSec)
		}
		if _, ok := got.Tech[store.CDRTechRingStartedAt]; ok {
			t.Error("tech.ringStartedAt on a call with no carrier-facing leg")
		}
	})
}

// Calls that are not placed to an outside number keep the agent-leg
// definition of ringSec, whatever the originator's own ring progress says.
func TestRingSecOfAnInboundCallIsStillTheAgentLegs(t *testing.T) {
	t.Parallel()
	agent := uuid.Must(uuid.NewV7())
	s := Snapshot{
		CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeInbound,
		CreatedAt: at(0), EndedAt: atPtr(60),
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "13800138000", ChannelID: "customer", CreatedAt: at(0),
				RingStartedAt: atPtr(1), AnsweredAt: atPtr(1), ReleasedAt: atPtr(60),
				Bridges: []BridgeSpan{{OtherChannelID: "agent", StartedAt: at(30), EndedAt: at(60)}}},
			{Role: RoleTarget, AgentID: &agent, ChannelID: "agent", Number: "1008",
				CreatedAt: at(22), AnsweredAt: atPtr(28), ReleasedAt: atPtr(60),
				Bridges: []BridgeSpan{{OtherChannelID: "customer", StartedAt: at(30), EndedAt: at(60)}}},
		},
	}
	got := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), s)
	if got.RingSec != 8 {
		t.Errorf("ringSec = %d, want 8 (agent leg created at 22, bridged at 30)", got.RingSec)
	}
	if _, ok := got.Tech[store.CDRTechRingStartedAt]; ok {
		t.Error("an inbound call carries tech.ringStartedAt")
	}
}

// Which side ended the carrier-facing leg reaches the CDR's tech as the
// domain word, never the switch's: a far end that ended the ringing INVITE
// with a 487 of its own, and our own cancel at the ring timeout. A leg the
// switch said nothing about carries no endedBy, and neither does a call that
// was not placed to an outside number.
func TestTheCDRSaysWhichSideEndedTheCarrierFacingLeg(t *testing.T) {
	t.Parallel()
	placed := func(mod func(*PartySnapshot)) Snapshot {
		leg := PartySnapshot{Role: RoleOriginator, Number: "+12025550116", ChannelID: "customer",
			CreatedAt: at(0), RingStartedAt: atPtr(1), ReleasedAt: atPtr(8)}
		mod(&leg)
		return Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(8), Bot: BotShare{DID: "95012"},
			Parties: []PartySnapshot{leg},
		}
	}
	assemble := func(s Snapshot) store.CDR {
		return newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), s)
	}
	tests := []struct {
		name        string
		snap        Snapshot
		wantEndedBy any // nil: absent
		wantStatus  any // nil: absent
	}{
		{"the far end's own 487 after ringing", placed(func(p *PartySnapshot) {
			p.ReleaseCause, p.SIPStatus, p.EndedBy = "ORIGINATOR_CANCEL", 487, EndedByPeer
		}), "PEER", 487},
		{"our own cancel at the ring timeout", placed(func(p *PartySnapshot) {
			p.ReleaseCause, p.EndedBy = "NO_ANSWER", EndedByPlatform
		}), "PLATFORM", nil},
		{"the switch did not say", placed(func(p *PartySnapshot) {
			p.ReleaseCause, p.SIPStatus = "ORIGINATOR_CANCEL", 487
		}), nil, 487},
		{"an inbound call", Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeInbound,
			CreatedAt: at(0), EndedAt: atPtr(8),
			Parties: []PartySnapshot{{Role: RoleOriginator, Number: "13800138000", ChannelID: "customer",
				CreatedAt: at(0), ReleasedAt: atPtr(8), ReleaseCause: "ORIGINATOR_CANCEL", EndedBy: EndedByPeer}},
		}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := assemble(tt.snap)
			if v, ok := got.Tech[store.CDRTechEndedBy]; ok != (tt.wantEndedBy != nil) || (ok && v != tt.wantEndedBy) {
				t.Errorf("tech.endedBy = %v (present %v), want %v", v, ok, tt.wantEndedBy)
			}
			if v, ok := got.Tech[store.CDRTechSIPStatus]; ok != (tt.wantStatus != nil) || (ok && v != tt.wantStatus) {
				t.Errorf("tech.sipStatus = %v (present %v), want %v", v, ok, tt.wantStatus)
			}
		})
	}
}
