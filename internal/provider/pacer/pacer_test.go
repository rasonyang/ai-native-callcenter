// SPDX-License-Identifier: Apache-2.0

package pacer

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The cadence, a tick at a time.
//
// Nothing here sleeps through a cadence. The clock is the test's: one tick is
// delivered, the pacer says it has dealt with it, and only then is anything
// asserted — which is what makes every assertion below exact rather than
// eventual, and what keeps a test of half a second's silence from taking half a
// second.
//
// Every assertion about what reached the wire comes with one about how much did.
// A frame that is right and written twice is a different fault from one that is
// wrong, and on an engine that reads its uplink as a clock it is the worse of
// the two.

// wire is the socket's place: it records every write attempt in order, and
// returns whatever failure the test has armed it with.
type wire struct {
	mu      sync.Mutex
	entries []string
	err     error
}

func (w *wire) write(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, string(data))
	return w.err
}

// failWith arms the socket. Later writes are still recorded, because a pacer
// that stops pacing on a failed write would leave the cadence broken for a
// session that has not ended.
func (w *wire) failWith(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

func (w *wire) written() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.entries)
}

// uplink is a pacer whose clock the test holds.
type uplink struct {
	*Pacer
	t        *testing.T
	wire     *wire
	ticks    chan time.Time
	ticked   chan struct{}
	stopping chan struct{}

	idles      atomic.Int64
	resumes    atomic.Int64
	isTickerUp atomic.Bool
}

// newUplink starts a pacer on the test's own clock. The hooks write the frames a
// client of this package would write — this one calls a silence a "hold" and the
// end of it a "resume" — so the order they reach the wire in is the order the
// assertions read.
func newUplink(t *testing.T, cfg Config) *uplink {
	t.Helper()

	u := &uplink{
		t:        t,
		wire:     &wire{},
		ticks:    make(chan time.Time),
		ticked:   make(chan struct{}),
		stopping: make(chan struct{}),
	}
	u.isTickerUp.Store(true)

	cfg.Ticks = u.ticks
	cfg.Stopping = u.stopping
	cfg.Ticked = u.ticked
	cfg.Write = u.wire.write
	if cfg.Encode == nil {
		cfg.Encode = func(frame []byte) []byte { return append([]byte("frame "), frame...) }
	}
	if cfg.OnIdle == nil {
		cfg.OnIdle = func() {
			u.idles.Add(1)
			u.assertNothingIsBuffered()
			u.Write([]byte("hold"))
		}
	}
	if cfg.OnResume == nil {
		cfg.OnResume = func() {
			u.resumes.Add(1)
			u.Write([]byte("resume"))
		}
	}
	cfg.StopTicker = func() { u.isTickerUp.Store(false) }

	u.Pacer = New(cfg)
	go u.Pacer.Run()
	t.Cleanup(u.stop)
	return u
}

// tick delivers one tick and waits for the pacer to finish with it.
func (u *uplink) tick() {
	u.t.Helper()
	select {
	case u.ticks <- time.Now():
	case <-time.After(2 * time.Second):
		u.t.Fatal("the pacer never took the tick")
	}
	select {
	case <-u.ticked:
	case <-time.After(2 * time.Second):
		u.t.Fatal("the pacer never finished the tick")
	}
}

// stop closes the session and waits for the goroutine to go. It is idempotent,
// because every test ends with it whether or not it asked for it.
func (u *uplink) stop() {
	u.t.Helper()
	select {
	case <-u.stopping:
	default:
		close(u.stopping)
	}
	select {
	case <-u.Stopped():
	case <-time.After(2 * time.Second):
		u.t.Fatal("the uplink goroutine never ended")
	}
}

// push queues one frame, marked so a test can tell which one reached the wire.
func (u *uplink) push(marker string) { u.Push([]byte(marker)) }

// assertNothingIsBuffered is the invariant a hold rests on, checked from inside
// the hook that declares one: the queue is empty before the client is told, and
// the lock is free — taking it here would deadlock outright if it were not.
func (u *uplink) assertNothingIsBuffered() {
	u.mu.Lock()
	queued := len(u.queue)
	u.mu.Unlock()
	if queued != 0 {
		u.t.Errorf("%d frames were still buffered when the quiet was declared", queued)
	}
}

// wrote asserts the wire saw exactly these, in this order.
func (u *uplink) wrote(want ...string) {
	u.t.Helper()
	got := u.wire.written()
	if !slices.Equal(got, want) {
		u.t.Fatalf("the wire saw %v, want %v", got, want)
	}
}

func (u *uplink) counted(sent, dropped int64) {
	u.t.Helper()
	stats := u.Stats()
	if stats.FramesSent != sent || stats.FramesDropped != dropped {
		u.t.Errorf("the pacer counted %+v, want %d sent and %d dropped",
			stats, sent, dropped)
	}
}

// One frame per tick is what an engine reading its uplink as a clock is owed,
// and an empty tick owes it nothing: a repeat would be audio the caller never
// made.
func TestOneTickWritesOneFrameAndNoMore(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1})

	u.push("01")
	u.tick()
	u.wrote("frame 01")

	u.tick()
	u.wrote("frame 01")
	u.counted(1, 0)
}

// Audio arrives from the telephone leg in bursts after a jitter gap. The oldest
// frames are the ones the caller has already moved past, so they are what goes —
// and the catch-up is never written as a burst, which is the other half of what
// such an engine calls a pacing error.
func TestAFullQueueDropsItsOldestAndNeverCatchesUpInABurst(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1})

	for _, marker := range []string{"01", "02", "03", "04", "05", "06"} {
		u.push(marker)
	}
	u.wrote()

	u.tick()
	u.wrote("frame 04")
	u.tick()
	u.tick()
	u.wrote("frame 04", "frame 05", "frame 06")

	u.tick()
	u.wrote("frame 04", "frame 05", "frame 06")
	u.counted(3, 3)
}

// A client whose engine takes audio faster than real time keeps its queue to
// survive a write that stalled, not to pace anything. Holding the backlog back a
// frame per tick would only make the stall longer, so the whole queue goes at
// once — in the order it arrived.
func TestDrainingEverythingClearsTheBacklogOnOneTickInOrder(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 8, MaxPerTick: DrainEverything})

	for _, marker := range []string{"01", "02", "03", "04", "05"} {
		u.push(marker)
	}
	u.tick()
	u.wrote("frame 01", "frame 02", "frame 03", "frame 04", "frame 05")
	u.counted(5, 0)

	// The queue is empty again, so the next tick has nothing to drain.
	u.tick()
	u.wrote("frame 01", "frame 02", "frame 03", "frame 04", "frame 05")

	u.push("06")
	u.tick()
	u.wrote("frame 01", "frame 02", "frame 03", "frame 04", "frame 05", "frame 06")
	u.counted(6, 0)
}

// A negative count means the same thing as DrainEverything rather than nothing
// at all: a pacer that wrote no frames would be a call in silence.
func TestANonsensicalFrameCountDrainsRatherThanStalls(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 4, MaxPerTick: -1})

	u.push("01")
	u.push("02")
	u.tick()
	u.wrote("frame 01", "frame 02")
	u.counted(2, 0)
}

// A queue has to hold something. A client that named no depth gets one frame,
// not a pacer that drops everything handed to it.
func TestADepthlessQueueStillHoldsOneFrame(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{MaxPerTick: 1})

	u.push("01")
	u.push("02")
	u.tick()
	u.wrote("frame 02")
	u.counted(1, 1)
}

// The quiet is the client's to announce, once, and the end of it has to reach
// the engine ahead of the audio that ended it.
func TestTheQuietIsDeclaredOnceAndTheResumeGoesFirst(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1, IdleTicks: 3})

	u.push("01")
	u.tick()

	// The run of empty ticks starts over once a frame goes out, so the quiet is
	// three ticks from here and not from the start.
	u.tick()
	u.tick()
	u.wrote("frame 01")
	u.tick()
	u.wrote("frame 01", "hold")

	// However long it lasts, it is said once.
	for range 10 {
		u.tick()
	}
	u.wrote("frame 01", "hold")

	u.push("02")
	u.tick()
	u.wrote("frame 01", "hold", "resume", "frame 02")

	if u.idles.Load() != 1 || u.resumes.Load() != 1 {
		t.Errorf("the client was told of %d quiets and %d resumes, want one of each",
			u.idles.Load(), u.resumes.Load())
	}
	u.counted(2, 0)
}

// Declaring the quiet early is the same declaration: whatever was queued when it
// began is audio from before the silence, and writing it on resume would play
// the caller a moment of their own past.
func TestNothingQueuedBeforeTheQuietSurvivesIt(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1, IdleTicks: 3})

	u.push("01")
	u.Idle()
	u.wrote("hold")

	// Asking twice is noise, not a second hold.
	u.Idle()
	u.wrote("hold")

	u.push("02")
	u.tick()
	u.wrote("hold", "resume", "frame 02")
	if u.idles.Load() != 1 {
		t.Errorf("the client was told of %d quiets, want one", u.idles.Load())
	}
	u.counted(1, 0)
}

// A client with nothing to say about silence is never asked about it. On the
// protocol this repository had first, an uplink that goes quiet is simply quiet.
func TestAClientThatNamesNoQuietIsNeverToldOfOne(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1})

	for range 50 {
		u.tick()
	}
	u.wrote()
	if u.idles.Load() != 0 {
		t.Errorf("the client was told of %d quiets, want none", u.idles.Load())
	}
	u.counted(0, 0)
}

// A write that failed is recorded rather than reported: the client returns it to
// whoever offers the next frame, and its read loop reports the connection
// itself, so one broken socket does not fail the call twice.
func TestAFailedWriteIsCapturedAndKeptForTheClientToReport(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1, IdleTicks: 3})
	brokenSocket := errors.New("the socket is gone")

	u.push("01")
	u.tick()
	if err := u.WriteError(); err != nil {
		t.Fatalf("a healthy write recorded %v", err)
	}

	u.wire.failWith(brokenSocket)
	u.push("02")
	u.tick()
	if err := u.WriteError(); !errors.Is(err, brokenSocket) {
		t.Errorf("the pacer recorded %v, want what the write failed with", err)
	}

	// The cadence carries on: the session has not ended, and the frames the
	// client asked for are still due.
	u.wire.failWith(nil)
	u.push("03")
	u.tick()
	u.wrote("frame 01", "frame 02", "frame 03")
	if err := u.WriteError(); !errors.Is(err, brokenSocket) {
		t.Errorf("the failure was forgotten (%v); it is the client's to report", err)
	}
	u.counted(3, 0)
}

// A frame the client writes on its own account fails the same way. It is one
// socket, and a hold nobody could send is the same broken session as a frame of
// audio nobody could send.
func TestAFailedHoldIsCapturedToo(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1, IdleTicks: 1})
	brokenSocket := errors.New("the socket is gone")

	u.wire.failWith(brokenSocket)
	u.tick()
	u.wrote("hold")
	if err := u.WriteError(); !errors.Is(err, brokenSocket) {
		t.Errorf("the pacer recorded %v, want what the write failed with", err)
	}
}

// Once the session is stopping there is no uplink at all. Queued frames are
// discarded rather than flushed: they are audio from a call that has ended.
func TestStoppingDiscardsWhatWasQueuedAndWritesNothingMore(t *testing.T) {
	t.Parallel()
	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1})

	u.push("01")
	u.push("02")
	u.stop()

	u.wrote()
	u.counted(0, 0)
	if u.isTickerUp.Load() {
		t.Error("the ticker is still running; the pacer owns it for exactly as long as it runs")
	}

	// A frame offered after the stop goes nowhere, because nothing is left to
	// take it off the queue.
	u.push("03")
	u.wrote()
}

// Nothing this package starts may outlive the stop that ended it. A leaked pacer
// goes on writing audio into a session that has ended, which shows up not as a
// failing assertion but as a process that grows over a day of calls.
//
// The check is a goroutine-stack diff rather than a dependency, and it names the
// one owner this package has.
func TestTheUplinkGoroutineDoesNotOutliveTheStop(t *testing.T) {
	defer noPacersLeft(t)()

	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1, IdleTicks: 2})
	u.push("01")
	u.tick()
	u.tick()
	u.tick()
	u.stop()
}

// A stop that lands while a tick is being handed back is still a stop. Without
// the second half of that handshake the goroutine would sit holding a channel
// nobody will ever read.
func TestAStopDuringTheTickHandshakeStillEndsTheGoroutine(t *testing.T) {
	defer noPacersLeft(t)()

	u := newUplink(t, Config{Depth: 3, MaxPerTick: 1})
	u.push("01")

	// Take the tick but never the acknowledgement of it.
	select {
	case u.ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("the pacer never took the tick")
	}
	u.stop()
	u.wrote("frame 01")
}
