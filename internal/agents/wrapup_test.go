// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// After-call work is for a call. The presence names it while the window is
// open, so what the agent files lands on that call and not on the next one.
func TestWrapUpNamesTheCallItIsFor(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	callID := uuid.New()
	p, err := svc.StartWrapUp(ctx, agentID, callID)
	if err != nil {
		t.Fatalf("StartWrapUp() error = %v", err)
	}
	if !p.IsInWrapUp() {
		t.Fatalf("presence = %s(%s), want NOT_READY(AFTER_CALL_WORK)", p.State, p.Reason)
	}
	if p.WrapUpCallID == nil || *p.WrapUpCallID != callID {
		t.Errorf("wrapUpCallId = %v, want %s", p.WrapUpCallID, callID)
	}
	if got, ok := svc.WrapUpCall(agentID); !ok || got != callID {
		t.Errorf("WrapUpCall() = %s, %v; want %s", got, ok, callID)
	}
	// The persisted row carries it too, so a restart does not orphan a
	// wrap-up that was under way.
	if saved := store.presence[agentID]; saved.WrapUpCallID == nil || *saved.WrapUpCallID != callID {
		t.Errorf("persisted wrapUpCallId = %v, want %s", saved.WrapUpCallID, callID)
	}
}

// EndWrapUp is the completion of after-call work: READY when the agent was
// still in it, and no opinion at all when they were not.
func TestEndWrapUpReturnsToReadyOnlyFromWrapUp(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartWrapUp(ctx, agentID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	p, err := svc.EndWrapUp(ctx, agentID)
	if err != nil {
		t.Fatalf("EndWrapUp() error = %v", err)
	}
	if p.State != StateReady || p.WrapUpCallID != nil {
		t.Errorf("after EndWrapUp presence = %+v, want READY with the wrap-up cleared", p)
	}

	// The agent chose lunch; completing a wrap-up must not drag them back.
	if _, err := svc.StartWrapUp(ctx, agentID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.NotReady(ctx, agentID, ReasonLunch); err != nil {
		t.Fatal(err)
	}
	p, err = svc.EndWrapUp(ctx, agentID)
	if err != nil {
		t.Fatalf("EndWrapUp() error = %v", err)
	}
	if p.State != StateNotReady || p.Reason != ReasonLunch {
		t.Errorf("presence = %s(%s), want the agent's own choice to stand", p.State, p.Reason)
	}
}

// Leaving after-call work is not the filing being refused: an agent who was
// taken out of it — by their own choice, or by a supervisor — may still file
// against the call they were writing up, until the next one begins.
func TestTheLastWrappedCallOutlivesTheState(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	svc.now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	callID := uuid.New()
	if _, err := svc.StartWrapUp(ctx, agentID, callID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.NotReady(ctx, agentID, ReasonLunch); err != nil {
		t.Fatal(err)
	}
	if p := svc.Presence(agentID); p.WrapUpCallID != nil {
		t.Fatalf("presence still names a wrap-up call after the agent chose lunch: %+v", p)
	}

	if got, ok := svc.WrapUpCall(agentID); !ok || got != callID {
		t.Errorf("WrapUpCall() = %s, %v; want the call to stay addressable", got, ok)
	}
	// Signing out is the end of it.
	if _, err := svc.Logout(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.WrapUpCall(agentID); ok {
		t.Error("WrapUpCall() after sign-out still answers; a new session starts with nothing to file")
	}
}

// The event says what happened: an agent has entered after-call work and is
// not taking calls, which is what a wallboard needs to hear.
func TestStartingWrapUpAnnouncesTheAgentIsNotReady(t *testing.T) {
	store := newFakeStore()
	pub := &fakePublisher{}
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	svc := NewService(store, &fakeSwitch{up: true}, pub)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	callID := uuid.New()
	if _, err := svc.StartWrapUp(ctx, agentID, callID); err != nil {
		t.Fatalf("StartWrapUp() error = %v", err)
	}

	last := pub.events[len(pub.events)-1]
	if last.Type != "AGENT_NOT_READY" {
		t.Errorf("published %s, want AGENT_NOT_READY", last.Type)
	}
	if last.Payload["reason"] != string(ReasonAfterCallWork) {
		t.Errorf("reason = %v, want AFTER_CALL_WORK", last.Payload["reason"])
	}
	if last.Payload["wrapUpCallId"] != callID {
		t.Errorf("wrapUpCallId = %v, want the call being wrapped up (%s)",
			last.Payload["wrapUpCallId"], callID)
	}
}

// After a restart the in-memory record is gone, but a wrap-up under way was
// persisted with its call and Restore brings it back.
func TestRestoreRecoversTheWrapUpCall(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	callID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	store.presence[agentID] = Presence{
		State: StateNotReady, Reason: ReasonAfterCallWork, ExtensionNumber: "1001",
		EnteredAt: now, WrapUpCallID: &callID,
	}

	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	if err := svc.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := svc.WrapUpCall(agentID); !ok || got != callID {
		t.Errorf("WrapUpCall() after restore = %s, %v; want %s", got, ok, callID)
	}
}
