// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"testing"
	"time"
)

type sweepRecorder struct {
	delivered, failed time.Time
	calls             int
}

func (s *sweepRecorder) Sweep(_ context.Context, deliveredBefore, failedBefore time.Time) (int64, error) {
	s.delivered, s.failed = deliveredBefore, failedBefore
	s.calls++
	return 0, nil
}

// Two windows, because the rows are read at different ages: a delivered one
// within hours of a customer asking "did you send it", a failed one weeks later
// by somebody reconciling a month and finding a gap.
func TestTheTwoWindowsAreCutSeparately(t *testing.T) {
	rec := &sweepRecorder{}
	NewSweeper(rec, 7, 30, discard()).sweep(t.Context())

	now := time.Now().UTC()
	within := func(got time.Time, days int) bool {
		want := now.AddDate(0, 0, -days)
		return got.Sub(want) < time.Minute && want.Sub(got) < time.Minute
	}
	if !within(rec.delivered, 7) {
		t.Errorf("delivered cutoff %v, want seven days ago", rec.delivered)
	}
	if !within(rec.failed, 30) {
		t.Errorf("failed cutoff %v, want thirty days ago", rec.failed)
	}
}

// Zero means keep, and the way a cutoff comparison reads "keep" is a date
// nothing is older than. Getting this backwards would delete the whole table on
// the first pass for a deployment that asked to keep everything.
func TestAWindowOfZeroKeepsThatKindForever(t *testing.T) {
	rec := &sweepRecorder{}
	NewSweeper(rec, 0, 30, discard()).sweep(t.Context())

	if !rec.delivered.Before(time.Now().UTC().AddDate(-50, 0, 0)) {
		t.Errorf("delivered cutoff %v — zero must mean keep, not delete everything older "+
			"than now", rec.delivered)
	}
	if rec.failed.After(time.Now().UTC().AddDate(0, 0, -29)) {
		t.Errorf("failed cutoff %v, want thirty days ago — one window at zero must not "+
			"disturb the other", rec.failed)
	}
}

// Both at zero and nothing runs at all: Run returns rather than ticking hourly
// over a query that can delete nothing.
func TestBothWindowsAtZeroDisableTheSweepEntirely(t *testing.T) {
	rec := &sweepRecorder{}
	s := NewSweeper(rec, 0, 0, discard())
	if s.IsEnabled() {
		t.Error("a sweeper with both windows at zero reports itself enabled")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.Run(ctx)
	if rec.calls != 0 {
		t.Errorf("it swept %d times with nothing to sweep", rec.calls)
	}
}
