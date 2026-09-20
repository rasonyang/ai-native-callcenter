// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

//
// The uplink.
//
// This provider takes audio as fast as it is given, and writes to it have been
// measured blocking for seconds at a time — at whose end is open
// (gemini-findings.md §2.5) — so the queue is deep and it is drained whole. The tests
// drive the cadence a tick at a time — sleeping through fifty real ticks would
// make every one of these a second slower and none of them more certain.
//

// pacedSession is a started session whose pacer only moves when the test says
// so. tick delivers one tick and waits for the pacer to finish with it, which is
// what makes every assertion below exact rather than eventual.
func pacedSession(t *testing.T, f *fakeGemini) (*Session, func()) {
	t.Helper()

	session := testSession(t, f)
	tick := pacedByHand(t, session)
	start(t, session, testConfig())
	return session, tick
}

// pacedByHand takes the cadence off the clock and gives it to the test. It runs
// before the session is started, because that is when the pacer is built.
func pacedByHand(t *testing.T, session *Session) func() {
	t.Helper()

	ticks := make(chan time.Time)
	paced := make(chan struct{})
	session.ticks = ticks
	session.paced = paced

	tick := func() {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(2 * time.Second):
			t.Fatal("the pacer never took the tick")
		}
		select {
		case <-paced:
		case <-time.After(2 * time.Second):
			t.Fatal("the pacer never finished the tick")
		}
	}
	return tick
}

// frameOf is one 20 ms frame of caller audio at 16 kHz, marked so a test can
// tell which one reached the wire. The bytes are silent on purpose: the speech
// detector is listening to every one of these, and a test about the uplink
// should not also be a test about that.
func frameOf(marker byte) []byte {
	frame := make([]byte, 640)
	frame[0] = marker
	return frame
}

// uplinkFrames is every frame of caller audio the provider received, in order
// and decoded, once there are exactly want of them and no more.
func uplinkFrames(t *testing.T, f *fakeGemini, want int) [][]byte {
	t.Helper()
	out := make([][]byte, 0)
	for _, message := range f.awaitMessages("realtimeInput", want) {
		input, ok := message["realtimeInput"].(map[string]any)
		if !ok {
			t.Fatalf("a realtime input frame is %T", message["realtimeInput"])
		}
		blob, ok := input["audio"].(map[string]any)
		if !ok {
			t.Fatalf("an uplink frame carried %T, want audio", input["audio"])
		}
		payload, ok := blob["data"].(string)
		if !ok {
			t.Fatalf("an uplink frame carried %T, want base64 audio", blob["data"])
		}
		audio, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatalf("an uplink frame was not base64: %v", err)
		}
		out = append(out, audio)
	}
	return out
}

// The frame around the audio, byte for byte. The rate in it is the session's
// own: the server resamples whatever it is given, so this has to be true rather
// than convenient.
func TestAnUplinkFrameIsTheBytesAndTheRateTheyAreIn(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick := pacedSession(t, f)

	frame := frameOf(0xE1)
	if err := session.SendAudio(frame); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	tick()

	raw := f.settledFrames(3)[2]
	want := `{"realtimeInput":{"audio":{"mimeType":"audio/pcm;rate=16000","data":"` +
		base64.StdEncoding.EncodeToString(frame) + `"}}}`
	if string(raw) != want {
		t.Errorf("the audio frame was written as\n got: %s\nwant: %s", raw, want)
	}
}

// Audio arrives from the telephone leg in bursts after a jitter gap, and a write
// to this endpoint can block for seconds. The whole backlog goes out on the first
// tick after the socket comes back, in order, with nothing dropped: there is no
// cadence to preserve here, and what dropping would buy is a sentence with a
// hole in it.
func TestABacklogIsWrittenWholeAndInOrder(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick := pacedSession(t, f)

	markers := []byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6}
	for _, marker := range markers {
		if err := session.SendAudio(frameOf(marker)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
	uplinkFrames(t, f, 0)

	tick()

	frames := uplinkFrames(t, f, len(markers))
	for i, want := range markers {
		if frames[i][0] != want {
			t.Errorf("frame %d is marked %#x, want %#x: the backlog goes out in order",
				i, frames[i][0], want)
		}
	}
	stats := session.Stats()
	if stats.FramesSent != int64(len(markers)) || stats.FramesDropped != 0 {
		t.Errorf("the client counted %+v, want six sent and none dropped", stats)
	}
}

// Five seconds of backlog is deeper than any stall measured. Past that the
// oldest frames go, because they are audio the conversation has moved beyond.
func TestPastFiveSecondsTheOldestAudioIsDropped(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick := pacedSession(t, f)

	for range queueDepth + 2 {
		if err := session.SendAudio(frameOf(0xB0)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
	tick()

	uplinkFrames(t, f, queueDepth)
	if got := session.Stats().FramesDropped; got != 2 {
		t.Errorf("the client counted %d frames dropped, want 2", got)
	}
}

// A second of nothing from the telephone leg is the caller's side having
// stopped, which the server is told about so it stops holding audio it will
// never be given the end of. It is said once per quiet, not on every tick of it.
func TestTheEndOfTheStreamIsDeclaredOnceAfterFiftyEmptyTicks(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick := pacedSession(t, f)

	for range idleAfterEmptyTicks - 1 {
		tick()
	}
	f.awaitMessages("realtimeInput", 0)

	tick()
	declared := f.awaitMessages("realtimeInput", 1)
	if got := nested(t, declared[0], "realtimeInput", "audioStreamEnd"); got != true {
		t.Errorf("the frame says %v, want the end of the stream", got)
	}
	if got := string(f.settledFrames(3)[2]); got != streamEndFrame {
		t.Errorf("the end of the stream was written as\n got: %s\nwant: %s",
			got, streamEndFrame)
	}

	for range 20 {
		tick()
	}
	f.awaitMessages("realtimeInput", 1)
	if got := session.Stats().StreamEnds; got != 1 {
		t.Errorf("the client counted %d stream ends, want 1", got)
	}

	// The stream reopens with the next frame of audio, and nothing is written
	// to announce that: there is no frame for it.
	if err := session.SendAudio(frameOf(0xC1)); err != nil {
		t.Fatalf("send audio after the quiet: %v", err)
	}
	tick()
	f.awaitMessages("realtimeInput", 2)
	if got := session.Stats().StreamEnds; got != 1 {
		t.Errorf("the client counted %d stream ends after the resume, want 1", got)
	}
}

// The frame is not ours: the call's converter writes the next one into the same
// buffer, and a queued frame would arrive as whatever came after it.
func TestSendAudioCopiesWhatItIsGiven(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick := pacedSession(t, f)

	frame := frameOf(0xD1)
	if err := session.SendAudio(frame); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	// The caller reuses the buffer before the pacer gets to it.
	for i := range frame {
		frame[i] = 0xFF
	}
	tick()

	frames := uplinkFrames(t, f, 1)
	if frames[0][0] != 0xD1 {
		t.Errorf("the frame is marked %#x, want the bytes as they were handed over",
			frames[0][0])
	}
}

// A write that failed is the caller's to hear about, once. The read loop reports
// the connection itself, so the pacer does not report it twice.
func TestAFailedWriteSurfacesOnTheNextSendAudio(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick := pacedSession(t, f)

	session.conn.Close()
	if err := session.SendAudio(frameOf(0xF1)); err != nil {
		t.Fatalf("the first send after the socket died returned %v, want it queued", err)
	}
	tick()

	err := session.SendAudio(frameOf(0xF2))
	if !errors.Is(err, wsconn.ErrSessionClosed) {
		t.Errorf("the next send returned %v, want what the write failed with", err)
	}
	drainEvents(t, session)
}

//
// Uplink trouble, while it is happening.
//
// Eleven live calls ended with a thousand dropped frames and an i/o timeout in
// the record and not one line about either while the caller was on the phone.
// These are about the log a person reads during a call: a write that blocks is
// the caller being heard late, and a drop is words that are gone.
//

// A socket whose writes take exactly as long as the test says, on a clock the
// test moves. A real stall is seconds long, and waiting one out would make this
// suite slower without making a single assertion more certain.
type timedSocket struct {
	mu   sync.Mutex
	now  time.Time
	cost time.Duration
	send func([]byte) error
}

func newTimedSocket() *timedSocket {
	return &timedSocket{now: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)}
}

// clock is what the pacer reads instead of the wall clock.
func (s *timedSocket) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// costs is how long every write from here on takes.
func (s *timedSocket) costs(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cost = d
}

// write spends the write's time on the clock and then writes for real, so the
// session behaves in every other way as it always does.
func (s *timedSocket) write(data []byte) error {
	s.mu.Lock()
	s.now = s.now.Add(s.cost)
	send := s.send
	s.mu.Unlock()
	return send(data)
}

// loggedLine is one line a session wrote, flattened to what an assertion needs.
type loggedLine struct {
	level   slog.Level
	message string
	attrs   map[string]any
}

// logRecorder keeps every line the session logged.
//
// It is a handler rather than a buffer that is parsed afterwards, because these
// assertions are about which line was written and what number it carried, and
// reading that back out of formatted text is string matching dressed up.
type logRecorder struct {
	mu    sync.Mutex
	lines []loggedLine
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *logRecorder) WithGroup(string) slog.Handler            { return r }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	line := loggedLine{
		level: record.Level, message: record.Message, attrs: map[string]any{}}
	record.Attrs(func(attr slog.Attr) bool {
		line.attrs[attr.Key] = attr.Value.Any()
		return true
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	return nil
}

// saying is every line with this message, in the order they were written.
func (r *logRecorder) saying(message string) []loggedLine {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]loggedLine, 0)
	for _, line := range r.lines {
		if line.message == message {
			out = append(out, line)
		}
	}
	return out
}

// The lines this uplink says about itself, by name rather than by copy.
const (
	lineSlowWrite   = "a write to the provider is slow"
	lineDropping    = "the uplink dropped the caller's audio while a write was blocked"
	lineSessionOver = "provider session finished"
)

// stalledSession is a paced session whose writes cost whatever the test says,
// whose clock the test moves, and whose every log line is kept.
func stalledSession(t *testing.T, f *fakeGemini) (
	*Session, func(), *timedSocket, *logRecorder) {
	t.Helper()

	t.Setenv("GEMINI_API_KEY", "test-key")
	logs := &logRecorder{}
	session, err := newSession(testProfile(f.endpoint()), slog.New(logs))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	socket := newTimedSocket()
	socket.send = session.sendFrame
	session.uplinkWrite = socket.write
	session.clock = socket.clock

	tick := pacedByHand(t, session)
	start(t, session, testConfig())
	return session, tick, socket, logs
}

// sendFrames hands the uplink n frames of caller audio.
func sendFrames(t *testing.T, session *Session, n int) {
	t.Helper()
	for range n {
		if err := session.SendAudio(frameOf(0x77)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
}

func attrOf(t *testing.T, line loggedLine, key string) int64 {
	t.Helper()
	value, ok := line.attrs[key].(int64)
	if !ok {
		t.Fatalf("the line carried %q as %T, want a number: %v",
			key, line.attrs[key], line.attrs)
	}
	return value
}

// A write that blocked is said out loud, with how long it took, while the call
// it is delaying is still going on.
func TestASlowWriteIsReportedWhileTheCallIsStillGoing(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick, socket, logs := stalledSession(t, f)

	socket.costs(800 * time.Millisecond)
	sendFrames(t, session, 1)
	tick()

	said := logs.saying(lineSlowWrite)
	if len(said) != 1 {
		t.Fatalf("a slow write was reported %d times, want once", len(said))
	}
	if said[0].level != slog.LevelWarn {
		t.Errorf("the slow write was logged at %v, want a warning", said[0].level)
	}
	if got := attrOf(t, said[0], "writeMs"); got != 800 {
		t.Errorf("the line says the write took %d ms, want 800", got)
	}

	stats := session.Stats()
	if stats.SlowWrites != 1 || stats.MaxWriteMs != 800 {
		t.Errorf("the client counted %+v, want one slow write of 800 ms", stats)
	}
}

// A stall is dozens of slow writes in a row, and each of them says the same
// thing. One line a second is what makes the log readable while it lasts.
func TestSlowWritesAreReportedAtMostOnceASecond(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick, socket, logs := stalledSession(t, f)

	// Six writes of 600 ms, drained on one tick: the clock passes 600, 1200,
	// 1800, … so the line is due at 600, again at 1800 and again at 3000, and
	// the three writes in between are suppressed.
	socket.costs(600 * time.Millisecond)
	sendFrames(t, session, 6)
	tick()

	said := logs.saying(lineSlowWrite)
	if len(said) != 3 {
		t.Fatalf("six slow writes over 3.6 s were reported %d times, want 3", len(said))
	}
	// The backlog is what the caller is waiting behind: five frames are still
	// unwritten while the first one is on the socket.
	if got := attrOf(t, said[0], "backlogFrames"); got != 5 {
		t.Errorf("the first line says %d frames are waiting, want 5", got)
	}

	stats := session.Stats()
	if stats.SlowWrites != 6 {
		t.Errorf("the client counted %d slow writes, want 6", stats.SlowWrites)
	}
	if stats.MaxBacklogFrames != 5 {
		t.Errorf("the client counted a deepest backlog of %d frames, want 5",
			stats.MaxBacklogFrames)
	}
}

// Dropped frames are the caller's words and they are not coming back. The loss
// happens while a write is blocked and is knowable only when that write returns,
// so that is where it is said — once, in one line, with the block that cost it.
//
// Live calls are what settled the shape: over 31 episodes the old pair of lines
// landed in the same millisecond carrying the same count, because the stall a
// "recovered" line announced the end of was already over when the drop was
// discovered.
func TestDroppedAudioIsReportedWhenTheBlockedWriteReturns(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick, socket, logs := stalledSession(t, f)

	// The socket is blocked, so the queue fills and the oldest frames go. This
	// is the shape of the live call that lost 1877 of 8409 frames.
	const lost = 3
	sendFrames(t, session, queueDepth+lost)
	socket.costs(700 * time.Millisecond)
	tick()

	dropping := logs.saying(lineDropping)
	if len(dropping) != 1 {
		t.Fatalf("the drops were reported %d times, want once", len(dropping))
	}
	if dropping[0].level != slog.LevelWarn {
		t.Errorf("the drops were logged at %v, want a warning", dropping[0].level)
	}
	if got := attrOf(t, dropping[0], "blockedMs"); got != 700 {
		t.Errorf("the line says the socket was blocked %d ms, want 700", got)
	}
	if got := attrOf(t, dropping[0], "framesDropped"); got != lost {
		t.Errorf("the line says %d frames were dropped, want %d", got, lost)
	}
	// The backlog at that moment: everything handed over but this one frame,
	// less what the queue threw away.
	if got := attrOf(t, dropping[0], "backlogFrames"); got != queueDepth-1 {
		t.Errorf("the line says %d frames are waiting, want %d", got, queueDepth-1)
	}

	// The socket comes back. There is nothing left to say: the episode was
	// reported in full by the write that lived through it.
	socket.costs(0)
	sendFrames(t, session, 1)
	tick()

	if got := logs.saying(lineDropping); len(got) != 1 {
		t.Errorf("the drops were reported %d times once the socket came back, want once",
			len(got))
	}
}

// Each blocked write names the frames it lost and nobody else's. A stall is one
// write after another, and a line that repeated the running total would make one
// caller's lost sentence look like several.
func TestEachBlockedWriteNamesOnlyTheFramesItLost(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick, socket, logs := stalledSession(t, f)

	socket.costs(700 * time.Millisecond)
	for range 2 {
		// The queue is drained whole on each tick, so it is filled again — and
		// overflowed again — before the next one.
		sendFrames(t, session, queueDepth+1)
		tick()
	}

	dropping := logs.saying(lineDropping)
	if len(dropping) != 2 {
		t.Fatalf("two episodes were reported in %d lines, want 2", len(dropping))
	}
	for i, line := range dropping {
		if got := attrOf(t, line, "framesDropped"); got != 1 {
			t.Errorf("episode %d says %d frames were dropped, want the 1 it lost",
				i, got)
		}
	}
	if got := session.Stats().FramesDropped; got != 2 {
		t.Errorf("the client counted %d frames dropped over both episodes, want 2", got)
	}
}

// The closing line is what an operator reads about a call that is already over,
// and a call whose caller was heard seconds late has these three numbers in it.
func TestTheClosingLineSaysWhatTheUplinkCost(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick, socket, logs := stalledSession(t, f)

	socket.costs(900 * time.Millisecond)
	sendFrames(t, session, 2)
	tick()

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	drainEvents(t, session)

	finished := logs.saying(lineSessionOver)
	if len(finished) != 1 {
		t.Fatalf("the session finished %d times, want once", len(finished))
	}
	if got := attrOf(t, finished[0], "slowWrites"); got != 2 {
		t.Errorf("the closing line counts %d slow writes, want 2", got)
	}
	if got := attrOf(t, finished[0], "maxWriteMs"); got != 900 {
		t.Errorf("the closing line counts %d ms as the worst write, want 900", got)
	}
	if got := attrOf(t, finished[0], "maxBacklogFrames"); got != 1 {
		t.Errorf("the closing line counts a deepest backlog of %d frames, want 1", got)
	}
}

// The normal case says nothing at all: a healthy uplink is not news, and a log
// that reports every write is one nobody reads.
func TestASocketThatKeepsUpSaysNothing(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, tick, socket, logs := stalledSession(t, f)

	socket.costs(20 * time.Millisecond)
	for range 5 {
		sendFrames(t, session, 3)
		tick()
	}

	for _, message := range []string{lineSlowWrite, lineDropping} {
		if got := logs.saying(message); len(got) != 0 {
			t.Errorf("a healthy uplink logged %q %d times", message, len(got))
		}
	}
	stats := session.Stats()
	if stats.SlowWrites != 0 || stats.MaxWriteMs != 0 || stats.MaxBacklogFrames != 0 {
		t.Errorf("a healthy uplink counted %+v, want nothing", stats)
	}
}

// Whatever is queued when a call ends is audio from a call that has ended.
func TestQueuedAudioIsDiscardedRatherThanFlushed(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session, _ := pacedSession(t, f)
	before := len(f.settledFrames(2))

	for range 10 {
		if err := session.SendAudio(frameOf(0x01)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	f.assertNothingFollowed(before)
	drainEvents(t, session)
}
