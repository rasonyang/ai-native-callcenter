// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Expected-answer admission and overflow control (design 10 sections 6.3 and
// 6.5). Everything here is arithmetic on counts; the dialer owns the clock, the
// store and the cache.

const (
	// RateWindow is how many of a campaign's most recent attempts the answer
	// rate is judged over, and how many answered calls the overflow rate is.
	RateWindow = 200
	// MinRateSamples is the fewest rung attempts at which the rate is trusted.
	// Below it the bound is 1: every ringing leg is planned to answer.
	MinRateSamples = 50

	// The z values of the two-sided Wilson interval: the bound is the
	// interval's upper end.
	z95 = 1.959964
	z99 = 2.575829
)

// DoesRing reports whether an outcome means the customer's phone rang, which is
// what makes an attempt a sample of the answer rate. The answered outcomes
// rang. So did NO_ANSWER, BUSY and REJECTED: the network reached the callee
// and the callee did not pick up.
//
// Left out, deliberately: TRANSIENT, LINE_REJECTED, ORIGINATE_FAILED,
// INVALID_NUMBER and DNC_SKIPPED never reached a phone, and UNKNOWN is an attempt that left no
// trace, so nothing says whether it rang. Counting an UNKNOWN as unanswered
// would pull the estimate down on no evidence, and the estimate must lean up.
func DoesRing(o Outcome) bool {
	return IsAnsweredOutcome(o) || o == OutcomeNoAnswer || o == OutcomeBusy || o == OutcomeRejected
}

// WilsonUpper is the upper end of the Wilson score interval for k successes in
// n trials at the given z. It is 1 for n = 0.
func WilsonUpper(k, n int, z float64) float64 {
	if n <= 0 {
		return 1
	}
	nf := float64(n)
	p := float64(k) / nf
	z2 := z * z
	centre := p + z2/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	return math.Min(1, (centre+margin)/(1+z2/nf))
}

// AnswerRateBound is the answer rate a campaign is planned with: the Wilson
// 95 % upper bound over answered of rang attempts, the 99 % bound when
// tightened, and 1 while there are fewer than MinRateSamples.
func AnswerRateBound(answered, rang int, isTightened bool) float64 {
	if rang < MinRateSamples {
		return 1
	}
	z := z95
	if isTightened {
		z = z99
	}
	return WilsonUpper(answered, rang, z)
}

// OverflowMode is where overflow control stands.
type OverflowMode int

const (
	// OverflowNormal: the 95 % bound.
	OverflowNormal OverflowMode = iota
	// OverflowTightened: the overflow rate is above the tighten rate; the 99 %
	// bound.
	OverflowTightened
	// OverflowStopped: the overflow rate went above the stop rate; no over-dial
	// (bound 1) until the rate is back under the tighten rate.
	OverflowStopped
)

// NextOverflowMode moves the mode for a new overflow rate. The hysteresis is
// the point: once stopped, the campaign stays stopped until the rate is under
// the tighten rate, not merely under the stop rate, so it does not flap around
// the stop line.
func NextOverflowMode(prev OverflowMode, rate, tighten, stop float64) OverflowMode {
	switch {
	case rate > stop:
		return OverflowStopped
	case prev == OverflowStopped:
		if rate < tighten {
			return OverflowNormal
		}
		return OverflowStopped
	case rate > tighten:
		return OverflowTightened
	}
	return OverflowNormal
}

// window is the last RateWindow booleans, each optionally tagged with the call
// it is a sample of.
type window struct {
	buf   [RateWindow]bool
	ids   [RateWindow]uuid.UUID
	n     int // how many are held
	pos   int // where the next goes
	count int // how many of the held are true
}

func (w *window) add(v bool, id uuid.UUID) {
	if w.n == RateWindow {
		if w.buf[w.pos] {
			w.count--
		}
	} else {
		w.n++
	}
	w.buf[w.pos], w.ids[w.pos] = v, id
	if v {
		w.count++
	}
	w.pos = (w.pos + 1) % RateWindow
}

// set turns the sample of call id to v and reports whether it was still held.
func (w *window) set(id uuid.UUID, v bool) bool {
	for i := 0; i < w.n; i++ {
		if w.ids[i] == id && id != uuid.Nil {
			if w.buf[i] != v {
				w.buf[i] = v
				if v {
					w.count++
				} else {
					w.count--
				}
			}
			return true
		}
	}
	return false
}

// rangSample is one attempt's answer-rate sample, placed by when the attempt
// was dialled. tie breaks the order of attempts dialled at the same instant at
// random, so that of a burst the part that fits the window is a fair sample of
// it however its members happened to arrive.
type rangSample struct {
	at    time.Time
	tie   uint64
	value bool
}

func (a rangSample) before(b rangSample) bool {
	if a.at.Equal(b.at) {
		return a.tie < b.tie
	}
	return a.at.Before(b.at)
}

// rangWindow holds the RateWindow most recently dialled samples, whatever order
// they arrive in.
type rangWindow struct {
	held  []rangSample // ascending
	count int          // how many are true
	state uint64
}

func (w *rangWindow) add(at time.Time, v bool) {
	w.state = w.state*6364136223846793005 + 1442695040888963407
	sm := rangSample{at: at, tie: w.state >> 16, value: v}
	if len(w.held) == RateWindow && sm.before(w.held[0]) {
		return // older than everything held
	}
	i := sort.Search(len(w.held), func(i int) bool { return sm.before(w.held[i]) })
	w.held = append(w.held, rangSample{})
	copy(w.held[i+1:], w.held[i:])
	w.held[i] = sm
	if v {
		w.count++
	}
	if len(w.held) > RateWindow {
		if w.held[0].value {
			w.count--
		}
		w.held = w.held[1:]
	}
}

// CapacityStats is what the dialer remembers about one campaign to plan its
// admission: the last RateWindow attempts that rang, and the last RateWindow
// that were answered. Not safe for concurrent use (the dialer's actor owns it).
//
// The two windows are fed differently, and the difference is the point.
//
// The rang window is about the answer rate, and an attempt's sample must not
// enter it before the attempt can no longer be told from a slower one: an answer
// is known after a few seconds of ringing and a non-answer only when the ring
// timeout has run out, so samples taken the moment they are known would put
// every answer ahead of its cohort's non-answers, and a window shorter than a
// burst of admissions would see nothing but non-answers and plan its way into
// overflow. The dialer therefore feeds it (Rang) at the time an attempt's ring
// timeout has passed, whichever way it went.
//
// The answered window is about overflow, which is known 1.5 s after the answer,
// so it is fed at the answer (Answered) and corrected by Overflowed.
type CapacityStats struct {
	tighten, stop float64
	rang          rangWindow
	replayed      int64
	answered      window // true = overflowed
	mode          OverflowMode
}

// NewCapacityStats starts a campaign with no history, so its bound is 1.
func NewCapacityStats(tightenRate, stopRate float64) *CapacityStats {
	return &CapacityStats{tighten: tightenRate, stop: stopRate}
}

func (s *CapacityStats) remode() {
	s.mode = NextOverflowMode(s.mode, s.OverflowRate(), s.tighten, s.stop)
}

// Rang records one attempt, dialled at the given time, whose ring has run its
// course: whether it was answered. Samples arrive in any order; the window keeps
// the most recently dialled.
func (s *CapacityStats) Rang(dialledAt time.Time, isAnswered bool) { s.rang.add(dialledAt, isAnswered) }

// Answered records that the customer of call id picked up, a call that has not
// met a missing bot so far.
func (s *CapacityStats) Answered(id uuid.UUID) {
	s.answered.add(false, id)
	s.remode()
}

// Overflowed turns the answered sample of call id into an overflow. A call
// whose sample has left the window is counted as a new one.
func (s *CapacityStats) Overflowed(id uuid.UUID) {
	if !s.answered.set(id, true) {
		s.answered.add(true, uuid.Nil)
	}
	s.remode()
}

// Observe replays a finished attempt known only by its outcome, as a history
// read from the store after a restart: it feeds both windows at once. Outcomes
// that say nothing about the answer rate are ignored.
func (s *CapacityStats) Observe(o Outcome) {
	if !DoesRing(o) {
		return
	}
	isAnswered := IsAnsweredOutcome(o)
	s.replayed++ // older than any live attempt, newer than the replay before it
	s.rang.add(time.Unix(0, s.replayed), isAnswered)
	if isAnswered {
		s.answered.add(o == OutcomeOverflow, uuid.Nil)
		s.remode()
	}
}

// OverflowRate is the share of the last RateWindow answered calls that met no
// bot; 0 with none.
func (s *CapacityStats) OverflowRate() float64 {
	if s.answered.n == 0 {
		return 0
	}
	return float64(s.answered.count) / float64(s.answered.n)
}

// Mode is where overflow control stands.
func (s *CapacityStats) Mode() OverflowMode { return s.mode }

// Samples is how many rung attempts the answer rate rests on.
func (s *CapacityStats) Samples() int { return len(s.rang.held) }

// Answers is how many of the Samples were answered.
func (s *CapacityStats) Answers() int { return s.rang.count }

// Bound is the answer rate to plan this campaign's ringing legs with.
func (s *CapacityStats) Bound() float64 {
	if s.mode == OverflowStopped {
		return 1
	}
	return AnswerRateBound(s.rang.count, len(s.rang.held), s.mode == OverflowTightened)
}
