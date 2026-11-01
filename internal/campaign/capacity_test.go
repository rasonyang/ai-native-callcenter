// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWilsonUpperKnownValues(t *testing.T) {
	// Values of the Wilson score interval worked by hand.
	tests := []struct {
		name string
		k, n int
		z    float64
		want float64
	}{
		{"no trials", 0, 0, z95, 1},
		{"none of fifty", 0, 50, z95, 0.0713},
		{"ten of fifty", 10, 50, z95, 0.3304},
		{"forty of two hundred", 40, 200, z95, 0.2609},
		{"all", 50, 50, z95, 1},
		{"forty of two hundred, 99 percent", 40, 200, z99, 0.2820},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WilsonUpper(tt.k, tt.n, tt.z); math.Abs(got-tt.want) > 0.001 {
				t.Errorf("WilsonUpper(%d,%d) = %.4f, want %.4f", tt.k, tt.n, got, tt.want)
			}
		})
	}
}

func TestWilsonUpperProperties(t *testing.T) {
	for n := 1; n <= 200; n += 7 {
		prev := -1.0
		for k := 0; k <= n; k++ {
			u := WilsonUpper(k, n, z95)
			if u < float64(k)/float64(n)-1e-9 || u > 1 {
				t.Fatalf("bound %.4f outside [rate, 1] for %d/%d", u, k, n)
			}
			if u < prev {
				t.Fatalf("bound fell as successes rose at %d/%d", k, n)
			}
			prev = u
			if WilsonUpper(k, n, z99) < u-1e-9 {
				t.Fatalf("99%% bound below the 95%% bound at %d/%d", k, n)
			}
		}
	}
}

func TestAnswerRateBoundColdStart(t *testing.T) {
	for _, tt := range []struct {
		answered, rang int
		tight          bool
		wantOne        bool
	}{
		{0, 0, false, true},
		{0, 49, false, true},
		{49, 49, true, true},
		{0, 50, false, false},
		{10, 50, true, false},
	} {
		got := AnswerRateBound(tt.answered, tt.rang, tt.tight)
		if (got == 1) != tt.wantOne {
			t.Errorf("AnswerRateBound(%d,%d,%v) = %.3f, wantOne=%v", tt.answered, tt.rang, tt.tight, got, tt.wantOne)
		}
	}
}

func TestDoesRingTable(t *testing.T) {
	rang := map[Outcome]bool{
		OutcomeAnswered: true, OutcomeBotFailed: true, OutcomeOverflow: true,
		OutcomeNoAnswer: true, OutcomeBusy: true, OutcomeRejected: true,
		OutcomeUnknown: false, OutcomeTransient: false, OutcomeOriginateFailed: false,
		OutcomeInvalidNumber: false, OutcomeDNCSkipped: false,
	}
	for o, want := range rang {
		if DoesRing(o) != want {
			t.Errorf("DoesRing(%s) = %v, want %v", o, !want, want)
		}
	}
}

func TestNextOverflowModeTable(t *testing.T) {
	const tighten, stop = 0.01, 0.03
	tests := []struct {
		name string
		prev OverflowMode
		rate float64
		want OverflowMode
	}{
		{"quiet", OverflowNormal, 0, OverflowNormal},
		{"at the tighten rate is not above it", OverflowNormal, 0.01, OverflowNormal},
		{"above tighten", OverflowNormal, 0.02, OverflowTightened},
		{"tightened recovers", OverflowTightened, 0.005, OverflowNormal},
		{"tightened to stopped", OverflowTightened, 0.031, OverflowStopped},
		{"at the stop rate is not above it", OverflowTightened, 0.03, OverflowTightened},
		{"normal jumps to stopped", OverflowNormal, 0.5, OverflowStopped},
		{"stopped stays between the rates", OverflowStopped, 0.02, OverflowStopped},
		{"stopped stays at the tighten rate", OverflowStopped, 0.01, OverflowStopped},
		{"stopped recovers under tighten", OverflowStopped, 0.0099, OverflowNormal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NextOverflowMode(tt.prev, tt.rate, tighten, stop); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCapacityStatsWindowsSlide(t *testing.T) {
	s := NewCapacityStats(0.01, 0.03)
	// 60 answered, then 400 unanswered: the answered ones leave the window.
	for i := 0; i < 60; i++ {
		s.Observe(OutcomeAnswered)
	}
	if s.Samples() != 60 {
		t.Fatalf("samples = %d", s.Samples())
	}
	high := s.Bound()
	for i := 0; i < 400; i++ {
		s.Observe(OutcomeNoAnswer)
	}
	if s.Samples() != RateWindow {
		t.Fatalf("window holds %d, want %d", s.Samples(), RateWindow)
	}
	if low := s.Bound(); low >= high || low > 0.05 {
		t.Errorf("bound did not follow the window: %.3f then %.3f", high, low)
	}
}

func TestCapacityStatsIgnoresWhatSaysNothing(t *testing.T) {
	s := NewCapacityStats(0.01, 0.03)
	for _, o := range []Outcome{OutcomeUnknown, OutcomeTransient, OutcomeOriginateFailed, OutcomeInvalidNumber, OutcomeDNCSkipped} {
		for i := 0; i < 100; i++ {
			s.Observe(o)
		}
	}
	if s.Samples() != 0 || s.Bound() != 1 {
		t.Errorf("samples = %d bound = %.2f", s.Samples(), s.Bound())
	}
}

func TestCapacityStatsOverflowFallsBackAndRecovers(t *testing.T) {
	s := NewCapacityStats(0.01, 0.03)
	// A settled 20 % answer rate, no overflow.
	for i := 0; i < 200; i++ {
		if i%5 == 0 {
			s.Observe(OutcomeAnswered)
		} else {
			s.Observe(OutcomeNoAnswer)
		}
	}
	if b := s.Bound(); b >= 0.3 || s.Mode() != OverflowNormal {
		t.Fatalf("bound %.3f mode %v before overflow", b, s.Mode())
	}
	// Three overflows among 81 answered: 3.7 % > 3 %.
	for i := 0; i < 38; i++ {
		s.Observe(OutcomeAnswered)
	}
	s.Observe(OutcomeOverflow)
	s.Observe(OutcomeOverflow)
	s.Observe(OutcomeOverflow)
	if s.Mode() != OverflowStopped || s.Bound() != 1 {
		t.Fatalf("mode %v bound %.3f, want stopped and 1 (rate %.3f)", s.Mode(), s.Bound(), s.OverflowRate())
	}
	// Answered calls that find a bot dilute the rate; it must pass through the
	// stop line and stay stopped until it is under the tighten rate.
	stopped := true
	for i := 0; i < 400 && stopped; i++ {
		s.Observe(OutcomeAnswered)
		if s.Mode() != OverflowStopped {
			stopped = false
			if s.OverflowRate() >= 0.01 {
				t.Fatalf("left the stopped state at rate %.4f, not under the tighten rate", s.OverflowRate())
			}
		}
	}
	if stopped {
		t.Fatal("never recovered")
	}
}

var t0 = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

func TestRangFeedsTheAnswerRateAndAnsweredFeedsOverflow(t *testing.T) {
	s := NewCapacityStats(0.01, 0.03)
	for i := 0; i < 100; i++ {
		s.Rang(t0.Add(time.Duration(i)*time.Second), i%5 == 0) // 20 % answered
	}
	if s.Samples() != 100 {
		t.Fatalf("samples = %d, want 100", s.Samples())
	}
	if b := s.Bound(); b < 0.2 || b > 0.35 {
		t.Fatalf("bound %.3f, want about the true 20%% plus its margin", b)
	}
	if s.OverflowRate() != 0 {
		t.Fatal("answer-rate samples moved the overflow rate")
	}
}

func TestAnOverflowTurnsItsAnsweredSampleIntoAnOverflow(t *testing.T) {
	s := NewCapacityStats(0.01, 0.03)
	ids := make([]uuid.UUID, 50)
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
		s.Answered(ids[i])
	}
	if s.OverflowRate() != 0 {
		t.Fatal("overflow before any")
	}
	s.Overflowed(ids[0])
	if got := s.OverflowRate(); got != 1.0/50 {
		t.Fatalf("overflow rate %.4f, want 0.02", got)
	}
	if s.Mode() != OverflowTightened {
		t.Errorf("mode %v, want tightened at 2%%", s.Mode())
	}
	// A sample that has left the window is counted as a new overflow.
	s.Overflowed(uuid.Must(uuid.NewV7()))
	if got := s.OverflowRate(); got != 2.0/51 {
		t.Errorf("overflow rate %.4f, want 2/51", got)
	}
}

// A burst dialled at one instant that rings out together arrives answers first;
// of a burst larger than the window, the part that stays must still be a fair
// sample of it.
func TestABurstLargerThanTheWindowIsSampledFairlyWhateverItsArrivalOrder(t *testing.T) {
	s := NewCapacityStats(0.01, 0.03)
	for i := 0; i < 1000; i++ { // 20 % answered, answers arriving first
		s.Rang(t0, i < 200)
	}
	if s.Samples() != RateWindow {
		t.Fatalf("samples = %d", s.Samples())
	}
	if got := float64(s.Answers()) / float64(s.Samples()); got < 0.12 || got > 0.28 {
		t.Fatalf("the window holds %.2f answered, want about the burst's 0.20", got)
	}
}

// Older samples never displace newer ones, however late they arrive.
func TestALateSampleOfAnOlderAttemptDoesNotDisplaceNewerOnes(t *testing.T) {
	s := NewCapacityStats(0.01, 0.03)
	for i := 0; i < RateWindow; i++ {
		s.Rang(t0.Add(time.Hour+time.Duration(i)*time.Second), false)
	}
	for i := 0; i < 50; i++ {
		s.Rang(t0, true) // dialled long before
	}
	if s.Answers() != 0 || s.Samples() != RateWindow {
		t.Fatalf("answers %d samples %d: a stale sample entered the window", s.Answers(), s.Samples())
	}
}
