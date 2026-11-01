// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// After-call work is started by the switch path and nothing else. Before this
// existed, `StartWrapUp` had no caller at all: the state the whole cockpit is
// built around could be reached in a unit test and never on a real call.

// presenceCalls records what call control told the agent service, in order,
// because the order is load-bearing: an agent still marked on a call derives
// ON_CALL, which outranks WRAP_UP.
//
// It knows two signed-in agents, so a call between colleagues has an agent at
// both ends: testAgentID at agentExtension and otherAgentID at otherExtension.
type presenceCalls struct {
	mu      sync.Mutex
	steps   []string
	calls   []uuid.UUID
	parties []uuid.UUID
	benched []uuid.UUID
}

func (p *presenceCalls) AgentAtExtension(ext string) (uuid.UUID, bool) {
	switch ext {
	case agentExtension:
		return testAgentID, true
	case otherExtension:
		return otherAgentID, true
	}
	return uuid.Nil, false
}

func (p *presenceCalls) AgentByCallcenterName(name string) (uuid.UUID, bool) {
	if name == agentCallcenterName {
		return testAgentID, true
	}
	return uuid.Nil, false
}

// The switch's own name for the agent behind agentExtension.
const agentCallcenterName = "agent-1001"

func (p *presenceCalls) SetOnCall(_ context.Context, agentID uuid.UUID, onCall bool, _ uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if onCall {
		p.steps = append(p.steps, "onCall:"+agentID.String())
		return
	}
	p.steps = append(p.steps, "offCall:"+agentID.String())
}

func (p *presenceCalls) BenchForNoAnswer(_ context.Context, agentID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.benched = append(p.benched, agentID)
}

func (p *presenceCalls) BeginAfterCallWork(_ context.Context, agentID, callID, partyID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, "wrapUp:"+agentID.String())
	p.calls = append(p.calls, callID)
	p.parties = append(p.parties, partyID)
}

func (p *presenceCalls) taken() ([]string, []uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.steps...), append([]uuid.UUID(nil), p.calls...)
}

// wrappedUp is who was put into after-call work, in order.
func (p *presenceCalls) wrappedUp() []uuid.UUID {
	steps, _ := p.taken()
	var out []uuid.UUID
	for _, step := range steps {
		if id, ok := strings.CutPrefix(step, "wrapUp:"); ok {
			out = append(out, uuid.MustParse(id))
		}
	}
	return out
}

func (p *presenceCalls) tookOffCall(agentID uuid.UUID) bool {
	steps, _ := p.taken()
	return indexOf(steps, "offCall:"+agentID.String()) >= 0
}

// answerAndEndAnAgentLeg drives one queue delivery to a conversation and then
// ends the agent's own leg, which is where their part of the call finishes —
// on a transfer the caller talks on without them.
func answerAndEndAnAgentLeg(t *testing.T, c *Coordinator, answered bool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	minted := uuid.Must(uuid.NewV7())
	vars := map[string]string{"variable_aicc_call_id": minted.String()}
	callerChan, agentChan := "caller-chan", "agent-chan"

	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	// A phone that was picked up answers and is then bridged; a phone that was
	// not does neither. The switch never bridges a leg nobody answered, and it
	// is the bridge that puts the agent in the conversation — so the two
	// events go together or not at all.
	if answered {
		c.Handle(ctx, raw("CHANNEL_ANSWER", agentChan, "outbound", vars))
		c.Handle(ctx, raw("CHANNEL_BRIDGE", agentChan, "outbound",
			merged(vars, map[string]string{"Other-Leg-Unique-ID": callerChan})))
	}
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", agentChan, "outbound", vars))
	return minted
}

func TestAnAnsweredAgentLegEndingStartsAfterCallWork(t *testing.T) {
	t.Parallel()
	agents := &presenceCalls{}
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, agents, nullPublisher{})

	minted := answerAndEndAnAgentLeg(t, c, true)

	waitFor(t, func() bool {
		_, calls := agents.taken()
		return len(calls) == 1
	})
	steps, calls := agents.taken()
	if calls[0] != minted {
		t.Errorf("wrap-up started for call %s, want the call the agent was on (%s)", calls[0], minted)
	}
	// The wrap-up is told which party was the agent's leg: the id the
	// transcript tap stamps on every line of that leg, whoever spoke.
	var agentParty uuid.UUID
	if err := registry.Do(minted, func(call *Call) {
		for _, p := range call.Parties {
			if p.AgentID != nil && *p.AgentID == testAgentID {
				agentParty = p.PartyID
			}
		}
	}); err != nil {
		t.Logf("call already retired: %v", err)
	}
	agents.mu.Lock()
	gotParty := agents.parties[0]
	agents.mu.Unlock()
	if gotParty == uuid.Nil {
		t.Error("wrap-up started with no party for the agent's leg")
	}
	if agentParty != uuid.Nil && gotParty != agentParty {
		t.Errorf("wrap-up party = %s, want the agent's party %s", gotParty, agentParty)
	}
	// Off the call first. The other order shows the agent as still talking to
	// somebody who has hung up, because being on a call outranks wrap-up when
	// availability is derived.
	offCall, wrapUp := indexOf(steps, "offCall:"+testAgentID.String()), indexOf(steps, "wrapUp:"+testAgentID.String())
	if offCall < 0 || wrapUp < 0 {
		t.Fatalf("steps = %v, want the agent taken off the call and put into wrap-up", steps)
	}
	if offCall > wrapUp {
		t.Errorf("steps = %v, want offCall before wrap-up: an agent still marked on a "+
			"call derives ON_CALL, which outranks WRAP_UP", steps)
	}
}

// A leg that rang and was never answered left the agent nothing to write up.
func TestAnUnansweredAgentLegStartsNoAfterCallWork(t *testing.T) {
	t.Parallel()
	agents := &presenceCalls{}
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, agents, nullPublisher{})

	answerAndEndAnAgentLeg(t, c, false)

	steps, calls := agents.taken()
	if len(calls) != 0 {
		t.Errorf("wrap-up started for %v after a call nobody answered", calls)
	}
	if indexOf(steps, "offCall:"+testAgentID.String()) < 0 {
		t.Errorf("steps = %v, want the agent taken off the call regardless", steps)
	}
}

// The caller's own leg ending is not an agent's part of a call ending.
func TestTheCallersLegEndingStartsNobodysAfterCallWork(t *testing.T) {
	t.Parallel()
	agents := &presenceCalls{}
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, agents, nullPublisher{})

	ctx := t.Context()
	vars := map[string]string{"variable_aicc_call_id": uuid.Must(uuid.NewV7()).String()}
	c.Handle(ctx, raw("CHANNEL_CREATE", "caller-chan", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", "caller-chan", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", "caller-chan", "inbound", vars))

	if _, calls := agents.taken(); len(calls) != 0 {
		t.Errorf("wrap-up started for %v on a call no agent was ever on", calls)
	}
}

func indexOf(steps []string, want string) int {
	for i, step := range steps {
		if step == want {
			return i
		}
	}
	return -1
}

// The event shapes below are the ones the switch sends for each kind of call,
// trimmed to what the coordinator reads. Each scenario ends every leg it opened
// and reports the call and the agents whose legs were on it.

// A customer's number, as the trunk and click-to-dial carry it.
const customerNumber = "13800138000"

// clickToDial places a call the way outbound.Service does: the agent's own
// phone is rung first and auto-answers, then the dialplan raises the far leg.
// callType is the stamp click-to-dial puts on the agent leg, decided by the
// callee. A callee at otherExtension is a colleague, raised by the aicc_internal
// dialplan rule; any other callee is a customer behind the trunk.
func clickToDial(t *testing.T, c *Coordinator, callType events.CallType, callee string, answered bool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	minted := uuid.Must(uuid.NewV7())
	agentChan, farChan := "agent-chan", "far-chan"
	agentVars := map[string]string{
		"variable_aicc_call_id":   minted.String(),
		"variable_aicc_call_type": string(callType),
		"variable_dialed_user":    agentExtension,
		"variable_aicc_extension": agentExtension,
		// Click-to-dial shows the agent who they are ringing, and the leg's
		// destination is the registration token the directory resolved.
		"Caller-Caller-ID-Number":   callee,
		"Caller-Destination-Number": "6p2g7hjk",
		"Caller-Context":            "aicc",
	}
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound", agentVars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", agentChan, "outbound", agentVars))

	// export_vars carries the identity and the type onto the far leg; the
	// internal rule adds the pointer back to the leg it was raised for.
	farVars := map[string]string{
		"variable_aicc_call_id":     minted.String(),
		"variable_aicc_call_type":   string(callType),
		"Caller-Caller-ID-Number":   agentExtension,
		"Caller-Destination-Number": callee,
		"Caller-Context":            "aicc",
	}
	if callee == otherExtension {
		farVars["variable_dialed_user"] = otherExtension
		farVars["variable_aicc_parent_channel"] = agentChan
	}
	c.Handle(ctx, raw("CHANNEL_CREATE", farChan, "outbound", farVars))
	if answered {
		c.Handle(ctx, raw("CHANNEL_ANSWER", farChan, "outbound", farVars))
		c.Handle(ctx, raw("CHANNEL_BRIDGE", farChan, "outbound",
			merged(farVars, map[string]string{"Other-Leg-Unique-ID": agentChan})))
		// The far end hangs up first; hangup_after_bridge takes the agent's
		// leg down after it.
		c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", farChan, "outbound", farVars))
		c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", agentChan, "outbound", agentVars))
		return minted
	}
	// Nobody picks up, and the agent gives up: their cancel ends the far leg.
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", agentChan, "outbound", agentVars))
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", farChan, "outbound",
		merged(farVars, map[string]string{"Hangup-Cause": "ORIGINATOR_CANCEL"})))
	return minted
}

// directDial is a colleague dialling agentExtension from the phone at
// otherExtension. The phone's own leg arrives before the dialplan runs, so it
// carries nothing of ours; the aicc_internal rule then exports the pointer and
// the INTERNAL stamp onto the leg it raises.
func directDial(t *testing.T, c *Coordinator) (callerChan, calleeChan string) {
	t.Helper()
	ctx := t.Context()
	callerChan, calleeChan = "caller-chan", "callee-chan"
	callerVars := map[string]string{
		"Caller-Caller-ID-Number":   otherExtension,
		"Caller-Destination-Number": agentExtension,
		"Caller-Context":            "aicc",
	}
	calleeVars := map[string]string{
		"variable_dialed_user":         agentExtension,
		"variable_aicc_parent_channel": callerChan,
		"variable_aicc_call_type":      string(events.CallTypeInternal),
		"Caller-Caller-ID-Number":      otherExtension,
		"Caller-Destination-Number":    agentExtension,
		"Caller-Context":               "aicc",
	}
	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", callerVars))
	c.Handle(ctx, raw("CHANNEL_CREATE", calleeChan, "outbound", calleeVars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", calleeChan, "outbound", calleeVars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", callerVars))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", calleeChan, "outbound",
		merged(calleeVars, map[string]string{"Other-Leg-Unique-ID": callerChan})))
	return callerChan, calleeChan
}

func callTypeOfChannel(t *testing.T, registry *Registry, channelID string) (uuid.UUID, events.CallType) {
	t.Helper()
	callID, ok := registry.CallForChannel(channelID)
	if !ok {
		t.Fatalf("channel %s is on no call", channelID)
	}
	var callType events.CallType
	if err := registry.Do(callID, func(call *Call) { callType = call.CallType }); err != nil {
		t.Fatalf("reading call %s: %v", callID, err)
	}
	return callID, callType
}

// After-call work is owed by an answered agent leg of any call that is not
// INTERNAL (#272). One colleague calling another is not a customer's call:
// opening a wrap-up there cost both of them a disposition and held both out of
// the queue. Every agent whose leg ended is still taken off the call.
func TestAfterCallWorkFollowsTheCallType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// place drives the call to its end and returns it.
		place func(t *testing.T, c *Coordinator) uuid.UUID
		// onCall are the agents who had a leg on it; wrapped are the ones who
		// owe a wrap-up for it.
		onCall  []uuid.UUID
		wrapped []uuid.UUID
	}{
		{
			name: "INBOUND queue call answered",
			place: func(t *testing.T, c *Coordinator) uuid.UUID {
				return answerAndEndAnAgentLeg(t, c, true)
			},
			onCall:  []uuid.UUID{testAgentID},
			wrapped: []uuid.UUID{testAgentID},
		},
		{
			name: "OUTBOUND click-to-dial answered",
			place: func(t *testing.T, c *Coordinator) uuid.UUID {
				return clickToDial(t, c, events.CallTypeOutbound, customerNumber, true)
			},
			onCall:  []uuid.UUID{testAgentID},
			wrapped: []uuid.UUID{testAgentID},
		},
		{
			name: "INTERNAL click-to-dial answered",
			place: func(t *testing.T, c *Coordinator) uuid.UUID {
				return clickToDial(t, c, events.CallTypeInternal, otherExtension, true)
			},
			onCall: []uuid.UUID{testAgentID, otherAgentID},
		},
		{
			// The caller's own leg was answered — click-to-dial's phone picks
			// itself up — so only the type keeps this from a wrap-up.
			name: "INTERNAL click-to-dial nobody answered",
			place: func(t *testing.T, c *Coordinator) uuid.UUID {
				return clickToDial(t, c, events.CallTypeInternal, otherExtension, false)
			},
			onCall: []uuid.UUID{testAgentID, otherAgentID},
		},
		{
			name: "INTERNAL direct dial answered",
			place: func(t *testing.T, c *Coordinator) uuid.UUID {
				callerChan, calleeChan := directDial(t, c)
				callID, ok := c.registry.CallForChannel(callerChan)
				if !ok {
					t.Fatal("the caller's leg is on no call")
				}
				ctx := t.Context()
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", calleeChan, "outbound", nil))
				c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", callerChan, "inbound", nil))
				return callID
			},
			onCall: []uuid.UUID{testAgentID, otherAgentID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agents := &presenceCalls{}
			registry := NewRegistry(nullPublisher{})
			t.Cleanup(registry.Shutdown)
			c := NewCoordinator(registry, nil, agents, nullPublisher{})

			callID := tc.place(t, c)

			if len(tc.wrapped) > 0 {
				waitFor(t, func() bool { return len(agents.wrappedUp()) >= len(tc.wrapped) })
			} else {
				settle(t)
			}
			if got := agents.wrappedUp(); !slices.Equal(got, tc.wrapped) {
				t.Errorf("after-call work started for %v, want %v", got, tc.wrapped)
			}
			_, calls := agents.taken()
			for _, id := range calls {
				if id != callID {
					t.Errorf("after-call work started for call %s, want the call the agent was on (%s)", id, callID)
				}
			}
			for _, agentID := range tc.onCall {
				if !agents.tookOffCall(agentID) {
					t.Errorf("agent %s was not taken off the call", agentID)
				}
			}
		})
	}
}

// CallType is stamped at creation and never changes, so a colleague's call
// that one of them transfers out to a customer is still INTERNAL, and neither
// agent owes a wrap-up for it: the type says what the call was.
//
// The transfer is the phone's own: the call control API refuses to transfer an
// INTERNAL call (ErrNotForCallType). The agent being called hands their caller
// on, their leg ends, and the caller's leg is bridged to the trunk by the
// dialplan, the new leg inheriting what the caller's leg exported.
func TestAnInternalCallTransferredOutOwesNoAfterCallWork(t *testing.T) {
	t.Parallel()
	agents := &presenceCalls{}
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, agents, nullPublisher{})
	ctx := t.Context()

	callerChan, calleeChan := directDial(t, c)
	callID, _ := callTypeOfChannel(t, registry, callerChan)

	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", calleeChan, "outbound",
		map[string]string{"Hangup-Cause": "BLIND_TRANSFER"}))

	trunkChan := "trunk-chan"
	trunkVars := map[string]string{
		"variable_aicc_parent_channel": callerChan,
		"variable_aicc_call_type":      string(events.CallTypeInternal),
		"Caller-Caller-ID-Number":      otherExtension,
		"Caller-Destination-Number":    customerNumber,
		"Caller-Context":               "aicc",
	}
	c.Handle(ctx, raw("CHANNEL_CREATE", trunkChan, "outbound", trunkVars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", trunkChan, "outbound", trunkVars))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", trunkChan, "outbound",
		merged(trunkVars, map[string]string{"Other-Leg-Unique-ID": callerChan})))

	trunkCall, callType := callTypeOfChannel(t, registry, trunkChan)
	if trunkCall != callID {
		t.Fatalf("the customer's leg is on call %s, want the transferred call %s", trunkCall, callID)
	}
	if callType != events.CallTypeInternal {
		t.Fatalf("the transferred call is %s, want INTERNAL: the type is stamped at creation", callType)
	}

	// The customer hangs up, and the caller's leg ends after them.
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", trunkChan, "outbound", nil))
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", callerChan, "inbound", nil))

	settle(t)
	if got := agents.wrappedUp(); len(got) != 0 {
		t.Errorf("after-call work started for %v on an internal call transferred out", got)
	}
	for _, agentID := range []uuid.UUID{testAgentID, otherAgentID} {
		if !agents.tookOffCall(agentID) {
			t.Errorf("agent %s was not taken off the call", agentID)
		}
	}
}

// A call that outlives an app restart is adopted from the switch's listing and
// stamped the way it was stamped live, so an internal call that ends after the
// restart opens no wrap-up either, and a customer's call still does. The
// listing order is the switch's, so the leg it names first must not matter.
func TestAnAdoptedCallOwesAfterCallWorkByItsType(t *testing.T) {
	t.Parallel()
	createdAt := time.Now().Add(-time.Minute).UnixMicro()
	answeredAt := createdAt + 3_000_000
	us := func(v int64) string { return strconv.FormatInt(v, 10) }

	// A colleague at otherExtension dialled agentExtension from their phone.
	// export put the pointer and the stamp on the caller's own leg too.
	internal := func(callerID, calleeID string) map[string]map[string]string {
		return map[string]map[string]string{
			callerID: {
				"Call-Direction": "inbound", "Answer-State": "answered",
				"Caller-Caller-ID-Number": otherExtension, "Caller-Destination-Number": agentExtension,
				"Caller-Context":               "aicc",
				"variable_aicc_parent_channel": callerID,
				"variable_aicc_call_type":      string(events.CallTypeInternal),
				"Other-Leg-Unique-ID":          calleeID,
			},
			calleeID: {
				"Call-Direction": "outbound", "Answer-State": "answered",
				"Caller-Caller-ID-Number": otherExtension, "Caller-Destination-Number": agentExtension,
				"Caller-Context":               "aicc",
				"variable_dialed_user":         agentExtension,
				"variable_aicc_parent_channel": callerID,
				"variable_aicc_call_type":      string(events.CallTypeInternal),
				"Other-Leg-Unique-ID":          callerID,
			},
		}
	}
	// A carrier's caller delivered from a queue to agentExtension.
	inbound := func(callerID, calleeID string) map[string]map[string]string {
		callID := uuid.NewString()
		return map[string]map[string]string{
			callerID: {
				"Call-Direction": "inbound", "Answer-State": "answered",
				"Caller-Caller-ID-Number": customerNumber, "Caller-Destination-Number": "95012",
				"Caller-Context":        "public",
				"variable_aicc_call_id": callID,
				"Other-Leg-Unique-ID":   calleeID,
			},
			calleeID: {
				"Call-Direction": "outbound", "Answer-State": "answered",
				"Caller-Caller-ID-Number": customerNumber, "Caller-Destination-Number": agentExtension,
				"Caller-Context":                  "default",
				"variable_dialed_user":            agentExtension,
				"variable_aicc_call_id":           callID,
				"variable_cc_member_session_uuid": callerID,
				"Other-Leg-Unique-ID":             callerID,
			},
		}
	}

	for _, tc := range []struct {
		name          string
		legs          func(callerID, calleeID string) map[string]map[string]string
		isCalleeFirst bool
		wantType      events.CallType
		onCall        []uuid.UUID
		wrapped       []uuid.UUID
	}{
		{"INTERNAL, caller listed first", internal, false, events.CallTypeInternal,
			[]uuid.UUID{testAgentID, otherAgentID}, nil},
		{"INTERNAL, callee listed first", internal, true, events.CallTypeInternal,
			[]uuid.UUID{testAgentID, otherAgentID}, nil},
		{"INBOUND queue call", inbound, false, events.CallTypeInbound,
			[]uuid.UUID{testAgentID}, []uuid.UUID{testAgentID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			callerID, calleeID := uuid.NewString(), uuid.NewString()
			dumps := tc.legs(callerID, calleeID)
			for id, h := range dumps {
				h["Unique-ID"] = id
				h["Caller-Channel-Created-Time"] = us(createdAt)
				h["Caller-Channel-Answered-Time"] = us(answeredAt)
				h["Caller-Channel-Bridged-Time"] = us(answeredAt)
			}
			order := []string{callerID, calleeID}
			if tc.isCalleeFirst {
				order = []string{calleeID, callerID}
			}
			var rows []string
			for _, id := range order {
				rows = append(rows, `{"uuid":"`+id+`","callstate":"ACTIVE","created_epoch":"`+
					us(createdAt/1_000_000)+`"}`)
			}
			listing := `{"row_count":2,"rows":[` + strings.Join(rows, ",") + `]}`
			cmd := &fakeCommander{up: true, replyFor: func(cmd string) (string, error) {
				switch {
				case cmd == "show channels as json":
					return listing, nil
				case strings.HasPrefix(cmd, "uuid_dump "):
					h, ok := dumps[strings.Fields(cmd)[1]]
					if !ok {
						return "-ERR No such channel!", nil
					}
					return jsonOf(t, h), nil
				}
				return "", nil
			}}

			agents := &presenceCalls{}
			registry := NewRegistry(nullPublisher{})
			t.Cleanup(registry.Shutdown)
			c := NewCoordinator(registry, NewAdapter(cmd, "aicc.test"), agents, nullPublisher{})

			if _, err := c.AdoptLiveChannels(t.Context()); err != nil {
				t.Fatal(err)
			}
			callID, callType := callTypeOfChannel(t, registry, callerID)
			if calleeCall, _ := callTypeOfChannel(t, registry, calleeID); calleeCall != callID {
				t.Fatalf("the adopted legs are on two calls (%s and %s)", callID, calleeCall)
			}
			if callType != tc.wantType {
				t.Fatalf("the adopted call is %s, want %s as it was stamped live", callType, tc.wantType)
			}

			// The call ends after the restart.
			ctx := t.Context()
			c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", calleeID, "outbound", nil))
			c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", callerID, "inbound", nil))

			if len(tc.wrapped) > 0 {
				waitFor(t, func() bool { return len(agents.wrappedUp()) >= len(tc.wrapped) })
			} else {
				settle(t)
			}
			if got := agents.wrappedUp(); !slices.Equal(got, tc.wrapped) {
				t.Errorf("after-call work started for %v, want %v", got, tc.wrapped)
			}
			for _, agentID := range tc.onCall {
				if !agents.tookOffCall(agentID) {
					t.Errorf("agent %s was not taken off the call", agentID)
				}
			}
		})
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
