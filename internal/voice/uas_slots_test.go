// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"testing"
	"time"
)

// The test peer stamps X-AICC-Call-ID: 0198-test on every INVITE, which is the
// key an outbound leg reserves under.
const peerReservationKey = "0198-test"

func slotsOf(t *testing.T, capacity, reserve int) *BotSlots {
	t.Helper()
	return newSlots(t, capacity, reserve, nil)
}

func TestInviteWithAReservationIsAcceptedAtCapacity(t *testing.T) {
	slots := slotsOf(t, 1, 0)
	_, hooks, p := startUAS(t, func(c *Config) { c.Slots = slots })
	if !slots.Reserve(peerReservationKey) {
		t.Fatal("setup")
	}
	// The only slot is reserved: nobody else can have it.
	if slots.TryAcquire() {
		t.Fatal("setup: the reservation did not hold the slot")
	}

	p.connect(hooks)

	if slots.Reserved() != 0 || slots.InUse() != 1 {
		t.Fatalf("reserved %d, in use %d after the INVITE", slots.Reserved(), slots.InUse())
	}
}

func TestInviteWithoutAReservationIsBusyAtCapacity(t *testing.T) {
	slots := slotsOf(t, 1, 0)
	_, hooks, p := startUAS(t, func(c *Config) { c.Slots = slots })
	p.connect(hooks)

	second := newPeer(t, p.uasAddr.Port)
	second.callID = "test-second"
	second.request("INVITE", second.offer())
	second.awaitStatus(486, 2*time.Second)
}

func TestAReservationForAnotherCallIsNotConsumed(t *testing.T) {
	slots := slotsOf(t, 1, 0)
	_, _, p := startUAS(t, func(c *Config) { c.Slots = slots })
	if !slots.Reserve("some-other-call") {
		t.Fatal("setup")
	}

	p.request("INVITE", p.offer()) // carries X-AICC-Call-ID: 0198-test
	p.awaitStatus(486, 2*time.Second)

	if slots.Reserved() != 1 {
		t.Fatalf("another call's reservation was consumed: reserved %d", slots.Reserved())
	}
	if !slots.Consume("some-other-call") {
		t.Fatal("the other call lost its reservation")
	}
}

// Inbound first: with the reserve at 1 of 2, outbound may take one slot and
// inbound the other, and a reserved slot never turns an inbound call away
// while one is free.
func TestInboundStillGetsItsSlotBesideAReservation(t *testing.T) {
	slots := slotsOf(t, 2, 1)
	_, hooks, p := startUAS(t, func(c *Config) { c.Slots = slots })
	if !slots.Reserve("some-other-call") {
		t.Fatal("setup")
	}
	if slots.Reserve("one-too-many") {
		t.Fatal("outbound ate into the inbound reserve")
	}
	p.connect(hooks)
	if slots.InUse() != 1 || slots.Reserved() != 1 {
		t.Fatalf("in use %d, reserved %d", slots.InUse(), slots.Reserved())
	}
}

func TestEndingTheCallGivesTheSlotBack(t *testing.T) {
	slots := slotsOf(t, 1, 0)
	_, hooks, p := startUAS(t, func(c *Config) { c.Slots = slots })
	p.connect(hooks)
	if slots.InUse() != 1 {
		t.Fatalf("in use %d", slots.InUse())
	}
	p.request("BYE", "")
	select {
	case <-hooks.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("call never ended")
	}
	waitFor(t, "the slot to come back", func() bool { return slots.InUse() == 0 })
}

func TestARefusedInviteDoesNotLeakItsSlot(t *testing.T) {
	slots := slotsOf(t, 2, 0)
	_, _, p := startUAS(t, func(c *Config) { c.Slots = slots })
	p.request("INVITE", "v=0\r\ns=broken\r\n") // 488, after the slot was taken
	p.awaitStatus(488, 2*time.Second)
	if slots.InUse() != 0 {
		t.Fatalf("a refused INVITE kept %d slot(s)", slots.InUse())
	}
}
