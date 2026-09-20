// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// Listening for the caller, in the one window where nobody else is.
//
// The rule these check is where the listening happens, not how well it hears:
// the thresholds are unmeasured defaults, but the window is a decision. While
// the model is speaking, or while what it said is still reaching the caller's
// ear, barge-in belongs to the server's own detector and this one is off.
//

// speechFrame is 20 ms at 16 kHz of something loud enough to be speech.
func speechFrame() []byte {
	return toneFrame(4000)
}

// quietFrame is the same length of comfort noise: a real telephone leg is never
// digitally silent, and a detector tuned on digital silence would hear a line.
func quietFrame() []byte {
	return toneFrame(60)
}

func toneFrame(amplitude float64) []byte {
	const samples = 320
	frame := make([]byte, 2*samples)
	for i := range samples {
		value := int16(amplitude * math.Sin(float64(i)/5.0))
		binary.LittleEndian.PutUint16(frame[2*i:], uint16(value))
	}
	return frame
}

// speak feeds n frames and returns everything the session had to say about it.
func speak(t *testing.T, session *Session, frame []byte, n int) {
	t.Helper()
	for range n {
		if err := session.SendAudio(frame); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
}

// The window: from the server saying the turn is over until the model produces
// anything. Inside it the caller is heard; outside it nothing is said at all.
func TestTheCallerIsHeardOnlyWhileNobodyIsSpeakingToThem(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, _ := pacedSession(t, f)

	// A turn is running. However loud the caller is, the server's own detector
	// is the one that decides, and it says so with an interruption.
	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)
	speak(t, session, speechFrame(), 20)
	refuteMoreEvents(t, session)

	// Generation has finished but the caller is still hearing the turn: the
	// downlink runs about three times faster than they can listen to it.
	f.send(generationComplete())
	expectEvents(t, session, provider.EventTypeResponseDone)
	speak(t, session, speechFrame(), 20)
	refuteMoreEvents(t, session)

	// The server says the playback is over. Now there is nobody else listening.
	// turnComplete is not an event, which is asserted here and is also what
	// makes the frames after it certain to be heard by an armed detector: the
	// read loop takes frames in order.
	f.send(turnComplete())
	refuteMoreEvents(t, session)

	speak(t, session, speechFrame(), framesToStart)
	expectEvents(t, session, provider.EventTypeSpeechStarted)
	refuteMoreEvents(t, session)

	// And the pause that ends their turn is reported once.
	speak(t, session, quietFrame(), defaultSilenceMs/frameMs)
	expectEvents(t, session, provider.EventTypeSpeechStopped)
	speak(t, session, quietFrame(), 30)
	refuteMoreEvents(t, session)

	// The model answering closes the window again, silently: a caller who was
	// still talking was answered, which is not the same as having stopped.
	speak(t, session, speechFrame(), framesToStart)
	expectEvents(t, session, provider.EventTypeSpeechStarted)
	f.send(modelAudio(encoded(0x02)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)
	speak(t, session, quietFrame(), 40)
	refuteMoreEvents(t, session)
}

// A turn that produced nothing to play has nothing playing. Waiting for the
// server to say so would leave the caller unheard for as long as the model takes
// to come back — which, with a function call outstanding, was measured at over a
// minute.
func TestTheCallerIsHeardWhileAToolCallIsOutstanding(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, _ := pacedSession(t, f)

	f.send(toolCallFrame(functionCallOf("fc_1", "lookup_balance", nil)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeToolCall,
		provider.EventTypeResponseDone)

	speak(t, session, speechFrame(), framesToStart)
	expectEvents(t, session, provider.EventTypeSpeechStarted)
	refuteMoreEvents(t, session)
}

// A turn the provider walked away from is never going to be declared over, so
// nothing else would reopen the window.
func TestTheCallerIsHeardAfterAnAbandonedTurn(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)
	session.firstAudioDeadline = 40 * time.Millisecond
	session.deltaStallDeadline = 40 * time.Millisecond
	ticks := make(chan time.Time)
	paced := make(chan struct{})
	session.ticks = ticks
	session.paced = paced
	start(t, session, testConfig())
	t.Cleanup(func() { close(ticks) })

	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted,
		provider.EventTypeAudioDelta,
		provider.EventTypeError,
		provider.EventTypeResponseDone)

	speak(t, session, speechFrame(), framesToStart)
	expectEvents(t, session, provider.EventTypeSpeechStarted)
}

// The caller may speak before the bot has said anything. Nothing is playing yet,
// so they are heard.
func TestTheCallerIsHeardBeforeTheBotHasSpoken(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, _ := pacedSession(t, f)

	speak(t, session, speechFrame(), framesToStart)
	expectEvents(t, session, provider.EventTypeSpeechStarted)
}

// A click, a moment of line noise, or the line itself is not somebody talking.
func TestNoiseOnTheLineIsNotSpeech(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, _ := pacedSession(t, f)

	speak(t, session, quietFrame(), 100)
	refuteMoreEvents(t, session)

	// One loud frame among the quiet ones is not sustained energy.
	for range 10 {
		speak(t, session, speechFrame(), 1)
		speak(t, session, quietFrame(), 5)
	}
	refuteMoreEvents(t, session)
}

// Nothing is said once the session has begun ending: the consumer may already
// have stopped draining, and the call is over either way.
func TestNothingIsHeardOnceTheSessionIsStopping(t *testing.T) {
	f := newFakeGemini(t, acceptSetup)
	session, _ := pacedSession(t, f)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	for range 20 {
		_ = session.SendAudio(speechFrame())
	}

	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeSpeechStarted); got != 0 {
		t.Errorf("the caller was heard %d times after the close (%v)", got, typesOf(events))
	}
}

//
// The detector itself.
//

// The pause that ends the caller's turn is the flow's, not this client's.
func TestThePauseThatEndsATurnIsTheFlowsOwn(t *testing.T) {
	cases := []struct {
		silenceMs  int
		wantFrames int
	}{
		{silenceMs: 500, wantFrames: 25},
		{silenceMs: 800, wantFrames: 40},
		{silenceMs: 0, wantFrames: defaultSilenceMs / frameMs},
		{silenceMs: 10, wantFrames: 1},
	}

	for _, testCase := range cases {
		detector := newSpeechDetector(testCase.silenceMs)
		if got := detector.quietFramesToEnd; got != testCase.wantFrames {
			t.Errorf("a %d ms pause is %d frames, want %d",
				testCase.silenceMs, got, testCase.wantFrames)
		}
	}
}

func TestADisarmedDetectorSaysNothingWhateverItHears(t *testing.T) {
	detector := newSpeechDetector(500)

	for range 100 {
		if got := detector.observe(speechFrame()); got != speechUnchanged {
			t.Fatalf("a disarmed detector reported %v", got)
		}
	}

	detector.arm()
	for i := range framesToStart {
		got := detector.observe(speechFrame())
		isLast := i == framesToStart-1
		if isLast && got != speechBegan {
			t.Errorf("frame %d reported %v, want the caller starting", i, got)
		}
		if !isLast && got != speechUnchanged {
			t.Errorf("frame %d reported %v, want nothing yet", i, got)
		}
	}

	// Disarming forgets the caller was talking: they were answered, and the
	// window that opens next is a new one.
	detector.disarm()
	detector.arm()
	if got := detector.observe(quietFrame()); got != speechUnchanged {
		t.Errorf("a rearmed detector reported %v, want nothing", got)
	}
}

// The line's own noise is what the threshold follows, so a noisy line does not
// report itself as a caller who never stops talking.
func TestTheThresholdFollowsTheLinesOwnNoise(t *testing.T) {
	detector := newSpeechDetector(500)
	detector.arm()

	// A line whose noise floor is well above digital silence.
	noisy := toneFrame(400)
	for range 200 {
		if got := detector.observe(noisy); got != speechUnchanged {
			t.Fatalf("the line's own noise was reported as the caller talking (%v)", got)
		}
	}
	if detector.noiseFloor < 100 {
		t.Errorf("the noise floor settled at %.0f, want it to have followed the line",
			detector.noiseFloor)
	}

	// Speech over that line is still speech.
	var began bool
	for range framesToStart {
		if detector.observe(speechFrame()) == speechBegan {
			began = true
		}
	}
	if !began {
		t.Error("speech over a noisy line was not heard")
	}
}

// This runs fifty times a second for every concurrent call. A detector that
// allocated per frame would be a second media path's worth of garbage.
func TestTheDetectorAllocatesNothingPerFrame(t *testing.T) {
	detector := newSpeechDetector(500)
	detector.arm()
	frame := speechFrame()
	quiet := quietFrame()

	allocations := testing.AllocsPerRun(1000, func() {
		detector.observe(frame)
		detector.observe(quiet)
	})
	if allocations != 0 {
		t.Errorf("the detector allocated %.1f times per frame pair, want none", allocations)
	}
}

func BenchmarkObserveOneFrame(b *testing.B) {
	detector := newSpeechDetector(500)
	detector.arm()
	frame := speechFrame()

	b.ReportAllocs()
	for b.Loop() {
		detector.observe(frame)
	}
}
