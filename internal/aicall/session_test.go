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

func startBridge(t *testing.T, profile provider.Profile) (*Session, *fakeLeg, *fakeModel) {
	t.Helper()

	leg := newFakeLeg(media.LawMu)
	model := newFakeModel()

	session, err := New(leg, model, profile, Config{
		Session: provider.SessionConfig{
			Instructions: "answer the phone",
			Turn:         provider.DefaultTurnDetection(),
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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
// Transcripts and tools.
//

func TestTranscriptsAndToolCallsAreReported(t *testing.T) {
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{
		Type: provider.EventTypeInputTranscript, Text: "check my bill", IsFinal: true,
	}
	caller := awaitBridgeEvent(t, session, EventTypeCallerSaid)
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
