// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// After-call work is started by the switch path and nothing else. Before this
// existed, `StartWrapUp` had no caller at all: the state the whole cockpit is
// built around could be reached in a unit test and never on a real call.

// presenceCalls records what call control told the agent service, in order,
// because the order is load-bearing: an agent still marked on a call derives
// ON_CALL, which outranks WRAP_UP.
type presenceCalls struct {
	mu    sync.Mutex
	steps []string
	calls []uuid.UUID
}

func (p *presenceCalls) AgentAtExtension(ext string) (uuid.UUID, bool) {
	if ext == agentExtension {
		return testAgentID, true
	}
	return uuid.Nil, false
}

func (p *presenceCalls) AgentByCallcenterName(string) (uuid.UUID, bool) { return uuid.Nil, false }

func (p *presenceCalls) SetOnCall(_ context.Context, _ uuid.UUID, onCall bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if onCall {
		p.steps = append(p.steps, "onCall")
		return
	}
	p.steps = append(p.steps, "offCall")
}

func (p *presenceCalls) BeginAfterCallWork(_ context.Context, agentID, callID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, "wrapUp:"+agentID.String())
	p.calls = append(p.calls, callID)
}

func (p *presenceCalls) taken() ([]string, []uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.steps...), append([]uuid.UUID(nil), p.calls...)
}

// answerAndEndAnAgentLeg drives one queue delivery to a conversation and then
// ends the agent's own leg, which is where their part of the call finishes —
// on a transfer the caller talks on without them.
func answerAndEndAnAgentLeg(t *testing.T, c *Coordinator, answered bool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	minted := uuid.New()
	vars := map[string]string{"variable_aicc_call_id": minted.String()}
	callerChan, agentChan := "caller-chan", "agent-chan"

	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", agentChan, "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": callerChan})))
	if answered {
		c.Handle(ctx, raw("CHANNEL_ANSWER", agentChan, "outbound", vars))
	}
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", agentChan, "outbound", vars))
	return minted
}

func TestAnAnsweredAgentLegEndingStartsAfterCallWork(t *testing.T) {
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
	// Off the call first. The other order shows the agent as still talking to
	// somebody who has hung up, because being on a call outranks wrap-up when
	// availability is derived.
	offCall, wrapUp := indexOf(steps, "offCall"), indexOf(steps, "wrapUp:"+testAgentID.String())
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
	agents := &presenceCalls{}
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, agents, nullPublisher{})

	answerAndEndAnAgentLeg(t, c, false)

	steps, calls := agents.taken()
	if len(calls) != 0 {
		t.Errorf("wrap-up started for %v after a call nobody answered", calls)
	}
	if indexOf(steps, "offCall") < 0 {
		t.Errorf("steps = %v, want the agent taken off the call regardless", steps)
	}
}

// The caller's own leg ending is not an agent's part of a call ending.
func TestTheCallersLegEndingStartsNobodysAfterCallWork(t *testing.T) {
	agents := &presenceCalls{}
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, agents, nullPublisher{})

	ctx := t.Context()
	vars := map[string]string{"variable_aicc_call_id": uuid.New().String()}
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
