// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// aicc_outbound.lua stamps the caller-id number on the trunk leg of every call
// an agent dials out (#160), so the per-DID line count can observe the leg.
// These tests pin what that stamp must not do: change a call's type or
// direction, or turn a human call's ledger row into a DID call.

func trunkLegHeaders(event string, extra map[string]string) map[string]string {
	h := map[string]string{
		"Event-Name":                event,
		"Unique-ID":                 "019ff973-6b32-7557-b7c4-11b3fdb692f1",
		"Channel-Name":              "sofia/external/18688886669@192.168.31.5",
		"Call-Direction":            "outbound",
		"Caller-Destination-Number": "18688886669",
		"Caller-Context":            "aicc",
		"variable_aicc_call_id":     "0c0ac1c1-0000-4000-8000-000000000001",
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func TestLegDIDIsReadOffAnyLegThatCarriesIt(t *testing.T) {
	t.Parallel()
	with, ok := Normalize(event(trunkLegHeaders("CHANNEL_CREATE", map[string]string{"variable_aicc_did": "95001"})))
	if !ok || with.LegDID != "95001" {
		t.Fatalf("LegDID = %q (ok=%v), want 95001", with.LegDID, ok)
	}
	without, ok := Normalize(event(trunkLegHeaders("CHANNEL_CREATE", nil)))
	if !ok || without.LegDID != "" {
		t.Fatalf("LegDID = %q (ok=%v), want empty on a leg that carries none", without.LegDID, ok)
	}
	// A hand-dialled trunk leg is observed through the same field: outbound,
	// carrying the id, no placement stamp.
	if with.Direction != DirectionOutbound {
		t.Errorf("Direction = %q, want OUTBOUND", with.Direction)
	}
}

func TestAStampedTrunkLegIsTypedByItsCalleeAsBefore(t *testing.T) {
	t.Parallel()
	for _, did := range []string{"", "95001"} {
		extra := map[string]string{}
		if did != "" {
			extra["variable_aicc_did"] = did
		}
		got, _ := Normalize(event(trunkLegHeaders("CHANNEL_CREATE", extra)))
		if ct := callTypeOf(got); ct != events.CallTypeOutbound {
			t.Errorf("did=%q: callTypeOf = %q, want OUTBOUND", did, ct)
		}
		if got.CallTypeHint != "" {
			t.Errorf("did=%q: CallTypeHint = %q, want none read off the DID", did, got.CallTypeHint)
		}
		if isBotLeg(got) {
			t.Errorf("did=%q: a trunk leg dialled at a customer was read as the bot leg", did)
		}
	}
}

func TestAStampedTrunkLegDoesNotMakeAHumanCallADIDCall(t *testing.T) {
	t.Parallel()
	agentID := uuid.Must(uuid.NewV7())
	hangup := func(extra map[string]string) SwitchEvent {
		ev, ok := Normalize(event(trunkLegHeaders("CHANNEL_HANGUP_COMPLETE", extra)))
		if !ok {
			t.Fatal("hangup did not normalize")
		}
		return ev
	}
	stamped := hangup(map[string]string{"variable_aicc_did": "95001"})
	if stamped.LegDID != "95001" {
		t.Fatalf("LegDID = %q, want 95001", stamped.LegDID)
	}
	if stamped.Bot.DID != "" {
		t.Fatalf("Bot = %+v, want the observed DID kept out of the bot share", stamped.Bot)
	}

	// The ledger row is the same with and without the stamp.
	rowFor := func(ev SwitchEvent) store.CDR {
		var bot BotShare
		bot.Merge(ev.Bot)
		snap := Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(30), Bot: bot,
			Parties: []PartySnapshot{
				{Role: RoleOriginator, Number: "1008", OtherNumber: "18688886669", AgentID: &agentID, ExtensionNumber: "1008", ChannelID: "chan-agent",
					AnsweredAt: atPtr(0), ReleasedAt: atPtr(30),
					Bridges: []BridgeSpan{{OtherChannelID: "chan-out", StartedAt: at(8), EndedAt: at(30)}}},
				{Role: RoleTarget, Number: "18688886669", ChannelID: "chan-out",
					CreatedAt: at(2), AnsweredAt: atPtr(8), ReleasedAt: atPtr(30),
					Bridges: []BridgeSpan{{OtherChannelID: "chan-agent", StartedAt: at(8), EndedAt: at(30)}}},
			},
		}
		return newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
	}
	before, after := rowFor(hangup(nil)), rowFor(stamped)
	if after.CallType != before.CallType || after.DID != before.DID ||
		after.FromNumber != before.FromNumber || after.ToNumber != before.ToNumber {
		t.Errorf("stamped row type/did/from/to = %s/%q/%q/%q, unstamped = %s/%q/%q/%q",
			after.CallType, after.DID, after.FromNumber, after.ToNumber,
			before.CallType, before.DID, before.FromNumber, before.ToNumber)
	}
	if after.ToNumber != "18688886669" || after.DID != "" {
		t.Errorf("row to/did = %q/%q, want the number dialled and no DID", after.ToNumber, after.DID)
	}
}

func TestTheDIDsThatMeanSomethingAreStillRead(t *testing.T) {
	t.Parallel()
	// The customer leg of an AI outbound call: its DID is the number the call
	// went out from, and the ledger reads it as such.
	ai, _ := Normalize(event(trunkLegHeaders("CHANNEL_HANGUP_COMPLETE", map[string]string{
		"variable_aicc_did": "95001", "variable_aicc_placed_by": PlacedByDialAI,
	})))
	if ai.Bot.DID != "95001" {
		t.Errorf("AI outbound customer leg: Bot.DID = %q, want 95001", ai.Bot.DID)
	}
	// The leg dialled at the bot: destination is the DID.
	bot, _ := Normalize(event(trunkLegHeaders("CHANNEL_HANGUP_COMPLETE", map[string]string{
		"Caller-Destination-Number": "95001", "variable_aicc_did": "95001",
	})))
	if bot.Bot.DID != "95001" {
		t.Errorf("bot leg: Bot.DID = %q, want 95001", bot.Bot.DID)
	}
	// The caller's inbound leg.
	in, _ := Normalize(event(trunkLegHeaders("CHANNEL_HANGUP_COMPLETE", map[string]string{
		"Call-Direction": "inbound", "variable_aicc_did": "95001",
	})))
	if in.Bot.DID != "95001" {
		t.Errorf("inbound leg: Bot.DID = %q, want 95001", in.Bot.DID)
	}
}
