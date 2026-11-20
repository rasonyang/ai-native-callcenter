// SPDX-License-Identifier: Apache-2.0

// Package ratemeter is the process's one account of how fast it is spending
// the speech provider's quota (#604, #644).
//
// The vendors here limit a rate, not a population: session starts, and tokens,
// each a count over a period the vendor chooses (docs/design/06-capacity.md
// section 4). The meter keeps both, for every session this process opens,
// inbound and outbound alike, and the time the provider last said the quota
// was spent. The campaign dialer reads it before each admission
// (docs/design/10-campaigns.md section 6.3), and a campaign call's session
// start waits on it for room in the session window (Pace, AwaitStart);
// nothing else decides on it.
//
// It imports nothing of the provider clients and they import nothing of the
// dialer: the clients feed it through provider.QuotaMeter, the orchestrator
// counts starts, and the dialer reads a Reading.
package ratemeter

import (
	"context"
	"sync"
	"time"
)

// Span is the period a count covers when none is configured: the vendors'
// documents state their limits per minute, and so do the RPM settings. An
// event at t counts while now < t+period.
const Span = time.Minute

// StartWaitBound is the longest a campaign call's session start waits for
// room in the session window (#644). The callee has answered and hears silence
// until the bot speaks, so the wait is short; but a start the provider refuses
// costs the whole call (a campaign call is hung up, BOT_FAILED), so a short
// wait is the better of the two. Two seconds lets a one-second window clear
// twice, and it is no longer than the silence a call that listens first
// already leaves the callee after the answer (callerFirstWait in
// internal/aicall, #594), which counts from the answer too. Past it the start
// is made as it was before the gate, and the provider decides.
const StartWaitBound = 2 * time.Second

// MinAdmissionWindow is the shortest window campaign admission judges the
// session limit over (AdmissionWindow). Admission counts each ringing leg's
// expected answer as a start to come, and the answers of the legs ringing at
// one moment land over the following seconds, as callees pick up; the start
// gate then holds a start up to StartWaitBound. Judged over a shorter period
// than that, a limit would have to hold all of them at once, and admission
// would place almost nothing (simulated at 3 per 1 s: 5 starts a minute);
// judged over a much longer one, admission places a wave of answers that one
// period cannot take even with the gate (simulated over a minute: 30% of
// starts refused). Ten seconds refused none in the simulation of
// internal/dialer (TestTheStartGateKeepsRefusalsNearZeroOverShortPeriods).
const MinAdmissionWindow = 10 * time.Second

// AdmissionWindow is the window campaign admission judges the session limit
// over (docs/design/10-campaigns.md section 6.3), with the limit scaled to
// it: the limit's own period, or MinAdmissionWindow when the period is
// shorter. A per-minute limit, the vendors' documents and the RPM settings, is
// judged over its minute as it always was. The start gate (Pace) keeps every
// shorter period inside the limit.
func AdmissionWindow(period time.Duration) time.Duration {
	return max(period, MinAdmissionWindow)
}

// Config is what a meter counts over, and the limit its start gate keeps.
type Config struct {
	// SessionPeriod and TokenPeriod are the periods the provider states its
	// limits over (AICC_PROVIDER_SESSION_PERIOD, AICC_PROVIDER_TOKEN_PERIOD);
	// zero is Span.
	SessionPeriod, TokenPeriod time.Duration
	// SessionLimit is the session starts the provider allows in a
	// SessionPeriod, which Pace keeps; 0 is no limit, and nothing waits.
	SessionLimit int
	// AfterFunc runs f once d has passed and returns what stops it, as
	// time.AfterFunc does; nil is the real clock. Tests pass one driven by the
	// same fake clock as now.
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
}

// Reading is the meter at one moment.
type Reading struct {
	// At is the moment, on the meter's clock.
	At time.Time
	// Starts is the session starts, counted when attempted, in the last
	// AdmissionWindow of the session period.
	Starts int
	// Tokens is the tokens, in and out together, that responses reported
	// in the last token period.
	Tokens int64
	// LastQuotaRefusal is when the provider last said the quota was spent:
	// a refused start, or an error mid-call. Zero when it never has.
	LastQuotaRefusal time.Time
	// StartsDropAt and TokensDropAt are when the oldest entry still counted
	// leaves its window, which is the next time that count can fall. Zero
	// when there is none.
	StartsDropAt time.Time
	TokensDropAt time.Time
	// StartsWaiting is the campaign session starts waiting in the start gate
	// for room in the session window (Pace): answered calls about to start.
	StartsWaiting int
}

// Start is how one paced session start went (Pace).
type Start struct {
	// Waited is how long it waited for room in the session window.
	Waited time.Duration
	// IsPastBound says it waited its whole bound and was made with the
	// window still full.
	IsPastBound bool
}

// Meter is safe for concurrent use; its zero value is not usable, New builds
// one. A nil *Meter records nothing, reads empty and never waits, so a process
// without one needs no guard at its call sites.
type Meter struct {
	now           func() time.Time
	afterFunc     func(time.Duration, func()) func() bool
	sessionPeriod time.Duration
	sessionLimit  int

	mu      sync.Mutex
	starts  window[struct{}] // over AdmissionWindow(sessionPeriod)
	tokens  window[int64]    // over the token period
	tokenIn int64            // the sum of tokens's values
	refused time.Time
	// queue is the paced starts waiting for room, oldest first, and stopWake
	// stops the timer that looks at it next; nil when none is set.
	queue    []*waiter
	stopWake func() bool
}

// waiter is one paced start in the queue.
type waiter struct {
	since, deadline time.Time
	start           func(Start)
	made            Start
}

// New builds a meter on a clock; nil is time.Now. Tests pass a fake one.
func New(now func() time.Time, cfg Config) *Meter {
	if now == nil {
		now = time.Now
	}
	if cfg.SessionPeriod <= 0 {
		cfg.SessionPeriod = Span
	}
	if cfg.TokenPeriod <= 0 {
		cfg.TokenPeriod = Span
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	return &Meter{
		now: now, afterFunc: cfg.AfterFunc,
		sessionPeriod: cfg.SessionPeriod, sessionLimit: max(cfg.SessionLimit, 0),
		starts: window[struct{}]{period: AdmissionWindow(cfg.SessionPeriod)},
		tokens: window[int64]{period: cfg.TokenPeriod},
	}
}

// SessionStarted counts one session start, at the moment it is attempted. It
// never waits: an inbound or internal call's start, which nothing holds
// (#644).
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

// Pace asks for one session start that waits for room in the session window
// (#644): a campaign call's. The start is counted, and start runs, once the
// window holds fewer than the limit and every paced start asked for before it
// has been made, or once maxWait has passed. start runs on the goroutine that
// made the start, outside the meter's lock: this one when there is room now,
// else the meter's timer or another caller's. With no limit, or maxWait 0, it
// is made at once.
//
// withdraw takes back a start not made yet, which then counts nothing and
// never runs start; it reports whether it did. A start already made is not
// taken back: it was counted, and its start runs or has run.
func (m *Meter) Pace(maxWait time.Duration, start func(Start)) (withdraw func() bool) {
	if m == nil {
		start(Start{})
		return func() bool { return false }
	}
	m.mu.Lock()
	now := m.now()
	w := &waiter{since: now, deadline: now.Add(max(maxWait, 0)), start: start}
	m.queue = append(m.queue, w)
	made := m.pumpLocked(now)
	m.mu.Unlock()
	runStarts(made)
	return func() bool { return m.withdraw(w) }
}

// AwaitStart is Pace for a caller that can wait: it returns once the start
// is made, or with ctx's error when ctx ends first, in which case no start was
// counted. Waited is set either way.
func (m *Meter) AwaitStart(ctx context.Context, maxWait time.Duration) (Start, error) {
	made := make(chan Start, 1)
	since := time.Time{}
	if m != nil {
		since = m.now()
	}
	withdraw := m.Pace(maxWait, func(s Start) { made <- s })
	select {
	case s := <-made:
		return s, nil
	case <-ctx.Done():
		if withdraw() {
			return Start{Waited: m.now().Sub(since)}, ctx.Err()
		}
		// Made meanwhile: it was counted, so it goes ahead.
		return <-made, nil
	}
}

func (m *Meter) withdraw(w *waiter) bool {
	m.mu.Lock()
	i := 0
	for i < len(m.queue) && m.queue[i] != w {
		i++
	}
	if i == len(m.queue) {
		m.mu.Unlock()
		return false
	}
	copy(m.queue[i:], m.queue[i+1:])
	m.queue[len(m.queue)-1] = nil
	m.queue = m.queue[:len(m.queue)-1]
	// The head may have changed: the next one's bound may already be past,
	// and the timer is set for the old head.
	made := m.pumpLocked(m.now())
	m.mu.Unlock()
	runStarts(made)
	return true
}

// wake is the timer's: the window may have room, or the head's bound passed.
func (m *Meter) wake() {
	m.mu.Lock()
	made := m.pumpLocked(m.now())
	m.mu.Unlock()
	runStarts(made)
}

// pumpLocked makes the paced starts at the head of the queue that may go now,
// in order, counting each, and sets the timer for the next moment one may.
// It returns them for their start to run once the lock is released.
func (m *Meter) pumpLocked(now time.Time) []*waiter {
	m.expireLocked(now)
	var made []*waiter
	inPeriod := 0
	if m.sessionLimit > 0 {
		inPeriod, _ = m.inSessionPeriodLocked(now)
	}
	for len(m.queue) > 0 {
		w := m.queue[0]
		isFull := m.sessionLimit > 0 && inPeriod >= m.sessionLimit
		if isFull && now.Before(w.deadline) {
			break
		}
		m.queue[0] = nil
		m.queue = m.queue[1:]
		m.starts.push(now, struct{}{})
		inPeriod++
		w.made = Start{Waited: now.Sub(w.since), IsPastBound: isFull}
		made = append(made, w)
	}
	if len(m.queue) == 0 {
		m.queue = nil
	}
	m.armLocked(now)
	return made
}

// armLocked sets the timer for the head of the queue: when the oldest start
// in the session period leaves it, or the head's bound passes, whichever is
// first. A queue that is not empty after pumpLocked is waiting on a full
// window, so both moments are after now.
func (m *Meter) armLocked(now time.Time) {
	if m.stopWake != nil {
		m.stopWake()
		m.stopWake = nil
	}
	if len(m.queue) == 0 {
		return
	}
	at := m.queue[0].deadline
	if _, dropAt := m.inSessionPeriodLocked(now); !dropAt.IsZero() && dropAt.Before(at) {
		at = dropAt
	}
	m.stopWake = m.afterFunc(at.Sub(now), m.wake)
}

func runStarts(made []*waiter) {
	for _, w := range made {
		w.start(w.made)
	}
}

// inSessionPeriodLocked counts the starts of the last session period, which
// may be shorter than the window they are kept in, and says when the first of
// them leaves it. Every entry is looked at, not the newest only, because one
// pushed after the clock stepped back can sit behind a later one.
func (m *Meter) inSessionPeriodLocked(now time.Time) (n int, dropAt time.Time) {
	for _, at := range m.starts.at[m.starts.head:] {
		if until := at.Add(m.sessionPeriod); now.Before(until) {
			n++
			if dropAt.IsZero() || until.Before(dropAt) {
				dropAt = until
			}
		}
	}
	return n, dropAt
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
	r := Reading{At: now, Starts: m.starts.len(), Tokens: m.tokenIn, LastQuotaRefusal: m.refused,
		StartsWaiting: len(m.queue)}
	if at, ok := m.starts.oldest(); ok {
		r.StartsDropAt = at.Add(m.starts.period)
	}
	if at, ok := m.tokens.oldest(); ok {
		r.TokensDropAt = at.Add(m.tokens.period)
	}
	return r
}

func (m *Meter) expireLocked(now time.Time) {
	m.starts.expire(now, nil)
	m.tokens.expire(now, func(n int64) { m.tokenIn -= n })
}

// window is a queue of timestamped entries, oldest first, each counted for
// period. Entries are pushed in clock order; one pushed out of order (a clock
// that stepped back) is kept at the end and expires no earlier than those
// before it.
type window[T any] struct {
	period time.Duration
	at     []time.Time
	vals   []T
	head   int
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
	for w.head < len(w.at) && !now.Before(w.at[w.head].Add(w.period)) {
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
