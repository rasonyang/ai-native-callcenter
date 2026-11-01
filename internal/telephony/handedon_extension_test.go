// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// stampedAt adds the switch's own clock to an event's headers, sec seconds into
// the call, so the journey is ordered by when things happened rather than by
// when the test got round to them.
func stampedAt(sec int, vars map[string]string) map[string]string {
	return merged(vars, map[string]string{
		"Event-Date-Timestamp": strconv.FormatInt(at(sec).UnixMicro(), 10),
	})
}

// A call an agent handed to one of our phones is an agent's call at that
// phone, whoever is signed in there (#284).
//
// The live call of 2026-10-22 (01a1618d): wei (1008) click-dialled
// 18688886666, talked, and transferred the customer to 1020, whose agent test1
// was signed out with the phone still registered. The leg the switch raised at
// user/1020 carries the directory's dialed_user and none of our own stamps, so
// it had no extension, and the CDR drew it as a carrier leg:
// [AGENT 1008, TRUNK 18688886666, TRUNK 1020 "did not answer"].
//
// Driven as the switch sends it, through the coordinator and into the
// assembler: the agent's leg our originate raises at user/1008, the trunk leg
// the dialplan raises from it, the transfer (uuid_transfer of the customer's
// leg, which releases wei's), and the onward leg.
func TestALegHandedToOurPhoneIsAnAgentsLegWhoeverIsSignedIn(t *testing.T) {
	t.Parallel()
	weiID, test1ID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	const (
		weiExt   = "1008"
		test1Ext = "1020"
		customer = "18688886666"
		onward   = "13900139000"
	)

	type onwardKind int
	const (
		toExtension onwardKind = iota // the aicc_internal rule bridges user/<ext>
		toCarrier                     // the deployment's trunk rule bridges the gateway
	)
	for _, tc := range []struct {
		name     string
		kind     onwardKind
		signedIn bool // test1 is signed in at 1020
		answered bool

		wantExtension string
		wantAgent     *uuid.UUID
		wantJourney   []string
		wantAgents    []uuid.UUID
	}{
		{
			// The live call.
			name: "to an extension nobody is signed in at, which rings out",
			kind: toExtension, wantExtension: test1Ext,
			wantJourney: []string{"AGENT:1008:20", "TRUNK:" + customer + ":50", "AGENT:1020:did not answer"},
			wantAgents:  []uuid.UUID{weiID},
		},
		{
			// Somebody picks up the phone at 1020 without signing in: still
			// a conversation at one of our phones, not a carrier's.
			name: "to an extension nobody is signed in at, which is answered",
			kind: toExtension, answered: true, wantExtension: test1Ext,
			wantJourney: []string{"AGENT:1008:20", "TRUNK:" + customer + ":43", "AGENT:1020:22"},
			wantAgents:  []uuid.UUID{weiID},
		},
		{
			name: "to an extension its agent is signed in at",
			kind: toExtension, signedIn: true, answered: true,
			wantExtension: test1Ext, wantAgent: &test1ID,
			wantJourney: []string{"AGENT:1008:20", "TRUNK:" + customer + ":43", "AGENT:1020:22"},
			wantAgents:  []uuid.UUID{weiID, test1ID},
		},
		{
			// An outside number goes through the carrier whatever the leg
			// presents as its caller id — here the transferring agent's own
			// extension, signed in.
			name: "to an outside number",
			kind: toCarrier, answered: true,
			wantJourney: []string{"AGENT:1008:20", "TRUNK:" + customer + ":43", "TRUNK:" + onward + ":22"},
			wantAgents:  []uuid.UUID{weiID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			directory := signedInDirectory{weiExt: weiID}
			if tc.signedIn {
				directory[test1Ext] = test1ID
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
			adapter, cmd := newTestAdapter()
			c := NewCoordinator(registry, adapter, directory, nullPublisher{})
			ctx := t.Context()

			minted := uuid.Must(uuid.NewV7())
			const weiChan, customerChan, onwardChan = "wei-chan", "customer-chan", "onward-chan"
			// The leg click-to-dial originates at user/1008: stamped by our
			// originate, and at the registration token the directory resolved.
			weiVars := map[string]string{
				"variable_aicc_call_id":     minted.String(),
				"variable_aicc_call_type":   string(events.CallTypeOutbound),
				"variable_dialed_user":      weiExt,
				"variable_aicc_extension":   weiExt,
				"Caller-Caller-ID-Number":   customer,
				"Caller-Destination-Number": "6p2g7hjk",
				"Caller-Context":            "aicc",
			}
			// The trunk leg: export_vars carries the identity, nothing else.
			customerVars := map[string]string{
				"variable_aicc_call_id":     minted.String(),
				"Caller-Caller-ID-Number":   "95001",
				"Caller-Destination-Number": customer,
				"Caller-Context":            "aicc",
			}
			var onwardVars map[string]string
			switch tc.kind {
			case toExtension:
				// aicc_internal exports the pointer and the type; the user
				// endpoint adds dialed_user, and the destination is the token
				// the phone at 1020 registered under.
				onwardVars = map[string]string{
					"variable_dialed_user":         test1Ext,
					"variable_aicc_parent_channel": customerChan,
					"variable_aicc_call_type":      string(events.CallTypeInternal),
					"Caller-Caller-ID-Number":      customer,
					"Caller-Destination-Number":    "g7bih4lv",
					"Caller-Context":               "aicc",
				}
			case toCarrier:
				onwardVars = map[string]string{
					"variable_aicc_call_id":     minted.String(),
					"Caller-ANI":                weiExt,
					"Caller-Caller-ID-Number":   weiExt,
					"Caller-Destination-Number": onward,
					"Caller-Context":            "aicc",
				}
			}

			c.Handle(ctx, raw("CHANNEL_CREATE", weiChan, "outbound", stampedAt(0, weiVars)))
			c.Handle(ctx, raw("CHANNEL_ANSWER", weiChan, "outbound", stampedAt(1, weiVars)))
			c.Handle(ctx, raw("CHANNEL_CREATE", customerChan, "outbound", stampedAt(1, customerVars)))
			c.Handle(ctx, raw("CHANNEL_ANSWER", customerChan, "outbound", stampedAt(6, customerVars)))
			c.Handle(ctx, raw("CHANNEL_BRIDGE", customerChan, "outbound",
				stampedAt(6, merged(customerVars, map[string]string{"Other-Leg-Unique-ID": weiChan}))))

			// The transfer: the customer's leg leaves the bridge and wei's,
			// left with nobody, is released.
			c.Handle(ctx, raw("CHANNEL_UNBRIDGE", customerChan, "outbound",
				stampedAt(26, merged(customerVars, map[string]string{"Other-Leg-Unique-ID": weiChan}))))
			c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", weiChan, "outbound", stampedAt(26, weiVars)))
			c.Handle(ctx, raw("CHANNEL_CREATE", onwardChan, "outbound", stampedAt(26, onwardVars)))

			waitFor(t, func() bool {
				for _, s := range c.AllCalls() {
					for _, p := range s.Parties {
						if p.ChannelID == onwardChan {
							return true
						}
					}
				}
				return false
			})
			var leg PartySnapshot
			for _, s := range c.AllCalls() {
				if s.CallID != minted {
					t.Fatalf("the onward leg is on call %s, want the placer's %s", s.CallID, minted)
				}
				for _, p := range s.Parties {
					if p.ChannelID == onwardChan {
						leg = p
					}
				}
			}
			if leg.ExtensionNumber != tc.wantExtension {
				t.Errorf("onward leg extension = %q, want %q", leg.ExtensionNumber, tc.wantExtension)
			}
			switch {
			case tc.wantAgent == nil && leg.AgentID != nil:
				t.Errorf("onward leg agent = %v, want none", *leg.AgentID)
			case tc.wantAgent != nil && (leg.AgentID == nil || *leg.AgentID != *tc.wantAgent):
				t.Errorf("onward leg agent = %v, want %v", leg.AgentID, *tc.wantAgent)
			}

			// With wei gone, the call's only leg at one of our phones is
			// 1020's, so a system ending the call ends it there. A carrier
			// leg is at no phone, and there is nothing to end it at.
			endErr := c.EndCall(ctx, minted)
			switch {
			case tc.kind == toExtension && endErr != nil:
				t.Errorf("EndCall() error = %v, want the leg at 1020 hung up", endErr)
			case tc.kind == toExtension && !strings.HasPrefix(cmd.last(), "uuid_kill "+onwardChan+" "):
				t.Errorf("EndCall() sent %q, want a kill of the leg at 1020", cmd.last())
			case tc.kind == toCarrier && endErr == nil:
				t.Errorf("EndCall() = nil on a call with no leg at one of our phones; sent %q", cmd.last())
			}

			if tc.answered {
				c.Handle(ctx, raw("CHANNEL_ANSWER", onwardChan, "outbound", stampedAt(27, onwardVars)))
				c.Handle(ctx, raw("CHANNEL_BRIDGE", onwardChan, "outbound",
					stampedAt(27, merged(onwardVars, map[string]string{"Other-Leg-Unique-ID": customerChan}))))
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", customerChan, "outbound",
					stampedAt(49, merged(customerVars, map[string]string{"Hangup-Cause": "NORMAL_UNSPECIFIED"}))))
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", onwardChan, "outbound", stampedAt(49, onwardVars)))
			} else {
				// 1020 rings out; the bridge fails and the customer's leg,
				// with nothing left to run, is hung up behind it.
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", onwardChan, "outbound",
					stampedAt(56, merged(onwardVars, map[string]string{"Hangup-Cause": "NO_ANSWER"}))))
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", customerChan, "outbound", stampedAt(56, customerVars)))
			}

			waitFor(t, func() bool {
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				return len(ledger.cdrs) > 0
			})
			ledger.mu.Lock()
			rows := slices.Clone(ledger.cdrs)
			ledger.mu.Unlock()
			if len(rows) != 1 {
				t.Fatalf("wrote %d rows for one call, want 1", len(rows))
			}
			row := rows[0]
			if got := journey(row.Legs); !slices.Equal(got, tc.wantJourney) {
				t.Errorf("journey = %v, want %v", got, tc.wantJourney)
			}
			// Which of them is listed first is the primary agent's business,
			// not this test's.
			byID := func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }
			gotAgents, wantAgents := slices.Clone(row.AgentIDs), slices.Clone(tc.wantAgents)
			slices.SortFunc(gotAgents, byID)
			slices.SortFunc(wantAgents, byID)
			if !slices.Equal(gotAgents, wantAgents) {
				t.Errorf("agent_ids = %v, want %v", row.AgentIDs, tc.wantAgents)
			}
			if events.CallType(row.CallType) != events.CallTypeOutbound {
				t.Errorf("call type = %q, want OUTBOUND: a type is stamped at creation", row.CallType)
			}
		})
	}
}

// A carrier caller whose number is an extension is at no phone of ours,
// signed in there or not (#105). Only what the leg says about itself places it
// at a phone, and a carrier's leg says nothing: its caller id is the carrier's
// to choose. It must not pick up the extension the leg it reached is at, either
// — the switch writes dialed_user onto the caller's own channel as it bridges.
func TestACarrierCallerWhoseNumberLooksLikeAnExtensionIsAtNoPhone(t *testing.T) {
	t.Parallel()
	weiID, benID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	const benExt = "1001"
	for _, tc := range []struct {
		name, ani string
		agent     *uuid.UUID // signed in at the caller's number
	}{
		{name: "an extension somebody is signed in at", ani: "1008", agent: &weiID},
		{name: "an extension nobody is signed in at", ani: "1020"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			directory := signedInDirectory{benExt: benID}
			if tc.agent != nil {
				directory[tc.ani] = *tc.agent
			}
			registry := NewRegistry(nullPublisher{})
			t.Cleanup(registry.Shutdown)
			ledger := &memoryLedger{}
			assembler := newAssembler(ledger, staticQueues{})
			registry.OnCallFinished = assembler.CallFinished
			c := NewCoordinator(registry, nil, directory, nullPublisher{})
			ctx := t.Context()

			minted := uuid.Must(uuid.NewV7())
			const callerChan, benChan = "caller-chan", "ben-chan"
			callerVars := map[string]string{
				"variable_aicc_call_id":     minted.String(),
				"Caller-Context":            "public",
				"Caller-ANI":                tc.ani,
				"Caller-Caller-ID-Number":   tc.ani,
				"Caller-Destination-Number": "95001",
			}
			benVars := map[string]string{
				"variable_aicc_call_id":           minted.String(),
				"variable_dialed_user":            benExt,
				"variable_cc_member_session_uuid": callerChan,
				"Caller-Caller-ID-Number":         tc.ani,
				"Caller-Destination-Number":       "hbp99nv8",
				"Caller-Context":                  "aicc",
			}
			c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", stampedAt(0, callerVars)))
			c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", stampedAt(0, callerVars)))
			c.Handle(ctx, raw("CHANNEL_CREATE", benChan, "outbound", stampedAt(5, benVars)))
			c.Handle(ctx, raw("CHANNEL_ANSWER", benChan, "outbound", stampedAt(7, benVars)))
			c.Handle(ctx, raw("CHANNEL_BRIDGE", benChan, "outbound",
				stampedAt(7, merged(benVars, map[string]string{"Other-Leg-Unique-ID": callerChan}))))

			waitFor(t, func() bool {
				for _, s := range c.AllCalls() {
					if len(s.Parties) == 2 {
						return true
					}
				}
				return false
			})
			for _, s := range c.AllCalls() {
				for _, p := range s.Parties {
					switch p.ChannelID {
					case callerChan:
						if p.ExtensionNumber != "" {
							t.Errorf("the carrier caller is at extension %q, want none", p.ExtensionNumber)
						}
					case benChan:
						if p.ExtensionNumber != benExt {
							t.Errorf("ben's leg is at extension %q, want %q", p.ExtensionNumber, benExt)
						}
					}
				}
			}

			// The caller's channel now names ben as its dialed user, as the
			// switch leaves it after the bridge; its hangup changes nothing.
			c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", callerChan, "inbound",
				stampedAt(30, merged(callerVars, map[string]string{"variable_dialed_user": benExt}))))
			c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", benChan, "outbound", stampedAt(30, benVars)))

			waitFor(t, func() bool {
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				return len(ledger.cdrs) > 0
			})
			ledger.mu.Lock()
			row := ledger.cdrs[0]
			ledger.mu.Unlock()
			if events.CallType(row.CallType) != events.CallTypeInbound {
				t.Errorf("call type = %q, want INBOUND", row.CallType)
			}
			if want := []string{"AGENT:" + benExt + ":23"}; !slices.Equal(journey(row.Legs), want) {
				t.Errorf("journey = %v, want %v: the caller is not a hop at any phone", journey(row.Legs), want)
			}
			if !slices.Equal(row.AgentIDs, []uuid.UUID{benID}) {
				t.Errorf("agent_ids = %v, want only ben", row.AgentIDs)
			}
		})
	}
}
