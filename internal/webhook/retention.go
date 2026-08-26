// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"log/slog"
	"time"
)

// Sweeper removes settled deliveries past their own window.
//
// Two windows rather than one, because the rows are read at different ages: a
// delivered row is checked within hours of a customer asking "did you send it",
// while a failed one is what gets looked up weeks later by somebody reconciling
// a month of records and finding a gap.
//
// PENDING is never swept. A delivery still owed is work, not history, and the
// retry schedule is what ends it — by reaching DELIVERED or FAILED.
//
// On by default, unlike the recording sweeper, and the difference is deliberate:
// recordings are irreplaceable customer audio, so deleting any unasked would be
// wrong; delivery rows are operational exhaust that grows with every call
// forever, and a table nobody prunes is a defect waiting on a busy month.
type Sweeper struct {
	store         SweepStore
	deliveredDays int
	failedDays    int
	every         time.Duration
	log           *slog.Logger
}

// SweepStore is the slice of the outbox the sweep uses.
type SweepStore interface {
	Sweep(ctx context.Context, deliveredBefore, failedBefore time.Time) (int64, error)
}

// NewSweeper builds the sweep. Either window at zero leaves that kind of row
// alone, for a deployment that wants to keep everything.
func NewSweeper(st SweepStore, deliveredDays, failedDays int, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{
		store: st, deliveredDays: deliveredDays, failedDays: failedDays,
		every: time.Hour, log: log,
	}
}

// IsEnabled reports whether anything is deleted at all.
func (s *Sweeper) IsEnabled() bool { return s.deliveredDays > 0 || s.failedDays > 0 }

// Run sweeps hourly until the context ends.
func (s *Sweeper) Run(ctx context.Context) {
	if !s.IsEnabled() {
		return
	}
	t := time.NewTicker(s.every)
	defer t.Stop()
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *Sweeper) sweep(ctx context.Context) {
	now := time.Now().UTC()
	// A window of zero must not become "delete everything older than now".
	// The far past is what "keep it all" looks like to a cutoff comparison.
	far := now.AddDate(-100, 0, 0)
	delivered, failed := far, far
	if s.deliveredDays > 0 {
		delivered = now.AddDate(0, 0, -s.deliveredDays)
	}
	if s.failedDays > 0 {
		failed = now.AddDate(0, 0, -s.failedDays)
	}

	n, err := s.store.Sweep(ctx, delivered, failed)
	if err != nil {
		s.log.ErrorContext(ctx, "could not sweep webhook deliveries", "error", err)
		return
	}
	if n > 0 {
		s.log.InfoContext(ctx, "swept webhook deliveries", "rows", n,
			"deliveredOlderThanDays", s.deliveredDays, "failedOlderThanDays", s.failedDays)
	}
}
