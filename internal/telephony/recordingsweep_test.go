// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// logLine is one record the assembler logged.
type logLine struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

type logCapture struct {
	mu    sync.Mutex
	lines []logLine
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	l := logLine{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool { l.attrs[a.Key] = a.Value.Any(); return true })
	c.mu.Lock()
	c.lines = append(c.lines, l)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) count(level slog.Level, msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.lines {
		if l.level == level && l.msg == msg {
			n++
		}
	}
	return n
}

type metricsCapture struct {
	mu       sync.Mutex
	failures map[string]int
	unbooked int
}

func (m *metricsCapture) IngestFailed(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures == nil {
		m.failures = map[string]int{}
	}
	m.failures[reason]++
}
func (m *metricsCapture) SpoolUnbooked(n int) {
	m.mu.Lock()
	m.unbooked = n
	m.mu.Unlock()
}
func (m *metricsCapture) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.failures {
		n += v
	}
	return n
}

// spoolStorage is an S3-with-spool stand-in: files in a temp directory, an
// ingest that never deletes them, and a release that does.
type spoolStorage struct {
	root      string
	mu        sync.Mutex
	ingestErr error
	ingested  []string
	released  []string
	started   chan struct{} // closed on the first Ingest, when set
	block     chan struct{} // Ingest waits for it, when set
}

func (s *spoolStorage) Backend() string { return "S3" }
func (s *spoolStorage) Bucket() string  { return "b" }
func (s *spoolStorage) Ingest(_ context.Context, key string) (int64, error) {
	if s.started != nil {
		select {
		case <-s.started:
		default:
			close(s.started)
		}
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingested = append(s.ingested, key)
	if s.ingestErr != nil {
		return 0, s.ingestErr
	}
	info, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(key)))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
func (s *spoolStorage) Release(_ context.Context, key string) error {
	s.mu.Lock()
	s.released = append(s.released, key)
	s.mu.Unlock()
	err := os.Remove(filepath.Join(s.root, filepath.FromSlash(key)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
func (s *spoolStorage) Pending(_ context.Context, before time.Time) ([]string, error) {
	return (&listOnly{s.root}).list(before)
}

func (s *spoolStorage) ingestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ingested)
}

// listOnly lists the temp directory the way the real spool does.
type listOnly struct{ root string }

func (l *listOnly) list(before time.Time) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(l.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(l.root, p)
		key := filepath.ToSlash(rel)
		if _, ok := recording.CallIDOf(key); !ok {
			return nil
		}
		info, err := d.Info()
		if err == nil && info.ModTime().Before(before) {
			keys = append(keys, key)
		}
		return nil
	})
	return keys, err
}

// bookLedger is a ledger that remembers recording rows.
type bookLedger struct {
	memoryLedger
	rmu       sync.Mutex
	rows      map[uuid.UUID]store.Recording
	insertErr error
	marked    []uuid.UUID
}

func (l *bookLedger) InsertRecording(_ context.Context, r store.Recording) (store.Recording, error) {
	l.rmu.Lock()
	defer l.rmu.Unlock()
	if l.insertErr != nil {
		return store.Recording{}, l.insertErr
	}
	if l.rows == nil {
		l.rows = map[uuid.UUID]store.Recording{}
	}
	l.rows[r.CallID] = r
	return r, nil
}
func (l *bookLedger) MarkRecorded(_ context.Context, id uuid.UUID) error {
	l.rmu.Lock()
	l.marked = append(l.marked, id)
	l.rmu.Unlock()
	return nil
}
func (l *bookLedger) BookedRecordingCallIDs(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	l.rmu.Lock()
	defer l.rmu.Unlock()
	var out []uuid.UUID
	for _, id := range ids {
		if _, ok := l.rows[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// hookLedger runs a function the first time the sweep asks its batched
// question, so a test can act between that query and the sweep's claim.
type hookLedger struct {
	*bookLedger
	fired atomic.Bool
	after func()
}

func (h *hookLedger) BookedRecordingCallIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	out, err := h.bookLedger.BookedRecordingCallIDs(ctx, ids)
	// The first question is the sweep's batched one. Mark before acting: the
	// hook itself asks again.
	if h.after != nil && h.fired.CompareAndSwap(false, true) {
		h.after()
	}
	return out, err
}

func (l *bookLedger) rowCount() int {
	l.rmu.Lock()
	defer l.rmu.Unlock()
	return len(l.rows)
}

// insertCounter counts InsertRecording calls, which a second row would raise.
type insertCounter struct {
	*bookLedger
	mu sync.Mutex
	n  int
}

func (c *insertCounter) InsertRecording(ctx context.Context, r store.Recording) (store.Recording, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.bookLedger.InsertRecording(ctx, r)
}

type countingOffer struct {
	mu sync.Mutex
	n  int
}

func (o *countingOffer) OfferRecording(store.Recording, bool, bool) {
	o.mu.Lock()
	o.n++
	o.mu.Unlock()
}

type sweepRig struct {
	a       *CDRAssembler
	storage *spoolStorage
	ledger  *bookLedger
	logs    *logCapture
	metrics *metricsCapture
	now     time.Time
}

func newSweepRig(t *testing.T) *sweepRig {
	t.Helper()
	r := &sweepRig{
		storage: &spoolStorage{root: t.TempDir()},
		ledger:  &bookLedger{},
		logs:    &logCapture{},
		metrics: &metricsCapture{},
		now:     time.Now(),
	}
	r.a = NewCDRAssembler(r.ledger, staticQueues{}, r.storage, slog.New(r.logs))
	r.a.flushWait = 0
	r.a.SetRecordingMetrics(r.metrics)
	r.a.now = func() time.Time { return r.now }
	return r
}

// spoolFile writes a recording into the spool, last written age ago.
func (r *sweepRig) spoolFile(t *testing.T, callID uuid.UUID, age time.Duration) (key, full string) {
	t.Helper()
	key = recording.Key(r.now, callID.String())
	full = filepath.Join(r.storage.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, 64044), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := r.now.Add(-age)
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return key, full
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func finishedSnap(callID uuid.UUID, recordingStarted bool) Snapshot {
	return Snapshot{CallID: callID, CallType: events.CallTypeInbound, CreatedAt: time.Now(),
		EndedAt: atPtr(60), IsRecordingStarted: recordingStarted}
}

func TestAnExpectedRecordingThatCannotBeIngestedIsAnErrorAndCounted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		err   error
		class string
	}{
		{"file missing", fs.ErrNotExist, ReasonMissing},
		{"refused", fs.ErrPermission, ReasonPermission},
		{"ran out of time", context.DeadlineExceeded, ReasonTimeout},
		{"store down", errors.New("connection refused"), ReasonStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSweepRig(t)
			r.storage.ingestErr = fmt.Errorf("upload: %w", tc.err)
			r.a.ingestRecording(finishedSnap(uuid.Must(uuid.NewV7()), true))
			if got := r.logs.count(slog.LevelError, "recording not ingested"); got != 1 {
				t.Errorf("ERROR lines = %d, want 1", got)
			}
			if r.metrics.failures[tc.class] != 1 || r.metrics.total() != 1 {
				t.Errorf("failures = %v, want one %s", r.metrics.failures, tc.class)
			}
		})
	}
}

func TestACallThatWasNeverRecordedIsSilentWhenTheFileIsAbsent(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	r.storage.ingestErr = fs.ErrNotExist
	r.a.ingestRecording(finishedSnap(uuid.Must(uuid.NewV7()), false))
	if r.logs.count(slog.LevelInfo, "no recording ingested") != 1 {
		t.Error("the absence was not logged at INFO")
	}
	if r.logs.count(slog.LevelError, "recording not ingested") != 0 || r.metrics.total() != 0 {
		t.Errorf("an unrecorded call raised an error: failures %v", r.metrics.failures)
	}
}

func TestAFileThatExistsButCannotBeReadIsAFailureEvenWithoutRecordStart(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	r.storage.ingestErr = fs.ErrPermission
	r.a.ingestRecording(finishedSnap(uuid.Must(uuid.NewV7()), false))
	if r.logs.count(slog.LevelError, "recording not ingested") != 1 || r.metrics.failures[ReasonPermission] != 1 {
		t.Errorf("logs %v failures %v", r.logs.lines, r.metrics.failures)
	}
}

func TestTheSpoolCopyOutlivesAFailedLedgerWriteAndGoesOnceBooked(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	callID := uuid.Must(uuid.NewV7())
	_, full := r.spoolFile(t, callID, 0)

	r.ledger.insertErr = errors.New("database is down")
	r.a.ingestRecording(Snapshot{CallID: callID, CreatedAt: time.Now(), EndedAt: atPtr(60), IsRecordingStarted: true})
	if !exists(full) {
		t.Fatal("the spool copy was removed although the row was never booked")
	}
	if r.metrics.failures[ReasonLedger] != 1 || r.logs.count(slog.LevelError, "could not book the recording") != 1 {
		t.Errorf("failures %v", r.metrics.failures)
	}

	r.ledger.insertErr = nil
	r.a.ingestRecording(Snapshot{CallID: callID, CreatedAt: time.Now(), EndedAt: atPtr(60), IsRecordingStarted: true})
	if exists(full) {
		t.Error("the spool copy survived a booked row")
	}
	if len(r.ledger.marked) != 1 {
		t.Errorf("marked = %v", r.ledger.marked)
	}
}

func TestTheSweepBooksAnOldUnbookedFileAndLeavesAYoungOne(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	old, young := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	_, oldFull := r.spoolFile(t, old, 10*time.Minute)
	_, youngFull := r.spoolFile(t, young, time.Minute)

	r.a.SweepSpool(t.Context())

	if _, ok := r.ledger.rows[old]; !ok {
		t.Fatal("the old recording was not booked")
	}
	if len(r.ledger.marked) != 1 || r.ledger.marked[0] != old {
		t.Errorf("marked = %v", r.ledger.marked)
	}
	if exists(oldFull) {
		t.Error("the spool copy of a booked recording was kept")
	}
	if _, ok := r.ledger.rows[young]; ok || !exists(youngFull) {
		t.Error("a file younger than the threshold was touched")
	}
	if r.logs.count(slog.LevelInfo, "recording booked from the spool") != 1 {
		t.Error("the booking was not logged")
	}
	if r.metrics.unbooked != 0 {
		t.Errorf("unbooked gauge = %d", r.metrics.unbooked)
	}
}

func TestTheSweepSkipsALiveCallAndAnInFlightKey(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	live, busy := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	r.spoolFile(t, live, 10*time.Minute)
	busyKey, _ := r.spoolFile(t, busy, 10*time.Minute)
	r.a.SetLiveCheck(func(id uuid.UUID) bool { return id == live })
	if !r.a.claim(busyKey) {
		t.Fatal("could not claim")
	}

	r.a.SweepSpool(t.Context())

	if r.storage.ingestCount() != 0 || len(r.ledger.rows) != 0 {
		t.Errorf("the sweep touched a live or in-flight recording: %v", r.storage.ingested)
	}
	r.a.unclaim(busyKey)
	r.a.SweepSpool(t.Context())
	if _, ok := r.ledger.rows[busy]; !ok {
		t.Error("the released key was never booked")
	}
	if _, ok := r.ledger.rows[live]; ok {
		t.Error("a live call's recording was booked")
	}
}

func TestTheHangupAndTheSweepNeverBookTheSameKeyTogether(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	callID := uuid.Must(uuid.NewV7())
	r.spoolFile(t, callID, 10*time.Minute)
	r.storage.started = make(chan struct{})
	r.storage.block = make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.a.ingestRecording(Snapshot{CallID: callID, CreatedAt: r.now, EndedAt: atPtr(60), IsRecordingStarted: true})
	}()
	<-r.storage.started // the hangup path is inside Ingest
	r.a.SweepSpool(t.Context())
	close(r.storage.block)
	<-done

	if got := len(r.ledger.rows); got != 1 || r.storage.ingestCount() != 1 {
		t.Errorf("rows = %d, ingests = %d; want one of each", got, r.storage.ingestCount())
	}
}

func TestABookedFileInTheSpoolIsOnlyReleased(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	callID := uuid.Must(uuid.NewV7())
	key, full := r.spoolFile(t, callID, 10*time.Minute)
	r.ledger.rows = map[uuid.UUID]store.Recording{callID: {CallID: callID}}

	r.a.SweepSpool(t.Context())

	if r.storage.ingestCount() != 0 || len(r.ledger.marked) != 0 {
		t.Error("a booked recording was ingested again")
	}
	if len(r.storage.released) != 1 || r.storage.released[0] != key || exists(full) {
		t.Errorf("released = %v", r.storage.released)
	}
}

func TestAFileThatKeepsFailingIsRetriedWithBackoffThenLeftWithOneError(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	callID := uuid.Must(uuid.NewV7())
	_, full := r.spoolFile(t, callID, 10*time.Minute)
	r.ledger.insertErr = errors.New("database is down")

	pass := func(advance time.Duration) {
		r.now = r.now.Add(advance)
		// Keep the file past the age threshold as the clock moves.
		old := r.now.Add(-time.Hour)
		_ = os.Chtimes(full, old, old)
		r.a.SweepSpool(t.Context())
	}

	pass(0)
	if r.storage.ingestCount() != 1 || r.metrics.unbooked != 1 {
		t.Fatalf("first pass: ingests %d, unbooked %d", r.storage.ingestCount(), r.metrics.unbooked)
	}
	pass(30 * time.Second) // inside the first minute of backoff
	if r.storage.ingestCount() != 1 {
		t.Errorf("retried inside the backoff: %d ingests", r.storage.ingestCount())
	}
	if r.metrics.unbooked != 1 {
		t.Errorf("a file waiting out its backoff left the gauge: %d", r.metrics.unbooked)
	}
	pass(31 * time.Second) // a minute after the first failure
	if r.storage.ingestCount() != 2 {
		t.Fatalf("second attempt missing: %d ingests", r.storage.ingestCount())
	}
	pass(90 * time.Second) // the wait is now two minutes
	if r.storage.ingestCount() != 2 {
		t.Errorf("backoff did not double: %d ingests", r.storage.ingestCount())
	}

	for i := 0; i < 20; i++ {
		pass(2 * time.Hour)
	}
	if got := r.storage.ingestCount(); got != spoolMaxAttempts {
		t.Errorf("attempts = %d, want the cap %d", got, spoolMaxAttempts)
	}
	if got := r.logs.count(slog.LevelError, "recording left in the spool; retries are exhausted until the application restarts"); got != 1 {
		t.Errorf("exhaustion errors = %d, want 1", got)
	}
	if r.metrics.failures[ReasonLedger] != spoolMaxAttempts {
		t.Errorf("failures = %v", r.metrics.failures)
	}
	if r.metrics.unbooked != 1 || !exists(full) {
		t.Errorf("the stuck file left the gauge (%d) or the spool", r.metrics.unbooked)
	}
}

func TestSpoolBackoffDoublesAndCaps(t *testing.T) {
	t.Parallel()
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := spoolBackoff(i + 1); got != w {
			t.Errorf("backoff after %d failures = %s, want %s", i+1, got, w)
		}
	}
}

func TestARecordStartMarksTheSnapshotAndStaysAfterStop(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(&capturingPublisher{})
	t.Cleanup(registry.Shutdown)
	callID := uuid.Must(uuid.NewV7())
	if _, err := registry.CreateCall(t.Context(), callID, events.CallTypeInbound, "zh", true, testTime); err != nil {
		t.Fatal(err)
	}
	if err := registry.BindChannel("rec-chan", callID); err != nil {
		t.Fatal(err)
	}
	if err := registry.Do(callID, func(c *Call) { c.AddParty("rec-chan", "18600000000", time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if snap, _ := registry.Snapshot(callID); snap.IsRecordingStarted {
		t.Fatal("a call that has not started recording says it has")
	}
	registry.Dispatch(SwitchEvent{Kind: KindRecordStart, ChannelID: "rec-chan", OccurredAt: time.Now()})
	registry.Dispatch(SwitchEvent{Kind: KindRecordStop, ChannelID: "rec-chan", OccurredAt: time.Now()})
	waitFor(t, func() bool {
		snap, err := registry.Snapshot(callID)
		return err == nil && snap.IsRecordingStarted
	})
	// RECORD_STOP has been processed after RECORD_START (one mailbox, in
	// order): a snapshot taken now still carries the fact.
	if snap, _ := registry.Snapshot(callID); !snap.IsRecordingStarted {
		t.Error("RECORD_STOP cleared the fact")
	}
	if !registry.IsLive(callID) {
		t.Error("IsLive denies a live call")
	}
}

func TestTheHangupBookingBetweenTheSweepsQueryAndClaimMakesOneRow(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	callID := uuid.Must(uuid.NewV7())
	r.spoolFile(t, callID, 10*time.Minute)
	counter := &insertCounter{bookLedger: r.ledger}
	hook := &hookLedger{bookLedger: r.ledger}
	r.a.ledger = &hookedCounter{insertCounter: counter, hook: hook}
	hook.after = func() {
		r.a.ingestRecording(Snapshot{CallID: callID, CreatedAt: r.now, EndedAt: atPtr(60), IsRecordingStarted: true})
	}

	r.a.SweepSpool(t.Context())

	if counter.n != 1 || r.ledger.rowCount() != 1 {
		t.Errorf("inserts = %d, rows = %d; want one", counter.n, r.ledger.rowCount())
	}
	if r.metrics.total() != 0 {
		t.Errorf("a lost race was counted as a failure: %v", r.metrics.failures)
	}
}

func TestTheHangupAfterTheSweepBookedDoesNotBookOrOfferAgain(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	counter := &insertCounter{bookLedger: r.ledger}
	r.a.ledger = counterLedger{counter}
	offer := &countingOffer{}
	r.a.SetRecordingOffer(offer)
	callID := uuid.Must(uuid.NewV7())
	_, full := r.spoolFile(t, callID, 10*time.Minute)

	r.a.SweepSpool(t.Context())
	// The file is back, as if the switch had written it again or the copy
	// had not been released; the hangup path must still book nothing.
	r.spoolFile(t, callID, 10*time.Minute)
	r.a.ingestRecording(Snapshot{CallID: callID, CreatedAt: r.now, EndedAt: atPtr(60), IsRecordingStarted: true})

	if counter.n != 1 || r.ledger.rowCount() != 1 {
		t.Errorf("inserts = %d, rows = %d; want one", counter.n, r.ledger.rowCount())
	}
	if offer.n != 0 {
		t.Errorf("offered %d times", offer.n)
	}
	if exists(full) {
		t.Error("the leftover copy was not released")
	}
	if len(r.ledger.marked) != 1 {
		t.Errorf("marked = %v", r.ledger.marked)
	}
}

// counterLedger is a CDRLedger whose InsertRecording is counted.
type counterLedger struct{ *insertCounter }

// hookedCounter counts inserts and runs the hook on the batched query.
type hookedCounter struct {
	*insertCounter
	hook *hookLedger
}

func (h *hookedCounter) BookedRecordingCallIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	return h.hook.BookedRecordingCallIDs(ctx, ids)
}
