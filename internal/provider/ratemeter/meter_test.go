// SPDX-License-Identifier: Apache-2.0

package ratemeter

import (
	"sync"
	"testing"
	"time"
)

// clock is a fake clock the test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)} }

// An event counts for exactly one minute: still at 59.999 s, gone at 60 s.
func TestAnEventCountsForExactlyOneMinute(t *testing.T) {
	c := newClock()
	m := New(c.Now)
	t0 := c.Now()
	m.SessionStarted()
	m.TokensUsed(1000)
	c.advance(10 * time.Second)
	m.SessionStarted()
	m.TokensUsed(500)

	c.advance(50*time.Second - time.Millisecond) // t0 + 59.999 s
	r := m.Read()
	if r.Starts != 2 || r.Tokens != 1500 {
		t.Fatalf("at 59.999 s: %d starts, %d tokens, want 2 and 1500", r.Starts, r.Tokens)
	}
	if !r.StartsDropAt.Equal(t0.Add(Span)) || !r.TokensDropAt.Equal(t0.Add(Span)) {
		t.Errorf("drop times %v / %v, want the first entry's minute, %v", r.StartsDropAt, r.TokensDropAt, t0.Add(Span))
	}

	c.advance(time.Millisecond) // t0 + 60 s
	r = m.Read()
	if r.Starts != 1 || r.Tokens != 500 {
		t.Fatalf("at 60 s: %d starts, %d tokens, want 1 and 500", r.Starts, r.Tokens)
	}
	if want := t0.Add(10 * time.Second).Add(Span); !r.StartsDropAt.Equal(want) {
		t.Errorf("drop time %v, want %v", r.StartsDropAt, want)
	}

	c.advance(10 * time.Second)
	r = m.Read()
	if r.Starts != 0 || r.Tokens != 0 || !r.StartsDropAt.IsZero() || !r.TokensDropAt.IsZero() {
		t.Errorf("an empty window reads %+v", r)
	}
}

// The latest refusal is the one kept, and it is not part of any window: the
// reader decides how long it holds.
func TestTheLatestQuotaRefusalIsKept(t *testing.T) {
	c := newClock()
	m := New(c.Now)
	if r := m.Read(); !r.LastQuotaRefusal.IsZero() {
		t.Fatalf("a fresh meter has a refusal at %v", r.LastQuotaRefusal)
	}
	m.QuotaRefused()
	first := c.Now()
	c.advance(5 * time.Minute)
	if r := m.Read(); !r.LastQuotaRefusal.Equal(first) {
		t.Errorf("refusal at %v, want %v", r.LastQuotaRefusal, first)
	}
	m.QuotaRefused()
	if r := m.Read(); !r.LastQuotaRefusal.Equal(c.Now()) {
		t.Errorf("refusal at %v, want the latest, %v", r.LastQuotaRefusal, c.Now())
	}
}

// Nothing that is not a cost is counted, and a nil meter is a meter that
// records nothing.
func TestNothingAndNilCountNothing(t *testing.T) {
	m := New(newClock().Now)
	m.TokensUsed(0)
	m.TokensUsed(-3)
	if r := m.Read(); r.Tokens != 0 || !r.TokensDropAt.IsZero() {
		t.Errorf("zero and negative costs read as %+v", r)
	}
	var none *Meter
	none.SessionStarted()
	none.TokensUsed(10)
	none.QuotaRefused()
	if r := none.Read(); r.Starts != 0 || r.Tokens != 0 || !r.LastQuotaRefusal.IsZero() {
		t.Errorf("a nil meter reads %+v", r)
	}
}

// A long run keeps only the last minute in memory.
func TestTheWindowDoesNotGrowWithTime(t *testing.T) {
	c := newClock()
	m := New(c.Now)
	for i := 0; i < 10000; i++ {
		m.SessionStarted()
		m.TokensUsed(1)
		c.advance(100 * time.Millisecond)
	}
	// Ten a second; the one made exactly a minute ago has just left.
	r := m.Read()
	if r.Starts != 599 || r.Tokens != 599 {
		t.Errorf("%d starts, %d tokens in the last minute, want 599 each", r.Starts, r.Tokens)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.starts.at); n > 1300 {
		t.Errorf("%d start entries are held for a minute of 600", n)
	}
}

// Sessions feed it from their own goroutines while the dialer reads.
func TestConcurrentUseIsSafe(t *testing.T) {
	m := New(nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				m.SessionStarted()
				m.TokensUsed(2)
				if i%100 == 0 {
					m.QuotaRefused()
				}
				_ = m.Read()
			}
		}()
	}
	wg.Wait()
	if r := m.Read(); r.Starts != 4000 || r.Tokens != 8000 || r.LastQuotaRefusal.IsZero() {
		t.Errorf("after 8 x 500: %+v", r)
	}
}
