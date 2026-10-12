// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

func slotted(t *testing.T, capacity, reserve int, hold time.Duration) (*Service, *fakeSwitch, *voice.BotSlots) {
	t.Helper()
	slots, err := voice.NewBotSlots(voice.BotSlotsConfig{Cap: capacity, InboundReserve: reserve})
	if err != nil {
		t.Fatal(err)
	}
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	s.cfg.Slots = slots
	s.cfg.OverflowHold = hold
	return s, sw, slots
}

func placeAI(t *testing.T, s *Service, sw *fakeSwitch) (callKey, leg string) {
	t.Helper()
	id, err := s.DialAI(context.Background(), AIDialRequest{To: "13912345678", DIDNumber: "95012"})
	if err != nil {
		t.Fatal(err)
	}
	return id.String(), sw.lastOriginate().partyID.String()
}

func TestAnsweredLegReservesItsSlotBeforeTheBridge(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, time.Second)
	key, leg := placeAI(t, s, sw)

	// Ringing holds nothing.
	if slots.Reserved() != 0 || slots.InUse() != 0 {
		t.Fatalf("a ringing leg holds a slot: %d reserved, %d in use", slots.Reserved(), slots.InUse())
	}
	answer(s, leg)
	waitBridges(t, sw, 1)
	if slots.Reserved() != 1 {
		t.Fatalf("reserved %d, want 1", slots.Reserved())
	}
	// The INVITE this bridge causes would find its reservation under the
	// call id it carries in X-AICC-Call-ID.
	if got := sw.lastBridge().vars["sip_h_X-AICC-Call-ID"]; got != key {
		t.Fatalf("bridge carries call id %q, reserved under %q", got, key)
	}
	if !slots.Consume(key) {
		t.Fatal("the reservation is not under the call id")
	}
}

func TestAnsweredLegWaitsForASlotThenBridges(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, 2*time.Second)
	slots.TryAcquire() // an inbound call holds the only slot
	_, leg := placeAI(t, s, sw)

	answer(s, leg)
	settle()
	if sw.bridgeCount() != 0 || len(sw.hangupList()) != 0 {
		t.Fatal("the leg was bridged or dropped while a slot might still free")
	}
	slots.Free()
	waitBridges(t, sw, 1)
	if h := sw.hangupList(); len(h) != 0 {
		t.Fatalf("hung up despite a slot freeing: %+v", h)
	}
}

func TestNoSlotWithinTheHoldHangsUpAsOverflow(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, 60*time.Millisecond)
	slots.TryAcquire()
	_, leg := placeAI(t, s, sw)

	answer(s, leg)
	h := waitHangups(t, sw, 1)
	if h[0].channelID != leg {
		t.Fatalf("hung up %q, want the customer leg %q", h[0].channelID, leg)
	}
	sw.mu.Lock()
	vars := append([][3]string(nil), sw.vars...)
	sw.mu.Unlock()
	if len(vars) != 1 || vars[0] != [3]string{leg, "aicc_hangup_cause", OverflowCause} {
		t.Fatalf("the OVERFLOW cause was not stamped before the hangup: %+v", vars)
	}
	if sw.bridgeCount() != 0 {
		t.Fatal("an overflowed leg was bridged")
	}
	if slots.Reserved() != 0 {
		t.Fatal("an overflowed leg holds a reservation")
	}
}

func TestNoHoldMeansImmediateOverflow(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, 0)
	slots.TryAcquire()
	_, leg := placeAI(t, s, sw)
	answer(s, leg)
	waitHangups(t, sw, 1)
}

func TestCustomerHangupDuringTheHoldReleasesAndDoesNotBridge(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, 5*time.Second)
	slots.TryAcquire()
	_, leg := placeAI(t, s, sw)
	answer(s, leg)
	settle()

	hangup(s, leg)
	settle()
	if sw.bridgeCount() != 0 {
		t.Fatal("bridged a customer who had hung up")
	}
	if h := sw.hangupList(); len(h) != 0 {
		t.Fatalf("hung up a leg that was already gone: %+v", h)
	}
	slots.Free()
	if slots.Reserved() != 0 {
		t.Fatalf("the abandoned wait took a slot afterwards: reserved %d", slots.Reserved())
	}
	if !slots.TryAcquire() {
		t.Fatal("the slot is still held")
	}
}

func TestCustomerHangupBeforeTheInviteReleasesTheReservation(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, time.Second)
	_, leg := placeAI(t, s, sw)
	answer(s, leg)
	waitBridges(t, sw, 1)
	if slots.Reserved() != 1 {
		t.Fatal("setup: nothing reserved")
	}
	hangup(s, leg)
	if slots.Reserved() != 0 {
		t.Fatalf("the reservation outlived the customer: %d", slots.Reserved())
	}
}

func TestHangupAfterTheInviteConsumedItLeavesTheSlotAlone(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, time.Second)
	key, leg := placeAI(t, s, sw)
	answer(s, leg)
	waitBridges(t, sw, 1)
	if !slots.Consume(key) {
		t.Fatal("setup")
	}
	hangup(s, leg)
	if slots.InUse() != 1 {
		t.Fatal("the originator freed a slot the UAS owns")
	}
}

func TestOutboundReservationRespectsTheInboundReserve(t *testing.T) {
	s, sw, slots := slotted(t, 2, 1, 40*time.Millisecond)
	slots.TryAcquire() // one inbound call; one slot left, but it is inbound's
	_, leg := placeAI(t, s, sw)
	answer(s, leg)
	waitHangups(t, sw, 1)
	if !slots.TryAcquire() {
		t.Fatal("inbound lost the slot it was reserved")
	}
}

func TestClickToDialTakesNoSlot(t *testing.T) {
	s, sw, slots := slotted(t, 1, 0, time.Second)
	slots.TryAcquire()
	if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1001", To: "13912345678"}); err != nil {
		t.Fatal(err)
	}
	answer(s, sw.lastOriginate().partyID.String())
	waitTransfers(t, sw, 1)
	if slots.Reserved() != 0 {
		t.Fatal("click-to-dial reserved a bot slot")
	}
}
