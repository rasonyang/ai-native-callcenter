// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// recorder captures published events for assertions.
type recorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recorder) Publish(_ context.Context, ev events.Event, _ events.Scope) events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return ev
}

func (r *recorder) types() []events.Type {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Type, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Type)
	}
	return out
}

func (r *recorder) waitFor(t *testing.T, want events.Type) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		r.mu.Lock()
		for _, ev := range r.events {
			if ev.Type == want {
				r.mu.Unlock()
				return ev
			}
		}
		r.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("event %s never arrived; got %v", want, r.types())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func newTestRegistry(t *testing.T) (*Registry, *recorder) {
	t.Helper()
	rec := &recorder{}
	reg := NewRegistry(rec)
	t.Cleanup(reg.Shutdown)
	return reg, rec
}

func TestRegistryRoutesEventsToTheOwningCall(t *testing.T) {
	reg, rec := newTestRegistry(t)
	callID := uuid.New()

	call, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "zh", false)
	if err != nil {
		t.Fatalf("CreateCall() error = %v", err)
	}
	call.AddParty("chan-a", "+8613800138000", testTime)
	if err := reg.BindChannel("chan-a", callID); err != nil {
		t.Fatalf("BindChannel() error = %v", err)
	}

	if !reg.Dispatch(SwitchEvent{Kind: KindChannelAnswer, ChannelID: "chan-a", OccurredAt: testTime}) {
		t.Fatal("Dispatch() did not route a bound channel")
	}

	ev := rec.waitFor(t, events.TypePartyEstablished)
	if ev.CallID == nil || *ev.CallID != callID {
		t.Errorf("event callId = %v, want %v", ev.CallID, callID)
	}
	if ev.CallType != events.CallTypeInbound {
		t.Errorf("event callType = %q, want INBOUND on every call event", ev.CallType)
	}
	if ev.PartyID == nil {
		t.Error("party event without a partyId")
	}

	snap, err := reg.Snapshot(callID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snap.Parties[0].State != PartyTalking {
		t.Errorf("party state = %s, want TALKING", snap.Parties[0].State)
	}
}

func TestDispatchIgnoresUnknownChannels(t *testing.T) {
	reg, _ := newTestRegistry(t)
	if reg.Dispatch(SwitchEvent{Kind: KindChannelAnswer, ChannelID: "unknown"}) {
		t.Error("Dispatch() claimed an unknown channel")
	}
	if reg.Dispatch(SwitchEvent{Kind: KindChannelAnswer}) {
		t.Error("Dispatch() accepted an event with no channel")
	}
}

func TestCallEndsWhenEveryLegReleases(t *testing.T) {
	reg, rec := newTestRegistry(t)
	callID := uuid.New()

	call, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "en", false)
	if err != nil {
		t.Fatal(err)
	}
	call.AddParty("chan-a", "+8613800138000", testTime)
	call.AddParty("chan-b", "1001", testTime)
	for _, ch := range []string{"chan-a", "chan-b"} {
		if err := reg.BindChannel(ch, callID); err != nil {
			t.Fatal(err)
		}
	}

	reg.Dispatch(SwitchEvent{Kind: KindChannelAnswer, ChannelID: "chan-a", OccurredAt: testTime})
	reg.Dispatch(SwitchEvent{Kind: KindChannelHangup, ChannelID: "chan-a",
		HangupCause: "NORMAL_CLEARING", OccurredAt: testTime.Add(time.Minute)})

	// One leg remains, so the call must not finalize yet.
	rec.waitFor(t, events.TypePartyReleased)
	for _, ty := range rec.types() {
		if ty == events.TypeCallCDR {
			t.Fatal("CDR emitted while a leg was still up")
		}
	}

	reg.Dispatch(SwitchEvent{Kind: KindChannelHangup, ChannelID: "chan-b",
		HangupCause: "NORMAL_CLEARING", OccurredAt: testTime.Add(2 * time.Minute)})
	rec.waitFor(t, events.TypeCallCDR)

	// The actor retires the call once it is finished.
	deadline := time.After(2 * time.Second)
	for reg.Count() > 0 {
		select {
		case <-deadline:
			t.Fatal("finished call was not removed from the registry")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestReleasedLegCarriesTransferFlag(t *testing.T) {
	reg, rec := newTestRegistry(t)
	callID := uuid.New()

	call, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "zh", false)
	if err != nil {
		t.Fatal(err)
	}
	call.AddParty("chan-bot", "bot", testTime)
	call.AddParty("chan-keep", "1001", testTime)
	for _, ch := range []string{"chan-bot", "chan-keep"} {
		if err := reg.BindChannel(ch, callID); err != nil {
			t.Fatal(err)
		}
	}

	reg.Dispatch(SwitchEvent{Kind: KindChannelHangup, ChannelID: "chan-bot",
		HangupCause: "NORMAL_CLEARING", TransferredAway: true, OccurredAt: testTime})

	ev := rec.waitFor(t, events.TypePartyReleased)
	if ev.Payload["isTransferredAway"] != true {
		t.Errorf("payload = %+v, want isTransferredAway true: a handoff is not a lost call", ev.Payload)
	}
}

func TestIllegalTransitionsDoNotCorruptState(t *testing.T) {
	reg, _ := newTestRegistry(t)
	callID := uuid.New()

	call, err := reg.CreateCall(context.Background(), callID, events.CallTypeInternal, "en", false)
	if err != nil {
		t.Fatal(err)
	}
	call.AddParty("chan-a", "1001", testTime)
	if err := reg.BindChannel("chan-a", callID); err != nil {
		t.Fatal(err)
	}

	// Retrieve before hold: the switch can deliver this after a reconnect.
	reg.Dispatch(SwitchEvent{Kind: KindChannelUnhold, ChannelID: "chan-a", OccurredAt: testTime})
	// Give the actor time to process before snapshotting.
	snap, err := reg.Snapshot(callID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Parties[0].State != PartyDialing {
		t.Errorf("state = %s, want DIALING to be preserved after a rejected trigger",
			snap.Parties[0].State)
	}
}

func TestSnapshotsAreSerializedWithMutations(t *testing.T) {
	reg, _ := newTestRegistry(t)
	callID := uuid.New()

	call, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "en", false)
	if err != nil {
		t.Fatal(err)
	}
	call.AddParty("chan-a", "+86138", testTime)
	if err := reg.BindChannel("chan-a", callID); err != nil {
		t.Fatal(err)
	}

	// Hammer the actor from several goroutines: the race detector proves the
	// actor model holds, and no snapshot may observe a torn call.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				reg.Dispatch(SwitchEvent{Kind: KindDTMF, ChannelID: "chan-a",
					Digit: "1", DurationMs: 250, OccurredAt: testTime})
				if _, err := reg.Snapshot(callID); err != nil {
					t.Errorf("Snapshot() error = %v", err)
					return
				}
				_ = reg.Do(callID, func(c *Call) {
					c.MergeUserData(map[string]any{"ticketId": "T-1"})
				})
			}
		}()
	}
	wg.Wait()

	snap, err := reg.Snapshot(callID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UserData["ticketId"] != "T-1" {
		t.Errorf("userData = %+v", snap.UserData)
	}
}

func TestDuplicateCallIDIsRejected(t *testing.T) {
	reg, _ := newTestRegistry(t)
	callID := uuid.New()

	if _, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "en", false); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "en", false); err == nil {
		t.Error("CreateCall() accepted a duplicate call id")
	}
}

func TestOperationsOnUnknownCalls(t *testing.T) {
	reg, _ := newTestRegistry(t)
	unknown := uuid.New()

	if _, err := reg.Snapshot(unknown); err != ErrCallNotFound {
		t.Errorf("Snapshot() error = %v, want ErrCallNotFound", err)
	}
	if err := reg.Do(unknown, func(*Call) {}); err != ErrCallNotFound {
		t.Errorf("Do() error = %v, want ErrCallNotFound", err)
	}
	if err := reg.BindChannel("chan-x", unknown); err != ErrCallNotFound {
		t.Errorf("BindChannel() error = %v, want ErrCallNotFound", err)
	}
}

// OnCallRetired is the hook that always runs, which is the whole reason it
// exists next to OnCallFinished. A call absorbed into another is retired
// mid-life and never finishes, so anything released on the finish path — a
// media tap on a leg, in this design — is simply never released for it.
func TestRetirementFiresOnEveryEndingAndFinishOnlyOnTheExpectedOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		end        func(reg *Registry, callID uuid.UUID)
		wantFinish bool
	}{
		{
			name: "every leg releases",
			end: func(reg *Registry, callID uuid.UUID) {
				reg.Dispatch(SwitchEvent{Kind: KindChannelHangup, ChannelID: "chan-a",
					HangupCause: "NORMAL_CLEARING", OccurredAt: testTime.Add(time.Minute)})
			},
			wantFinish: true,
		},
		{
			name: "absorbed into another call",
			end: func(reg *Registry, callID uuid.UUID) {
				reg.Retire(callID)
			},
			wantFinish: false,
		},
		{
			name: "the process shuts down with the call still up",
			end: func(reg *Registry, _ uuid.UUID) {
				reg.Shutdown()
			},
			wantFinish: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := newTestRegistry(t)
			var mu sync.Mutex
			var retired []uuid.UUID
			var finished int
			reg.OnCallRetired = func(id uuid.UUID) {
				mu.Lock()
				retired = append(retired, id)
				mu.Unlock()
			}
			reg.OnCallFinished = func(Snapshot) {
				mu.Lock()
				finished++
				mu.Unlock()
			}

			callID := uuid.New()
			call, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "en", false)
			if err != nil {
				t.Fatal(err)
			}
			call.AddParty("chan-a", "+8613800138000", testTime)
			if err := reg.BindChannel("chan-a", callID); err != nil {
				t.Fatal(err)
			}
			reg.Dispatch(SwitchEvent{Kind: KindChannelAnswer, ChannelID: "chan-a", OccurredAt: testTime})

			tc.end(reg, callID)

			deadline := time.After(2 * time.Second)
			for {
				mu.Lock()
				got := len(retired)
				mu.Unlock()
				if got == 1 {
					break
				}
				select {
				case <-deadline:
					t.Fatalf("the call was retired %d times, want exactly once", got)
				case <-time.After(2 * time.Millisecond):
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if retired[0] != callID {
				t.Errorf("retired %s, want %s", retired[0], callID)
			}
			if tc.wantFinish && finished != 1 {
				t.Errorf("finished %d times, want 1", finished)
			}
			if !tc.wantFinish && finished != 0 {
				t.Errorf("finished %d times on an ending that is not a finish", finished)
			}
		})
	}
}

// The customer hangs up and the agent's softphone clears — on the agent's own
// release, which is the event their screen is about.
//
// This was once written the other way round. A live call on 2026-08-18 left an
// agent's bar up after the customer left, and the fix widened every party event
// to all agents on the call so the customer's release would reach the agent
// too. That masked the real defect: ending the customer's leg ends the agent's,
// so the agent's own release is always coming, and if it does not arrive that
// is the bug to find. Widening also gave both parties of an agent-to-agent call
// the same audience, which is how an agent being rung came to be shown their
// caller's leg (owner directive 2026-08-22).
//
// So the test now hangs up both legs, as the switch does, and asks what the
// agent is told about their own.
func TestTheAgentIsToldTheirOwnLegEnded(t *testing.T) {
	agentID := uuid.New()
	for _, tc := range []struct {
		name    string
		who     events.Subscriber
		wantSaw bool
	}{
		{name: "the agent whose leg it was", wantSaw: true,
			who: events.Subscriber{AgentID: &agentID}},
		{name: "an agent on another call", wantSaw: false,
			who: events.Subscriber{AgentID: ptr(uuid.New())}},
		{name: "a supervisor", wantSaw: true,
			who: events.Subscriber{IsSupervisor: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := events.NewHub(events.NewSequence(seqStub{}, "events"))
			reg := NewRegistry(hub)
			t.Cleanup(reg.Shutdown)

			sub, _, _ := hub.Subscribe(tc.who, 0)
			defer sub.Close()

			callID := uuid.New()
			call, err := reg.CreateCall(context.Background(), callID, events.CallTypeInbound, "en", true)
			if err != nil {
				t.Fatal(err)
			}
			// The caller, with no agent of their own, and the agent's leg.
			call.AddParty("caller-chan", "+8613800138000", testTime)
			agentParty := call.AddParty("agent-chan", "1001", testTime)
			agentParty.AgentID = &agentID
			for _, ch := range []string{"caller-chan", "agent-chan"} {
				if err := reg.BindChannel(ch, callID); err != nil {
					t.Fatal(err)
				}
			}
			reg.Dispatch(SwitchEvent{Kind: KindChannelAnswer, ChannelID: "caller-chan", OccurredAt: testTime})
			reg.Dispatch(SwitchEvent{Kind: KindChannelAnswer, ChannelID: "agent-chan", OccurredAt: testTime})

			// The customer hangs up, which ends the agent's leg with it —
			// hangup_after_bridge, and what the switch was seen doing live.
			reg.Dispatch(SwitchEvent{Kind: KindChannelHangup, ChannelID: "caller-chan",
				HangupCause: "NORMAL_CLEARING", OccurredAt: testTime.Add(time.Minute)})
			reg.Dispatch(SwitchEvent{Kind: KindChannelHangup, ChannelID: "agent-chan",
				HangupCause: "NORMAL_CLEARING", OccurredAt: testTime.Add(time.Minute)})

			saw := false
			deadline := time.After(2 * time.Second)
		collect:
			for {
				select {
				case ev := <-sub.C:
					if ev.Type == events.TypePartyReleased {
						saw = true
						break collect
					}
				case <-deadline:
					break collect
				}
			}
			if saw != tc.wantSaw {
				t.Errorf("saw a release = %v, want %v — an agent whose customer hung "+
					"up must be told their own leg ended, or their softphone stays "+
					"on a call that is over", saw, tc.wantSaw)
			}
		})
	}
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }

type seqStub struct{}

func (seqStub) ReserveSeqBlock(context.Context, string, int64) (int64, error) { return 1, nil }

// Whether a conversation is being recorded is a fact about the call, so it
// reaches everyone on the call rather than one leg's agent. The signal was
// normalized long before anything published it: RECORD_START and RECORD_STOP
// arrived, were understood, and went nowhere (W7 group one).
func TestTheCallSaysWhenItIsBeingRecorded(t *testing.T) {
	pub := &capturingPublisher{}
	registry := NewRegistry(pub)
	t.Cleanup(registry.Shutdown)

	callID := uuid.New()
	if _, err := registry.CreateCall(t.Context(), callID, events.CallTypeInbound, "zh", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := registry.BindChannel("caller-chan", callID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Recording starts on a channel that is a leg of the call, as it does live.
	if err := registry.Do(callID, func(call *Call) {
		call.AddParty("caller-chan", "18600000000", time.Now())
	}); err != nil {
		t.Fatalf("add party: %v", err)
	}

	registry.Dispatch(SwitchEvent{
		Kind: KindRecordStart, ChannelID: "caller-chan", OccurredAt: time.Now(),
		RecordingPath: "/usr/local/freeswitch/recordings/2026/08/23/x.wav",
	})
	registry.Dispatch(SwitchEvent{
		Kind: KindRecordStop, ChannelID: "caller-chan", OccurredAt: time.Now(),
		RecordingPath: "/usr/local/freeswitch/recordings/2026/08/23/x.wav",
	})
	waitFor(t, func() bool {
		pub.mu.Lock()
		defer pub.mu.Unlock()
		return len(pub.events) >= 2
	})

	pub.mu.Lock()
	seen := append([]events.Event(nil), pub.events...)
	pub.mu.Unlock()

	var started, stopped *events.Event
	for _, ev := range seen {
		switch ev.Type {
		case events.TypeCallRecordingStarted:
			e := ev
			started = &e
		case events.TypeCallRecordingStopped:
			e := ev
			stopped = &e
		}
	}
	if started == nil || stopped == nil {
		t.Fatalf("recording was never announced: %+v", seen)
	}
	if started.PartyID != nil || started.AgentID != nil {
		t.Error("the recording was announced as a leg's event; it is the call that " +
			"is being recorded, and everyone on it should hear so")
	}
	// The switch names a path on its own disk. Nobody holding a browser can
	// use it, and the recording itself is fetched by call id.
	for _, ev := range []*events.Event{started, stopped} {
		for k, v := range ev.Payload {
			if s, ok := v.(string); ok && strings.Contains(s, "/freeswitch/") {
				t.Errorf("the switch's own file path went out on the wire as %s=%v", k, v)
			}
		}
	}
}
