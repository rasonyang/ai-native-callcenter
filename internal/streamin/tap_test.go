// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

// fakeSwitch records the tap commands the adapter would have sent, and can be
// told to refuse one. Command *strings* are the adapter's business; what this
// asserts is which channel was asked to do what, and in what order.
type fakeSwitch struct {
	mu       sync.Mutex
	started  []string
	stopped  []string
	paused   []string
	resumed  []string
	urls     map[string]string
	down     bool
	startErr error
	// onStart runs with the stream URL before the command returns. A test
	// uses it to dial the ingest back *inside* the attach, which is the live
	// order the tap has to survive (#287).
	onStart func(wsURL string)
}

func (f *fakeSwitch) StartAudioStream(channelID, wsURL string, _ int, _ string) error {
	f.mu.Lock()
	if f.urls == nil {
		f.urls = map[string]string{}
	}
	f.urls[channelID] = wsURL
	err, hook := f.startErr, f.onStart
	f.mu.Unlock()
	if hook != nil {
		hook(wsURL)
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.started = append(f.started, channelID)
	f.mu.Unlock()
	return nil
}

// dialURL is what the switch was asked to dial for a channel. The module
// presents exactly this URL, token included, so a test dials it too.
func (f *fakeSwitch) dialURL(channelID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.urls[channelID]
}

func (f *fakeSwitch) StopAudioStream(channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, channelID)
	return nil
}

func (f *fakeSwitch) PauseAudioStream(channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, channelID)
	return nil
}

func (f *fakeSwitch) ResumeAudioStream(channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, channelID)
	return nil
}

func (f *fakeSwitch) IsUp() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.down
}

func (f *fakeSwitch) snapshot() (started, stopped, paused, resumed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), append([]string(nil), f.stopped...),
		append([]string(nil), f.paused...), append([]string(nil), f.resumed...)
}

// tapRig builds a Tap over a real ingest Server — real listener, real
// websockets — because Attach mints a token, registers an expectation on it and
// a test may dial the very URL the switch was given. A fake there would be
// testing the fake.
type tapRig struct {
	tap    *Tap
	sw     *fakeSwitch
	srv    *Server
	callID uuid.UUID
	pub    *statePub
}

func tapRigFor(t *testing.T) *tapRig {
	t.Helper()
	pub := &statePub{}
	reg := transcript.NewRegistry(nil, pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	callID := uuid.Must(uuid.NewV7())
	reg.For(callID, "INBOUND", time.Now())
	t.Cleanup(func() { reg.Close(callID) })

	srv, err := New(Config{
		Addr: "127.0.0.1:0", Secret: []byte("k"),
		NewSession:  func() (transcribe.Session, error) { return newStallSession(), nil },
		Transcripts: recordingTranscripts{reg: reg, callID: callID},
		Logger:      nopLogger{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	sw := &fakeSwitch{urls: map[string]string{}}
	tap := NewTap(srv, sw, "ws://"+srv.Addr()+StreamPath, 24000, time.Minute, nopLogger{})
	return &tapRig{tap: tap, sw: sw, srv: srv, callID: callID, pub: pub}
}

// tokenOn is the token the channel's live attachment handed the switch.
func (t *Tap) tokenOn(channelID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tokens[t.current[channelID]]
}

func tapFixture(t *testing.T) (*Tap, *fakeSwitch, uuid.UUID) {
	t.Helper()
	rig := tapRigFor(t)
	return rig.tap, rig.sw, rig.callID
}

// One bug per channel. The module enforces it too, and asking twice would
// leave a stream nothing owns.
func TestAttachingTwiceStartsOneStream(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")
	tap.Attach(callID, nil, nil, "chan-a", "en")

	if started, _, _, _ := sw.snapshot(); len(started) != 1 {
		t.Fatalf("started %v, want exactly one", started)
	}
}

// A refused attach must leave nothing behind, or the channel can never be
// tapped again for the life of the call — and nothing may be waiting to report
// a stream that the switch never even read.
func TestARefusedAttachIsNotRemembered(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = errors.New("-ERR no such channel")
	rig.sw.mu.Unlock()

	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	if rig.srv.isExpectingAny() {
		t.Error("a refused attach left a connect-grace watchdog armed")
	}
	rig.sw.mu.Lock()
	rig.sw.startErr = nil
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

	if started, _, _, _ := rig.sw.snapshot(); len(started) != 1 {
		t.Fatalf("started %v, want the retry to have succeeded", started)
	}
}

// A URL the switch cannot dial is a configuration fault, not a call fault: it
// is refused before any command is sent.
func TestAnUnusableStreamURLNeverReachesTheSwitch(t *testing.T) {
	t.Parallel()
	_, sw, callID := tapFixture(t)
	reg := transcript.NewRegistry(nil, &statePub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg.For(callID, "INBOUND", time.Now())
	t.Cleanup(func() { reg.Close(callID) })
	srv, _ := New(Config{
		Addr: "127.0.0.1:0", Secret: []byte("k"),
		NewSession:  func() (transcribe.Session, error) { return newStallSession(), nil },
		Transcripts: recordingTranscripts{reg: reg, callID: callID},
		Logger:      nopLogger{},
	})
	tap := NewTap(srv, sw, "http://127.0.0.1:9999/stream", 24000, time.Minute, nopLogger{})

	tap.Attach(callID, nil, nil, "chan-a", "en")

	if started, _, _, _ := sw.snapshot(); len(started) != 0 {
		t.Fatalf("started %v over a non-websocket url", started)
	}
}

// Hold is a private side-call and music, neither of which is this
// conversation, so the tap pauses rather than recording it.
func TestPauseAndResumeReachTheSwitchWhileTapped(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	if err := tap.Pause("chan-a"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := tap.Resume("chan-a"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	_, _, paused, resumed := sw.snapshot()
	if len(paused) != 1 || paused[0] != "chan-a" {
		t.Errorf("paused %v, want chan-a", paused)
	}
	if len(resumed) != 1 || resumed[0] != "chan-a" {
		t.Errorf("resumed %v, want chan-a", resumed)
	}
}

// Detach stops the stream, and only for a channel that had one: a hangup
// arrives for every leg of every call, and most of them were never tapped.
func TestDetachStopsATappedChannelAndIgnoresAnUntappedOne(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	tap.Detach("chan-b") // the caller's leg: never tapped
	tap.Detach("chan-a")

	_, stopped, _, _ := sw.snapshot()
	if len(stopped) != 1 || stopped[0] != "chan-a" {
		t.Fatalf("stopped %v, want chan-a alone", stopped)
	}
}

// A stream URL keeps whatever the deployment already put on it.
func TestWithTokenPreservesExistingQuery(t *testing.T) {
	t.Parallel()
	got, err := withToken("ws://host:9/stream?x=1", "tok")
	if err != nil {
		t.Fatalf("withToken: %v", err)
	}
	if !strings.Contains(got, "x=1") || !strings.Contains(got, "t=tok") {
		t.Errorf("url = %s, want both parameters", got)
	}
}

// --- B2: an attachment is not a channel -------------------------------------

// The convergence guarantee. CHANNEL_UNBRIDGE and CHANNEL_HANGUP are not owed
// to us — a leg transferred away, an ESL link that reconnected across the
// hangup — and without this the module keeps pumping a finished call's audio
// for the life of the process, with nothing reporting a fault.
func TestDetachCallStopsATapNoSwitchEventEverEnded(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	tap.DetachCall(callID) // no UNBRIDGE, no HANGUP: the call simply ended

	_, stopped, _, _ := sw.snapshot()
	if len(stopped) != 1 || stopped[0] != "chan-a" {
		t.Fatalf("stopped %v, want the tap stopped by the call's own retirement", stopped)
	}
}

// It will usually run second, after the switch event already stopped the tap.
func TestDetachCallIsIdempotent(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	tap.Detach("chan-a")
	tap.DetachCall(callID)
	tap.DetachCall(callID)

	if _, stopped, _, _ := sw.snapshot(); len(stopped) != 1 {
		t.Fatalf("stopped %v, want exactly one stop for one tap", stopped)
	}
}

// A call's taps are its own. Retiring one call must not touch another's, which
// is only expressible because the key carries the call.
func TestDetachCallLeavesAnotherCallsTapAlone(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	other := uuid.Must(uuid.NewV7())
	tap.Attach(callID, nil, nil, "chan-a", "en")
	tap.Attach(other, nil, nil, "chan-b", "en")

	tap.DetachCall(other)

	_, stopped, _, _ := sw.snapshot()
	if len(stopped) != 1 || stopped[0] != "chan-b" {
		t.Fatalf("stopped %v, want only the retired call's tap", stopped)
	}
	if err := tap.Pause("chan-a"); err != nil {
		t.Errorf("the surviving tap answered %v, want it still live", err)
	}
}

// No silent action on a dead tap: the command was not carried out and the
// caller is told which kind of nothing happened.
func TestPauseAndResumeReportAChannelWithNoTap(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")
	tap.Detach("chan-a")

	if err := tap.Pause("chan-a"); !errors.Is(err, telephony.ErrNoTap) {
		t.Errorf("pause on a retired tap = %v, want ErrNoTap", err)
	}
	if err := tap.Resume("chan-a"); !errors.Is(err, telephony.ErrNoTap) {
		t.Errorf("resume on a retired tap = %v, want ErrNoTap", err)
	}
	if err := tap.Pause("never-tapped"); !errors.Is(err, telephony.ErrNoTap) {
		t.Errorf("pause on an untapped channel = %v, want ErrNoTap", err)
	}
	if _, _, paused, resumed := sw.snapshot(); len(paused) != 0 || len(resumed) != 0 {
		t.Errorf("paused %v resumed %v, want no command against a tap that is gone",
			paused, resumed)
	}
}

// A tap that exists but cannot be commanded is a different answer from one
// that does not exist, and the two must not collapse into each other.
func TestPauseReportsTheSwitchBeingDownSeparately(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")
	sw.mu.Lock()
	sw.down = true
	sw.mu.Unlock()

	err := tap.Pause("chan-a")
	if errors.Is(err, telephony.ErrNoTap) || !errors.Is(err, ErrSwitchDown) {
		t.Errorf("pause with the link down = %v, want ErrSwitchDown", err)
	}
}

// Two calls turning out to be one conversation renames a leg's call under it.
// The old stream reports into the absorbed call's transcript actor, which is
// retired without ever finishing, so it has to be stopped and re-attached
// under the call the leg now belongs to.
func TestReattachingUnderANewCallStopsTheOldStream(t *testing.T) {
	t.Parallel()
	tap, sw, absorbed := tapFixture(t)
	kept := uuid.Must(uuid.NewV7())

	tap.Attach(absorbed, nil, nil, "chan-a", "en")
	tap.Attach(kept, nil, nil, "chan-a", "en")

	started, stopped, _, _ := sw.snapshot()
	if len(started) != 2 {
		t.Fatalf("started %v, want the leg re-tapped under the kept call", started)
	}
	if len(stopped) != 1 || stopped[0] != "chan-a" {
		t.Fatalf("stopped %v, want the absorbed call's stream ended first", stopped)
	}
	// And the absorbed call retiring must not now take the live tap with it.
	tap.DetachCall(absorbed)
	if err := tap.Pause("chan-a"); err != nil {
		t.Errorf("the re-attached tap answered %v after the absorbed call retired", err)
	}
}

// The reason the key carries an epoch, half one: a continuation from an
// attachment that has already been retired — a failed start racing a re-attach
// on the same channel and the same call — finds nothing to retire. Keyed by
// channel alone the two attachments are the same entry and the first one's
// failure path takes the second one down with it.
//
// The other half, that such a continuation cannot evict its successor from the
// channel index, is what TestReattachingUnderANewCallStopsTheOldStream drives:
// there the replaced attachment is still live when its stream is stopped.
func TestAStaleAttachmentCannotEvictItsSuccessor(t *testing.T) {
	t.Parallel()
	tap, _, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	stale, ok := tap.currentKey("chan-a")
	if !ok {
		t.Fatal("no tap after attach")
	}
	tap.Detach("chan-a")
	tap.Attach(callID, nil, nil, "chan-a", "en")

	if tap.forget(stale) {
		t.Error("a replaced attachment was still considered live")
	}
	if err := tap.Pause("chan-a"); err != nil {
		t.Errorf("the live tap answered %v after a stale key was forgotten", err)
	}
}

// A stream the module ended is a stream we must not then ask it to stop.
//
// Observed on every tapped call (2026-08-18): the channel goes away, the module
// closes the socket, and ~20ms later our Detach issues
// `uuid_audio_stream <uuid> stop` for a channel FreeSWITCH has already
// destroyed — which mod_audio_stream logs at ERR. One per tapped call. A log
// where teardown is always an error is a log where a real error is invisible.
func TestAStreamTheModuleEndedIsNotStoppedAgain(t *testing.T) {
	t.Parallel()
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	// The module closes the stream because the channel went away.
	tap.streamEnded("chan-a", tap.tokenOn("chan-a"))

	// Both teardown paths now find nothing to do.
	tap.Detach("chan-a")
	tap.DetachCall(callID)

	if _, stopped, _, _ := sw.snapshot(); len(stopped) != 0 {
		t.Errorf("stopped %v after the module had already ended the stream", stopped)
	}
}

// A stream ending cannot retire an attachment that belongs to a later call on
// the same channel, which is the whole reason the key carries the call.
func TestAStreamEndingCannotRetireALaterCallsTap(t *testing.T) {
	t.Parallel()
	tap, sw, absorbed := tapFixture(t)
	kept := uuid.Must(uuid.NewV7())
	tap.Attach(absorbed, nil, nil, "chan-a", "en")
	first := tap.tokenOn("chan-a")
	tap.Attach(kept, nil, nil, "chan-a", "en") // the leg was folded into another call

	// The first stream's socket closes, late.
	tap.streamEnded("chan-a", first)

	// The live tap is untouched.
	if err := tap.Pause("chan-a"); err != nil {
		t.Errorf("the live tap answered %v after a stale stream ended", err)
	}
	tap.Detach("chan-a")
	_, stopped, _, _ := sw.snapshot()
	if len(stopped) != 2 {
		t.Errorf("stopped %v, want the absorbed call's stream and then the kept "+
			"call's", stopped)
	}
}

// Only a close the module initiated means the channel is gone. A socket that
// simply dropped is not evidence of anything — the channel may still be up and
// still streaming — so the stop must still be issued.
func TestOnlyAGracefulCloseRetiresTheAttachment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		closeNicely bool
		wantStops   int
	}{
		{name: "the module ends the stream", closeNicely: true, wantStops: 0},
		{name: "the socket just drops", closeNicely: false, wantStops: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := tapRigFor(t)
			rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

			// The module dials back, as it does after +OK, presenting the URL
			// the attach actually handed it.
			conn, _, err := websocket.DefaultDialer.Dial(rig.sw.dialURL("chan-a"), nil)
			if err != nil {
				t.Fatalf("the tap could not connect: %v", err)
			}

			if tc.closeNicely {
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			}
			_ = conn.Close()

			// Let the ingest observe the close before the hangup arrives —
			// which is the real order: the module closes the socket ~20ms
			// before CHANNEL_HANGUP reaches us.
			waitUntil(t, func() bool {
				_, live := rig.tap.currentKey("chan-a")
				return live != tc.closeNicely
			}, "the ingest to notice the stream ended")

			// The channel then hangs up and the coordinator detaches. Once.
			rig.tap.Detach("chan-a")

			_, stopped, _, _ := rig.sw.snapshot()
			if len(stopped) != tc.wantStops {
				t.Errorf("issued %d stops, want %d — a stop for a channel the "+
					"switch has already destroyed is an ERR in every call's log",
					len(stopped), tc.wantStops)
			}
		})
	}
}

// A stream that connects before its attach command returns, and ends before
// the tap arms its expectation, must not be reported as one that never
// connected.
//
// Seen live on 2026-10-20 (call 01a15750-f774-…, tap 01a15751-555c-…): the
// module dialled back at 46.238 and the tap logged its attach in the same
// millisecond; the stream ended at 54.064, inside the 12 s grace, and the
// ingest reported "attached but never connected" at 58.239 for a stream it had
// already transcribed (#287).
func TestAStreamThatConnectsBeforeTheAttachReturnsIsNotReportedMissing(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	dialled := false
	rig.sw.onStart = func(wsURL string) {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Errorf("the tap could not connect inside the attach: %v", err)
			return
		}
		dialled = true
		// Return only once the ingest has recorded the arrival: that is the
		// order this test is about, and a wait makes it the order every run.
		waitUntil(t, func() bool {
			return rig.srv.hasArrived(tokenOf(wsURL))
		}, "the ingest to record the early stream")
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = conn.Close()
	}

	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	if !dialled {
		t.Fatal("the fake switch never dialled back; the rest of this proves nothing")
	}

	if rig.srv.isExpectingAny() {
		t.Error("a stream that arrived before its attach returned armed a watchdog")
	}
	// Firing the check now is what the grace expiring does; it must stay quiet.
	rig.srv.expireExpectations()
	if rig.pub.has("STREAM_NEVER_CONNECTED") {
		t.Errorf("a connected stream was reported as never arriving: %v", rig.pub.all())
	}
}

// A timed-out attach was sent, not failed: the switch may run it late. So the
// attachment stays tracked (a later detach can stop it) and the connect grace
// is the thing that decides whether the stream ever arrived (#312).
func TestATimedOutAttachIsSentNotFailed(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = fmt.Errorf("uuid_audio_stream: %w", esl.ErrTimeout)
	rig.sw.mu.Unlock()

	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

	if _, ok := rig.tap.currentKey("chan-a"); !ok {
		t.Fatal("a timed-out attach was forgotten, so no detach could ever stop its stream")
	}
	if !rig.srv.isExpectingAny() {
		t.Error("a timed-out attach armed no watchdog, so a stream that never comes says nothing")
	}
	if !rig.pub.has("CONNECTING") {
		t.Errorf("states = %v, want CONNECTING for a stream the switch may still start", rig.pub.all())
	}

	// The detach that follows a transfer must still reach the switch.
	rig.tap.Detach("chan-a")
	if _, stopped, _, _ := rig.sw.snapshot(); len(stopped) != 1 {
		t.Errorf("stopped %v, want the timed-out attachment stopped", stopped)
	}
}

// The late stream a timed-out attach eventually produces is tracked: it becomes
// the live stream, and it is the one the next detach stops.
func TestAStreamThatArrivesAfterATimedOutAttachIsTracked(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = fmt.Errorf("uuid_audio_stream: %w", esl.ErrTimeout)
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

	// The switch runs the command late, 41 s after it was written in the live
	// trace, and the module dials back with the token from that attach.
	conn, _, err := websocket.DefaultDialer.Dial(rig.sw.dialURL("chan-a"), nil)
	if err != nil {
		t.Fatalf("the late stream could not connect: %v", err)
	}
	defer conn.Close()

	waitUntil(t, func() bool { return !rig.srv.isExpectingAny() },
		"the late stream to disarm the watchdog")
	rig.srv.expireExpectations()
	if rig.pub.has("STREAM_NEVER_CONNECTED") {
		t.Errorf("a stream that arrived late was reported as never arriving: %v", rig.pub.all())
	}

	rig.tap.Detach("chan-a")
	if _, stopped, _, _ := rig.sw.snapshot(); len(stopped) != 1 {
		t.Errorf("stopped %v, want the stream the switch started late", stopped)
	}
}

// A timed-out attach whose stream really never arrives is still reported. The
// fix keeps the wait honest, it does not silence it.
func TestAnAttachThatTimesOutAndNeverConnectsSaysSo(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = fmt.Errorf("uuid_audio_stream: %w", esl.ErrTimeout)
	rig.sw.mu.Unlock()

	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	rig.srv.expireExpectations()

	if !rig.pub.has("STREAM_NEVER_CONNECTED") {
		t.Errorf("states = %v, want an ERROR naming the stream that never arrived", rig.pub.all())
	}
}

// A detach can overtake the start it is meant to stop: the stop is written
// first, the switch runs the start after it. The late stream must be refused
// rather than left running with nothing owning it (#312).
func TestADetachBeforeALateStreamLeavesNothingRunning(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = fmt.Errorf("uuid_audio_stream: %w", esl.ErrTimeout)
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

	rig.tap.Detach("chan-a")
	if _, stopped, _, _ := rig.sw.snapshot(); len(stopped) != 1 {
		t.Fatalf("stopped %v, want exactly one stop before the late start runs", stopped)
	}

	// The switch gets round to the start only now.
	if conn, _, err := websocket.DefaultDialer.Dial(rig.sw.dialURL("chan-a"), nil); err == nil {
		conn.Close()
		t.Fatal("a stream for a detached attachment was accepted")
	}
	rig.srv.expireExpectations()
	if rig.pub.has("STREAM_NEVER_CONNECTED") {
		t.Errorf("a stream its own call retired was reported as never arriving: %v", rig.pub.all())
	}
}

// Revocation is per attachment. The late stream of a detached attach must not
// be able to join — or pass for — the next attach on the same channel.
func TestALateStreamCannotJoinTheNextAttachOnTheSameChannel(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = fmt.Errorf("uuid_audio_stream: %w", esl.ErrTimeout)
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	first := rig.sw.dialURL("chan-a")

	rig.tap.Detach("chan-a")
	rig.sw.mu.Lock()
	rig.sw.startErr = nil
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

	// The first attach's start runs now, after the re-attach.
	if conn, _, err := websocket.DefaultDialer.Dial(first, nil); err == nil {
		conn.Close()
		t.Error("the detached attachment's late stream was accepted over its successor")
	}

	// The second attach's own stream is the one live stream.
	conn, _, err := websocket.DefaultDialer.Dial(rig.sw.dialURL("chan-a"), nil)
	if err != nil {
		t.Fatalf("the live attach could not connect: %v", err)
	}
	conn.Close()

	waitUntil(t, func() bool { return !rig.srv.isExpectingAny() },
		"the live stream to disarm its watchdog")
	if rig.pub.has("STREAM_NEVER_CONNECTED") {
		t.Errorf("a live re-attach was reported as never arriving: %v", rig.pub.all())
	}
}

// A hangup can retire an attachment while its attach command is still in
// flight — a stalled switch holds the reply for up to ten seconds. When the
// command finally returns, the retire it was meant to become is not an
// expectation again.
func TestADetachWhileTheAttachIsInFlightArmsNothing(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.onStart = func(string) { rig.tap.Detach("chan-a") }

	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

	if rig.srv.isExpectingAny() {
		t.Error("an attachment retired during its attach left a watchdog armed")
	}
	rig.srv.expireExpectations()
	if rig.pub.has("STREAM_NEVER_CONNECTED") {
		t.Errorf("a retired attachment was reported as never arriving: %v", rig.pub.all())
	}
}

// A graceful close is matched by the stream's token, not by its call and
// channel. The leg is detached and re-attached under the same call while the
// first stream is still open; that stream's close, arriving now, must not
// retire the attachment that replaced it, or the hangup that follows sends no
// stop and the second stream runs with nothing owning it.
func TestALateCloseFromAReplacedStreamDoesNotForgetItsSuccessor(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	conn, _, err := websocket.DefaultDialer.Dial(rig.sw.dialURL("chan-a"), nil)
	if err != nil {
		t.Fatalf("the first stream could not connect: %v", err)
	}
	defer conn.Close()
	waitUntil(t, func() bool { return !rig.srv.isExpectingAny() },
		"the first stream to arrive")

	rig.tap.Detach("chan-a")
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	second := rig.tap.tokenOn("chan-a")
	if second == "" {
		t.Fatal("the re-attach left no attachment; the rest of this proves nothing")
	}

	// The first stream's module closes it only now, after its successor exists.
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	waitUntil(t, func() bool { return !rig.srv.hasSessionOn(rig.callID, "chan-a") },
		"the first stream's close to reach the tap")

	if got := rig.tap.tokenOn("chan-a"); got != second {
		t.Error("the replaced stream's close forgot the attachment that replaced it")
	}
	rig.tap.Detach("chan-a")
	if _, stopped, _, _ := rig.sw.snapshot(); len(stopped) != 2 {
		t.Errorf("stopped %v, want the first stream's stop and then its successor's", stopped)
	}
}

// A re-attach of the same leg replaces an attachment whose grace gave up. A
// timed-out start is kept as an attachment (#312); without this, a start the
// switch never ran pins the channel, every later bridge of the leg finds it
// already tapped, and the call is never transcribed again.
func TestAReattachReplacesAnAttachThatNeverConnected(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = fmt.Errorf("uuid_audio_stream: %w", esl.ErrTimeout)
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	first := rig.sw.dialURL("chan-a")

	rig.srv.expireExpectations()
	if !rig.pub.has("STREAM_NEVER_CONNECTED") {
		t.Fatalf("states = %v, want the grace to have given up; the rest of this proves nothing",
			rig.pub.all())
	}

	// The leg is bridged again, and this time the switch answers.
	rig.sw.mu.Lock()
	rig.sw.startErr = nil
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")

	started, stopped, _, _ := rig.sw.snapshot()
	if len(started) != 1 {
		t.Fatalf("started %v, want the re-attach sent to the switch", started)
	}
	if len(stopped) != 1 {
		t.Errorf("stopped %v, want the given-up attachment stopped before its replacement", stopped)
	}

	// The given-up start, if the switch runs it after all, is refused.
	if conn, _, err := websocket.DefaultDialer.Dial(first, nil); err == nil {
		conn.Close()
		t.Error("the replaced attachment's late stream was accepted over its successor")
	}
	conn, _, err := websocket.DefaultDialer.Dial(rig.sw.dialURL("chan-a"), nil)
	if err != nil {
		t.Fatalf("the re-attach's own stream could not connect: %v", err)
	}
	defer conn.Close()
	waitUntil(t, func() bool { return !rig.srv.isExpectingAny() },
		"the re-attach's stream to disarm its watchdog")
}

// Giving up is a report, not a retirement. With no re-attach, the late stream
// of a timed-out start is still owned: it is let in, it moves the panel from
// ERROR to LIVE, the next detach stops it, and once it is here a re-attach of
// the same leg leaves it alone.
func TestALateStreamAfterTheGraceGaveUpIsStillOwned(t *testing.T) {
	t.Parallel()
	rig := tapRigFor(t)
	rig.sw.mu.Lock()
	rig.sw.startErr = fmt.Errorf("uuid_audio_stream: %w", esl.ErrTimeout)
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	token := rig.tap.tokenOn("chan-a")
	rig.srv.expireExpectations()

	conn, _, err := websocket.DefaultDialer.Dial(rig.sw.dialURL("chan-a"), nil)
	if err != nil {
		t.Fatalf("the late stream was refused after the grace gave up: %v", err)
	}
	defer conn.Close()
	waitUntil(t, func() bool { return rig.srv.hasSessionOn(rig.callID, "chan-a") },
		"the late stream to arrive")
	waitUntil(t, func() bool { return rig.pub.has("LIVE") }, "the late stream to go live")
	states := rig.pub.all()
	errAt, liveAt := -1, -1
	for i, st := range states {
		if strings.Contains(st, "STREAM_NEVER_CONNECTED") && errAt < 0 {
			errAt = i
		}
		if strings.HasPrefix(st, "LIVE") {
			liveAt = i
		}
	}
	if errAt < 0 || liveAt < errAt {
		t.Errorf("states = %v, want ERROR and then LIVE", states)
	}
	if rig.srv.hasGivenUp(token) {
		t.Error("a stream that arrived is still marked as given up, so a re-attach would replace it")
	}

	rig.sw.mu.Lock()
	rig.sw.startErr = nil
	rig.sw.mu.Unlock()
	rig.tap.Attach(rig.callID, nil, nil, "chan-a", "en")
	if started, stopped, _, _ := rig.sw.snapshot(); len(started) != 0 || len(stopped) != 0 {
		t.Errorf("started %v stopped %v, want a re-attach to leave the live stream alone",
			started, stopped)
	}

	rig.tap.Detach("chan-a")
	if _, stopped, _, _ := rig.sw.snapshot(); len(stopped) != 1 {
		t.Errorf("stopped %v, want the late stream stopped by the detach", stopped)
	}
}
