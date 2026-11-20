// SPDX-License-Identifier: Apache-2.0

// Package ratemeter is the process's one account of how fast it is spending
// the speech provider's quota (#604).
//
// The vendors here limit a rate, not a population: session starts per minute
// and tokens per minute (docs/design/06-capacity.md section 4). The meter keeps
// the last minute of both, for every session this process opens, inbound and
// outbound alike, and the time the provider last said the quota was spent. The
// campaign dialer reads it before each admission (docs/design/10-campaigns.md
// section 6.3); nothing else decides on it.
//
// It imports nothing of the provider clients and they import nothing of the
// dialer: the clients feed it through provider.QuotaMeter, the composition root
// counts starts, and the dialer reads a Reading.
package ratemeter

import (
	"sync"
	"time"
)

// Span is the window every count covers: the vendors state their limits per
// minute. An event at t counts while now < t+Span.
const Span = time.Minute

// Reading is the meter at one moment.
type Reading struct {
	// At is the moment, on the meter's clock.
	At time.Time
	// Starts is the session starts in the last Span, counted before the dial.
	Starts int
	// Tokens is the tokens, in and out together, that responses reported
	// in the last Span.
	Tokens int64
	// LastQuotaRefusal is when the provider last said the quota was spent:
	// a refused start, or an error mid-call. Zero when it never has.
	LastQuotaRefusal time.Time
	// StartsDropAt and TokensDropAt are when the oldest entry still counted
	// leaves the window, which is the next time that count can fall. Zero
	// when there is none.
	StartsDropAt time.Time
	TokensDropAt time.Time
}

// Meter is safe for concurrent use; its zero value is not usable, New builds
// one. A nil *Meter records nothing and reads empty, so a process without one
// needs no guard at its call sites.
type Meter struct {
	now func() time.Time

	mu      sync.Mutex
	starts  window[struct{}]
	tokens  window[int64]
	tokenIn int64 // the sum of tokens's values
	refused time.Time
}

// New builds a meter on a clock; nil is time.Now. Tests pass a fake one.
func New(now func() time.Time) *Meter {
	if now == nil {
		now = time.Now
	}
	return &Meter{now: now}
}

// SessionStarted counts one session start, at the moment it is attempted.
func (m *Meter) SessionStarted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.expireLocked(now)
	m.starts.push(now, struct{}{})
}

// TokensUsed counts what one response cost. It satisfies provider.QuotaMeter.
func (m *Meter) TokensUsed(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.expireLocked(now)
	m.tokens.push(now, int64(n))
	m.tokenIn += int64(n)
}

// QuotaRefused notes that the provider said its quota is spent, now. It
// satisfies provider.QuotaMeter.
func (m *Meter) QuotaRefused() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if now := m.now(); now.After(m.refused) {
		m.refused = now
	}
}

// Read is the meter now.
func (m *Meter) Read() Reading {
	if m == nil {
		return Reading{At: time.Now()}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.expireLocked(now)
	r := Reading{At: now, Starts: m.starts.len(), Tokens: m.tokenIn, LastQuotaRefusal: m.refused}
	if at, ok := m.starts.oldest(); ok {
		r.StartsDropAt = at.Add(Span)
	}
	if at, ok := m.tokens.oldest(); ok {
		r.TokensDropAt = at.Add(Span)
	}
	return r
}

func (m *Meter) expireLocked(now time.Time) {
	m.starts.expire(now, nil)
	m.tokens.expire(now, func(n int64) { m.tokenIn -= n })
}

// window is a queue of timestamped entries, oldest first. Entries are pushed
// in clock order; one pushed out of order (a clock that stepped back) is kept
// at the end and expires no earlier than those before it.
type window[T any] struct {
	at   []time.Time
	vals []T
	head int
}

func (w *window[T]) push(at time.Time, v T) {
	w.at = append(w.at, at)
	w.vals = append(w.vals, v)
}

func (w *window[T]) len() int { return len(w.at) - w.head }

func (w *window[T]) oldest() (time.Time, bool) {
	if w.len() == 0 {
		return time.Time{}, false
	}
	return w.at[w.head], true
}

// expire drops the entries that have left the window at now, handing each
// value to gone, and compacts the backing arrays once most of them is spent.
func (w *window[T]) expire(now time.Time, gone func(T)) {
	for w.head < len(w.at) && !now.Before(w.at[w.head].Add(Span)) {
		if gone != nil {
			gone(w.vals[w.head])
		}
		w.head++
	}
	if w.head > 0 && w.head*2 >= len(w.at) {
		n := copy(w.at, w.at[w.head:])
		copy(w.vals, w.vals[w.head:])
		clear(w.vals[n:])
		w.at, w.vals, w.head = w.at[:n], w.vals[:n], 0
	}
}
