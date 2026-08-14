// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"sync"
	"time"
)

// Limiter is a token bucket pacing originates towards the switch.
//
// The switch enforces its own sessions-per-second cap by *rejecting* work
// above it; this bucket keeps us just under that cliff by *delaying* work
// instead, so a burst of API calls becomes a fast queue rather than a pile
// of failed originates.
type Limiter struct {
	mu       sync.Mutex
	rate     float64 // tokens per second
	burst    float64
	tokens   float64
	lastFill time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// NewLimiter builds a bucket allowing rate originates per second, with the
// same amount of burst headroom.
func NewLimiter(rate int) *Limiter {
	if rate <= 0 {
		rate = 100
	}
	l := &Limiter{
		rate:  float64(rate),
		burst: float64(rate),
		now:   time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
	l.tokens = l.burst
	l.lastFill = l.now()
	return l
}

// Take blocks until an originate may proceed, or the context ends.
func (l *Limiter) Take(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := l.now()
		l.tokens += now.Sub(l.lastFill).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.lastFill = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}
