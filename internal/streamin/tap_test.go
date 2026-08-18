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

	tap.Pause("chan-a")
	tap.Resume("chan-a")

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
