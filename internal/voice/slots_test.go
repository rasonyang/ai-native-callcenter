// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"
)

// manualClock fires timers when a test advances it, in deadline order.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[int]*manualTimer
	next   int
}

type manualTimer struct {
	at time.Time
	f  func()
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Unix(2_000_000, 0), timers: map[int]*manualTimer{}}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.timers[id] = &manualTimer{at: c.now.Add(d), f: f}
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, live := c.timers[id]
		delete(c.timers, id)
		return live
	}
}

func (c *manualClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// advance moves time forward, firing every timer that falls due on the way at
// its own instant.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		ids := make([]int, 0, len(c.timers))
		for id, tm := range c.timers {
			if !tm.at.After(target) {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			c.now = target
			c.mu.Unlock()
			return
		}
		sort.Slice(ids, func(i, j int) bool {
			a, b := c.timers[ids[i]], c.timers[ids[j]]
			if a.at.Equal(b.at) {
				return ids[i] < ids[j]
			}
			return a.at.Before(b.at)
		})
		tm := c.timers[ids[0]]
		delete(c.timers, ids[0])
		if tm.at.After(c.now) {
			c.now = tm.at
		}
		c.mu.Unlock()
		tm.f()
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newSlots(t *testing.T, capacity, reserve int, clock SlotClock) *BotSlots {
	t.Helper()
	b, err := NewBotSlots(BotSlotsConfig{Cap: capacity, InboundReserve: reserve, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBotSlotsConfigIsValidated(t *testing.T) {
	for _, c := range []BotSlotsConfig{
		{Cap: 0}, {Cap: -1}, {Cap: 5, InboundReserve: -1},
		{Cap: 5, InboundReserve: 5}, {Cap: 5, InboundReserve: 9},
	} {
		if _, err := NewBotSlots(c); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
	if _, err := NewBotSlots(BotSlotsConfig{Cap: 5, InboundReserve: 4}); err != nil {
		t.Errorf("reserve 4 of 5 refused: %v", err)
	}
}

// The counter against a model, over a seeded stream of everything that can
// happen to it: inbound INVITEs, sessions ending, reservations (duplicates
// included), consumes (late ones included), releases and the passage of time.
func TestBotSlotsCounterProperty(t *testing.T) {
	for _, reserve := range []int{0, 2} {
		t.Run(fmt.Sprintf("reserve=%d", reserve), func(t *testing.T) {
			const capacity, events = 5, 10_000
			clock := newManualClock()
			b := newSlots(t, capacity, reserve, clock)
			rng := rand.New(rand.NewSource(158))

			held := 0                      // sessions the model believes are up
			live := map[string]time.Time{} // reservations and when they expire
			keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
			var late, dups int

			for i := 0; i < events; i++ {
				switch op := rng.Intn(100); {
				case op < 20: // inbound INVITE
					want := held+len(live) < capacity
					if got := b.TryAcquire(); got != want {
						t.Fatalf("event %d: TryAcquire=%v, model %v", i, got, want)
					}
					if want {
						held++
					}
				case op < 35: // a session ends
					if held > 0 {
						b.Free()
						held--
					}
				case op < 55: // outbound reservation, sometimes a duplicate
					k := keys[rng.Intn(len(keys))]
					_, exists := live[k]
					want := exists || held+len(live) < capacity-reserve
					if got := b.Reserve(k); got != want {
						t.Fatalf("event %d: Reserve(%s)=%v, model %v", i, k, got, want)
					}
					if exists {
						dups++
					} else if want {
						live[k] = clock.Now().Add(DefaultReservationTTL)
					}
				case op < 75: // the INVITE for a key; often there is nothing to take
					k := keys[rng.Intn(len(keys))]
					_, exists := live[k]
					if got := b.Consume(k); got != exists {
						t.Fatalf("event %d: Consume(%s)=%v, model %v", i, k, got, exists)
					}
					if exists {
						delete(live, k)
						held++
					} else {
						late++
					}
				case op < 85: // customer hung up
					k := keys[rng.Intn(len(keys))]
					b.Release(k)
					delete(live, k)
				default: // time passes
					clock.advance(time.Duration(rng.Intn(4000)) * time.Millisecond)
					for k, at := range live {
						if !at.After(clock.Now()) {
							delete(live, k)
						}
					}
				}
				if in := b.InUse(); in != held || b.Reserved() != len(live) {
					t.Fatalf("event %d: counter in-use=%d reserved=%d, model %d/%d",
						i, in, b.Reserved(), held, len(live))
				}
				if b.InUse()+b.Reserved() > capacity {
					t.Fatalf("event %d: over capacity: %d in use + %d reserved",
						i, b.InUse(), b.Reserved())
				}
			}
			if late == 0 || dups == 0 {
				t.Fatalf("generator never produced late consumes (%d) or duplicates (%d)", late, dups)
			}

			// Wind down: every session ends, every reservation resolves.
			for ; held > 0; held-- {
				b.Free()
			}
			clock.advance(2 * DefaultReservationTTL)
			if b.InUse() != 0 || b.Reserved() != 0 {
				t.Fatalf("leak: %d in use, %d reserved", b.InUse(), b.Reserved())
			}
			st := b.Stats()
			if st.Made != st.Consumed+st.Released+st.Expired {
				t.Fatalf("a reservation ended nowhere: %+v", st)
			}
			if st.Consumed == 0 || st.Released == 0 || st.Expired == 0 {
				t.Fatalf("not every ending was exercised: %+v", st)
			}
			if clock.pending() != 0 {
				t.Fatalf("%d timers outlive the run", clock.pending())
			}
		})
	}
}

func TestBotSlotsConcurrentUseNeverExceedsCapacity(t *testing.T) {
	b := newSlots(t, 5, 0, nil)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 2000; i++ {
				k := fmt.Sprintf("k%d", rng.Intn(8))
				switch rng.Intn(5) {
				case 0:
					if b.TryAcquire() {
						b.Free()
					}
				case 1:
					b.Reserve(k)
				case 2:
					if b.Consume(k) {
						b.Free()
					}
				case 3:
					b.Release(k)
				default:
					ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
					b.ReserveWait(ctx, k, time.Millisecond)
					cancel()
				}
				if n := b.total(); n > 5 {
					t.Errorf("over capacity: %d", n)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		b.Release(fmt.Sprintf("k%d", i))
	}
	if b.InUse() != 0 || b.Reserved() != 0 {
		t.Fatalf("leak: %d in use, %d reserved", b.InUse(), b.Reserved())
	}
}

// A slot freed 700 ms into the hold is bridged then, not at the end of it.
func TestHoldBridgesAsSoonAsASlotFrees(t *testing.T) {
	clock := newManualClock()
	b := newSlots(t, 1, 0, clock)
	if !b.TryAcquire() {
		t.Fatal("setup")
	}
	type result struct {
		ok     bool
		waited time.Duration
	}
	done := make(chan result, 1)
	go func() {
		ok, waited := b.ReserveWait(context.Background(), "call-1", 1500*time.Millisecond)
		done <- result{ok, waited}
	}()
	waitFor(t, "the hold timer", func() bool { return clock.pending() == 1 })

	clock.advance(700 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("returned before a slot freed: %+v", r)
	case <-time.After(20 * time.Millisecond):
	}
	b.Free()
	r := <-done
	if !r.ok || r.waited != 700*time.Millisecond {
		t.Fatalf("got %+v, want a reservation after 700ms", r)
	}
	if b.Reserved() != 1 || b.InUse() != 0 {
		t.Fatalf("in use %d, reserved %d", b.InUse(), b.Reserved())
	}
	if !b.Consume("call-1") {
		t.Fatal("the reservation could not be consumed")
	}
	if clock.pending() != 0 {
		t.Fatalf("%d timers left behind", clock.pending())
	}
}

func TestHoldGivesUpAtTheOverflowHold(t *testing.T) {
	clock := newManualClock()
	b := newSlots(t, 1, 0, clock)
	b.TryAcquire()
	type result struct {
		ok     bool
		waited time.Duration
	}
	done := make(chan result, 1)
	go func() {
		ok, w := b.ReserveWait(context.Background(), "call-1", 1500*time.Millisecond)
		done <- result{ok, w}
	}()
	waitFor(t, "the hold timer", func() bool { return clock.pending() == 1 })
	clock.advance(1499 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("gave up early")
	case <-time.After(20 * time.Millisecond):
	}
	clock.advance(time.Millisecond)
	r := <-done
	if r.ok {
		t.Fatal("reserved a slot that never freed")
	}
	if r.waited != 1500*time.Millisecond {
		t.Fatalf("waited %s", r.waited)
	}
	if b.Reserved() != 0 {
		t.Fatal("a failed hold left a reservation")
	}
}

func TestCustomerHangupDuringTheHoldReleasesIt(t *testing.T) {
	clock := newManualClock()
	b := newSlots(t, 1, 0, clock)
	b.TryAcquire()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		ok, _ := b.ReserveWait(ctx, "call-1", 1500*time.Millisecond)
		done <- ok
	}()
	waitFor(t, "the hold timer", func() bool { return clock.pending() == 1 })
	cancel()
	if ok := <-done; ok {
		t.Fatal("reserved after the customer left")
	}
	b.Free() // the slot the next caller gets
	if !b.TryAcquire() {
		t.Fatal("the abandoned hold kept a slot")
	}
	if clock.pending() != 0 {
		t.Fatalf("%d timers left behind", clock.pending())
	}
}

// A reserved slot the customer hangs up on before the INVITE goes back to the
// pool at once; one nobody claims goes back when it expires.
func TestReservationReleasedOrExpiredFreesTheSlot(t *testing.T) {
	clock := newManualClock()
	b := newSlots(t, 1, 0, clock)
	b.Reserve("gone")
	if b.TryAcquire() {
		t.Fatal("inbound took a reserved slot")
	}
	b.Release("gone")
	if !b.TryAcquire() {
		t.Fatal("release did not free the slot")
	}
	b.Free()

	b.Reserve("lost")
	clock.advance(DefaultReservationTTL - time.Millisecond)
	if b.TryAcquire() {
		t.Fatal("the reservation expired early")
	}
	clock.advance(time.Millisecond)
	if !b.TryAcquire() {
		t.Fatal("an unclaimed reservation was never given back")
	}
	if b.Consume("lost") {
		t.Fatal("a late INVITE consumed an expired reservation")
	}
	if st := b.Stats(); st.Expired != 1 || st.Released != 1 {
		t.Fatalf("%+v", st)
	}
}

// Inbound comes first: with a reserve of 2 of 5, an outbound reservation stops
// when 3 are taken, and inbound still has the last two.
func TestInboundReserveKeepsTheLastSlotsForInbound(t *testing.T) {
	b := newSlots(t, 5, 2, newManualClock())
	for _, k := range []string{"a", "b", "c"} {
		if !b.Reserve(k) {
			t.Fatalf("reservation %s refused with room outside the reserve", k)
		}
	}
	if b.Reserve("d") {
		t.Fatal("an outbound reservation ate into the inbound reserve")
	}
	if !b.TryAcquire() || !b.TryAcquire() {
		t.Fatal("inbound could not use the reserve")
	}
	if b.TryAcquire() {
		t.Fatal("inbound exceeded the capacity")
	}
	// Consuming does not change the total, so the bridge is never refused.
	if !b.Consume("a") || !b.Consume("b") || !b.Consume("c") {
		t.Fatal("a reserved slot was lost")
	}
	if got := b.InUse(); got != 5 {
		t.Fatalf("in use %d", got)
	}
}

// With nothing reserved, behaviour is the old one: capacity, then refusal.
func TestWithoutReservationsTheCounterIsAPlainCap(t *testing.T) {
	b := newSlots(t, 3, 0, nil)
	for i := 0; i < 3; i++ {
		if !b.TryAcquire() {
			t.Fatalf("slot %d refused", i)
		}
	}
	if b.TryAcquire() {
		t.Fatal("fourth slot granted")
	}
	b.Free()
	if !b.TryAcquire() {
		t.Fatal("freed slot not reusable")
	}
}

// total reads both halves under one lock.
func (b *BotSlots) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse + len(b.reserved)
}
