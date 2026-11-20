// SPDX-License-Identifier: Apache-2.0

package ratemeter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// clock is a fake clock the test moves by hand. Its timers (AfterFunc) run on
// the goroutine that moves it, each at its own time, in order.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at        time.Time
	f         func()
	isStopped bool
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.isStopped
		t.isStopped = true
		return was
	}
}

// advance moves the clock on by d, running every timer that falls due on the
// way at its own moment.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.isStopped && !t.at.After(end) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		next.isStopped = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
		c.mu.Lock()
	}
	c.now = end
	keep := c.timers[:0]
	for _, t := range c.timers {
		if !t.isStopped {
			keep = append(keep, t)
		}
	}
	c.timers = keep
	c.mu.Unlock()
}

// set puts the clock at now, which may be earlier: a clock that steps back.
func (c *clock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// pending is the timers set and not yet run or stopped.
func (c *clock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.isStopped {
			n++
		}
	}
	return n
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)} }

// An event counts for exactly one minute when no period is configured: still
// at 59.999 s, gone at 60 s.
func TestAnEventCountsForExactlyOneMinute(t *testing.T) {
	c := newClock()
	m := New(c.Now, Config{})
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

// Each count keeps its own period (#644): tokens over the token period, here
// 12 s, starts over the window admission judges them in, the session period
// or ten seconds when it is shorter, here 1 s. An entry leaves at its
// period's end, and the drop times follow each period.
func TestEachCountKeepsItsOwnPeriod(t *testing.T) {
	c := newClock()
	m := New(c.Now, Config{SessionPeriod: time.Second, TokenPeriod: 12 * time.Second})
	t0 := c.Now()
	m.SessionStarted()
	m.TokensUsed(40000)
	c.advance(5 * time.Second)
	m.SessionStarted()
	m.TokensUsed(15000)

	r := m.Read()
	if r.Starts != 2 || r.Tokens != 55000 {
		t.Fatalf("at 5 s: %d starts, %d tokens, want 2 and 55000", r.Starts, r.Tokens)
	}
	if want := t0.Add(MinAdmissionWindow); !r.StartsDropAt.Equal(want) {
		t.Errorf("starts drop at %v, want the admission window's end, %v", r.StartsDropAt, want)
	}
	if want := t0.Add(12 * time.Second); !r.TokensDropAt.Equal(want) {
		t.Errorf("tokens drop at %v, want the token period's end, %v", r.TokensDropAt, want)
	}

	c.advance(5*time.Second - time.Millisecond) // t0 + 9.999 s
	if r := m.Read(); r.Starts != 2 {
		t.Errorf("at 9.999 s: %d starts, want 2", r.Starts)
	}
	c.advance(time.Millisecond) // t0 + 10 s: the first start has left
	if r := m.Read(); r.Starts != 1 || r.Tokens != 55000 {
		t.Errorf("at 10 s: %d starts, %d tokens, want 1 and 55000", r.Starts, r.Tokens)
	}
	c.advance(2*time.Second - time.Millisecond) // t0 + 11.999 s
	if r := m.Read(); r.Tokens != 55000 {
		t.Errorf("at 11.999 s: %d tokens, want 55000", r.Tokens)
	}
	c.advance(time.Millisecond) // t0 + 12 s: the first tokens have left
	r = m.Read()
	if r.Tokens != 15000 || !r.TokensDropAt.Equal(t0.Add(17*time.Second)) {
		t.Errorf("at 12 s: %d tokens dropping at %v, want 15000 at %v", r.Tokens, r.TokensDropAt, t0.Add(17*time.Second))
	}
	c.advance(5 * time.Second) // t0 + 17 s
	if r := m.Read(); r.Starts != 0 || r.Tokens != 0 {
		t.Errorf("at 17 s: %+v, want both windows empty", r)
	}
}

// A period longer than the admission window's minimum is the window itself,
// and a period of a minute is a minute: the RPM form's.
func TestTheAdmissionWindow(t *testing.T) {
	for _, tt := range []struct{ period, want time.Duration }{
		{time.Second, MinAdmissionWindow},
		{12 * time.Second, 12 * time.Second},
		{time.Minute, time.Minute},
		{time.Hour, time.Hour},
	} {
		if got := AdmissionWindow(tt.period); got != tt.want {
			t.Errorf("AdmissionWindow(%s) = %s, want %s", tt.period, got, tt.want)
		}
	}
}

// A clock that steps back neither loses what was counted nor lets an entry
// out early: one pushed out of order is kept behind the later ones and leaves
// no earlier than they do.
func TestAClockThatStepsBack(t *testing.T) {
	c := newClock()
	m := New(c.Now, Config{TokenPeriod: 12 * time.Second})
	t0 := c.Now()
	c.advance(10 * time.Second)
	m.TokensUsed(100) // at t0+10 s
	c.set(t0.Add(4 * time.Second))
	m.TokensUsed(200) // at t0+4 s, after it
	if r := m.Read(); r.Tokens != 300 || !r.TokensDropAt.Equal(t0.Add(22*time.Second)) {
		t.Fatalf("after the step back: %d tokens dropping at %v, want 300 at %v", r.Tokens, r.TokensDropAt, t0.Add(22*time.Second))
	}
	c.set(t0.Add(16 * time.Second)) // the later entry's own end has passed, not the first's
	if r := m.Read(); r.Tokens != 300 {
		t.Errorf("at 16 s: %d tokens, want 300: an entry behind a later one waits for it", r.Tokens)
	}
	c.set(t0.Add(22 * time.Second))
	if r := m.Read(); r.Tokens != 0 {
		t.Errorf("at 22 s: %d tokens, want 0", r.Tokens)
	}
}

// The latest refusal is the one kept, and it is not part of any window: the
// reader decides how long it holds.
func TestTheLatestQuotaRefusalIsKept(t *testing.T) {
	c := newClock()
	m := New(c.Now, Config{})
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
// records nothing and never waits.
func TestNothingAndNilCountNothing(t *testing.T) {
	m := New(newClock().Now, Config{})
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
	if s, err := none.AwaitStart(context.Background(), time.Hour); err != nil || s != (Start{}) {
		t.Errorf("a nil meter's start = %+v, %v; want made at once", s, err)
	}
}

// A long run keeps only the window in memory.
func TestTheWindowDoesNotGrowWithTime(t *testing.T) {
	c := newClock()
	m := New(c.Now, Config{})
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

// Sessions feed it from their own goroutines while the dialer reads, and
// campaign calls wait in the gate meanwhile.
func TestConcurrentUseIsSafe(t *testing.T) {
	m := New(nil, Config{SessionLimit: 1000, SessionPeriod: time.Minute})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if i%2 == 0 {
					m.SessionStarted()
				} else if _, err := m.AwaitStart(context.Background(), time.Millisecond); err != nil {
					t.Error(err)
				}
				m.TokensUsed(2)
				if i%100 == 0 {
					m.QuotaRefused()
				}
				_ = m.Read()
			}
		}()
	}
	wg.Wait()
	if r := m.Read(); r.Starts != 4000 || r.Tokens != 8000 || r.LastQuotaRefusal.IsZero() || r.StartsWaiting != 0 {
		t.Errorf("after 8 x 500: %+v", r)
	}
}

// gated is a meter with the start gate of #644 at 3 starts per 1 s, on a fake
// clock, and a record of the starts made: who, when and how.
type gated struct {
	c    *clock
	m    *Meter
	t0   time.Time
	made []madeStart
}

type madeStart struct {
	who   int
	at    time.Time
	start Start
}

func newGated(limit int, period time.Duration) *gated {
	c := newClock()
	return &gated{c: c, t0: c.Now(),
		m: New(c.Now, Config{SessionLimit: limit, SessionPeriod: period, AfterFunc: c.AfterFunc})}
}

// pace asks for paced start who, recorded when it is made.
func (g *gated) pace(who int, maxWait time.Duration) func() bool {
	return g.m.Pace(maxWait, func(s Start) {
		g.made = append(g.made, madeStart{who: who, at: g.c.Now(), start: s})
	})
}

func (g *gated) order() []int {
	out := make([]int, len(g.made))
	for i, s := range g.made {
		out[i] = s.who
	}
	return out
}

// countIn is the starts made in [from, from+period).
func countIn(made []madeStart, from time.Time, period time.Duration) int {
	n := 0
	for _, s := range made {
		if !s.at.Before(from) && s.at.Before(from.Add(period)) {
			n++
		}
	}
	return n
}

// Ten campaign calls answered in the same instant open at most three
// sessions in any one second, in the order they asked, each waiting for the
// oldest start to leave the window.
func TestABurstOfStartsIsPacedByTheWindowInOrder(t *testing.T) {
	g := newGated(3, time.Second)
	for i := 0; i < 10; i++ {
		g.pace(i, 5*time.Second)
	}
	if len(g.made) != 3 {
		t.Fatalf("%d made at once, want the window's 3", len(g.made))
	}
	if r := g.m.Read(); r.StartsWaiting != 7 || r.Starts != 3 {
		t.Errorf("reading %+v, want 3 starts and 7 waiting", r)
	}
	g.c.advance(5 * time.Second)

	want := []time.Duration{0, 0, 0, time.Second, time.Second, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 3 * time.Second}
	if len(g.made) != 10 {
		t.Fatalf("%d of 10 made", len(g.made))
	}
	for i, s := range g.made {
		if s.who != i {
			t.Fatalf("made in the order %v, want the order asked", g.order())
		}
		if !s.at.Equal(g.t0.Add(want[i])) || s.start.Waited != want[i] || s.start.IsPastBound {
			t.Errorf("start %d made at +%s having waited %s (past the bound %v), want +%s",
				i, s.at.Sub(g.t0), s.start.Waited, s.start.IsPastBound, want[i])
		}
	}
	for _, s := range g.made {
		if n := countIn(g.made, s.at, time.Second); n > 3 {
			t.Errorf("%d starts in the second from +%s", n, s.at.Sub(g.t0))
		}
	}
	if r := g.m.Read(); r.StartsWaiting != 0 || r.Starts != 10 {
		t.Errorf("after the burst: %+v, want 10 starts and none waiting", r)
	}
	if n := g.c.pending(); n != 0 {
		t.Errorf("%d timers left with nothing waiting", n)
	}
}

// The edge of the period: a start waiting for the oldest is still held at
// 0.999 s and made at 1 s.
func TestAStartIsMadeTheMomentTheOldestLeaves(t *testing.T) {
	g := newGated(1, time.Second)
	g.pace(0, time.Minute)
	g.pace(1, time.Minute)
	g.c.advance(time.Second - time.Millisecond)
	if len(g.made) != 1 {
		t.Fatalf("at 0.999 s %d made, want 1", len(g.made))
	}
	g.c.advance(time.Millisecond)
	if len(g.made) != 2 || !g.made[1].at.Equal(g.t0.Add(time.Second)) {
		t.Fatalf("at 1 s: %+v", g.made)
	}
}

// A call that ends while its start waits gives up its place: no start is
// counted for it, it is never made, and the ones behind it move up.
func TestAWithdrawnStartCountsNothingAndGivesUpItsPlace(t *testing.T) {
	g := newGated(3, time.Second)
	withdraw := make([]func() bool, 6)
	for i := range withdraw {
		withdraw[i] = g.pace(i, 5*time.Second)
	}
	if !withdraw[3]() {
		t.Fatal("a waiting start could not be withdrawn")
	}
	if withdraw[3]() {
		t.Error("a start was withdrawn twice")
	}
	if withdraw[0]() {
		t.Error("a start already made was withdrawn")
	}
	if r := g.m.Read(); r.StartsWaiting != 2 || r.Starts != 3 {
		t.Errorf("after the withdrawal: %+v, want 3 starts and 2 waiting", r)
	}
	g.c.advance(5 * time.Second)
	if got := g.order(); len(got) != 5 || got[3] != 4 || got[4] != 5 {
		t.Fatalf("made %v, want 0 1 2 4 5", got)
	}
	if !g.made[3].at.Equal(g.t0.Add(time.Second)) || !g.made[4].at.Equal(g.t0.Add(time.Second)) {
		t.Errorf("4 and 5 made at +%s and +%s, want both at +1s, in 3's place",
			g.made[3].at.Sub(g.t0), g.made[4].at.Sub(g.t0))
	}
	if r := g.m.Read(); r.Starts != 5 {
		t.Errorf("%d starts counted, want 5: the withdrawn one counts nothing", r.Starts)
	}
}

// The wait is bounded: past its bound a start is made with the window still
// full, as it was before the gate, and says so.
func TestABoundedWaitEndsAndTheStartIsMade(t *testing.T) {
	g := newGated(1, time.Minute)
	g.pace(0, StartWaitBound)
	g.pace(1, StartWaitBound)
	g.c.advance(StartWaitBound - time.Millisecond)
	if len(g.made) != 1 {
		t.Fatalf("%d made before the bound, want 1", len(g.made))
	}
	g.c.advance(time.Millisecond)
	if len(g.made) != 2 {
		t.Fatalf("%d made at the bound, want 2", len(g.made))
	}
	if s := g.made[1].start; s.Waited != StartWaitBound || !s.IsPastBound {
		t.Errorf("the second start = %+v, want made past the bound after %s", s, StartWaitBound)
	}
	if r := g.m.Read(); r.Starts != 2 || r.StartsWaiting != 0 {
		t.Errorf("reading %+v, want both counted", r)
	}
}

// Inbound starts are counted at once whatever waits, and the waiting starts
// then find the window that much fuller.
func TestInboundStartsNeverWaitAndAreCounted(t *testing.T) {
	g := newGated(3, time.Second)
	for i := 0; i < 5; i++ {
		g.pace(i, 5*time.Second)
	}
	g.c.advance(500 * time.Millisecond)
	g.m.SessionStarted() // an inbound call at +0.5 s, with two waiting
	if r := g.m.Read(); r.Starts != 4 || r.StartsWaiting != 2 {
		t.Fatalf("after the inbound start: %+v, want 4 starts, 2 still waiting", r)
	}
	g.c.advance(5 * time.Second)
	// At +1 s the three of +0 s leave but the inbound one is still in the
	// window: two more fit, and nothing is left waiting.
	if len(g.made) != 5 || !g.made[3].at.Equal(g.t0.Add(time.Second)) || !g.made[4].at.Equal(g.t0.Add(time.Second)) {
		t.Fatalf("made %+v", g.made)
	}

	g = newGated(3, time.Second)
	for i := 0; i < 4; i++ {
		g.pace(i, 5*time.Second)
	}
	g.m.SessionStarted() // inbound, with the window full and one waiting
	g.c.advance(time.Second)
	// At +1 s all four of +0 s leave: the waiting one is made.
	if len(g.made) != 4 || !g.made[3].at.Equal(g.t0.Add(time.Second)) {
		t.Fatalf("made %+v", g.made)
	}
}

// No limit, a limit of 0 or no bound: nothing ever waits.
func TestNoLimitNeverWaits(t *testing.T) {
	for _, g := range []*gated{newGated(0, time.Second), newGated(0, 0)} {
		for i := 0; i < 10; i++ {
			g.pace(i, time.Minute)
		}
		if len(g.made) != 10 || g.c.pending() != 0 {
			t.Errorf("%d of 10 made at once with no limit, %d timers set", len(g.made), g.c.pending())
		}
		for _, s := range g.made {
			if s.start != (Start{}) {
				t.Errorf("a start with no limit = %+v", s.start)
			}
		}
	}
	g := newGated(1, time.Second)
	g.pace(0, 0)
	g.pace(1, 0)
	if len(g.made) != 2 || !g.made[1].start.IsPastBound {
		t.Errorf("with no bound: %+v, want both made at once, the second past its bound", g.made)
	}
}

// AwaitStart, as a call uses it: it returns when the start is made, and when
// the call ends first it returns the call's error with nothing counted.
func TestAwaitStartWaitsAndLeavesWithTheCall(t *testing.T) {
	g := newGated(1, time.Second)
	g.m.SessionStarted() // the window is full

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		start Start
		err   error
	}
	ended := make(chan result, 1)
	go func() {
		s, err := g.m.AwaitStart(ctx, 5*time.Second)
		ended <- result{s, err}
	}()
	waitFor(t, func() bool { return g.m.Read().StartsWaiting == 1 })
	cancel()
	r := <-ended
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("AwaitStart = %+v, %v; want the call's error", r.start, r.err)
	}
	if rd := g.m.Read(); rd.Starts != 1 || rd.StartsWaiting != 0 {
		t.Errorf("after the call ended: %+v, want only the first start counted", rd)
	}

	made := make(chan result, 1)
	go func() {
		s, err := g.m.AwaitStart(context.Background(), 5*time.Second)
		made <- result{s, err}
	}()
	waitFor(t, func() bool { return g.m.Read().StartsWaiting == 1 })
	g.c.advance(time.Second)
	r = <-made
	if r.err != nil || r.start.Waited != time.Second || r.start.IsPastBound {
		t.Errorf("AwaitStart = %+v, %v; want made after 1s", r.start, r.err)
	}
	if rd := g.m.Read(); rd.Starts != 2 || rd.StartsWaiting != 0 {
		t.Errorf("reading %+v, want the first start and the one just made", rd)
	}
}

// waitFor waits for a condition another goroutine makes true.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 10 s")
		}
		time.Sleep(time.Millisecond)
	}
}
