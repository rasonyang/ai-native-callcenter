// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Expired is a recording past its retention period, as the sweep needs it.
type Expired struct {
	ID  uuid.UUID
	Key string
}

// Ledger is the recording bookkeeping the sweep reads and stamps.
type Ledger interface {
	// ExpiredRecordings returns recordings older than retentionDays that have
	// not been swept, oldest first, at most limit of them.
	ExpiredRecordings(ctx context.Context, retentionDays, limit int) ([]Expired, error)
	// MarkRecordingDeleted records that the audio is gone. The row stays: what
	// a call was and how long it lasted is not the recording.
	MarkRecordingDeleted(ctx context.Context, id uuid.UUID) error
}

// Sweeper deletes recordings past their retention period.
//
// The row is kept and only its audio removed. A CDR without its recording is
// still the record of a call; a CDR that vanished with its audio is a call
// that never happened, which is a different and false claim.
type Sweeper struct {
	ledger        Ledger
	storage       Storage
	retentionDays int
	// batch bounds one pass, so a deployment switching retention on for the
	// first time deletes steadily instead of issuing a hundred thousand object
	// deletes in one minute.
	batch int
	log   *slog.Logger
}

// NewSweeper builds the retention sweep. A retention of zero disables it.
func NewSweeper(ledger Ledger, storage Storage, retentionDays int, log *slog.Logger) *Sweeper {
	return &Sweeper{
		ledger: ledger, storage: storage, retentionDays: retentionDays,
		batch: 500, log: log,
	}
}

// IsEnabled reports whether anything is deleted at all.
func (s *Sweeper) IsEnabled() bool { return s.retentionDays > 0 }

// Run sweeps hourly until the context ends.
//
// Hourly rather than daily: the period is measured in days, so the hour a
// sweep happens changes nothing about what it deletes, and an hourly loop
// means a process restarted at noon does not skip the day.
func (s *Sweeper) Run(ctx context.Context) {
	if !s.IsEnabled() {
		s.log.Info("recording retention is off", "reason", "AICC_RECORDING_RETENTION_DAYS is 0")
		return
	}
	s.log.Info("recording retention on", "days", s.retentionDays)

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := s.Sweep(ctx); err != nil {
				s.log.Error("recording retention sweep", "error", err)
			} else if n > 0 {
				s.log.Info("recordings deleted by retention", "count", n, "days", s.retentionDays)
			}
		}
	}
}

// Sweep deletes one batch and reports how many recordings it removed.
//
// The object goes first and the row is stamped only once it has. The other
// order loses track of bytes: a row marked deleted whose object survived is an
// orphan nothing will ever look for again, paid for for ever. This way round,
// a failure between the two leaves a recording that lists but will not play —
// visible, and repaired by the next sweep, because deleting an object that is
// already gone is not an error.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	due, err := s.ledger.ExpiredRecordings(ctx, s.retentionDays, s.batch)
	if err != nil {
		return 0, fmt.Errorf("list expired recordings: %w", err)
	}

	deleted := 0
	for _, rec := range due {
		if err := s.storage.Delete(ctx, rec.Key); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// One unreachable object must not stop the rest: the next
			// recording along may be perfectly deletable, and a sweep that
			// gives up on the first failure never gets past it.
			s.log.Error("cannot delete a recording's audio",
				"recordingId", rec.ID, "key", rec.Key, "error", err)
			continue
		}
		if err := s.ledger.MarkRecordingDeleted(ctx, rec.ID); err != nil {
			s.log.Error("audio deleted but the row was not marked",
				"recordingId", rec.ID, "key", rec.Key, "error", err)
			continue
		}
		deleted++
	}
	return deleted, nil
}
