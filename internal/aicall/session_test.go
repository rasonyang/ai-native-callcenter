// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// Fakes.
//

// fakeLeg stands in for the telephone side.
type fakeLeg struct {
	law     media.Law
	frames  chan []byte
	digits  chan string
	stopped chan struct{}

	mu sync.Mutex
	// sent holds every frame handed to the wire.
	sent [][]byte
	// isSendRefused makes the queue report itself full.
	isSendRefused bool
	// pendingFrames is what the next flush will report as never played. The
	// fake drains instantly, so it is zero unless a test says otherwise.
	pendingFrames int
	clearedCalls  int
	stopOnce      sync.Once
}

func newFakeLeg(law media.Law) *fakeLeg {
	return &fakeLeg{
		law:     law,
		frames:  make(chan []byte, 64),
		digits:  make(chan string, 8),
		stopped: make(chan struct{}),
	}
}

func (l *fakeLeg) ID() string               { return "call-test" }
func (l *fakeLeg) Law() media.Law           { return l.law }
func (l *fakeLeg) Frames() <-chan []byte    { return l.frames }
func (l *fakeLeg) Digits() <-chan string    { return l.digits }
func (l *fakeLeg) Stopped() <-chan struct{} { return l.stopped }

func (l *fakeLeg) Send(frame []byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isSendRefused {
		return false
	}
	l.sent = append(l.sent, append([]byte(nil), frame...))
	return true
}

// ClearTx discards everything queued. The fake plays out instantly, so nothing
// is ever pending unless a test says so.
func (l *fakeLeg) ClearTx() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clearedCalls++
	return l.pendingFrames
}

func (l *fakeLeg) Pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pendingFrames
}

// holdFrames makes the leg report audio still waiting to go out, so a test can
// separate "the model finished" from "the caller has heard it".
func (l *fakeLeg) holdFrames(n int) {
	l.mu.Lock()
	l.pendingFrames = n
	l.mu.Unlock()
}

func (l *fakeLeg) Stop() { l.stopOnce.Do(func() { close(l.stopped) }) }

func (l *fakeLeg) sentFrames() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.sent...)
}

func (l *fakeLeg) clears() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.clearedCalls
}

// fakeModel stands in for the speech provider.
type fakeModel struct {
	events chan provider.Event

	mu           sync.Mutex
	audioIn      [][]byte
	userText     []string
	toolResults  []toolResult
	instructions []string
	interrupts   []interrupt
	sendErr      error
	isClosed     bool
	closeOnce    sync.Once
}

type toolResult struct{ id, output, hint string }
type interrupt struct {
	reason   provider.InterruptReason
	playedMs int
}

func newFakeModel() *fakeModel {
	return &fakeModel{events: make(chan provider.Event, 64)}
}

func (m *fakeModel) Start(context.Context, provider.SessionConfig) error { return nil }
func (m *fakeModel) Events() <-chan provider.Event                       { return m.events }

func (m *fakeModel) SendAudio(audio []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	m.audioIn = append(m.audioIn, append([]byte(nil), audio...))
	return nil
}

func (m *fakeModel) SendUserText(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userText = append(m.userText, text)
	return nil
}

func (m *fakeModel) SendToolResult(id, output, hint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolResults = append(m.toolResults, toolResult{id, output, hint})
	return nil
}

func (m *fakeModel) UpdateInstructions(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instructions = append(m.instructions, text)
	return nil
}

func (m *fakeModel) Interrupt(reason provider.InterruptReason, playedMs int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interrupts = append(m.interrupts, interrupt{reason, playedMs})
	return nil
}

func (m *fakeModel) Close(context.Context) error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.isClosed = true
		m.mu.Unlock()
		close(m.events)
	})
	return nil
}

func (m *fakeModel) receivedAudio() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.audioIn...)
}

func (m *fakeModel) recordedInterrupts() []interrupt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]interrupt(nil), m.interrupts...)
}

func (m *fakeModel) recordedUserText() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.userText...)
}

func (m *fakeModel) failSends(err error) {
	m.mu.Lock()
	m.sendErr = err
	m.mu.Unlock()
}

//
// Harness.
//

// startBridge builds a bridge with the timing guards switched off, so a test
// exercises one behaviour at a time. The guards have tests of their own.
func startBridge(t *testing.T, profile provider.Profile) (*Session, *fakeLeg, *fakeModel) {
	t.Helper()
	return startBridgeWith(t, profile, Config{BargeGuard: -1, NoInput: -1})
}

func startBridgeWith(t *testing.T, profile provider.Profile, cfg Config) (*Session, *fakeLeg, *fakeModel) {
	t.Helper()

	leg := newFakeLeg(media.LawMu)
	model := newFakeModel()

	cfg.Session = provider.SessionConfig{
		Instructions: "answer the phone",
		Turn:         provider.DefaultTurnDetection(),
	}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	session, err := New(leg, model, profile, cfg)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })

	return session, leg, model
}

func awaitBridgeEvent(t *testing.T, session *Session, want EventType) Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	var seen []EventType
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatalf("the stream closed while waiting for %s (saw %v)", want, seen)
			}
			if event.Type == want {
				return event
			}
			seen = append(seen, event.Type)
		case <-deadline:
			t.Fatalf("no %s event arrived (saw %v)", want, seen)
		}
	}
}

// callerFrame builds one frame of caller audio in the leg's law.
func callerFrame(law media.Law, marker byte) []byte {
	frame := media.GetBytes(media.FrameSamples)
	for range media.FrameSamples {
		frame = append(frame, marker)
	}
	return frame
}

//
// Audio path.
//

// Where the provider takes G.711, the caller's bytes must reach it untouched —
// that is the entire point of negotiating passthrough.
func TestCallerAudioReachesTheModelUntouchedOnThePassthroughPath(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	leg.frames <- callerFrame(media.LawMu, 0x42)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if received := model.receivedAudio(); len(received) > 0 {
			frame := received[0]
			if len(frame) != media.FrameSamples {
				t.Fatalf("model got %d bytes, want an untouched %d-byte frame",
					len(frame), media.FrameSamples)
			}
			if frame[0] != 0x42 {
				t.Errorf("the frame was altered: first byte %#x", frame[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("caller audio never reached the model")
}

// Where it does not, the same audio must arrive converted to the rate the
// provider actually speaks.
func TestCallerAudioIsConvertedForAProviderThatNeedsLinearAudio(t *testing.T) {
	session, leg, model := startBridge(t, provider.QwenProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	leg.frames <- callerFrame(media.LawMu, 0x42)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if received := model.receivedAudio(); len(received) > 0 {
			// 160 companded samples at 8 kHz become 320 linear samples at
			// 16 kHz, two bytes each.
			if want := media.FrameSamples * 2 * 2; len(received[0]) != want {
				t.Fatalf("model got %d bytes, want %d", len(received[0]), want)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("caller audio never reached the model")
}

// The model speaks in whatever chunks suit it; the wire needs exact frames.
func TestModelAudioIsCutIntoWireFrames(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	// Two and a half frames of speech, which is not a whole number of frames.
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*2+80),
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frames := leg.sentFrames(); len(frames) >= 2 {
			for i, frame := range frames {
				if len(frame) != media.FrameSamples {
					t.Fatalf("frame %d is %d bytes, want exactly %d",
						i, len(frame), media.FrameSamples)
				}
			}
			// The half frame is held back rather than sent short.
			if len(frames) != 2 {
				t.Errorf("sent %d frames, want the partial one held back", len(frames))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("model audio never reached the wire")
}

// The tail of a sentence is a fraction of a frame. Dropping it clips the last
// word; padding it is inaudible.
func TestTheTailOfATurnIsPaddedAndSent(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 40)}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}

	awaitBridgeEvent(t, session, EventTypeTurnDone)

	frames := leg.sentFrames()
	if len(frames) != 1 {
		t.Fatalf("sent %d frames, want the tail padded into one", len(frames))
	}
	if len(frames[0]) != media.FrameSamples {
		t.Errorf("padded frame is %d bytes", len(frames[0]))
	}
	if frames[0][100] != media.LawMu.Silence() {
		t.Errorf("padding is %#x, want the law's silence byte", frames[0][100])
	}
}

//
// Barge-in.
//

// The caller speaking over the bot must silence it, and the model must be told
// how much was actually heard so its history is not a fiction.
func TestBargeInFlushesLocallyAndReportsWhatWasHeard(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*5),
	}

	// Wait for the audio to be queued before interrupting.
	deadline := time.Now().Add(2 * time.Second)
	for len(leg.sentFrames()) < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	awaitBridgeEvent(t, session, EventTypeBargeIn)

	// Flushing is the part the caller notices, and it must always happen.
	if leg.clears() == 0 {
		t.Error("queued speech was not flushed")
	}

	interrupts := model.recordedInterrupts()
	if len(interrupts) != 1 {
		t.Fatalf("recorded %d interrupts, want 1", len(interrupts))
	}
	if interrupts[0].reason != provider.InterruptReasonSpeech {
		t.Errorf("reason = %q", interrupts[0].reason)
	}
	// Five frames went out and nothing was left queued, so the caller heard
	// all hundred milliseconds.
	if interrupts[0].playedMs != 5*frameDurationMs {
		t.Errorf("reported %dms as heard, want %dms",
			interrupts[0].playedMs, 5*frameDurationMs)
	}
}

// With nothing playing there is nothing to interrupt, and telling the model
// otherwise would truncate a turn that never happened.
func TestSpeechWithNothingPlayingIsNotAnInterruption(t *testing.T) {
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	time.Sleep(200 * time.Millisecond)

	if got := model.recordedInterrupts(); len(got) != 0 {
		t.Errorf("recorded %d interrupts while the bot was silent", len(got))
	}
}

// The provider may cancel a turn on its own. The caller must not go on hearing
// an answer the model has already abandoned.
func TestAProviderInitiatedCancelStopsPlayback(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*3),
	}
	model.events <- provider.Event{
		Type: provider.EventTypeInterrupted, Status: "cancelled",
	}

	event := awaitBridgeEvent(t, session, EventTypeTurnDone)
	if event.Status != "cancelled" {
		t.Errorf("status = %q, want the turn reported as cut short", event.Status)
	}
	if leg.clears() == 0 {
		t.Error("playback continued after the provider abandoned the turn")
	}
}

//
// The barge-in guard.
//

// On a real line the bot's own voice comes back through the caller's handset,
// the detector calls it speech, and the bot interrupts itself mid-greeting.
// The first moments of a turn are therefore not trusted.
func TestSpeechDetectedImmediatelyAfterSpeakingIsIgnored(t *testing.T) {
	session, _, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: 500 * time.Millisecond, NoInput: -1})
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*4),
	}
	// Detected the instant the greeting starts, which is what echo looks like.
	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}

	time.Sleep(200 * time.Millisecond)
	if got := model.recordedInterrupts(); len(got) != 0 {
		t.Errorf("the bot interrupted itself inside the guard window: %+v", got)
	}
}

// Past the window, the caller genuinely is talking over the bot.
func TestSpeechDetectedAfterTheGuardInterrupts(t *testing.T) {
	session, _, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: 100 * time.Millisecond, NoInput: -1})
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*4),
	}
	time.Sleep(200 * time.Millisecond)
	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}

	awaitBridgeEvent(t, session, EventTypeBargeIn)
	if got := model.recordedInterrupts(); len(got) != 1 {
		t.Errorf("recorded %d interrupts, want 1", len(got))
	}
}

// A keypress cannot be an echo of anything, so the guard does not apply to it.
func TestAKeypressInterruptsEvenInsideTheGuard(t *testing.T) {
	session, leg, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: 10 * time.Second, NoInput: -1})
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*4),
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(leg.sentFrames()) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	leg.digits <- "0"
	awaitBridgeEvent(t, session, EventTypeBargeIn)

	interrupts := model.recordedInterrupts()
	if len(interrupts) != 1 || interrupts[0].reason != provider.InterruptReasonDTMF {
		t.Errorf("interrupts = %+v, want the keypress to have cut in", interrupts)
	}
}

//
// Playback completion.
//

// The model finishing a sentence and the caller having heard it are separated
// by everything still queued. A transfer or a goodbye that fires on the first
// cuts the bot off mid-word.
func TestPlaybackCompletionIsReportedSeparatelyFromGeneration(t *testing.T) {
	session, leg, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: -1, NoInput: -1})
	awaitBridgeEvent(t, session, EventTypeReady)

	// Audio is still on its way to the caller when the model finishes.
	leg.holdFrames(5)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*5),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}

	awaitBridgeEvent(t, session, EventTypeTurnDone)

	// Nothing should claim the caller has heard it while frames are queued.
	select {
	case event := <-session.Events():
		if event.Type == EventTypePlaybackDone {
			t.Fatal("playback was reported finished while audio was still queued")
		}
	case <-time.After(150 * time.Millisecond):
	}

	leg.holdFrames(0)
	awaitBridgeEvent(t, session, EventTypePlaybackDone)
}

// An interrupted turn never finishes playing, so nothing should claim it did.
func TestAnInterruptedTurnReportsNoPlaybackCompletion(t *testing.T) {
	session, leg, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: -1, NoInput: -1})
	awaitBridgeEvent(t, session, EventTypeReady)

	leg.holdFrames(5)
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*5),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
	awaitBridgeEvent(t, session, EventTypeTurnDone)

	// The caller cuts in before the queue drains. Speech over the tail is an
	// interruption even though generation has finished: the boundary is the
	// last frame heard, not the last frame made.
	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	awaitBridgeEvent(t, session, EventTypeBargeIn)
	if leg.clears() == 0 {
		t.Error("the tail of the interrupted turn was not flushed")
	}
	leg.holdFrames(0)

	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case event := <-session.Events():
			if event.Type == EventTypePlaybackDone {
				t.Fatal("a turn the caller talked over was reported as fully heard")
			}
		case <-deadline:
			return
		}
	}
}

//
// Dead air.
//

func TestDeadAirIsReportedWhenTheCallerSaysNothing(t *testing.T) {
	session, _, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: -1, NoInput: 150 * time.Millisecond})
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}

	awaitBridgeEvent(t, session, EventTypePlaybackDone)
	awaitBridgeEvent(t, session, EventTypeNoInput)
}

// A caller who answers has not gone quiet, and reporting dead air over them
// would have the bot prompt someone mid-sentence.
func TestACallerWhoSpeaksCancelsTheDeadAirWatch(t *testing.T) {
	session, _, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: -1, NoInput: 300 * time.Millisecond})
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
	awaitBridgeEvent(t, session, EventTypePlaybackDone)

	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}

	deadline := time.After(700 * time.Millisecond)
	for {
		select {
		case event := <-session.Events():
			if event.Type == EventTypeNoInput {
				t.Fatal("dead air was reported over a caller who was speaking")
			}
		case <-deadline:
			return
		}
	}
}

//
// Keypresses.
//

// Someone pressing a key while the bot talks has decided they are done
// listening, and the model has no other way to learn what was pressed.
func TestAKeypressInterruptsAndReachesTheModel(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*4),
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(leg.sentFrames()) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	leg.digits <- "2"

	event := awaitBridgeEvent(t, session, EventTypeDigit)
	if event.Text != "2" {
		t.Errorf("digit = %q", event.Text)
	}

	interrupts := model.recordedInterrupts()
	if len(interrupts) != 1 || interrupts[0].reason != provider.InterruptReasonDTMF {
		t.Errorf("interrupts = %+v, want one caused by the keypress", interrupts)
	}

	for time.Now().Before(deadline) {
		if text := model.recordedUserText(); len(text) > 0 {
			// Phrased as something the caller did, not something they said, so
			// the model does not answer as if they had spoken a number.
			if !strings.Contains(text[0], "pressed 2") {
				t.Errorf("the model was told %q", text[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the keypress never reached the model")
}

//
// Turn latency.
//

// The latency budget is measured per turn: caller stops → first reply frame on
// the wire. Turns without caller speech — the greeting, dead-air prompts — are
// not measurements of anything and must not pollute the histogram.
func TestTurnLatencyIsMeasuredFromSpeechStoppedToFirstFrame(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	// The greeting arrives with no speech before it: no measurement.
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples),
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(leg.sentFrames()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	session.mu.Lock()
	if !session.timer.speechStoppedAt.IsZero() {
		t.Error("a measurement was in flight before any caller speech")
	}
	session.mu.Unlock()

	// A real turn: the caller speaks, stops, and the reply reaches the wire.
	model.events <- provider.Event{Type: provider.EventTypeResponseDone, Status: "completed"}
	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	model.events <- provider.Event{Type: provider.EventTypeSpeechStopped}
	time.Sleep(30 * time.Millisecond) // measurable latency
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples),
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		isClosed := session.timer.speechStoppedAt.IsZero()
		session.mu.Unlock()
		if isClosed && len(leg.sentFrames()) >= 2 {
			return // the measurement opened on speech and closed on the frame
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the turn measurement never closed")
}

// A caller who resumes talking before any reply abandons the measurement:
// there is no answer latency to measure for a turn that never got an answer.
func TestResumedSpeechAbandonsTheMeasurement(t *testing.T) {
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeSpeechStopped}
	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		isAbandoned := session.timer.speechStoppedAt.IsZero()
		session.mu.Unlock()
		if isAbandoned {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the measurement survived the caller resuming speech")
}

//
// Transcripts and tools.
//

func TestTranscriptsAndToolCallsAreReported(t *testing.T) {
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{
		Type: provider.EventTypeInputTranscript, Text: "check my bill", IsFinal: true,
	}
	caller := awaitBridgeEvent(t, session, EventTypeCustomerSaid)
	if caller.Text != "check my bill" || !caller.IsFinal {
		t.Errorf("caller transcript = %q final=%v", caller.Text, caller.IsFinal)
	}

	model.events <- provider.Event{
		Type: provider.EventTypeOutputTranscript, Text: "Of course.", IsFinal: true,
	}
	bot := awaitBridgeEvent(t, session, EventTypeBotSaid)
	if bot.Text != "Of course." {
		t.Errorf("bot transcript = %q", bot.Text)
	}

	model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_1",
		ToolName: "transfer_to_agent", ToolArgs: `{"queue":"billing"}`,
	}
	tool := awaitBridgeEvent(t, session, EventTypeToolCall)
	if tool.ToolCallID != "fc_1" || tool.ToolName != "transfer_to_agent" {
		t.Errorf("tool call = %s/%s", tool.ToolCallID, tool.ToolName)
	}
	if tool.ToolArgs != `{"queue":"billing"}` {
		t.Errorf("tool args = %s", tool.ToolArgs)
	}
}

//
// Lifecycle.
//

// The caller hanging up must end the model session too, or the provider is
// billed for a conversation with nobody on the line.
func TestTheCallerHangingUpEndsTheModelSession(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	leg.Stop()

	awaitBridgeEvent(t, session, EventTypeEnded)

	model.mu.Lock()
	isClosed := model.isClosed
	model.mu.Unlock()
	if !isClosed {
		t.Error("the model session outlived the call")
	}
}

// A model that cannot be fed is a call that cannot continue; leaving it up
// would leave the caller listening to silence.
func TestAModelThatStopsAcceptingAudioEndsTheCall(t *testing.T) {
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.failSends(errors.New("connection reset"))
	leg.frames <- callerFrame(media.LawMu, 0x01)

	event := awaitBridgeEvent(t, session, EventTypeFailed)
	if event.Err == nil {
		t.Error("the failure carried no cause")
	}
	awaitBridgeEvent(t, session, EventTypeEnded)

	select {
	case <-leg.stopped:
	case <-time.After(time.Second):
		t.Error("the telephone leg was left up after the model failed")
	}
}

// The stream must be safe to range over: several goroutines publish to it, and
// closing it while any of them is still running would panic.
func TestTheEventStreamClosesExactlyOnce(t *testing.T) {
	session, leg, _ := startBridge(t, provider.OpenAIProfile())

	drained := make(chan []EventType, 1)
	go func() {
		var seen []EventType
		for event := range session.Events() {
			seen = append(seen, event.Type)
		}
		drained <- seen
	}()

	leg.digits <- "5"
	leg.frames <- callerFrame(media.LawMu, 0x01)
	leg.Stop()

	select {
	case seen := <-drained:
		if len(seen) == 0 || seen[len(seen)-1] != EventTypeEnded {
			t.Errorf("stream ended with %v, want ENDED last", seen)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the event stream never closed")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	session, _, _ := startBridge(t, provider.OpenAIProfile())

	session.Close(context.Background())
	session.Close(context.Background())

	awaitBridgeEvent(t, session, EventTypeEnded)
}

func TestNewRejectsAnIncompleteSession(t *testing.T) {
	if _, err := New(nil, newFakeModel(), provider.OpenAIProfile(), Config{}); err == nil {
		t.Error("a session was built with no leg")
	}
	if _, err := New(newFakeLeg(media.LawMu), nil, provider.OpenAIProfile(), Config{}); err == nil {
		t.Error("a session was built with no model")
	}
}
