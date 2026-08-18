// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	down     bool
	startErr error
}

func (f *fakeSwitch) StartAudioStream(channelID, _ string, _ int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, channelID)
	return nil
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

// tapFixture builds a Tap over a real ingest Server, because Attach mints a
// token and registers an expectation on it — a fake there would be testing the
// fake.
func tapFixture(t *testing.T) (*Tap, *fakeSwitch, uuid.UUID) {
	t.Helper()
	reg := transcript.NewRegistry(nil, &statePub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	callID := uuid.New()
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
	sw := &fakeSwitch{}
	return NewTap(srv, sw, "ws://127.0.0.1:9999/stream", 24000, time.Minute, nopLogger{}), sw, callID
}

// The tap dials the configured URL with a token appended, and the token is
// what the ingest will accept — this is the only place the two halves meet.
func TestAttachStartsOneStreamCarryingAUsableToken(t *testing.T) {
	tap, sw, callID := tapFixture(t)
	agentID, partyID := uuid.New(), uuid.New()

	tap.Attach(callID, &agentID, &partyID, "chan-a", "zh")

	started, _, _, _ := sw.snapshot()
	if len(started) != 1 || started[0] != "chan-a" {
		t.Fatalf("started %v, want one stream on chan-a", started)
	}
}

// One bug per channel. The module enforces it too, and asking twice would
// leave a stream nothing owns.
func TestAttachingTwiceStartsOneStream(t *testing.T) {
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")
	tap.Attach(callID, nil, nil, "chan-a", "en")

	if started, _, _, _ := sw.snapshot(); len(started) != 1 {
		t.Fatalf("started %v, want exactly one", started)
	}
}

// A refused attach must leave nothing behind, or the channel can never be
// tapped again for the life of the call.
func TestARefusedAttachIsNotRemembered(t *testing.T) {
	tap, sw, callID := tapFixture(t)
	sw.startErr = errors.New("-ERR no such channel")

	tap.Attach(callID, nil, nil, "chan-a", "en")
	sw.mu.Lock()
	sw.startErr = nil
	sw.mu.Unlock()
	tap.Attach(callID, nil, nil, "chan-a", "en")

	if started, _, _, _ := sw.snapshot(); len(started) != 1 {
		t.Fatalf("started %v, want the retry to have succeeded", started)
	}
}

// A URL the switch cannot dial is a configuration fault, not a call fault: it
// is refused before any command is sent.
func TestAnUnusableStreamURLNeverReachesTheSwitch(t *testing.T) {
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
	tap, sw, callID := tapFixture(t)
	other := uuid.New()
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
	tap, sw, absorbed := tapFixture(t)
	kept := uuid.New()

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
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	// The module closes the stream because the channel went away.
	tap.streamEnded(callID, "chan-a")

	// Both teardown paths now find nothing to do.
	tap.Detach("chan-a")
	tap.DetachCall(callID)

	if _, stopped, _, _ := sw.snapshot(); len(stopped) != 0 {
		t.Errorf("stopped %v after the module had already ended the stream", stopped)
	}
}

// A stream that dropped without the module ending it is a different case: the
// channel may well be alive and still streaming, so the stop must still go.
func TestAnAbnormalStreamEndStillStopsTheTap(t *testing.T) {
	tap, sw, callID := tapFixture(t)
	tap.Attach(callID, nil, nil, "chan-a", "en")

	// Nothing calls streamEnded: the socket dropped, the module did not close
	// it, and for all we know the channel is still up.
	tap.Detach("chan-a")

	_, stopped, _, _ := sw.snapshot()
	if len(stopped) != 1 || stopped[0] != "chan-a" {
		t.Fatalf("stopped %v, want the tap stopped — a dropped socket is not "+
			"evidence the channel ended", stopped)
	}
}

// A stream ending cannot retire an attachment that belongs to a later call on
// the same channel, which is the whole reason the key carries the call.
func TestAStreamEndingCannotRetireALaterCallsTap(t *testing.T) {
	tap, sw, absorbed := tapFixture(t)
	kept := uuid.New()
	tap.Attach(absorbed, nil, nil, "chan-a", "en")
	tap.Attach(kept, nil, nil, "chan-a", "en") // the leg was folded into another call

	// The first stream's socket closes, late.
	tap.streamEnded(absorbed, "chan-a")

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
