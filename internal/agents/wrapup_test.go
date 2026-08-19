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
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001", WrapUpTimeSec: 30}
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
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001", WrapUpTimeSec: 30}
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
	if p.State != StateReady || p.WrapUpCallID != nil || !p.WrapUpEndsAt.IsZero() {
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

// The window closing is not the filing being refused: an agent still typing
// when the timer returned them to READY may still file against that call.
func TestTheLastWrappedCallOutlivesTheWindow(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001", WrapUpTimeSec: 1}
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
	// The window ran out.
	svc.now = func() time.Time { return now.Add(2 * time.Second) }
	svc.expireWrapUp(agentID, svc.wrapUpGen[agentID])
	if p := svc.Presence(agentID); p.State != StateReady || p.WrapUpCallID != nil {
		t.Fatalf("presence after expiry = %+v, want READY with no wrap-up call on it", p)
	}

	if got, ok := svc.WrapUpCall(agentID); !ok || got != callID {
		t.Errorf("WrapUpCall() after expiry = %s, %v; want the call to stay addressable", got, ok)
	}
	// Signing out is the end of it.
	if _, err := svc.Logout(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.WrapUpCall(agentID); ok {
		t.Error("WrapUpCall() after sign-out still answers; a new session starts with nothing to file")
	}
}

// A zero-length wrap-up is a READY transition and must publish as one; the
// call is still remembered, because the agent still handled it.
func TestZeroWrapUpGoesStraightToReadyAndStillRemembersTheCall(t *testing.T) {
	store := newFakeStore()
	pub := &fakePublisher{}
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001", WrapUpTimeSec: 0}
	svc := NewService(store, &fakeSwitch{up: true}, pub)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	callID := uuid.New()
	p, err := svc.StartWrapUp(ctx, agentID, callID)
	if err != nil {
		t.Fatalf("StartWrapUp() error = %v", err)
	}
	if p.State != StateReady {
		t.Errorf("state = %s, want READY", p.State)
	}
	if got, ok := svc.WrapUpCall(agentID); !ok || got != callID {
		t.Errorf("WrapUpCall() = %s, %v; want %s", got, ok, callID)
	}
	last := pub.events[len(pub.events)-1]
	if last.Type != "AGENT_READY" {
		t.Errorf("published %s, want AGENT_READY for a wrap-up that never held the agent", last.Type)
	}
}

// After a restart the in-memory record is gone, but a wrap-up under way was
// persisted with its call and Restore brings it back.
func TestRestoreRecoversTheWrapUpCall(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	callID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001", WrapUpTimeSec: 30}
	ends := now.Add(30 * time.Second)
	store.presence[agentID] = Presence{
		State: StateNotReady, Reason: ReasonAfterCallWork, ExtensionNumber: "1001",
		EnteredAt: now, WrapUpEndsAt: ends, WrapUpCallID: &callID,
	}

	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	if err := svc.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := svc.WrapUpCall(agentID); !ok || got != callID {
		t.Errorf("WrapUpCall() after restore = %s, %v; want %s", got, ok, callID)
	}
}
