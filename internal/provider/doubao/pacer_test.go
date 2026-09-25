// SPDX-License-Identifier: Apache-2.0

package doubao

import (
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
