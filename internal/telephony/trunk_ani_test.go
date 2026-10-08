// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// signedInDirectory is a presence directory with named people at named
// extensions, so a test can sign somebody in at any number it likes.
type signedInDirectory map[string]uuid.UUID

func (d signedInDirectory) AgentAtExtension(ext string) (uuid.UUID, bool) {
	id, ok := d[ext]
	return id, ok
}
func (signedInDirectory) AgentByCallcenterName(string) (uuid.UUID, bool)           { return uuid.Nil, false }
func (signedInDirectory) SetOnCall(context.Context, uuid.UUID, bool, uuid.UUID)    {}
func (signedInDirectory) BeginAfterCallWork(context.Context, uuid.UUID, uuid.UUID) {}
func (signedInDirectory) BenchForNoAnswer(context.Context, uuid.UUID)              {}

// A call from the carrier whose caller id happens to be an extension number
// is a customer's call, whoever is signed in at that number (#105). Presence
// matches the leg to that agent for the screen pop, and #108 let that match
// decide that "an agent's phone placed the call", so the human path and the
// bot ledger both wrote the one row and timing picked the winner.
//
// Every case here is driven as the switch would send it, through the
// coordinator and into the assembler, and judged on the two questions that
// matter: which side writes the row (exactly one), and what the row says when
// the assembler is the one.
func TestWhoWritesACallIsDecidedByTheSwitchNotByTheCallersNumber(t *testing.T) {
	t.Parallel()
	weiID, benID := uuid.New(), uuid.New()
	const (
		weiExt = "1008" // wei, signed in
		benExt = "1001" // ben, signed in: the queue delivers handed-over calls to him
	)

	type kind int
	const (
		trunkKept     kind = iota // a carrier call the bot keeps to the end
		trunkHandover             // a carrier call the bot transfers to ben
		clickToDial               // an agent's phone is rung, then dials the bot number
		keypadDial                // an agent dials the bot number on the keypad
	)
	for _, tc := range []struct {
		name string
		kind kind
		// number is the caller id of a trunk call, or the extension of the
		// agent's phone for the agent-placed kinds.
		number string
		// who is signed in there, if anybody.
		agent *uuid.UUID

		wantAssemblerWrites bool
		wantAgents          []uuid.UUID
		wantPrimary         *uuid.UUID
		wantType            events.CallType
		wantBilled          bool
	}{
		// A carrier call stays the bot's when it is kept, whatever the number.
		{name: "trunk, caller id is a signed-in agent's extension, bot keeps it",
			kind: trunkKept, number: weiExt, agent: &weiID, wantType: events.CallTypeInbound},
		{name: "trunk, caller id is a signed-out extension, bot keeps it",
			kind: trunkKept, number: "1004", wantType: events.CallTypeInbound},
		{name: "trunk, caller id is an extension bound to nobody, bot keeps it",
			kind: trunkKept, number: "1099", wantType: events.CallTypeInbound},
		{name: "trunk, caller id is a long mobile number, bot keeps it",
			kind: trunkKept, number: "8613800138000", wantType: events.CallTypeInbound},

		// Handed over, the row is the human path's, and the person who took
		// it is ben whatever the caller's number resembled.
		{name: "trunk, caller id is a signed-in agent's extension, handed over",
			kind: trunkHandover, number: weiExt, agent: &weiID, wantAssemblerWrites: true,
			wantAgents: []uuid.UUID{benID}, wantPrimary: &benID, wantType: events.CallTypeInbound, wantBilled: true},
		{name: "trunk, caller id is a signed-out extension, handed over",
			kind: trunkHandover, number: "1004", wantAssemblerWrites: true,
			wantAgents: []uuid.UUID{benID}, wantPrimary: &benID, wantType: events.CallTypeInbound, wantBilled: true},
		{name: "trunk, caller id is an extension bound to nobody, handed over",
			kind: trunkHandover, number: "1099", wantAssemblerWrites: true,
			wantAgents: []uuid.UUID{benID}, wantPrimary: &benID, wantType: events.CallTypeInbound, wantBilled: true},
		{name: "trunk, caller id is a long mobile number, handed over",
			kind: trunkHandover, number: "8613800138000", wantAssemblerWrites: true,
			wantAgents: []uuid.UUID{benID}, wantPrimary: &benID, wantType: events.CallTypeInbound, wantBilled: true},

		// The calls an agent's phone placed stay the human path's (#108).
		{name: "signed-in agent clicks to dial the bot number",
			kind: clickToDial, number: weiExt, agent: &weiID, wantAssemblerWrites: true,
			wantAgents: []uuid.UUID{weiID}, wantPrimary: &weiID, wantType: events.CallTypeOutbound},
		{name: "signed-in agent dials the bot number on the keypad",
			kind: keypadDial, number: weiExt, agent: &weiID, wantAssemblerWrites: true,
			wantAgents: []uuid.UUID{weiID}, wantPrimary: &weiID, wantType: events.CallTypeInternal},
		{name: "signed-out extension dials the bot number on the keypad",
			kind: keypadDial, number: "1004", wantAssemblerWrites: true, wantType: events.CallTypeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			directory := signedInDirectory{benExt: benID}
			if tc.agent != nil {
				directory[tc.number] = *tc.agent
			}

			registry := NewRegistry(nullPublisher{})
			t.Cleanup(registry.Shutdown)
			ledger := &memoryLedger{}
			assembler := newAssembler(ledger, staticQueues{})
			finished := &finishedCollector{}
			registry.OnCallFinished = func(s Snapshot) {
				finished.add(s)
				assembler.CallFinished(s)
			}
			c := NewCoordinator(registry, nil, directory, nullPublisher{})
			ctx := t.Context()

			minted := uuid.New().String()
			exported := map[string]string{
				"variable_aicc_call_id":  minted,
				"variable_aicc_did":      "95001",
				"variable_aicc_language": "en",
			}
			const callerChan, botChan, benChan = "caller-chan", "bot-chan", "ben-chan"
			botLeg := merged(exported, map[string]string{"Caller-Destination-Number": "95001"})

			var callerVars, callerEnd map[string]string
			switch tc.kind {
			case trunkKept, trunkHandover:
				// The carrier's leg: the public context, and a caller id that
				// is whatever the carrier says it is.
				callerVars = merged(exported, map[string]string{
					"Caller-Context": "public", "Caller-ANI": tc.number,
					"Caller-Caller-ID-Number": tc.number, "Caller-Destination-Number": "95001"})
			case clickToDial:
				// The leg our own originate rings first: outbound, stamped
				// with the extension and the type.
				callerVars = merged(exported, map[string]string{
					"Caller-Context": "aicc", "Caller-Caller-ID-Number": "95001",
					"Caller-Destination-Number":  tc.number,
					"variable_dialed_user":       tc.number,
					"variable_aicc_extension":    tc.number,
					"variable_aicc_call_type":    "OUTBOUND",
					"variable_aicc_parent_agent": tc.number})
			case keypadDial:
				// The phone's own INVITE: the directory stamps the extension
				// on an authenticated managed user, signed in or not.
				callerVars = merged(exported, map[string]string{
					"Caller-Context": "aicc", "Caller-ANI": tc.number,
					"Caller-Caller-ID-Number": tc.number, "Caller-Destination-Number": "95001",
					"variable_aicc_extension": tc.number})
			}
			direction := "inbound"
			if tc.kind == clickToDial {
				direction = "outbound"
			}

			c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, direction, callerVars))
			c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, direction, callerVars))
			c.Handle(ctx, raw("CHANNEL_CREATE", botChan, "outbound", botLeg))
			c.Handle(ctx, raw("CHANNEL_ANSWER", botChan, "outbound", botLeg))
			c.Handle(ctx, raw("CHANNEL_BRIDGE", botChan, "outbound",
				merged(botLeg, map[string]string{"Other-Leg-Unique-ID": callerChan})))

			// Long enough for the billed seconds to be a whole one.
			time.Sleep(1100 * time.Millisecond)

			switch tc.kind {
			case trunkHandover:
				// The bot stamps its tally and the queue delivers ben. The
				// caller's leg is the one that carries the stamp out.
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", botChan, "outbound", botLeg))
				c.Handle(ctx, raw("CHANNEL_CREATE", benChan, "outbound", map[string]string{
					"variable_dialed_user":            benExt,
					"variable_cc_member_session_uuid": callerChan,
					"variable_aicc_call_id":           minted,
					"Caller-Context":                  "aicc"}))
				c.Handle(ctx, raw("CHANNEL_ANSWER", benChan, "outbound", map[string]string{
					"variable_dialed_user": benExt, "variable_aicc_call_id": minted}))
				c.Handle(ctx, raw("CHANNEL_BRIDGE", benChan, "outbound", map[string]string{
					"variable_dialed_user": benExt, "variable_aicc_call_id": minted,
					"Other-Leg-Unique-ID": callerChan}))
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", benChan, "outbound", map[string]string{
					"variable_dialed_user": benExt, "variable_aicc_call_id": minted}))
				callerEnd = merged(callerVars, map[string]string{"variable_aicc_bot_sec": "1"})
			case clickToDial, keypadDial:
				// The bot ends it and has stamped its tally on the agent's
				// channel on the way out (aicc_bot_sec).
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", botChan, "outbound", botLeg))
				callerEnd = merged(callerVars, map[string]string{"variable_aicc_bot_sec": "1"})
			default:
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", botChan, "outbound", botLeg))
				callerEnd = callerVars
			}
			c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", callerChan, direction, callerEnd))

			waitFor(t, func() bool { return len(finished.all()) == 1 })
			snap := finished.all()[0]
			time.Sleep(200 * time.Millisecond) // a wrong extra write needs time to land

			// The bot ledger (internal/aicall/ledger.go) declines a call that
			// was handed over, and one the dialplan typed as an agent's own
			// (OUTBOUND or INTERNAL, not placed to a customer). It writes the
			// rest. Both read the same two facts off this snapshot.
			botLedgerWrites := hasBotLeg(snap) && !snap.Bot.HandedOver() &&
				snap.CallType == events.CallTypeInbound
			ledger.mu.Lock()
			rows := slices.Clone(ledger.cdrs)
			ledger.mu.Unlock()
			assemblerWrites := len(rows) > 0

			if assemblerWrites != tc.wantAssemblerWrites {
				t.Errorf("the human path wrote %d rows, want writes=%v", len(rows), tc.wantAssemblerWrites)
			}
			if assemblerWrites == botLedgerWrites {
				t.Fatalf("assembler writes=%v and the bot ledger writes=%v: a call needs exactly one writer",
					assemblerWrites, botLedgerWrites)
			}
			if len(rows) > 1 {
				t.Fatalf("the human path wrote %d rows for one call", len(rows))
			}
			if !assemblerWrites {
				return
			}

			row := rows[0]
			if events.CallType(row.CallType) != tc.wantType {
				t.Errorf("call type = %q, want %q", row.CallType, tc.wantType)
			}
			if !slices.Equal(row.AgentIDs, tc.wantAgents) {
				t.Errorf("agent_ids = %v, want %v", row.AgentIDs, tc.wantAgents)
			}
			switch {
			case tc.wantPrimary == nil && row.PrimaryAgentID != nil:
				t.Errorf("primary agent = %v, want none", *row.PrimaryAgentID)
			case tc.wantPrimary != nil && (row.PrimaryAgentID == nil || *row.PrimaryAgentID != *tc.wantPrimary):
				t.Errorf("primary agent = %v, want %v", row.PrimaryAgentID, *tc.wantPrimary)
			}
			if row.IsContained {
				t.Error("is_contained = true on a call a person took or an agent placed")
			}
			if tc.wantBilled && row.BillSec < 1 {
				t.Errorf("bill_sec = %d, want the answered carrier leg billed", row.BillSec)
			}
			if !tc.wantBilled && row.BillSec != 0 {
				t.Errorf("bill_sec = %d, want 0: no carrier leg was answered", row.BillSec)
			}
		})
	}
}
