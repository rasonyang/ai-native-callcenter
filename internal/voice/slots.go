// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// DefaultReservationTTL is how long a reservation waits for its INVITE.
//
// The reservation is made when the customer answers, and the INVITE follows
// the bridge command by one switch round trip plus a SIP hop: tens of
// milliseconds on a healthy path. Ten seconds is two orders of magnitude of
// slack, long enough that a slow switch or a retransmitted INVITE is never
// refused its slot, and short enough that a bridge lost on the way cannot hold
// a bot slot for the length of a call. The customer hanging up releases it
// sooner.
const DefaultReservationTTL = 10 * time.Second

// SlotClock is the time the counter runs on. Tests replace it.
type SlotClock interface {
	Now() time.Time
	// AfterFunc runs f once after d and returns a function that stops it,
	// reporting whether it stopped before f ran.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// BotSlotsConfig configures the bot-capacity counter.
type BotSlotsConfig struct {
	// Cap is the number of concurrent bot sessions (AICC_BOT_MAX_CALLS).
	Cap int
	// InboundReserve is how many slots an outbound reservation may not take:
	// a reservation fails when in-use plus reserved has reached Cap minus
	// this. Inbound may use all of Cap. 0 <= InboundReserve < Cap.
	InboundReserve int
	// ReservationTTL defaults to DefaultReservationTTL.
	ReservationTTL time.Duration
	Clock          SlotClock
}

// BotSlots is the one bot-capacity number, shared by inbound and outbound.
//
// A slot is either in use (a SIP dialog the UAS accepted) or reserved (an
// answered outbound leg that is about to INVITE, held under its call id). Both
// count against Cap, so a reserved slot cannot be taken from under the call
// that is about to use it, and the INVITE that comes for it does not compete
// with anyone.
type BotSlots struct {
	cfg BotSlotsConfig

	mu       sync.Mutex
	inUse    int
	reserved map[string]*reservation
	// changed is closed, and replaced, whenever capacity may have appeared.
	// Waiting is therefore event-driven: no polling, and no missed wake-up
	// (a waiter takes the channel under the same lock as its failed attempt).
	changed chan struct{}
	stats   SlotStats
}

type reservation struct{ stop func() bool }

// SlotStats counts how reservations ended. Every reservation ends in exactly
// one of Consumed, Released and Expired.
type SlotStats struct {
	Made, Consumed, Released, Expired int64
}

// NewBotSlots validates cfg and builds the counter.
func NewBotSlots(cfg BotSlotsConfig) (*BotSlots, error) {
	if cfg.Cap < 1 {
		return nil, fmt.Errorf("bot slots: capacity must be at least 1, got %d", cfg.Cap)
	}
	if cfg.InboundReserve < 0 || cfg.InboundReserve >= cfg.Cap {
		return nil, fmt.Errorf("bot slots: inbound reserve must be 0 or more and below the capacity %d, got %d",
			cfg.Cap, cfg.InboundReserve)
	}
	if cfg.ReservationTTL <= 0 {
		cfg.ReservationTTL = DefaultReservationTTL
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	return &BotSlots{
		cfg:      cfg,
		reserved: map[string]*reservation{},
		changed:  make(chan struct{}),
	}, nil
}

// TryAcquire takes a slot for an INVITE that holds no reservation. It fails
// only when in-use plus reserved has reached the capacity.
func (b *BotSlots) TryAcquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse+len(b.reserved) >= b.cfg.Cap {
		return false
	}
	b.inUse++
	obs.BotSlotsChanged(obs.BotSlotInUse, 1)
	return true
}

// Free returns a slot taken by TryAcquire or Consume.
func (b *BotSlots) Free() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse == 0 {
		return
	}
	b.inUse--
	obs.BotSlotsChanged(obs.BotSlotInUse, -1)
	b.wakeLocked()
}

// Consume turns the reservation under key into a slot in use, for the INVITE
// that carries that key. False means there is none (never made, already
// consumed, released or expired); the caller then competes like any inbound.
func (b *BotSlots) Consume(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.reserved[key]
	if !ok {
		return false
	}
	r.stop()
	delete(b.reserved, key)
	b.inUse++
	b.stats.Consumed++
	obs.BotSlotsChanged(obs.BotSlotReserved, -1)
	obs.BotSlotsChanged(obs.BotSlotInUse, 1)
	return true
}

// Release gives back an unconsumed reservation: the customer hung up, or the
// bridge failed. It is a no-op for a key that holds none, so it is safe to
// call after the INVITE has consumed it.
func (b *BotSlots) Release(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.reserved[key]
	if !ok {
		return
	}
	r.stop()
	delete(b.reserved, key)
	b.stats.Released++
	obs.BotSlotsChanged(obs.BotSlotReserved, -1)
	b.wakeLocked()
}

// Reserve holds a slot for key without waiting. A second reservation under a
// key that already holds one succeeds without taking another. It fails when
// in-use plus reserved has reached Cap minus the inbound reserve.
func (b *BotSlots) Reserve(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserveLocked(key)
}

func (b *BotSlots) reserveLocked(key string) bool {
	if _, ok := b.reserved[key]; ok {
		return true
	}
	if b.inUse+len(b.reserved) >= b.cfg.Cap-b.cfg.InboundReserve {
		return false
	}
	r := &reservation{}
	r.stop = b.cfg.Clock.AfterFunc(b.cfg.ReservationTTL, func() { b.expire(key, r) })
	b.reserved[key] = r
	b.stats.Made++
	obs.BotSlotsChanged(obs.BotSlotReserved, 1)
	return true
}

func (b *BotSlots) expire(key string, r *reservation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reserved[key] != r {
		return // consumed or released while the timer was firing
	}
	delete(b.reserved, key)
	b.stats.Expired++
	obs.BotSlotsChanged(obs.BotSlotReserved, -1)
	b.wakeLocked()
}

func (b *BotSlots) wakeLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// ReserveWait reserves a slot under key, waiting up to hold for one to free.
// It wakes when a session ends or a reservation is released or expires, and
// gives up at hold or when ctx is done (the customer hung up). waited is how
// long it was parked; zero when the first attempt succeeded.
func (b *BotSlots) ReserveWait(ctx context.Context, key string, hold time.Duration) (ok bool, waited time.Duration) {
	if b.Reserve(key) {
		return true, 0
	}
	if hold <= 0 {
		return false, 0
	}

	start := b.cfg.Clock.Now()
	timedOut := make(chan struct{})
	stop := b.cfg.Clock.AfterFunc(hold, func() { close(timedOut) })
	defer stop()
	for {
		b.mu.Lock()
		if b.reserveLocked(key) {
			b.mu.Unlock()
			return true, b.cfg.Clock.Now().Sub(start)
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-timedOut:
			return b.Reserve(key), b.cfg.Clock.Now().Sub(start)
		case <-ctx.Done():
			return false, b.cfg.Clock.Now().Sub(start)
		}
	}
}

// InUse is the number of slots held by accepted dialogs.
func (b *BotSlots) InUse() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

// Reserved is the number of reservations awaiting their INVITE.
func (b *BotSlots) Reserved() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reserved)
}

// Stats returns the reservation outcome counters.
func (b *BotSlots) Stats() SlotStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

// Cap is the capacity this counter enforces.
func (b *BotSlots) Cap() int { return b.cfg.Cap }
