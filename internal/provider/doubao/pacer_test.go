// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

//
// The uplink.
//
// This provider reads the upstream stream as a clock: too fast or too slow is
// an error, and a silence nobody declared stops the model answering at all. So
// the cadence is the client's own and the tests drive it a tick at a time —
// sleeping through 25 real ticks would make every one of these a half-second
// slower and none of them more certain.
//

// pacedSession is a started session whose pacer only moves when the test says
// so. tick delivers one tick and waits for the pacer to finish with it, which
// is what makes every assertion below exact rather than eventual.
func pacedSession(t *testing.T, f *fakeDoubao) (*Session, func()) {
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

// frameOf is one 20 ms frame of caller audio, marked so a test can tell which
// one reached the wire.
func frameOf(marker byte) []byte {
	frame := make([]byte, 640)
	frame[0] = marker
	return frame
}

// appendedFrames is every frame of caller audio the provider received, in
// order and decoded, once there are exactly want of them and no more.
func appendedFrames(t *testing.T, f *fakeDoubao, want int) [][]byte {
	t.Helper()
	out := make([][]byte, 0)
	for _, message := range f.awaitMessages("input_audio_buffer.append", want) {
		payload, ok := message["audio"].(string)
		if !ok {
			t.Fatalf("an append frame carried %T, want base64 audio", message["audio"])
		}
		audio, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatalf("an append frame was not base64: %v", err)
		}
		out = append(out, audio)
	}
	return out
}

func TestOneTickSendsOneFrameAndNoMore(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	if err := session.SendAudio(frameOf(0xA1)); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	tick()

	frames := appendedFrames(t, f, 1)
	if len(frames[0]) != 640 || frames[0][0] != 0xA1 {
		t.Errorf("the frame is %d bytes marked %#x, want the 640 that were sent",
			len(frames[0]), frames[0][0])
	}

	// An empty queue writes nothing at all: a repeat would be audio the caller
	// never made.
	tick()
	appendedFrames(t, f, 1)
	if got := session.Stats().FramesSent; got != 1 {
		t.Errorf("the client counted %d frames sent, want 1", got)
	}
}

// Audio arrives from the telephone leg in bursts after a jitter gap. The
// oldest frames are the ones the caller has already moved past, so they are
// what goes — and the catch-up must never be written as a burst, which is the
// other half of what this provider calls a pacing error.
func TestALagBurstDropsTheOldestAndIsNeverWrittenAsABurst(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	for _, marker := range []byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6} {
		if err := session.SendAudio(frameOf(marker)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
	appendedFrames(t, f, 0)

	for range 4 {
		tick()
	}

	frames := appendedFrames(t, f, 3)
	for i, want := range []byte{0xA4, 0xA5, 0xA6} {
		if frames[i][0] != want {
			t.Errorf("frame %d is marked %#x, want %#x: the oldest are the ones to drop",
				i, frames[i][0], want)
		}
	}
	if got := session.Stats().FramesDropped; got != 3 {
		t.Errorf("the client counted %d frames dropped, want 3", got)
	}
}

// The model reads the uplink as a keepalive: stop feeding it without saying so
// and it stops answering. A hold has to be declared once, not on every tick of
// it.
func TestSilenceIsDeclaredOnceAfterTwentyFiveEmptyTicks(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	for range muteAfterEmptyTicks - 1 {
		tick()
	}
	f.awaitMessages("input_audio_mute.commit", 0)

	tick()
	f.awaitMessages("input_audio_mute.commit", 1)

	for range 10 {
		tick()
	}
	f.awaitMessages("input_audio_mute.commit", 1)
	f.awaitMessages("input_audio_buffer.append", 0)
	if got := session.Stats().Mutes; got != 1 {
		t.Errorf("the client counted %d mutes, want 1", got)
	}
}

// Whatever was in the queue when the hold began is audio from before the
// silence. Sending it on resume would play the caller a second of their own
// past.
func TestNothingQueuedBeforeTheHoldSurvivesIt(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	if err := session.SendAudio(frameOf(0xB1)); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	session.pacer.muteNow()
	f.awaitMessages("input_audio_mute.commit", 1)

	if err := session.SendAudio(frameOf(0xB2)); err != nil {
		t.Fatalf("send audio after the hold: %v", err)
	}
	tick()

	frames := appendedFrames(t, f, 1)
	if frames[0][0] != 0xB2 {
		t.Errorf("the frame is marked %#x, want the live one", frames[0][0])
	}
}

// Resuming is a frame the provider has to be ready for: the unmute goes first,
// on the same tick, and the audio follows it.
func TestResumingSaysSoBeforeTheFirstFrame(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	for range muteAfterEmptyTicks {
		tick()
	}
	f.awaitMessages("input_audio_mute.commit", 1)

	if err := session.SendAudio(frameOf(0xC1)); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	tick()

	f.awaitFrames(4)
	types := f.frameTypes()
	want := []string{"session.create", "input_audio_mute.commit",
		"input_audio_unmute.commit", "input_audio_buffer.append"}
	if len(types) != len(want) {
		t.Fatalf("the provider saw %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("the provider saw %v, want %v", types, want)
		}
	}
	if got := session.Stats().Unmutes; got != 1 {
		t.Errorf("the client counted %d unmutes, want 1", got)
	}
}

// Five holds and five resumes were measured harmless; what matters is that
// each is said exactly once.
func TestFiveRapidHoldsAndResumes(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	for cycle := range 5 {
		for range muteAfterEmptyTicks {
			tick()
		}
		if err := session.SendAudio(frameOf(byte(0xD0 + cycle))); err != nil {
			t.Fatalf("send audio in cycle %d: %v", cycle, err)
		}
		tick()
	}

	f.awaitMessages("input_audio_mute.commit", 5)
	f.awaitMessages("input_audio_unmute.commit", 5)
	frames := appendedFrames(t, f, 5)
	for i, frame := range frames {
		if frame[0] != byte(0xD0+i) {
			t.Errorf("frame %d is marked %#x, want %#x", i, frame[0], 0xD0+i)
		}
	}
	stats := session.Stats()
	if stats.Mutes != 5 || stats.Unmutes != 5 || stats.FramesSent != 5 {
		t.Errorf("the client counted %+v, want five of each", stats)
	}
}

// Audio is the one upstream event that carries no id of its own: fifty of them
// a second would make a provider-side trace unreadable, and there is nothing
// to match them against.
func TestAnAudioFrameIsTheBytesAndNothingElse(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	frame := frameOf(0xE1)
	if err := session.SendAudio(frame); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	tick()

	raw := f.settledFrames(2)[1]
	want := `{"type":"input_audio_buffer.append","audio":"` +
		base64.StdEncoding.EncodeToString(frame) + `"}`
	if string(raw) != want {
		t.Errorf("the audio frame was written as\n got: %s\nwant: %s", raw, want)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the audio frame is not JSON: %v", err)
	}
	if _, isPresent := decoded["event_id"]; isPresent {
		t.Error("the audio frame carried an event id")
	}
}

// A write that failed is the caller's to hear about, once. The read loop
// reports the connection itself, so the pacer does not report it twice.
func TestAFailedWriteSurfacesOnTheNextSendAudio(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
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

// Once the session is stopping there is no uplink at all, whatever the call
// actor still has in hand.
func TestNoAudioIsTakenOnceTheSessionIsStopping(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, _ := pacedSession(t, f)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := session.SendAudio(frameOf(0x01)); !errors.Is(err, wsconn.ErrSessionClosed) {
		t.Errorf("audio after close returned %v, want the closed sentinel", err)
	}
	f.awaitMessages("input_audio_buffer.append", 0)
}
