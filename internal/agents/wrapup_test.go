// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"context"
	"errors"
	"sync"
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

	// The phone the agent is signed in at, as the switch reports it. Ending
	// after-call work returns them to READY, which needs one.
	svc.NoteDevice("1001", true, true)
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

// openedWrapUps records the after-call records the service asked the ledger to
// open, which is the invariant this model rests on.
type openedWrapUps struct {
	mu     sync.Mutex
	opened [][2]uuid.UUID
	err    error
}

func (o *openedWrapUps) OpenWrapUp(_ context.Context, callID, agentID uuid.UUID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	o.opened = append(o.opened, [2]uuid.UUID{callID, agentID})
	return nil
}

func (o *openedWrapUps) all() [][2]uuid.UUID {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][2]uuid.UUID(nil), o.opened...)
}

// A finished call always has an after-call record, opened when the work
// begins rather than when somebody remembers to file one. Without it, "nobody
// wrote this call up" and "the agent is still typing" are the same absence,
// and no report can tell them apart.
func TestAfterCallWorkOpensItsRecordAtOnce(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	ledger := &openedWrapUps{}
	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	svc.AttachWrapUps(ledger)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	callID := uuid.New()
	if _, err := svc.StartWrapUp(ctx, agentID, callID); err != nil {
		t.Fatal(err)
	}

	opened := ledger.all()
	if len(opened) != 1 || opened[0] != [2]uuid.UUID{callID, agentID} {
		t.Fatalf("opened %v, want one record for the call that just ended (%s)", opened, callID)
	}
}

// A ledger that refuses does not leave the agent taking calls: after-call work
// has begun either way, and the confirmation will create the record.
func TestAfterCallWorkStartsEvenIfTheRecordCannotBeOpened(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	svc.AttachWrapUps(&openedWrapUps{err: errors.New("the database is down")})
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	p, err := svc.StartWrapUp(ctx, agentID, uuid.New())
	if err != nil {
		t.Fatalf("StartWrapUp() error = %v, want the presence change to stand", err)
	}
	if !p.IsInWrapUp() {
		t.Errorf("presence = %s(%s), want after-call work regardless of the ledger", p.State, p.Reason)
	}
}

// An agent whose phone went away while they were writing up the call still
// files it, and the filing is still accepted. What they cannot be is READY —
// so the wrap-up ends in NOT_READY(DEVICE_LOST), which is where a device-loss
// release would have put them anyway.
//
// Refusing the completion instead would show the agent a rejection for work
// the ledger has already recorded, and leave them held in a wrap-up that is
// over.
func TestEndingWrapUpWithNoPhoneLandsInDeviceLostRatherThanFailing(t *testing.T) {
	store := newFakeStore()
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	svc := NewService(store, &fakeSwitch{up: true}, &fakePublisher{})
	ctx := context.Background()

	svc.NoteDevice("1001", true, true)
	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartWrapUp(ctx, agentID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// The browser tab closed while the note was being typed.
	svc.ObserveDevice(ctx, "1001", SignalUnregistered)

	p, err := svc.EndWrapUp(ctx, agentID)
	if err != nil {
		t.Fatalf("EndWrapUp() error = %v, want the completion to be accepted", err)
	}
	if p.CurrentState() != StateNotReady || p.Reason != ReasonDeviceLost {
		t.Errorf("presence = %s(%s), want NOT_READY(DEVICE_LOST)", p.CurrentState(), p.Reason)
	}
	if p.WrapUpCallID != nil {
		t.Error("the after-call work is over; the presence still names its call")
	}
}
