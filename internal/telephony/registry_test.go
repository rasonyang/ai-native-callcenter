// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
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
