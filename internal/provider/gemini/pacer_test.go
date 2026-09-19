// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

//
// The uplink.
//
// This provider takes audio as fast as it is given and its sockets stall for
// seconds at a time, so the queue is deep and it is drained whole. The tests
// drive the cadence a tick at a time — sleeping through fifty real ticks would
// make every one of these a second slower and none of them more certain.
//

// pacedSession is a started session whose pacer only moves when the test says
// so. tick delivers one tick and waits for the pacer to finish with it, which is
// what makes every assertion below exact rather than eventual.
func pacedSession(t *testing.T, f *fakeGemini) (*Session, func()) {
	t.Helper()

	session := testSession(t, f)
	ticks := make(chan time.Time)
	paced := make(chan struct{})
	session.ticks = ticks
	session.paced = paced
	start(t, session, testConfig())

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
	return session, tick
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

// Audio arrives from the telephone leg in bursts after a jitter gap, and this
// provider's sockets stall for seconds. The whole backlog goes out on the first
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
