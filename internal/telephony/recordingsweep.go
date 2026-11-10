// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// The classes a failed booking falls into, as the metric's reason label.
const (
	// ReasonMissing: there is no file (a recording was expected) or no object.
	ReasonMissing = "MISSING"
	// ReasonPermission: the disk or the store refused the caller's rights.
	ReasonPermission = "PERMISSION"
	// ReasonTimeout: the attempt ran out of time.
	ReasonTimeout = "TIMEOUT"
	// ReasonStore: any other refusal by the disk or the object store.
	ReasonStore = "STORE"
	// ReasonLedger: the audio is stored and the database would not take the row.
	ReasonLedger = "LEDGER"
)

// The spool sweep's constants. They are deliberately not settings: the
// cadence only decides how soon a stuck file is noticed, and the by-hand route
// (fix the cause, restart the application) resets every count.
const (
	// spoolSweepEvery is the time between passes.
	spoolSweepEvery = time.Minute
	// spoolMinAge is how long a file must have been left alone (by
	// modification time) before the sweep touches it: the hangup path books a
	// recording within seconds, and a file this old is one it did not.
	spoolMinAge = 5 * time.Minute
	// spoolBackoffStart and spoolBackoffMax bound the wait before a failed
	// key is tried again; it doubles from the first to the second.
	spoolBackoffStart = time.Minute
	spoolBackoffMax   = time.Hour
	// spoolMaxAttempts is how many times one key is tried per process.
	spoolMaxAttempts = 10
	// spoolAttemptTimeout bounds one booking attempt.
	spoolAttemptTimeout = 30 * time.Second
)

// RecordingMetrics is where the assembler reports what it could not book.
type RecordingMetrics interface {
	// IngestFailed counts one failed attempt, by reason class.
	IngestFailed(reason string)
	// SpoolUnbooked is how many old recordings the last sweep pass left
	// unbooked.
	SpoolUnbooked(n int)
}

type noRecordingMetrics struct{}

func (noRecordingMetrics) IngestFailed(string) {}
func (noRecordingMetrics) SpoolUnbooked(int)   {}

// SetRecordingMetrics attaches the instruments. Set before the first call
// finishes; never changed afterwards.
func (a *CDRAssembler) SetRecordingMetrics(m RecordingMetrics) {
	if m != nil {
		a.metrics = m
	}
}

// SetLiveCheck tells the sweep which calls are still up, so it never books
// the file of a conversation in progress. Without it nothing is considered
// live and the file's age is the only guard.
func (a *CDRAssembler) SetLiveCheck(isLive func(uuid.UUID) bool) { a.isLive = isLive }

// spoolAttempt is the sweep's memory of one key.
type spoolAttempt struct {
	failures  int
	next      time.Time
	exhausted bool
}

// bookFailure is why a booking did not complete.
type bookFailure struct {
	class string
	err   error
}

// classify puts an error into one of the metric's reason classes.
func classify(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ReasonMissing
	case errors.Is(err, fs.ErrPermission):
		return ReasonPermission
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return ReasonTimeout
	}
	return ReasonStore
}

// bookOutcome says what book did for a key.
type bookOutcome int

const (
	// bookedNow: this call wrote the row.
	bookedNow bookOutcome = iota
	// alreadyBooked: the ledger already held a row for the call; the key was
	// only released.
	alreadyBooked
)

// claim reserves a key for one booking at a time; false means another is
// working on it.
func (a *CDRAssembler) claim(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, busy := a.inflight[key]; busy {
		return false
	}
	a.inflight[key] = struct{}{}
	return true
}

func (a *CDRAssembler) unclaim(key string) {
	a.mu.Lock()
	delete(a.inflight, key)
	a.mu.Unlock()
}

// book is the one path that turns a recording on disk into a ledger row, for
// the hangup and the sweep alike: make the audio durable, book the row, flag
// the CDR, and only then let go of the local copy. The caller holds the key's
// claim. A failure leaves the local copy where it is, which is what lets the
// sweep try again.
//
// Under the claim it first asks the ledger about this one call: the sweep's
// batched pre-filter is older than the claim, and the other path may have
// booked the key in between. A row that exists means only a release is owed.
// It logs nothing on success; each caller says what it did.
func (a *CDRAssembler) book(ctx context.Context, callID uuid.UUID, key string) (store.Recording, bookOutcome, *bookFailure) {
	have, err := a.ledger.BookedRecordingCallIDs(ctx, []uuid.UUID{callID})
	if err != nil {
		return store.Recording{}, bookedNow, &bookFailure{class: ReasonLedger, err: err}
	}
	if len(have) > 0 {
		if err := a.storage.Release(ctx, key); err != nil {
			a.log.Warn("could not remove the spool copy of a booked recording; the sweep retries",
				"callId", callID, "key", key, "error", err)
		}
		return store.Recording{}, alreadyBooked, nil
	}
	size, err := a.storage.Ingest(ctx, key)
	if err != nil {
		return store.Recording{}, bookedNow, &bookFailure{class: classify(err), err: err}
	}
	booked, err := a.ledger.InsertRecording(ctx, store.Recording{
		CallID:      callID,
		Backend:     a.storage.Backend(),
		Bucket:      a.storage.Bucket(),
		ObjectKey:   key,
		SizeBytes:   size,
		DurationSec: recording.DurationSec(size),
	})
	if err != nil {
		return store.Recording{}, bookedNow, &bookFailure{class: ReasonLedger, err: err}
	}
	if err := a.ledger.MarkRecorded(ctx, callID); err != nil {
		// The row exists, so this is not retried (a booked key is only ever
		// released); the screens read the recordings table, not the flag.
		a.metrics.IngestFailed(ReasonLedger)
		a.log.Error("could not flag the cdr as recorded", "callId", callID, "key", key,
			"reasonClass", ReasonLedger, "error", err)
	}
	if err := a.storage.Release(ctx, key); err != nil {
		a.log.Warn("could not remove the spool copy of a booked recording; the sweep retries",
			"callId", callID, "key", key, "error", err)
	}
	return booked, bookedNow, nil
}

// reportFailure logs a failed booking at ERROR and counts it.
func (a *CDRAssembler) reportFailure(callID uuid.UUID, key string, f *bookFailure) {
	a.metrics.IngestFailed(f.class)
	msg := "recording not ingested"
	if f.class == ReasonLedger {
		msg = "could not book the recording"
	}
	a.log.Error(msg, "callId", callID, "key", key, "reason", f.err, "reasonClass", f.class)
}

// RunSpoolSweep retries recordings the hangup path could not book, every
// spoolSweepEvery until ctx ends.
func (a *CDRAssembler) RunSpoolSweep(ctx context.Context) {
	if a.storage == nil {
		return
	}
	a.SweepSpool(ctx)
	t := time.NewTicker(spoolSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.SweepSpool(ctx)
		}
	}
}

// SweepSpool is one pass: every recording the switch wrote that is older than
// spoolMinAge and has no row is booked, and every one that has a row is only
// released.
//
// A recording booked here is not offered to post-call transcription: no
// snapshot is left to say who answered. It is not lost to it, though: the
// backfill command (CallsAwaitingTranscription) lists recorded calls with no
// run by the recording row's creation time, which is the booking.
//
// A call adopted after a restart mid-call never saw its RECORD_START, so the
// hangup path treats its missing file as unremarkable; this pass is what
// books such a file once it is old enough.
func (a *CDRAssembler) SweepSpool(ctx context.Context) {
	if a.storage == nil {
		return
	}
	now := a.now()
	keys, err := a.storage.Pending(ctx, now.Add(-spoolMinAge))
	if err != nil {
		a.log.Warn("could not list the recording spool", "error", err)
		return
	}
	callIDs := make(map[string]uuid.UUID, len(keys))
	ids := make([]uuid.UUID, 0, len(keys))
	for _, key := range keys {
		if id, ok := recording.CallIDOf(key); ok {
			callIDs[key] = id
			ids = append(ids, id)
		}
	}
	bookedIDs, err := a.ledger.BookedRecordingCallIDs(ctx, ids)
	if err != nil {
		a.log.Warn("could not ask the ledger which recordings are booked", "error", err)
		return
	}
	booked := make(map[uuid.UUID]bool, len(bookedIDs))
	for _, id := range bookedIDs {
		booked[id] = true
	}

	unbooked := 0
	seen := make(map[string]bool, len(callIDs))
	for _, key := range keys {
		callID, ok := callIDs[key]
		if !ok {
			continue
		}
		seen[key] = true
		if ctx.Err() != nil {
			return
		}
		if booked[callID] {
			a.releaseBooked(ctx, callID, key)
			continue
		}
		if a.isLive != nil && a.isLive(callID) {
			continue
		}
		if !a.claim(key) {
			continue // the hangup path is on it
		}
		if a.sweepOne(ctx, callID, key, now) {
			unbooked++
		}
		a.unclaim(key)
	}
	a.mu.Lock()
	for key := range a.attempts {
		if !seen[key] {
			delete(a.attempts, key)
		}
	}
	a.mu.Unlock()
	a.metrics.SpoolUnbooked(unbooked)
}

// releaseBooked lets go of the spool copy of a recording the ledger already
// holds (a release that failed earlier, or a restart between booking and it).
func (a *CDRAssembler) releaseBooked(ctx context.Context, callID uuid.UUID, key string) {
	if !a.claim(key) {
		return
	}
	defer a.unclaim(key)
	if err := a.storage.Release(ctx, key); err != nil {
		a.log.Warn("could not remove the spool copy of a booked recording; the sweep retries",
			"callId", callID, "key", key, "error", err)
	}
	a.mu.Lock()
	delete(a.attempts, key)
	a.mu.Unlock()
}

// sweepOne tries to book one unbooked key if its backoff allows, and reports
// whether it is still unbooked afterwards. The caller holds the claim.
func (a *CDRAssembler) sweepOne(ctx context.Context, callID uuid.UUID, key string, now time.Time) (stillUnbooked bool) {
	a.mu.Lock()
	st := a.attempts[key]
	if st == nil {
		st = &spoolAttempt{}
		a.attempts[key] = st
	}
	waiting := st.exhausted || now.Before(st.next)
	a.mu.Unlock()
	if waiting {
		return true
	}

	actx, cancel := context.WithTimeout(ctx, spoolAttemptTimeout)
	defer cancel()
	booked, outcome, fail := a.book(actx, callID, key)
	if fail == nil {
		if outcome == bookedNow {
			a.log.Info("recording booked from the spool", "callId", callID, "key", key,
				"sizeBytes", booked.SizeBytes)
		}
		a.mu.Lock()
		delete(a.attempts, key)
		a.mu.Unlock()
		return false
	}

	a.reportFailure(callID, key, fail)
	a.mu.Lock()
	st.failures++
	exhausted := st.failures >= spoolMaxAttempts
	if exhausted {
		st.exhausted = true
	} else {
		st.next = now.Add(spoolBackoff(st.failures))
	}
	a.mu.Unlock()
	if exhausted {
		a.log.Error("recording left in the spool; retries are exhausted until the application restarts",
			"callId", callID, "key", key, "attempts", spoolMaxAttempts, "reasonClass", fail.class)
	}
	return true
}

// spoolBackoff is the wait after the nth consecutive failure: a minute, then
// doubling, up to an hour.
func spoolBackoff(failures int) time.Duration {
	d := spoolBackoffStart
	for i := 1; i < failures && d < spoolBackoffMax; i++ {
		d *= 2
	}
	return min(d, spoolBackoffMax)
}
